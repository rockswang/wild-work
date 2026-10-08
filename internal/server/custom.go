// custom.go 自定义模型直转：把命中自定义配置的 chat/completions 请求
// 原样转发给第三方 OpenAI 兼容源（不经渠道账号池）。
//
// 路由优先级（与渠道互斥，见 chatCompletions 接入点）：
//   - 渠道前缀（kind/model）永远走渠道，本路径完全不介入（自定义表入库时拒绝
//     渠道 Kind 作前缀，渠道名不可能命中自定义表）；
//   - 裸名（deepseek-chat）或「源名/模型」（YD/GLM-5.3）命中启用中的自定义模型
//     → 本路径直转第三方；
//   - 未命中 → 照旧走上游原有路径，零行为变化。
//
// 错误语义对齐渠道路径（不变量 8）：第三方 ≥400 原文透传不包装；传输层
// 不可达回 502。瞬时错误（传输层错误 / 5xx）自动重试一次——瞬时故障重试
// 一次显著降低第三方源抖动时的失败率；4xx/2xx/内容错误不重试。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/custommodels"
	statsEngine "wild-work/internal/stats"
)

// customRetryDelay 瞬时错误重试间隔：错开网关瞬时窗口又不显著拖长失败路径。
// var 而非 const：测试注入缩短间隔（生产 1.5s）。
var customRetryDelay = 1500 * time.Millisecond

// customNonStreamTimeout 非流式直转总超时兜底：一问一答需上限，防止第三方
// 半死连接把请求无限挂起。流式不设总超时（R35 同理：Client.Timeout 覆盖整个
// 请求生命周期含读 body，长回答会被掐断且无终止帧）。
const customNonStreamTimeout = 5 * time.Minute

// customUsageBodyCap 非流式 usage 解析的体积护栏：正常一问一答远小于此；
// 超限（病态大响应）自动降级纯透传，不解析不断流。var 而非 const：测试注入缩短。
var customUsageBodyCap int64 = 16 << 20

// customUsageScanLineCap 流式 SSE 扫描的单行累积上限：真实 usage 帧远小于此，
// 超过视为病态帧，弃扫防占内存（转发不受影响，只是放弃记账）。
const customUsageScanLineCap = 1 << 20

// hopHeaders 逐跳头不转发（代理语义；其余响应头原样透传）。
var customHopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
	"TE": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// initCustomClients 构造直转用的两个 HTTP client（流式无总超时 / 非流式带总超时）。
func (h *Handler) initCustomClients() {
	h.customStream = &http.Client{} // 走 http.DefaultTransport（含环境代理），不设 Timeout
	h.customJSON = &http.Client{Timeout: customNonStreamTimeout}
}

// CustomClients 返回自定义模型直转的两个 HTTP client（流式 / 非流式）。
// 供 cmd 装配层给自定义出站统一套传输链包装（与渠道出站共用同一开关与热更新口径）。
func (h *Handler) CustomClients() []*http.Client {
	return []*http.Client{h.customStream, h.customJSON}
}

