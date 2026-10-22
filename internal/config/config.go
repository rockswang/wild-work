// Package config 加载 JSON 配置 + 环境变量覆盖，并支持原子写回（GUI 面板修改）。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/backuplink"
	"wild-work/internal/custom"
)

// Listen 监听地址：Host 为空表示全部接口。
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// IsLoopback 是否是「仅本机可访问」的监听地址：环回 IP / localhost / 空主机名
// （空主机名在 Go 里等同 :port，即全部网卡，但旧配置里 ":7863" 曾被当作本机语义，
// 这里按「显式写环回才算安全」处理——空主机名一律视为对外暴露）。
func (l Listen) IsLoopback() bool {
	h := strings.TrimSpace(strings.ToLower(l.Host))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Addr 返回 net.Listen 使用的地址串，如 "127.0.0.1:7863" / ":7863"。
func (l Listen) Addr() string {
	host := l.Host
	port := l.Port
	if port <= 0 {
		port = 7863
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// UnmarshalJSON 兼容旧版字符串形式（":7863" / "127.0.0.1:9999" / "9999"）。
func (l *Listen) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		l2, err := ParseListen(s)
		if err != nil {
			return err
		}
		*l = l2
		return nil
	}
	var o struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	l.Host, l.Port = o.Host, o.Port
	return nil
}

// ParseListen 解析 "host:port" / ":port" / "port" 三种形式。
func ParseListen(s string) (Listen, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Listen{}, fmt.Errorf("empty listen address")
	}
	host, portStr := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, portStr = s[:i], s[i+1:]
	} else if n, err := strconv.Atoi(host); err == nil {
		// 无冒号且整体是数字 → 仅端口（兼容旧配置 "7863"）
		return Listen{Port: n}, nil
	}
	port := 0
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n <= 0 || n > 65535 {
			return Listen{}, fmt.Errorf("bad listen port %q", portStr)
		}
		port = n
	}
	return Listen{Host: host, Port: port}, nil
}

