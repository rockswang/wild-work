package traework

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
)

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
// 状态码优先于 body 硬余额标记：429/401 的响应体也会携带业务码 1005
// （上游报错格式不受本端控制），先判 1005 会把限流/登录失效误判成硬余额、
// 触发 12h 硬冷却，且 401 会绕过自愈重登——对齐 qodercn/qodercom/qwenwork
// 的「状态码 → hard 标记」判定顺序。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return provider.ErrSessionDead
			}
		}
		return provider.ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return provider.ErrHardCredit
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		return provider.ErrClient
	}
	return provider.ErrNone
}

// Client Trae SOLO 上游 HTTP 客户端。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client
	AgentHost  string
	UgHost     string
	OAuthHost  string
	// PricingHost 定价接口基址；空则回落到 WorkHost。
	// 单列为字段是为了让测试可指向本地 mock。
	PricingHost       string
	ClientID          string
	CheckinRetryDelay time.Duration // 9074 限流后的重试等待；生产默认 8s
	// IdleTimeout 聊天 SSE 流中空闲超时（StreamHTTP 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration

	// Function 对话/模型列表接口的 function 值（solo_work_lite / solo_agent）。
	Function string
	// PricingFunctions 定价接口 functions 查询参数（各渠道口径不同）。
	PricingFunctions string
	// PricingChannel 定价条目落库的渠道标签（traework / traecode）。
	PricingChannel string
	// PricingPrimary 渠道主 function，去重时优先（即对话真实扣费的那一组）。
	PricingPrimary string
}

func New() *Client {
	tr := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 120 * time.Second}
	return &Client{HTTP: &http.Client{Timeout: 120 * time.Second, Transport: tr}, StreamHTTP: &http.Client{Transport: tr}, AgentHost: AgentHost, UgHost: UgHost, OAuthHost: OAuthHost, PricingHost: WorkHost, ClientID: ClientID, CheckinRetryDelay: 8 * time.Second, IdleTimeout: idle.DefaultTimeout, Function: Function, PricingFunctions: PricingFunctionsWork, PricingChannel: "traework", PricingPrimary: PricingPrimaryWork}
}

// NewTraeCode 返回 TraeCode（代码版）专用 Client：function=solo_agent。
//
// TraeCode 与 TraeWork 是**同一上游的不同 function**，账号体系相同，
// 因此二者共用一份凭证（调用方应共享同一个 pool，避免 refresh token 轮换冲突）；
// 差异只在 function 与定价分组口径。
func NewTraeCode() *Client {
	c := New()
	c.Function = FunctionCode
	c.PricingFunctions = PricingFunctionsCode
	c.PricingChannel = "traecode"
	c.PricingPrimary = PricingPrimaryCode
	return c
}

// pricingBase 定价接口基址（未显式设置时回落到 WorkHost）。
func (c *Client) pricingBase() string {
	if c.PricingHost != "" {
		return c.PricingHost
	}
	return WorkHost
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// maxJSONBody doJSON 单次响应的读取上限。
//
// 上限的意义不是「够不够用」，而是给上游异常返回一个内存上界。目录接口
// （get_detail_param）在 solo_agent 下实测 1.28MB，而旧上限 1MB 会把 JSON
// 截成半截、解析失败后静默回退静态兜底表（面板只显示 16 个模型，issue #41）。
const maxJSONBody = 8 << 20

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 多读 1 字节用于判定是否被截断：读满 maxJSONBody+1 说明上游响应超限，
	// 此时显式报错——否则半截 JSON 会被 json.Unmarshal 误报成语法错误（issue #41）。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	if len(raw) > maxJSONBody {
		return nil, fmt.Errorf("traework: response body exceeds %d bytes", maxJSONBody)
	}
	return raw, nil
}

