// client.go WorkBuddy 国际版上游客户端：对话 / 刷新 / 目录 / 余额 / 签到，
// 实现 provider.Upstream 接口。
package workbuddyai

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
)

// Client 国际版上游 HTTP 客户端。Base 可覆盖以便测试。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 用于对话流（SSE）。与 traework/glm 同款模式（见 R35）：
	// 流式请求不得设 Client.Timeout——它覆盖整个响应体读取，长回答会在超时点被
	// 掐断且无终止帧（issue #42 实测：tool_call 写到一半被断）。
	StreamHTTP *http.Client
	// IdleTimeout 聊天 SSE 流中空闲超时（StreamHTTP 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration
	Base       string // 默认 https://www.workbuddy.ai
}

// New 生产默认。配置连接池减少 TLS 握手。
func New() *Client { return NewWithTimeout(120 * time.Second) }

// NewWithTimeout 指定上游超时。Transport 与 CN 同构：禁 h2 + Dial/keepalive/TLS 握手 + ResponseHeaderTimeout。
func NewWithTimeout(timeout time.Duration) *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &Client{
		HTTP:        &http.Client{Timeout: timeout, Transport: tr},
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport（连接池复用），不设 Timeout
		IdleTimeout: idle.DefaultTimeout,          // 流中空闲监控兜底（见 internal/idle）
		Base:        WBAIHost,
	}
}

// NewWithBase 测试用：覆盖 base。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

func (c *Client) base() string {
	if c.Base == "" {
		return WBAIHost
	}
	return c.Base
}

// ---------------------------------------------------------------------------
// 请求头
// ---------------------------------------------------------------------------

// commonHeaders 所有 API 共享的头。
func commonHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originRef)
	req.Header.Set("Referer", originRef+"/")
	req.Header.Set("User-Agent", clientUA)
}

// chatHeaders 对话专有头：Authorization 族 + X-No-* 缺省约定。
// 安全红线：绝不携带 X-Refresh-Token。
func chatHeaders(req *http.Request, a *auth.Auth) {
	commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	req.Header.Set("X-Domain", wbaiDomain)
	injectAttributionWBAI(req)
	// 会话头族
	mid := newMessageIDWBAI()
	req.Header.Set("X-Conversation-Request-ID", mid)
	req.Header.Set("X-Conversation-Message-ID", mid)
	req.Header.Set("X-Request-ID", mid)
	req.Header.Set("X-Root-Request-ID", mid)
}

func injectAttributionWBAI(req *http.Request) {
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", "5.5.4")
	req.Header.Set("X-Product", "WorkBuddy")
}

func newMessageIDWBAI() string {
	var b [16]byte
	n := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		b[i] = byte(n >> (i * 8))
		b[15-i] = byte(n >> ((7 - i) * 8))
	}
	return hex.EncodeToString(b[:])
}

// billingHeaders 余额 / 签到接口头。
func billingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Domain", wbaiDomain)
}

// refreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
// refreshHeaders refresh 端点专属头。调用方必须持有 a.Lock()，本函数直接读 RefreshToken。
func refreshHeaders(req *http.Request, a *auth.Auth) {
	commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

// ---------------------------------------------------------------------------
// 信封与错误
// ---------------------------------------------------------------------------

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带分类的 *provider.Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == provider.ErrNone {
			kind = provider.ErrClient
		}
		return nil, &provider.Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// RefreshToken 刷新 access token；成功时更新 a 的字段，调用方负责 SaveAtomic。
// 全程持 a 锁，防止并发读写半更新 token。refreshToken 会轮换，必须写回。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	log.Printf("workbuddyai refresh start uid=%s", a.UID)
	req, err := http.NewRequest(http.MethodPost, c.base()+EpRefresh, nil)
	if err != nil {
		return err
	}
	refreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("workbuddyai refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		err := fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
		log.Printf("workbuddyai refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	oldRefresh := a.RefreshToken
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// 响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	log.Printf("workbuddyai refresh success uid=%s refresh_rotated=%t expires_at=%d",
		a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

// ChatStream 发对话请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc=nil、respBody=上游响应体（供调用方 Classify）、err=nil；
// 仅传输层失败才返回 err。
//
// 对网关类 5xx（502/503/504）与瞬态传输错误做有限重试（见 retry.go）；
// 4xx 不重试（风控/余额/参数语义，重试无益）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	prepared := PrepareBody(body)
	for attempt := 1; attempt <= retryMaxAttempts; attempt++ {
		rc, status, respBody, err = c.chatStreamOnce(a, prepared)
		// 成功或 4xx：直接返回，不重试
		if err == nil && !retryableStatus(status) {
			return rc, status, respBody, nil
		}
		// 传输层错误：仅在可重试时继续
		if err != nil && !retryableErr(err) {
			return nil, 0, nil, err
		}
		if attempt == retryMaxAttempts {
			break
		}
		why := fmt.Sprintf("http %d", status)
		if err != nil {
			why = err.Error()
		}
		wait := backoff(attempt)
		log.Printf("workbuddyai chat_stream uid=%s: %s，%v 后重试 (%d/%d)",
			a.UID, truncate(why, 80), wait.Round(time.Millisecond), attempt, retryMaxAttempts)
		time.Sleep(wait)
	}
	return rc, status, respBody, err
}

// chatStreamOnce 单次对话请求（不做重试）。
func (c *Client) chatStreamOnce(a *auth.Auth, prepared []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.base()+EpChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	chatHeaders(req, a)
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		log.Printf("workbuddyai chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("workbuddyai chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, Classify(resp.StatusCode, string(raw)), truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 idle.Monitor（其 Close 会 cancel；静默超时也会 cancel）。
	return idle.Monitor(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// FetchModels 拉取国际版模型目录。
// 目录内模型剔除 brokenModels（实测 11102 不可用），再补入 extraModels
// （目录不返回但实测可用，含免费模型 deepseek-v4.1-flash）。
// FetchModels 返回当前可用模型清单（issue #39 起为 v2 ∪ v3 合并口径）。
//
// 目录来源两层：
//   - /v2/enterprises/personal/models（cli agent 面，权威顺序）；
//   - /v3/config（全量面，2026-09-28 实测可用，带能力与倍率字段）。
//
// v3 失败不拖累主路：退化为原 v2+extraModels 行为。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	raws, err := c.fetchCatalog(a)
	if err != nil {
		return nil, err
	}
	// v3 增强：补 v2 缺失的模型、以 v3 的能力/容量字段修正重复项。
	// v3 里的模型也要过 brokenModels/disabled 过滤（同一套口径）。
	if v3, verr := c.fetchV3Catalog(a); verr == nil {
		raws, _ = mergeV3(raws, v3)
	}
	seen := make(map[string]bool, len(raws)+len(extraModels))
	out := make([]provider.ModelInfo, 0, len(raws)+len(extraModels))
	for _, m := range raws {
		if m.ID == "" || m.Disabled || brokenModels[m.ID] || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, provider.ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			// 目录接口真实返回的容量
			ContextFromAPI: true,
			// 能力：直接透传上游声明（不自行纠正上游与实际不符的情况）
			SupportsImages:    m.imageOK(),
			SupportsReasoning: m.SupportsReasoning,
			SupportsTools:     m.SupportsToolCall,
		})
	}
	// 硬编码补入目录外可用模型
	for _, m := range extraModels {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// FetchModelPricing 拉取模型倍率。费率面板「仅供参考」，
// 因此直接引用目录返回的 credits，不做增删（不含 extraModels、不剔除 brokenModels）。
//
// issue #39：v2 目录不含 deepseek-v4.1-flash/gpt-6-astra/kimi-k2.8-preview 等
// extraModels，它们此前永远显示 unknown。/v3/config 对这批模型返回了真实倍率
// （x0.00 / x6.67 / x0.77，2026-09-28 实测），故在 v2 基础上并入 v3 的倍率表。
// v3 也没有的模型（deepseek-v3/glm-5.1/glm-5v-turbo/hy4-preview-f/kimi-k2.7/
// minimax-m3）上游确实无费率数据，继续由面板显示 unknown——不编数据。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	raws, err := c.fetchCatalog(a)
	if err != nil {
		return nil, err
	}
	// v3 倍率并入（同 id 时 v3 非空优先，v3 独有模型直接新增）
	var v3Rates map[string]string
	if v3, verr := c.fetchV3Catalog(a); verr == nil {
		raws, v3Rates = mergeV3(raws, v3)
	}
	// 促销折扣（modelPromotions）优先：discount.factor==0 表示当前免费。
	promo := c.fetchPromotions(a)
	out := make([]provider.ModelPricing, 0, len(raws))
	for _, m := range raws {
		if m.ID == "" {
			continue
		}
		rate := parseCredits(m.Credits)
		note := badgeOf(m.ID)
		if r, ok := promo[m.ID]; ok {
			rate = r
			if note == "" {
				note = "限时免费"
			}
		}
		out = append(out, provider.ModelPricing{
			Model:   m.ID,
			Channel: ChannelName,
			Rate:    rate,
			Note:    note,
			Color:   colorOf(m.ID),
		})
	}
	// v3 独有但 mergeV3 后仍在 raws 里的条目已含 credits；
	// 这里补一层：v3 有倍率而 v2 条目 credits 为空的（如 v3 面更新的情况）。
	if v3Rates != nil {
		for i := range out {
			if r, ok := v3Rates[out[i].Model]; ok && parseCredits(r) > 0 && out[i].Rate == 0 {
				out[i].Rate = parseCredits(r)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pricing api returned empty models")
	}
	return out, nil
}

// UserResource 查询账号可花费积分余额（所有套餐 CycleCapacity* 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	total, _, err := c.userResource(a)
	return total, err
}

// UserResourceDetail 查询积分明细（所有套餐条目）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return c.userResource(a)
}

// softRateResetLoc 上游墙钟时间口径：固定按 UTC+8 解释。
// 上游下发的 CycleEndTime 等时间串均为国内时区墙钟，用 time.Local 解析会在
// 非 UTC+8 机器上把到期日算错一天。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// userResource 单次请求同时产出总额与明细。
func (c *Client) userResource(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              ProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	req, err := http.NewRequest(http.MethodPost, c.base()+EpUserResource, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	billingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					// CycleEndTime 到期时间（"2006-01-02 15:04:05"，UTC+8 墙钟）。
					CycleEndTime string `json:"CycleEndTime"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, nil, fmt.Errorf("resource parse: %w", err)
	}
	var total int64
	items := make([]provider.ResourceItem, 0, len(resp.Response.Data.Accounts))
	for _, acct := range resp.Response.Data.Accounts {
		// 周期额度 > 0 时以周期口径为准，否则回退容量口径（与国内版一致）。
		var tot, used, remain int64
		switch {
		case acct.CycleCapacitySize > 0:
			tot, used, remain = acct.CycleCapacitySize, acct.CycleCapacityUsed, acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			tot, used, remain = acct.CycleCapacityRemain+acct.CycleCapacityUsed, acct.CycleCapacityUsed, acct.CycleCapacityRemain
		default:
			tot, used, remain = acct.CapacitySize, acct.CapacityUsed, acct.CapacityRemain
		}
		if remain < 0 {
			remain = 0
		}
		total += remain
		items = append(items, provider.ResourceItem{
			Name:     acct.PackageName,
			Total:    tot,
			Used:     used,
			Remain:   remain,
			ExpireAt: expireDate(acct.CycleEndTime),
			// 套餐名+到期日组合伪键（同国内版口径）
			Key:    fmt.Sprintf("%s|%s", acct.PackageName, expireDate(acct.CycleEndTime)),
			Usable: true, // 国际版无端点分区，所有套餐均可被本工具消耗
		})
	}
	return total, items, nil
}

// expireDate 把上游墙钟时间串（UTC+8）转为 YYYY-MM-DD；缺失/不可解析时返回空串。
func expireDate(ts string) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, softRateResetLoc); err == nil {
		return t.Format("2006-01-02")
	}
	return ""
}

// DailyCheckin 国际版的「每日活跃」任务：用免费模型对话一次保持账号活跃，
// 并顺带探测签到活动是否开启（开启则自动领取）。
//
// 之所以复用 DailyCheckin 接口：调度器已在每个签到时刻对每个绑定账号调用它，
// 正好满足「签到定时器每次触发时每个账号对话一次」的要求，无需新增调度路径。
//
// 对用户透明：不产生前端界面、不写签到状态标记（RecordCheckin 由调度器按
// 返回结果自行处理，这里始终返回 nil 以避免污染签到状态）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	// 1) 签到活动探测 + 领取（国际版当前通常返回 10001 未开启，属正常）
	c.tryClaimCheckin(a)
	// 2) 免费模型对话一次，保持活跃
	c.pokeActivity(a)
	return nil
}

// tryClaimCheckin 探测签到活动；仅当 active 且未领取时才调用领取端点。
// 任何失败静默忽略（活动未开启是常态，不应污染账号健康状态）。
func (c *Client) tryClaimCheckin(a *auth.Auth) {
	req, err := http.NewRequest(http.MethodPost, c.base()+"/billing/meter/checkin-activity-status", bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	billingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("workbuddyai activity probe uid=%s: %v", a.UID, truncate(err.Error(), 120))
		return
	}
	var st struct {
		Active         bool `json:"active"`
		TodayCheckedIn bool `json:"today_checked_in"`
		DailyCredit    int  `json:"daily_credit"`
	}
	if json.Unmarshal(data, &st) != nil || !st.Active || st.TodayCheckedIn {
		return
	}
	req2, err := http.NewRequest(http.MethodPost, c.base()+EpDailyCheckin, bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	billingHeaders(req2, a)
	if _, err := c.doJSON(req2); err != nil {
		log.Printf("workbuddyai checkin claim uid=%s: %v", a.UID, truncate(err.Error(), 120))
		return
	}
	log.Printf("workbuddyai checkin claimed uid=%s daily_credit=%d", a.UID, st.DailyCredit)
}

// pokeActivity 用免费模型发一次最小对话请求，保持账号活跃。
// 模型严格取自 freeModels（实测 x0.00，不扣积分）；全部失败时静默放弃。
func (c *Client) pokeActivity(a *auth.Auth) {
	payload, _ := json.Marshal(map[string]any{
		"model":      freeModels[0],
		"stream":     true,
		"max_tokens": 16,
		"messages": []map[string]any{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "hi"},
		},
	})
	for _, m := range freeModels {
		var p map[string]any
		_ = json.Unmarshal(payload, &p)
		p["model"] = m
		raw, _ := json.Marshal(p)
		rc, status, _, err := c.ChatStream(a, raw)
		if err != nil {
			log.Printf("workbuddyai activity poke uid=%s model=%s: %v", a.UID, m, truncate(err.Error(), 120))
			continue
		}
		if rc == nil {
			log.Printf("workbuddyai activity poke uid=%s model=%s: upstream %d", a.UID, m, status)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		log.Printf("workbuddyai activity poke uid=%s model=%s ok", a.UID, m)
		return
	}
	log.Printf("workbuddyai activity poke uid=%s: all free models failed", a.UID)
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（国际版已是 OpenAI SSE，直接透传）。
// 返回值为末帧捕获的 usage（供记账，上游未返回时为 nil）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := StreamCapture(w, r, func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 实现 provider.Upstream。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) { return Aggregate(r) }