// serveCustom 直转第三方 OpenAI 兼容源。
// body 已缓冲（重试需可重读）；stream 取自客户端请求的 stream 字段。
// 运行统计插桩与渠道路径同款（statWriter + AddRequestRow + usage 记账），
// 模型名记客户端请求的自定义模型名（配置名），统计行与面板配置一一对应；
// 无 usage 的响应只计请求数，不计 token（与渠道路径口径一致）。
func (h *Handler) serveCustom(w http.ResponseWriter, r *http.Request, body []byte, stream bool, tgt custommodels.Target) {
	// upstream_id 非空且 ≠ 对外名 → 只改写 body 顶层 model 字段；改写失败不中断，
	// 原样转发让第三方给出权威错误。
	if tgt.Upstream != tgt.Model {
		if nb, err := rewriteCustomModel(body, tgt.Upstream); err == nil {
			body = nb
		} else {
			log.Printf("custom model 重写失败 model=%s target=%s err=%v", tgt.Model, tgt.Upstream, err)
		}
	}
	log.Printf("custom model forward: model=%s source=%s target=%s stream=%t", tgt.Model, tgt.Source, tgt.Upstream, stream)

	// 运行统计插桩：结构对齐 chatCompletions 的既有写法。
	var statRow statsEngine.ReqRow
	statRow.Model = tgt.Model
	if stream {
		statRow.Mode = "流"
	} else {
		statRow.Mode = "非流"
	}
	sw := &statWriter{ResponseWriter: w, start: time.Now()}
	w = sw // 此后所有写出统一经 sw 计时
	if h.stats != nil {
		defer func() {
			now := time.Now()
			statRow.Time = now.Format("01-02 15:04:05")
			statRow.TTFBMs = sw.ttfbMs
			statRow.TotalSec = now.Sub(sw.start).Seconds()
			if statRow.Status == 0 {
				statRow.Status = http.StatusOK
			}
			h.stats.AddRequestRow(statRow)
		}()
	}

	client := h.customStream
	if !stream {
		client = h.customJSON
	}

	// 瞬时错误重试一次：仅传输层错误 / 5xx；4xx/2xx/内容错误不重试。
	// 客户端已断开（ctx 取消）不重试——重试结果无处投递。
	resp, err := doCustomOnce(client, r, body, tgt)
	if (err != nil || (resp != nil && resp.StatusCode >= 500)) && r.Context().Err() == nil {
		if err != nil {
			log.Printf("custom model %s 传输层错误（%v），%.1fs 后自动重试一次", tgt.Model, err, customRetryDelay.Seconds())
		} else {
			_ = resp.Body.Close()
			log.Printf("custom model %s 上游 %d（瞬时错误），%.1fs 后自动重试一次", tgt.Model, resp.StatusCode, customRetryDelay.Seconds())
		}
		time.Sleep(customRetryDelay)
		resp, err = doCustomOnce(client, r, body, tgt)
	}
	if err != nil {
		statRow.Status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	statRow.Status = resp.StatusCode

	// 响应头透传（跳过逐跳头）。响应体两条 usage 记账路径（口径对齐渠道路径）：
	//   - 非流式：护栏内缓冲解析 usage 后一次性写回；
	//   - 流式：边转发边扫描 SSE data: 行（usage 通常在尾帧），转发零延迟。
	for k, vv := range resp.Header {
		if customHopHeaders[k] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if stream {
		scan := &sseUsageScan{}
		copyCustomBody(w, resp.Body, scan)
		h.recordCustomUsage(tgt.Model, scan.usage, sw, &statRow)
		return
	}
	all, rerr := io.ReadAll(io.LimitReader(resp.Body, customUsageBodyCap+1))
	if rerr == nil && int64(len(all)) <= customUsageBodyCap {
		var obj struct {
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal(all, &obj) == nil {
			h.recordCustomUsage(tgt.Model, obj.Usage, sw, &statRow)
		}
		_, _ = w.Write(all)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	// 降级纯透传：超限（病态大响应）或读到一半出错——已读部分先写回，
	// 剩余继续流式续传（放弃 usage 解析，保证响应完整下发）。
	_, _ = w.Write(all)
	copyCustomBody(w, resp.Body, nil)
}

// recordCustomUsage 把自定义直转响应里的 usage 计入运行统计（模型消耗表 +
// 今日全局口径）。口径对齐渠道路径：token 字段兼容 Anthropic 命名（usageTokens），
// genSec = 总耗时 − TTFB（引擎对 ≤50ms 样本只计 TTFB）。credit 恒 0——第三方
// 定价不在渠道积分体系内，硬折算会得出误导性消耗；usage 缺失只计请求数。
func (h *Handler) recordCustomUsage(model string, usage map[string]any, sw *statWriter, row *statsEngine.ReqRow) {
	if h.stats == nil || usage == nil {
		return
	}
	pt, ct, _ := usageTokens(usage)
	if pt <= 0 && ct <= 0 {
		return
	}
	gen := time.Since(sw.start).Seconds() - float64(sw.ttfbMs)/1000
	h.stats.AddUsage(model, pt, ct, 0, sw.ttfbMs, gen, time.Now())
	row.Tok = ct
	if gen > 0 {
		row.TokPerSec = float64(ct) / gen
	}
}

// sseUsageScan 增量扫描 SSE 流的 data: 行，捕获 usage（OpenAI 语义下在尾帧）。
// 扫描发生在转发之后：客户端延迟零影响；多帧 usage 后帧覆盖前帧。解析失败
// 静默跳过——记账是锦上添花，不较真第三方帧格式。
type sseUsageScan struct {
	line  []byte
	usage map[string]any
	drop  bool
}

// feed 吸收一段刚转发完的字节流，按行切分找 usage。
func (s *sseUsageScan) feed(buf []byte) {
	if s.drop {
		return
	}
	s.line = append(s.line, buf...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			if len(s.line) > customUsageScanLineCap {
				s.drop = true
				s.line = nil
			}
			return
		}
		line := s.line[:i]
		s.line = s.line[i+1:]
		if usage := sseLineUsage(line); usage != nil {
			s.usage = usage
		}
		if len(s.line) > customUsageScanLineCap {
			s.drop = true
			s.line = nil
			return
		}
	}
}

// sseLineUsage 解析单条 SSE data: 行里的 usage 对象；非 data 行 / 无 usage /
// JSON 解析失败一律返回 nil。
func sseLineUsage(line []byte) map[string]any {
	t := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(t, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(t[len("data:"):])
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return nil
	}
	var obj struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(payload, &obj) != nil {
		return nil
	}
	return obj.Usage
}

// copyCustomBody 流式友好的透传拷贝：每写一段就 Flush，SSE 帧即时下发
// （net/http 响应默认经 4KB bufio 缓冲，不主动 Flush 会让 SSE 退化为整段突发；
// 非流式多刷几次无害）。写失败（客户端断开）静默收尾，读剩余 body 交给 defer Close。
// scan 非 nil 时在写出之后增量扫描 SSE usage（记账不添转发延迟）。
func copyCustomBody(w http.ResponseWriter, src io.Reader, scan *sseUsageScan) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if scan != nil && !scan.drop {
				scan.feed(buf[:n])
			}
		}
		if rerr != nil {
			return
		}
	}
}