// Config 顶层配置。
type Config struct {
	Listen    Listen `json:"listen"`
	APIKey    string `json:"api_key"`        // 空 = 不鉴权
	AdminPass string `json:"admin_password"` // 空 = 管理面板不鉴权（仅允许监听环回地址时为空）
	AuthDir   string `json:"auth_dir"`       // ./auths
	StateFile string `json:"state_file"`     // ./data/state.json
	Region    string `json:"region"`         // 只收 "cn"

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h" 硬冷却上限
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 默认 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
		// DailyGrantAt 每日额度发放时刻（UTC+8 墙钟 "HH:MM"）。非空时硬冷却
		// （402/余额不足）**截到下一个发奖窗口**而不跑满 hard_credit：额度一到
		// 账就能用，不让固定 12h 把刚补发的额度白锁几小时。
		// 仅适用于「每日固定时刻发放」的渠道（qwenwork 00:00 UTC+8 实测 00:01~00:20
		// 到账）；traework/raccoon 发奖时刻不固定，保持空串 = 不启用。
		DailyGrantAt string `json:"daily_grant_at,omitempty"`
		// DailyGrantBuffer 发奖窗口缓冲（默认 30m）。账本实测 qwenwork 到账
		// 落在 00:01~00:20，取 30m 覆盖实测最晚值：既不在到账前空转撞 402，
		// 也不把解冻拖到发奖之后。
		DailyGrantBuffer string `json:"daily_grant_buffer,omitempty"`
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int    `json:"checkin_hours,omitempty"` // 旧格式：[9,21]
		CheckinTimes   []string `json:"checkin_times,omitempty"` // 新格式：["09:00","21:30"]
		KeepaliveHours []int    `json:"keepalive_hours"`         // [22]
		// 临期阈值已于 2026-10-20 删除（R112）：选号改用「最早到期时刻升序」，
		// 不再需要「N 天内算临期」这个开关；面板临期额度固定按 provider.ExpiringDays 算。
		// 旧配置里的 expiring_threshold_hours 字段会被静默忽略（json 未知字段不报错）。
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
	} `json:"upstream"`

	// Server 入站 HTTP 服务参数。
	Server struct {
		// MaxRequestBodyMB 入站请求体上限（MiB）。0 = 默认 128。
		// 为什么要给到这么高：无状态客户端（Codex）每轮把整段历史原样重放，
		// 多张截图的 base64 会线性累积；gateway 转发前会把图缩到 1568px/JPEG，
		// 所以这个上限只需容纳**客户端原始体**，不代表上游会收到这么大的包（issue #176）。
		MaxRequestBodyMB int `json:"max_request_body_mb,omitempty"`
	} `json:"server"`

	// Login 登录流程的可调项。
	Login struct {
		// LobsterAICallbackPort 固定 LobsterAI 的 OAuth 回调端口（0 = 每次随机）。
		//
		// 为什么需要它：有道 portal 的回调派发器**只认**
		// `http://127.0.0.1:<port>/auth/callback`（hostname 严格等于 127.0.0.1，
		// 否则丢弃该地址、改发 `lobsterai://` deep-link —— 见 docs/决议项.md R97）。
		// 所以「浏览器与网关不同机」（局域网/远程访问面板）时要自动完成回调，
		// 只能让浏览器侧也能连到那个环回端口：在那台机器上建端口转发
		// （`ssh -L 127.0.0.1:<port>:127.0.0.1:<port> <网关>`）。
		// 随机端口无法预建隧道，故允许写死一个端口。留空/0 保持随机（更省心，
		// 但异地浏览器只能用面板的「粘贴回调地址」兜底）。
		LobsterAICallbackPort int `json:"lobsterai_callback_port,omitempty"`
	} `json:"login"`

	// Compat 三接口兼容层（OpenAI Responses / Anthropic Messages）配置。
	// 零值即关闭模型名映射，仅接受 "channel/model" 形式。
	Compat struct {
		// DefaultChannel 无前缀模型名的傅底渠道，如 "workbuddy"。
		DefaultChannel string `json:"default_channel"`
		// ModelMap 裸模型名 → "channel/model"。key 以 * 结尾时按前缀通配匹配。
		ModelMap map[string]string `json:"model_map"`
		// MaxTokensCap 转发上游前对 max_tokens 封顶（0 = 不限制）。
		// Anthropic 客户端常发 64000，而多数上游上限更低，导致直接 400。
		MaxTokensCap int `json:"max_tokens_cap"`
	} `json:"compat"`

	// BackupProxy 应用内**备用上游通道**（单一代理地址，http/https/socks5）。
	// **不参与任何常规请求**：各渠道一律直连；仅当渠道自己判定「上游按地区拒绝」时，
	// 才在同一次请求内经它重放一次（留空 = 无备用通道，失败原样透传）。决议 R57。
	// 当前唯一取用方：cline（muse-spark 系国内直连被地区拒）。oczen 待上游恢复后再接。
	// 本字段只负责持久化，生效态由 internal/backuplink 持有（零接线：渠道按需自取）。
	BackupProxy string `json:"backup_proxy,omitempty"`

	// LegacyProxies 是已废弃的「每渠道上游代理」（旧 `proxies` 段）。保留仅为**读取旧配置**
	// 以便 normalize 一次性迁移进 BackupProxy；迁移后置 nil，下次保存即从文件消失。
	// 不要再新增任何消费方：除 cline 外的渠道代理能力已整体删除。
	LegacyProxies map[string]string `json:"proxies,omitempty"`

	// LegacyProxyNote 旧 proxies 段的迁移结论（供 main 在日志接管后提示；不进 JSON）。
	// 格式：「cline→socks5://…」+ 被丢弃的渠道列表。
	LegacyProxyNote string `json:"-"`

	// OczenAPIKey OpenCodeZen 渠道自定义 API key（sk-...）；空 = 匿名凭证（public）。
	// 自定义 key 有独立配额（不受匿名通道共享限流），且可调用付费模型（需账户余额）。
	OczenAPIKey string `json:"oczen_api_key,omitempty"`

	// BodyLog 诊断开关：把每次请求的「上下文构成摘要」写入 data/ledger/body-trace-*.jsonl，
	// 用于定位上下文为何膨胀（哪条消息/哪段文本占了大头）。默认关。
	// 摘要含消息内容前 200 字预览，属临时排查工具，用完请关掉。
	BodyLog bool `json:"body_log,omitempty"`

	// ModelFallback 模型降级链：请求模型在该渠道全部账号上限流冷却（no_healthy_account
	// 的「模型全冷」形态）时，按 To 顺序找第一个仍有可用账号的备胎模型顶上，
	// 响应头 X-Model-Fallback 标注实际使用的备胎。仅全冷场景触发，正常路由零开销；
	// 不粘滞——每个请求都按原模型重新判定，上游解冻即自动切回。
	// From/To 均为 "channel/model" 全路径；面板可视化编辑，保存走 POST /api/config/model_fallback。
	ModelFallback []ModelFallbackRule `json:"model_fallback,omitempty"`

	// ReasoningEfforts 思考强度注入映射：客户端请求体**未带** reasoning_effort 键时，
	// 按请求模型 ID（"channel/model" 全路径）注入配置档位；`*` 为全局默认（精确未命中时回退）。
	// 键已存在（任意值含 null，客户端显式意图）一律不碰；未配置（空 map）= 零开销直通。
	// 哑管道：不校验档位取值合法性，上游吃什么档位由渠道模型自己说了算。
	// 面板可视化编辑，保存走 POST /api/config/reasoning_efforts。
	ReasoningEfforts map[string]string `json:"reasoning_efforts,omitempty"`

	// Features 功能开关（面板「设置」可切换，默认全部开启）。
	Features struct {
		// LoopGuard 思考链死循环检测（发癫拦截）：流式生成中检测推理复读/空转，
		// 命中即中断 SSE 并返回错误，由下游 agent 自行重试。默认 true。
		LoopGuard bool `json:"loop_guard"`
	} `json:"features"`

	// CustomChannels 自定义第三方 OpenAI 兼容渠道列表（R66）。
	CustomChannels []custom.ChannelConfig `json:"custom_channels,omitempty"`

	// Updater 自动更新配置。
	Updater UpdaterConfig `json:"updater"`

	// 解析后
	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
	// DailyGrantAt 每日发奖窗口（UTC+8 墙钟时刻）。注意 00:00 解析后是 0，
	// 与「未配置」不可区分，故是否启用看 DailyGrantEnabled。
	DailyGrantAt time.Duration `json:"-"`
	// DailyGrantEnabled 是否启用「硬冷却截到发奖窗口」策略（配置了 daily_grant_at 即启用）。
	DailyGrantEnabled bool `json:"-"`
	// DailyGrantBufferDur 发奖窗口缓冲（默认 30m）。
	DailyGrantBufferDur time.Duration `json:"-"`
}