func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("traework refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no refreshToken")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ClientID": c.ClientID, "RefreshToken": a.RefreshToken, "ClientSecret": "-", "UserID": ""}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("exchange parse: %w", err)
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.Result.Token == "" {
		err := fmt.Errorf("refresh_failed: no token in response — re-login required")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		d := time.Duration(resp.Result.TokenExpireDuration)
		if resp.Result.TokenExpireDuration > 1e9 { // 上游通常是毫秒
			d *= time.Millisecond
		} else {
			d *= time.Second
		}
		a.ExpiresAt = time.Now().Add(d).Unix()
	}
	log.Printf("traework refresh success uid=%s refresh_rotated=%t expires_at=%d", a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body, c.Function)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		log.Printf("traework chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("traework chat_stream uid=%s: upstream %d %s body=%s", a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 idle.Monitor（其 Close 会 cancel；静默超时也会 cancel）。
	return idle.Monitor(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	// traework 上游 llm_utils_chat 强制 stream=true（见 PrepareBody），
	// 所有模型均为流式模式；非流式请求由本地 Aggregate() 缓冲 SSE 后聚合。
	//
	// 双池合并：get_detail_param 的目录不保证按 function 过滤，而 chat 按
	// function 选池——只拉本渠道池会出现「列表里有、调用 4001」的错位。
	// 故两个池都拉一遍（先本渠道池、后另一池），按 config_name 先到先得
	// 去重，并把每个模型记入其实际归属池（PrepareBody 据此覆盖 function）。
	// 本渠道池失败照旧报错；另一池失败只记日志、用本渠道池结果继续。
	seen := make(map[string]bool, 64)
	out := make([]provider.ModelInfo, 0, 64)
	// 顺序固定：本渠道池先到先得（归属登记与去重都吃顺序），不能用 map 遍历。
	pools := []string{c.Function, otherFunction(c.Function)}
	for i, fn := range pools {
		primary := i == 0
		list, err := c.fetchModelsForPool(a, fn)
		if err != nil {
			if primary {
				return nil, err
			}
			log.Printf("traework models: %s 池拉取失败（继续用 %s 池结果）: %v", fn, c.Function, err)
			continue
		}
		for _, cfg := range list {
			name := strings.TrimSpace(cfg.ConfigName)
			if name == "" || seen[name] {
				continue
			}
			// 跳过自定义模型：部分模型上游不返回 is_custom_model，直接用前缀匹配兜底
			if cfg.DisplayConfig.IsCustomModel || strings.HasPrefix(name, "custom_model_") {
				log.Printf("traework skip custom model: %s", name)
				continue
			}
			seen[name] = true
			recordModelFunc(name, fn)
			out = append(out, provider.ModelInfo{ID: name, Name: cfg.DisplayConfig.DisplayName})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchModelsForPool 拉取单个 function 池的模型配置（mode_type=nil 返回全部，
// 按 config_name 去重避免流式/非流式重复——池内去重，跨池去重在 FetchModels）。
func (c *Client) fetchModelsForPool(a *auth.Auth, function string) ([]poolEntry, error) {
	body := map[string]any{"function": function, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []poolEntry `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	return resp.ConfigInfoList, nil
}

// poolEntry 单池目录条目（get_detail_param）。
type poolEntry struct {
	ConfigName    string `json:"config_name"`
	DisplayConfig struct {
		DisplayName   string `json:"display_name"`
		IsCustomModel bool   `json:"is_custom_model"` // 自定义模型（第三方代理）需额外授权
	} `json:"display_config"`
}

// FetchModelPricing 从 /api/remote/v1/models 拉取模型积分倍率。
// 按 config_name 去重，解析 features.consumption_rate.rate。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	url := c.pricingBase() + EpModelsPricing + "?functions=" + c.PricingFunctions + "&show_custom_model=true"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("X-Trae-Client-Type", "web")
	req.Header.Set("X-Trae-User-Timezone", "Asia/Shanghai")
	req.Header.Set("X-Preferenced-Language", "zh-cn")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://work.trae.cn/")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 8MB 上限（本地补丁 0005）：与 doJSON 同步放宽——模型定价列表随模型数膨胀，防同类截断。
	// 多读 1 字节判定超限：超限时显式报错，而非截成半截后由 json.Unmarshal 误报语法错误（对齐 doJSON）。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae pricing api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	if len(raw) > maxJSONBody {
		return nil, fmt.Errorf("traework: pricing response body exceeds %d bytes", maxJSONBody)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				Function string `json:"function"`
				Models   []struct {
					Name        string `json:"name"`
					DisplayName string `json:"display_name"`
					Features    string `json:"features"` // JSON 字符串
				} `json:"models"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("trae pricing parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("trae pricing api code=%d", env.Code)
	}
	// 去重：同一模型可能出现在多个 function 分组下，且**倍率未必一致**
	// （实测豆包 Seed-2.1-Pro：solo_agent=0.08 / _remote=0.8）。
	// 对话按主 function 计费，故主 function 的结果优先，非主 function 仅作兜底。
	byName := make(map[string]provider.ModelPricing)
	for _, fn := range env.Data.List {
		isPrimary := c.PricingPrimary != "" && fn.Function == c.PricingPrimary
		for _, m := range fn.Models {
			rate := parseTraeFeatures(m.Features)
			if rate <= 0 {
				continue
			}
			if _, seen := byName[m.Name]; !seen || isPrimary {
				byName[m.Name] = provider.ModelPricing{
					Model:   m.Name,
					Channel: c.PricingChannel,
					Rate:    rate,
				}
			}
		}
	}
	out := make([]provider.ModelPricing, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("trae pricing api returned empty")
	}
	return out, nil
}

// parseTraeFeatures 解析 features JSON 字符串中的 consumption_rate.rate。
// 若有 discount 则优先取折扣价。
func parseTraeFeatures(features string) float64 {
	if features == "" {
		return 0
	}
	var f struct {
		ConsumptionRate struct {
			Enable bool `json:"enable"`
			Data   struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
		Discount struct {
			Enable bool `json:"enable"`
			Data   struct {
				ConsumptionRate float64 `json:"consumption_rate"`
			} `json:"data"`
		} `json:"discount"`
	}
	if err := json.Unmarshal([]byte(features), &f); err != nil {
		return 0
	}
	// 折扣价优先
	if f.Discount.Enable && f.Discount.Data.ConsumptionRate > 0 {
		return f.Discount.Data.ConsumptionRate
	}
	if f.ConsumptionRate.Enable && f.ConsumptionRate.Data.Rate > 0 {
		return f.ConsumptionRate.Data.Rate
	}
	return 0
}

func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("checkin status parse: %w", err)
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	if resp.Code != 0 {
		err := fmt.Errorf("checkin status code=%d msg=%s", resp.Code, checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	if resp.Success != nil && !*resp.Success {
		err := fmt.Errorf("checkin status failed: %s", checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	log.Printf("traework checkin status uid=%s checked_in=%t credits=%d enable=%t", a.UID, resp.CheckedIn, resp.Credits, resp.Enable)
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

func (c *Client) CheckinClaim(a *auth.Auth) error {
	log.Printf("traework checkin claim start uid=%s", a.UID)
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		code, msg, success, err := parseCheckinResponse(data)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		if code == 9074 && attempt == 0 {
			delay := c.CheckinRetryDelay
			if delay > 0 {
				log.Printf("traework checkin claim rate-limited uid=%s code=9074 retry_after=%s", a.UID, delay)
				time.Sleep(delay)
			}
			continue
		}
		if code != 0 {
			err := fmt.Errorf("checkin claim code=%d msg=%s", code, msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		if success != nil && !*success {
			err := fmt.Errorf("checkin claim failed: %s", msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		log.Printf("traework checkin claim response uid=%s code=%d msg=%s", a.UID, code, msg)
		return nil // 9095 等业务无害响应交给后置 status 验证最终状态
	}
	return fmt.Errorf("checkin claim rate limited: code=9074")
}

func (c *Client) DailyCheckin(a *auth.Auth) error {
	log.Printf("traework checkin start uid=%s", a.UID)
	checked, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return err
	}
	if checked {
		log.Printf("traework checkin already uid=%s", a.UID)
		return fmt.Errorf("已签到")
	}
	if !enable {
		err := fmt.Errorf("checkin disabled")
		log.Printf("traework checkin rejected uid=%s err=%v", a.UID, err)
		return err
	}
	if err := c.CheckinClaim(a); err != nil {
		return err
	}
	checked, _, _, err = c.CheckinStatus(a)
	if err != nil {
		return fmt.Errorf("checkin verification: %w", err)
	}
	if !checked {
		err := fmt.Errorf("checkin verification failed: checked_in=false")
		log.Printf("traework checkin failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("traework checkin verified uid=%s", a.UID)
	return nil
}

func parseCheckinResponse(data []byte) (code int, msg string, success *bool, err error) {
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", nil, fmt.Errorf("checkin response parse: %w", err)
	}
	return resp.Code, checkinResponseMessage(resp.Message, resp.Msg), resp.Success, nil
}

func checkinResponseMessage(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(msg)
}

// UserResource 查询账号当前可花费积分余额（= UserEntUsage 的可消耗口径，仅 ep=0 池）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) { return c.UserEntUsage(a) }

// UserEntUsage 查询可消耗余额（pool 路由依据）。
// 内部走 fetchEntUsage：只累加 available_endpoint==0 的包，ep=1 官方客户端专用池不计入。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	// 网页版 web_user_ent_usage：require_usage=true 返回每个包的 usage.credits_amount（实际用量）。
	_, remain, err = c.fetchEntUsage(a)
	return remain, err
}

// entPackage 上游权益包条目（UserEntUsage / UserResourceDetail 共用结构）。
type entPackage struct {
	EntitlementBaseInfo struct {
		// AvailableEndpoint 可用端点：0=通用池，1=官方客户端专用池。
		// 2026-09-23 起上游不再下发 ep=1（专用池也标成 0），该字段仅作历史兜底。
		AvailableEndpoint int `json:"available_endpoint"`
		// ProductID 商品 ID：209=200 档每日签到（官方客户端专用，本工具扣不到），
		// 208=150 档每日签到（通用）、221=每月登录（通用）。这是当前唯一能区分
		// 「专用/通用」的稳定判据（2026-09-23 三账号实测：209 的 used 恒为 0）。
		ProductID int `json:"product_id"`
		Quota     struct {
			CreditsLimit float64 `json:"credits_limit"`
		} `json:"quota"`
		// EntitlementID 条目唯一标识（如 "340864129538"/"checkin_20260817_..."），ledger 差分对账用。
		EntitlementID string `json:"entitlement_id"`
		PackageName   string `json:"package_name"`
		PackageType   string `json:"package_type"`
	} `json:"entitlement_base_info"`
	DisplayDesc string `json:"display_desc"`
	GroupName   string `json:"group_name"`
	GroupType   int    `json:"group_type"`
	// ExpireTime 到期 Unix 秒；上游对无到期条目下发 0。
	ExpireTime int64 `json:"expire_time"`
	Usage      struct {
		CreditsAmount float64 `json:"credits_amount"`
	} `json:"usage"`
}

// usable 判定该包是否属于本工具可消耗的额度池。
//
// 判据（2026-09-26 更新，issue #44）：只有 available_endpoint==1（上游显式声明的
// 专用端点池）不可用，其余一律可消耗。
//
// 历史沿革：
//   - 2026-09-18：专用池下发 ep=1，判据 = ep==0；
//   - 2026-09-23（f6f41a4）：上游不再下发 ep=1，改判 pid==209；
//   - 2026-09-26（issue #44）：pid==209 判据被证伪——pid=209 是「200 档每日签到」，
//     上游已把它升级为通用积分。本仓自有日志铁证：2026-09-23 15:20–16:10 该账号
//     连发 99 次对话，期间 remain（208/221 池）纹丝不动停在 3086，而「不可用」小计
//     从 2200 降到 1846（-354，正是那 99 次对话的消耗量）——
//     即**扣费实际就发生在这个被判为不可用的池上**。把它排除只会让
//     pool 按虚低的余额选号、面板把真实可用的积分标成「不可用」。
//     故撤回 pid==209 判据，仅保留 ep==1 作历史兜底（上游若重新显式声明则仍尊重）。
func (p entPackage) usable() bool {
	return p.EntitlementBaseInfo.AvailableEndpoint != 1
}

// fetchEntUsage 调用上游积分接口，返回全部条目 + 可消耗余额（仅 usable() 判定为可用的包）。
// 不可消耗余额由调用方对条目按 Usable 标记汇总（provider.Summarize），本函数不重复算。
//
// 可用性判据见 entPackage.usable：仅 available_endpoint==1 不可用（issue #44）。
// 早期实现用 group_type!=1 判定，会把「用户福利」等误计入；后改为
// available_endpoint==0，2026-09-23 起该字段也失效（专用池被标成 0），
// 一度改用 product_id==209，2026-09-26 证伪后撤回（详见 usable 注释）。
func (c *Client) fetchEntUsage(a *auth.Auth) ([]entPackage, int64, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte(`{"require_usage":true}`)))
	if err != nil {
		return nil, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, 0, err
	}
	var resp struct {
		UserEntitlementPackList []entPackage `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	var usable int64
	for _, p := range resp.UserEntitlementPackList {
		if p.usable() {
			usable += packRemain(p)
		}
	}
	return resp.UserEntitlementPackList, usable, nil
}

// packRemain 单个权益包的剩余额度（limit - used，负值钳 0）。
func packRemain(p entPackage) int64 {
	limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
	used := int64(p.Usage.CreditsAmount)
	if remain := limit - used; remain > 0 {
		return remain
	}
	return 0
}

// UserResourceDetail 查询 TraeWork 积分明细（每个套餐条目）。
// 返回的 remain 为可消耗余额（pool 路由依据），items 包含全部条目。
// 每个条目带 expire_at（到期日）与 usable（是否属于本工具可消耗的 ep=0 池）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	packs, usable, err := c.fetchEntUsage(a)
	if err != nil {
		return 0, nil, err
	}
	items := make([]provider.ResourceItem, 0, len(packs))
	for _, p := range packs {
		// 零额度条目（如免费的 0 限额包）不展示：既无信息量，又会把列表搅得很长。
		if p.EntitlementBaseInfo.Quota.CreditsLimit <= 0 {
			continue
		}
		name := p.GroupName
		if name == "" {
			name = p.DisplayDesc
		}
		if name == "" {
			name = p.EntitlementBaseInfo.PackageName
		}
		if name == "" {
			name = p.EntitlementBaseInfo.PackageType
		}
		if name == "" {
			name = fmt.Sprintf("套餐 (group_type=%d)", p.GroupType)
		}
		items = append(items, provider.ResourceItem{
			Name:     name,
			Total:    int64(p.EntitlementBaseInfo.Quota.CreditsLimit),
			Used:     int64(p.Usage.CreditsAmount),
			Remain:   packRemain(p),
			ExpireAt: unixDate(p.ExpireTime),
			// entitlement_id 是上游稳定标识，供 ledger 差分对账（过期/消耗归因）
			Key:    p.EntitlementBaseInfo.EntitlementID,
			Usable: p.usable(),
		})
	}
	return usable, items, nil
}

// unixDate 把上游 Unix 秒转为 UTC+8 的 YYYY-MM-DD；0/负值（上游未下发到期时间）返回空串。
func unixDate(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(softRateResetLoc).Format("2006-01-02")
}

func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return StreamWithModel(w, r, model)
}
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return AggregateWithModel(r, model)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