// doCustomOnce 发送一次直转请求（body 为缓冲副本，可重复读）。
// ctx 取自客户端请求：客户端断开即中止第三方连接。
func doCustomOnce(client *http.Client, r *http.Request, body []byte, tgt custommodels.Target) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, customChatURL(tgt.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	// Accept 透传客户端原值（部分第三方按它决定是否发 SSE）；未带则不设置。
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("Authorization", "Bearer "+tgt.APIKey)
	return client.Do(req)
}

// rewriteCustomModel 只改写请求体的顶层 model 字段（不动 messages 等其余内容）。
// 与渠道路径的 rewriteModel 不同：**不做** normalizeToolTurnContent——直转语义是
// 原样转发，第三方兼容性由第三方自己裁决。失败返回 error，调用方保持原样转发。
func rewriteCustomModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, io.ErrUnexpectedEOF
	}
	obj["model"] = model
	return json.Marshal(obj)
}

// customChatURL 拼第三方 chat 端点。BaseURL 存储时已 trim + 去尾 "/"；
// 兼容用户填 https://api.deepseek.com 与 https://api.deepseek.com/v1 两种习惯。
func customChatURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/chat/completions"
	}
	return baseURL + "/v1/chat/completions"
}

// customModelEntries /v1/models 合并条目：自定义模型以裸名列出（决策：不带
// 任何前缀），owned_by=custom 与渠道条目区分。
func customModelEntries(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{
			"id": name, "object": "model", "created": 1753600000, "owned_by": "custom",
		})
	}
	return out
}