// UpdaterConfig 自动更新相关配置。
type UpdaterConfig struct {
	Enabled            bool     `json:"enabled"`              // 是否启用更新检测，默认 true
	AutoCheck          bool     `json:"auto_check"`           // 是否启动及后台定期自动检测，默认 true
	CheckIntervalHours int      `json:"check_interval_hours"` // 检测间隔小时数，默认 6
	Repo               string   `json:"repo"`                 // GitHub 仓库，默认 "rockswang/wild-work"
	Proxies            []string `json:"download_proxies,omitempty"` // 公共加速代理前缀列表
}

// ModelFallbackRule 单条模型降级规则：From（"channel/model" 全路径）在其渠道内
// 全部账号上限流冷却时，按 To 顺序找第一个仍有可用账号的备胎模型顶上。
// 约束：From 全局唯一；To 内不含 From（不自环）；To 至少一项。
type ModelFallbackRule struct {
	From string   `json:"from"`
	To   []string `json:"to"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    Listen{Host: "127.0.0.1", Port: 7863},
		APIKey:    "WildWorkAPI",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		Region:    "cn",
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	// 每日发奖窗口：qwenwork 每日 00:00（UTC+8）服务端自动发放，账本实测
	// 00:01~00:20 到账。硬冷却据此截到「下个发奖窗口 + 缓冲」，不再跑满 12h。
	c.Cooldown.DailyGrantAt = "00:00"
	c.Cooldown.DailyGrantBuffer = "30m"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.CheckinTimes = []string{"09:00", "21:00"}
	c.Schedule.KeepaliveHours = []int{22}
	c.Upstream.TimeoutSeconds = 120
	c.Compat.DefaultChannel = "workbuddy"
	c.Compat.MaxTokensCap = 32000
	c.Features.LoopGuard = true // 思考链死循环检测默认开启（面板可关）
	c.Updater.Enabled = true
	c.Updater.AutoCheck = true
	c.Updater.CheckIntervalHours = 6
	c.Updater.Repo = "rockswang/wild-work"
	c.Updater.Proxies = []string{"https://gh-proxy.com/", "https://ghfast.top/"}
	return c
}

// Load 从文件读，再用 WILDWORK_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		// Default() 带有新字段默认值；老配置没有 checkin_times 时必须让旧的
		// checkin_hours 生效，而不能被 Default 的 [09:00,21:00] 覆盖。
		var shape struct {
			Schedule map[string]json.RawMessage `json:"schedule"`
		}
		if json.Unmarshal(raw, &shape) == nil {
			if _, ok := shape.Schedule["checkin_times"]; !ok {
				c.Schedule.CheckinTimes = nil
			}
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Save 以 0600 原子写回配置。
func Save(c *Config, path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ParseClockTimes 将 HH:MM 列表转换为当天分钟数（0..1439）。
func ParseClockTimes(values []string) ([]int, error) {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		parts := strings.Split(strings.TrimSpace(v), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid time %q", v)
		}
		h, errH := strconv.Atoi(parts[0])
		m, errM := strconv.Atoi(parts[1])
		if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return nil, fmt.Errorf("invalid time %q", v)
		}
		minute := h*60 + m
		if !seen[minute] {
			seen[minute] = true
			out = append(out, minute)
		}
	}
	sort.Ints(out)
	return out, nil
}

// parseClockHM 解析 "HH:MM" 为当天分钟偏移（0 ~ 1439）。
func parseClockHM(v string) (time.Duration, error) {
	mins, err := ParseClockTimes([]string{v})
	if err != nil || len(mins) != 1 {
		return 0, fmt.Errorf("invalid HH:MM %q", v)
	}
	return time.Duration(mins[0]) * time.Minute, nil
}

// FormatClockTimes 将当天分钟数格式化为排序后的 HH:MM 列表。
func FormatClockTimes(minutes []int) []string {
	out := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	formatted := make([]string, 0, len(out))
	for _, m := range out {
		formatted = append(formatted, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return formatted
}

func applyEnv(c *Config) {
	if v := os.Getenv("WILDWORK_UPDATER_REPO"); v != "" {
		c.Updater.Repo = v
	}
	if v := os.Getenv("WILDWORK_LISTEN"); v != "" {
		if l, err := ParseListen(v); err == nil {
			c.Listen = l
		}
	}
	if v := os.Getenv("WILDWORK_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WILDWORK_ADMIN_PASSWORD"); v != "" {
		c.AdminPass = v
	}
	if v := os.Getenv("WILDWORK_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WILDWORK_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WILDWORK_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("WILDWORK_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("WILDWORK_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WILDWORK_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("WILDWORK_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("WILDWORK_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WILDWORK_DEFAULT_CHANNEL"); v != "" {
		c.Compat.DefaultChannel = v
	}
	if v := os.Getenv("WILDWORK_MAX_TOKENS_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Compat.MaxTokensCap = n
		}
	}
	if v := os.Getenv("WILDWORK_LOOP_GUARD"); v != "" {
		// 环境变量覆盖："0"/"false"/"no" 关闭，其余开启。
		c.Features.LoopGuard = !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no"))
	}
}

// migrateLegacyProxies 把旧「每渠道代理」段（proxies）迁移成全局备用通道（决议 R57）。
// 取值优先级：cline（当前唯一取用方）> 按渠道名排序后的第一个非空值；其余丢弃。
// 迁移后把 LegacyProxies 置 nil，下一次 config.Save 就让旧段从文件里消失。
// 返回迁移说明（无旧段时返回空串，不进日志）。
func migrateLegacyProxies(c *Config) string {
	if len(c.LegacyProxies) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.LegacyProxies))
	for k, v := range c.LegacyProxies {
		if strings.TrimSpace(v) == "" {
			continue // 空值本来就是「直连」，不算被丢弃
		}
		names = append(names, k)
	}
	sort.Strings(names)
	picked := ""
	if v := strings.TrimSpace(c.LegacyProxies["cline"]); v != "" {
		picked = "cline"
	} else if len(names) > 0 {
		picked = names[0]
	}
	var dropped []string
	for _, n := range names {
		if n != picked {
			dropped = append(dropped, n)
		}
	}
	note := ""
	if picked != "" {
		raw := strings.TrimSpace(c.LegacyProxies[picked])
		_, perr := backuplink.Parse(raw)
		switch {
		case strings.TrimSpace(c.BackupProxy) != "":
			// 新字段已配 ⇒ 以 backup_proxy 为准，旧值只丢弃不迁移
			note = fmt.Sprintf("已同时配置 backup_proxy 与旧 proxies[%s]，以 backup_proxy 为准", picked)
		case perr != nil:
			// 旧值本身不合法 ⇒ **不迁移**（否则多年前的脏值会把启动直接 fatal），只记日志
			note = fmt.Sprintf("旧 proxies[%s]=%q 地址不合法，未迁移为备用上游通道", picked, raw)
		default:
			c.BackupProxy = raw
			note = fmt.Sprintf("旧 proxies 段已迁移为备用上游通道（取自 %s=%s）", picked, raw)
		}
	}
	if len(dropped) > 0 {
		note += fmt.Sprintf("；%s 的旧代理已丢弃（该渠道不再支持代理，一律直连）", strings.Join(dropped, ","))
	}
	c.LegacyProxies = nil
	return note
}

func (c *Config) normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	// 每日发奖窗口：空串 = 不启用（走完整 hard_credit）；非空必须是合法 "HH:MM"。
	if c.Cooldown.DailyGrantAt != "" {
		c.DailyGrantEnabled = true
		if c.DailyGrantAt, err = parseClockHM(c.Cooldown.DailyGrantAt); err != nil {
			return fmt.Errorf("cooldown.daily_grant_at: %w", err)
		}
		if c.Cooldown.DailyGrantBuffer == "" {
			c.Cooldown.DailyGrantBuffer = "30m"
		}
		if c.DailyGrantBufferDur, err = time.ParseDuration(c.Cooldown.DailyGrantBuffer); err != nil {
			return fmt.Errorf("cooldown.daily_grant_buffer: %w", err)
		}
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Compat.MaxTokensCap < 0 {
		c.Compat.MaxTokensCap = 0 // 负数视为「不限制」，避免误用导致 max_tokens 被置 0
	}
	// 入站请求体上限：0 = 默认；负数/过大一律钳到合理区间（下限 8MiB 沿用历史最低值，
	// 上限 1024MiB 防手滑写成天文数字把内存撑爆）。
	if c.Server.MaxRequestBodyMB < 0 {
		c.Server.MaxRequestBodyMB = 0
	}
	if c.Server.MaxRequestBodyMB > 0 && c.Server.MaxRequestBodyMB < 8 {
		c.Server.MaxRequestBodyMB = 8
	}
	if c.Server.MaxRequestBodyMB > 1024 {
		c.Server.MaxRequestBodyMB = 1024
	}
	if c.Listen.Port <= 0 {
		c.Listen.Port = 7863
	}
	c.AdminPass = strings.TrimSpace(c.AdminPass)
	// 备用上游通道：先把旧 `proxies` 段一次性迁移（决议 R57），再校验现值。
	// 迁移取值优先级：cline（当前唯一取用方）> 按渠道名排序后的第一个非空值；
	// 其余地址一律丢弃并记入提示 —— 渠道侧代理逻辑已删除，留着只会造成「配了就生效」的错觉。
	if note := migrateLegacyProxies(c); note != "" {
		c.LegacyProxyNote = note
	}
	if raw := strings.TrimSpace(c.BackupProxy); raw != "" {
		if _, err := backuplink.Parse(raw); err != nil { // 判据与面板保存同源（避免两处漂移）
			return err
		}
		c.BackupProxy = raw
	} else {
		c.BackupProxy = ""
	}
	if c.Region == "" {
		c.Region = "cn"
	}
	c.Region = strings.ToLower(c.Region)
	if c.Region != "cn" && c.Region != "global" {
		return fmt.Errorf("region must be cn or global, got %q", c.Region)
	}
	// 兼容旧版 checkin_hours；新版本统一规范化为 HH:MM。
	if len(c.Schedule.CheckinTimes) == 0 {
		c.Schedule.CheckinTimes = make([]string, 0, len(c.Schedule.CheckinHours))
		for _, h := range c.Schedule.CheckinHours {
			if h < 0 || h > 23 {
				return fmt.Errorf("schedule.checkin_hours: hour out of range: %d", h)
			}
			c.Schedule.CheckinTimes = append(c.Schedule.CheckinTimes, fmt.Sprintf("%02d:00", h))
		}
	}
	mins, err := ParseClockTimes(c.Schedule.CheckinTimes)
	if err != nil {
		return fmt.Errorf("schedule.checkin_times: %w", err)
	}
	c.Schedule.CheckinTimes = FormatClockTimes(mins)
	if len(c.Schedule.CheckinHours) == 0 {
		for _, m := range mins {
			if m%60 == 0 {
				c.Schedule.CheckinHours = append(c.Schedule.CheckinHours, m/60)
			}
		}
	}
	if c.Updater.CheckIntervalHours <= 0 {
		c.Updater.CheckIntervalHours = 6
	}
	if c.Updater.Repo == "" {
		c.Updater.Repo = "rockswang/wild-work"
	}
	if len(c.Updater.Proxies) == 0 {
		c.Updater.Proxies = []string{"https://gh-proxy.com/", "https://ghfast.top/"}
	}
	seenCustom := map[string]bool{}
	for i := range c.CustomChannels {
		ch := &c.CustomChannels[i]
		if err := ch.Validate(); err != nil {
			return fmt.Errorf("custom_channels[%d]: %w", i, err)
		}
		if seenCustom[ch.Name] {
			return fmt.Errorf("custom_channels[%d]: duplicate channel name %q", i, ch.Name)
		}
		seenCustom[ch.Name] = true
	}
	return nil
}

// hardCooldownLoc 每日发奖窗口的时区口径：固定 UTC+8 墙钟（与各渠道上游一致）。
var hardCooldownLoc = time.FixedZone("UTC+8", 8*60*60)

// HardCooldownFor 返回一次「硬冷却」（402 余额/权益不足）应持续多久。
//
// 为什么不是固定 HardCreditDur：qwenwork 等渠道每日 00:00（UTC+8）服务端自动
// 发放额度（账本实测 00:01~00:20 到账）。固定 12h 会把「19:07 撞 402」锁到次日
// 07:07——额度早在 00:20 就到账了，却被白锁 7 小时。故当 DailyGrantAt 已配置时，
// 冷却截到「下一个发奖窗口 + 缓冲」，让解冻点落在额度到账之后、且不早于到账
// （避免解冻后立刻再撞 402）。窗口配置为空时退回完整 HardCreditDur（老行为）。
//
// 上限保护：截断只会缩短、不会延长——超过 HardCreditDur 时仍按 HardCreditDur。
func (c *Config) HardCooldownFor(now time.Time) time.Duration {
	base := c.HardCreditDur
	if !c.DailyGrantEnabled {
		return base
	}
	until := NextDailyGrant(now, c.DailyGrantAt, c.DailyGrantBufferDur)
	d := until.Sub(now)
	if d <= 0 || d > base {
		return base
	}
	return d
}

// NextDailyGrant 返回 now 之后（严格晚于）最近一个「发奖时刻 + 缓冲」。
// 已过当日窗口则顺延到次日；时刻按 UTC+8 墙钟解释，与上游发奖口径一致。
func NextDailyGrant(now time.Time, at, buffer time.Duration) time.Time {
	local := now.In(hardCooldownLoc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, hardCooldownLoc)
	next := day.Add(at + buffer)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
