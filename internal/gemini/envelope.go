// envelope.go v1internal 信封：结构定义、**逐层字母序序列化**、sessionId / requestId 派生。
//
// ## ⚠️ 本文件最容易写错的一处：信封必须逐层字母序序列化
//
// 上游（以及参考实现 `marshalAlphabetical`，gemini.ts:366-397）看到的是**字节**：
// 整个信封树（含 tools 的 JSON Schema、工具调用 args）都要按键名字典序排列。
//
// 本包的落地方式是**用结构体、并把字段按字母序声明**：
// Go 的 `encoding/json` 对结构体按**字段声明序**输出，故「声明序 == 字典序」就是全部保障，
// 且它在代码里是**编译期可见**的（谁改了字段顺序，线上字节立刻变，评审一眼能看见）。
//
// ⚠️ 为什么**不**用 `map[string]any` 承载信封：
//   - Go 的 `encoding/json` 确实会对 map 键排序（键为字符串时按字节序），但它是一个
//     **实现细节**，而且 map 无法表达「这个键必须有 / 必须没有」——加一个键、少一个键
//     不会有任何编译期提示，与本渠道「字节必须与上游一致」的要求相性极差；
//   - map 的数值会被解成 float64，序列化时可能把整数字面量重排（1e21 → 1e+21 之类），
//     而工具 schema / args 里的数字是**用户给的原始文本**，不该被本地重排。
//
// 因此：信封与其中的固定形状用结构体（声明序即字典序）；只有两处**形状不固定**的
// 用户数据（工具 JSON Schema `parameters`、工具调用 `args`）用 `alphaJSON`（已规范化
// 的原始 JSON 字节），它们经 canonicalize() 递归按键排序后原样嵌入，叶子字节不动。
package gemini

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 信封结构（**字段声明序 == 键名字典序**，每一层都要成立）
// ---------------------------------------------------------------------------

// envelope 顶层信封。
// 声明序 = model < project < request < requestId < userAgent（逐字节比较，短的在前）。
type envelope struct {
	Model     string     `json:"model"`
	Project   string     `json:"project"`
	Request   genRequest `json:"request"`
	RequestID string     `json:"requestId"`
	UserAgent string     `json:"userAgent"`
}

// genRequest 信封内层。
// 声明序 = contents < generationConfig < sessionId < systemInstruction < toolConfig < tools。
//
// ⚠️ `tools` 排在 `toolConfig` **之后**是正确且必要的：比较第 4 个字符时
// `toolConfig` 的 'C'(0x43) < `tools` 的 's'(0x73)——大写字母在小写字母之前。
// "看起来像写反了"的这处正是本渠道最容易改错的字节。
type genRequest struct {
	Contents          []content         `json:"contents"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	SessionID         string            `json:"sessionId"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	Tools             []toolGroup       `json:"tools,omitempty"`
}

// generationConfig 生成参数。
// 声明序 = maxOutputTokens < temperature < thinkingConfig。
type generationConfig struct {
	MaxOutputTokens int64           `json:"maxOutputTokens"`
	Temperature     *float64        `json:"temperature,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

// thinkingConfig 思考配置。
// 声明序 = includeThoughts < thinkingBudget。
type thinkingConfig struct {
	// IncludeThoughts **恒 true**（协议 §2.4）：上游「关闭思考」是假关
	// （关掉照样思考照样计费，实测 195 token），故不提供 none 档。
	IncludeThoughts bool `json:"includeThoughts"`
	// ThinkingBudget 仅非 tiered 档下发；tiered 交给上游自适应（nil ⇒ 键不出现）。
	ThinkingBudget *int64 `json:"thinkingBudget,omitempty"`
}

// content 一条消息。
// 声明序 = parts < role。
type content struct {
	Parts []part `json:"parts"`
	Role  string `json:"role"`
}

// part 一个内容片段。
// 声明序 = functionCall < functionResponse < inlineData < text。
//
// ⚠️ 请求侧**只**发这四种：历史 thinking / reasoning 块**不回传**（协议 §2.4 表内
// 「历史 thinking 块」行），`thoughtSignature` 也不回填（见包注释的「未确认项」）。
type part struct {
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	Text             string            `json:"text,omitempty"`
}

// functionCall 工具调用。
// 声明序 = args < name。
type functionCall struct {
	// Args 参数对象（**已字母序规范化**的原始 JSON；上游对 args 内部的键序同样敏感）。
	Args alphaJSON `json:"args"`
	Name string    `json:"name"`
}

// functionResponse 工具结果。
// 声明序 = name < response。
type functionResponse struct {
	// Name **必须**来自对应 tool_call 的 function name（上游按 name 配对，不认 id）。
	Name     string     `json:"name"`
	Response respObject `json:"response"`
}

// respObject 工具结果体。字段名照参考实现（gemini-messages.ts:246-250）：
// `content` 是结果文本，`error` 标记该次工具调用失败。
// 声明序 = content < error。
type respObject struct {
	Content string `json:"content"`
	Error   bool   `json:"error,omitempty"`
}

// inlineData 内联图片。`data` 是**裸 base64**（不含 `data:` 前缀）。
// 声明序 = data < mimeType。
type inlineData struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// toolGroup 工具组（上游形状恒为「一个 functionDeclarations 数组」）。
type toolGroup struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

// functionDeclaration 工具声明。
// 声明序 = description < name < parameters。
type functionDeclaration struct {
	Description string    `json:"description,omitempty"`
	Name        string    `json:"name"`
	Parameters  alphaJSON `json:"parameters"`
}

// toolConfig 工具选择配置。
type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

// functionCallingConfig 工具选择模式。
// 声明序 = allowedFunctionNames < mode。
type functionCallingConfig struct {
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	Mode                 string   `json:"mode"`
}

// alphaJSON 已是「按键名字典序」的原始 JSON 字节，序列化时原样嵌入。
type alphaJSON = json.RawMessage

// MaxRequestBodyBytes 出站请求体上限（协议 §2.4：64 MiB，发送前**真实检查**）。
// 超限归 ErrPromptTooLong（触发上层压缩重试）而不是 ErrBadParams（死路）。
const MaxRequestBodyBytes = 64 << 20

// 本地出站体错误码（进 OpenAI 错误体，由 Classify 识别成语义分类）。
const (
	errCodeBadParams     = "bad_params"
	errCodePromptTooLong = "prompt_too_long"
)

// ---------------------------------------------------------------------------
// 可注入钩子（仅测试替换；生产为下面的默认实现）
// ---------------------------------------------------------------------------

var (
	// nowMillisFn 取当前毫秒（requestId 与工具调用 id 用）。
	nowMillisFn = func() int64 { return time.Now().UnixMilli() }
	// randomHexFn 生成 n 字节随机数的 hex。
	randomHexFn = func(n int) string {
		buf := make([]byte, n)
		if _, err := rand.Read(buf); err != nil {
			// 随机源失败极少见：退到时间纳秒，保证 requestId 仍然唯一可读（不 panic）。
			return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
		}
		return hex.EncodeToString(buf)
	}
	// deriveSessionIDFn 是 sessionId 派生的注入点（测试钉死取值用）。
	deriveSessionIDFn = deriveSessionID
)

// newRequestID 生成每请求随机的 requestId：`agent/<nowMs>/<8 hex>`（gemini.ts:399-402）。
func newRequestID() string {
	return fmt.Sprintf("agent/%d/%s", nowMillisFn(), randomHexFn(4))
}

// ---------------------------------------------------------------------------
// 请求体组装
// ---------------------------------------------------------------------------

// buildInternalBody 把内层 OpenAI Chat 请求体翻成 v1internal 信封。
//
// 返回值约定：失败时返回 (nil, code, err)，code ∈ {errCodeBadParams, errCodePromptTooLong}，
// 调用方据此产出**本地 400**（而不是把错误当传输层错误去惩罚账号——那是为客户端的问题
// 罚健康账号，见 minimax.ChatStream 的同款取舍）。
func buildInternalBody(raw []byte, project string) ([]byte, string, error) {
	in, err := decodeRequest(raw)
	if err != nil {
		return nil, errCodeBadParams, err
	}

	// 模型：客户端 ID（可能带 `gemini/` 前缀）→ 目录裸名 → **必须**是本包唯一模型。
	// ⚠️ 严格校验是刻意的（gemini.ts:520-554）：宽容放行会让写错模型名的请求静默跑出
	// 3.8 的答案（不报错、不降级、答案就是不对，极难排查）。
	rawModel := jsonString(in["model"])
	catalogID := canonicalModelID(upstreamModelName(rawModel))
	if catalogID != ModelFlash {
		return nil, errCodeBadParams, fmt.Errorf("模型 %q 不在本 provider 目录中（仅支持 %s）", rawModel, ModelFlash)
	}
	tier := tierOf(jsonString(in["reasoning_effort"]))

	contents, sysTexts, err := translateMessages(in["messages"])
	if err != nil {
		return nil, errCodeBadParams, err
	}
	if len(contents) == 0 {
		// 空 contents 上游会 400，而错误信息与「消息不可用」完全无关，排查成本极高（gemini-messages.ts:284）。
		return nil, errCodeBadParams, errors.New("messages 里没有可用内容")
	}

	if project = trimSpace(project); project == "" {
		project = DefaultProject
	}
	req := genRequest{
		Contents:         contents,
		GenerationConfig: buildGenerationConfig(in, tier),
		// ⚠️ sessionId **不是常量**（协议 §2.4）：按 (project, 首条 user 文本, lane) 派生。
		// 写死会让所有账号、所有对话共用一个会话——上游的会话归并与签名回填都挂在它上面。
		SessionID: deriveSessionIDFn(project, firstUserText(contents), SessionLaneInfer, 0),
	}
	if sys := joinSystemTexts(sysTexts); sys != "" {
		req.SystemInstruction = &content{Role: "system", Parts: []part{{Text: sys}}}
	}
	if tools, ok := collectTools(in["tools"]); ok {
		req.Tools = tools
		req.ToolConfig = buildToolConfig(in["tool_choice"])
	}

	out, err := marshalEnvelope(envelope{
		Model:     ModelFlash + "-" + tier, // 档位后缀 = 上游准入钥匙（协议 §2.4）
		Project:   project,
		Request:   req,
		RequestID: newRequestID(),
		UserAgent: EnvelopeUserAgent,
	})
	if err != nil {
		return nil, errCodeBadParams, err
	}
	if len(out) > MaxRequestBodyBytes {
		return nil, errCodePromptTooLong,
			fmt.Errorf("出站请求体 %d 字节超过上限 %d 字节", len(out), MaxRequestBodyBytes)
	}
	return out, "", nil
}

// buildGenerationConfig 组装 generationConfig。
//
// 三处对齐细节（gemini-messages.ts:344-358）：
//   - `maxOutputTokens` 缺省 64_000；
//   - `includeThoughts` **恒 true**；
//   - `thinkingBudget` 仅非 tiered 档下发（tiered 交给上游自适应）。
func buildGenerationConfig(in map[string]any, tier string) *generationConfig {
	maxTokens := jsonInt64(in["max_tokens"])
	if maxTokens <= 0 {
		maxTokens = jsonInt64(in["max_completion_tokens"])
	}
	if maxTokens <= 0 {
		maxTokens = jsonInt64(in["max_output_tokens"])
	}
	if maxTokens <= 0 {
		maxTokens = MaxOutputTokens
	}
	cfg := &generationConfig{
		MaxOutputTokens: maxTokens,
		ThinkingConfig:  &thinkingConfig{IncludeThoughts: true},
	}
	if budget := thinkingBudget(tier); budget >= 0 {
		b := budget
		cfg.ThinkingConfig.ThinkingBudget = &b
	}
	// temperature 只在调用方显式给出**有限非负数**时透传（否则整个键不出现，
	// 与参考实现的 `if (temperature !== undefined)` 同口径）。
	if f, ok := jsonFloat(in["temperature"]); ok && f >= 0 {
		cfg.Temperature = &f
	}
	return cfg
}

// buildToolConfig 把 `tool_choice` 映射成上游 toolConfig（gemini-messages.ts:360-398）。
//
// ⚠️ 只有 tools 非空才构造它（参考实现同款）：没有工具却声明 mode 是自相矛盾的形状。
// 默认 AUTO：本仓的 GenerateOptions 不带 toolChoice，绝大多数请求落在这里。
func buildToolConfig(choice any) *toolConfig {
	mode := "AUTO"
	name := ""
	switch v := choice.(type) {
	case string:
		switch trimSpace(v) {
		case "none":
			mode = "NONE"
		case "any", "required":
			mode = "ANY"
		}
	case map[string]any:
		switch jsonString(v["type"]) {
		case "none":
			mode = "NONE"
		case "any", "tool":
			mode = "ANY"
		}
		name = jsonString(v["name"])
		if name == "" {
			if tool, ok := v["tool"].(map[string]any); ok {
				name = jsonString(tool["name"])
			}
		}
	}
	cfg := toolConfig{FunctionCallingConfig: functionCallingConfig{Mode: mode}}
	if mode == "ANY" && name != "" {
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{name}
	}
	return &cfg
}

// ---------------------------------------------------------------------------
// 序列化
// ---------------------------------------------------------------------------

// marshalEnvelope 序列化信封（字段声明序即字典序，见文件头）。
//
// ⚠️ 关掉 HTML 转义（`<` `>` `&` 原样输出）：参考实现走 `JSON.stringify`，不做 HTML 转义。
// 转义后的 JSON 语义相同（上游是 JSON 解析器），但字节与参考实现不一致，而本渠道的
// 对齐要求就是「字节一致」——顺手对齐掉。
func marshalEnvelope(v any) ([]byte, error) { return marshalNoHTMLEscape(v) }

// marshalNoHTMLEscape 与 json.Marshal 等价，但不转义 HTML 字符、不带结尾换行。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// canonicalRaw 把任意 Go 值编成「按键名字典序、叶子字节不变」的 JSON。
func canonicalRaw(v any) (alphaJSON, error) {
	b, err := marshalNoHTMLEscape(v)
	if err != nil {
		return nil, err
	}
	return canonicalize(b)
}

// canonicalize 递归按键名字典序重建 JSON；数组保持顺序；**叶子字节原样保留**。
//
// 叶子用 json.RawMessage 承接（而不是解成 any），所以数字/字符串的原始写法不会被
// float64 往返改形（工具 schema 里的 minimum/maximum、args 里的长整数都属此类）。
// 对象内部用 map 承接后**显式** sort.Strings 排序——不依赖任何实现细节。
func canonicalize(raw []byte) (alphaJSON, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("空 JSON")
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			val, err := canonicalize(obj[k])
			if err != nil {
				return nil, err
			}
			buf.Write(val)
		}
		buf.WriteByte('}')
		return alphaJSON(buf.Bytes()), nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			val, err := canonicalize(item)
			if err != nil {
				return nil, err
			}
			buf.Write(val)
		}
		buf.WriteByte(']')
		return alphaJSON(buf.Bytes()), nil
	default:
		return alphaJSON(append([]byte(nil), trimmed...)), nil
	}
}

// ---------------------------------------------------------------------------
// sessionId 派生
// ---------------------------------------------------------------------------

// deriveSessionID 按 (project, 首条 user 文本, lane[, generation]) 确定性派生 sessionId。
//
// 逐点复刻参考实现（gemini.ts:186-203）：
//   - FNV-1a 64 位；输入是 `project \0 lane \0 firstUserText`，generation > 0 时再拼 `\0 <gen>`；
//   - 输出按**有符号** int64 的十进制串（原版取值正负都有）。
//
// ⚠️ **存疑项（未确认，禁止当事实用）**：原版的哈希函数本体**尚未反推出来**
// （已被否证 50+ 种：FNV-1a/1、murmur64A、MD5/SHA 系列等），故本实现复刻的是
// **依赖维度与「同输入同输出」**，取值与原版**不逐字相同**。潜在影响：若上游对 sessionId
// 做取值/格式校验（例如要求它必须与某个服务端会话表对上），本实现派生的值可能不被承认，
// 表现为会话归并失效或签名回填失效；文档未给出该风险的实测结论，故**不假装已对齐**。
func deriveSessionID(project, firstUserText, lane string, generation int) string {
	const (
		offset64 = 0xcbf29ce484222325
		prime64  = 0x100000001b3
	)
	base := project + "\x00" + lane + "\x00" + firstUserText
	if generation > 0 {
		base += "\x00" + strconv.Itoa(generation)
	}
	var h uint64 = offset64
	for i := 0; i < len(base); i++ {
		h ^= uint64(base[i])
		h *= prime64
	}
	return strconv.FormatInt(int64(h), 10)
}

// firstUserText 取**第一条 user 消息的首个非空文本 part**（sessionId 派生的输入之一）。
//
// 口径照参考实现（gemini-messages.ts:178-186）：只认 role 为 `user`（或空）的消息，
// 且只看其中的文本 part——后续文本、其他消息都不进哈希。
func firstUserText(contents []content) string {
	for _, c := range contents {
		if c.Role != "" && c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// JSON 取值小工具（解码时开了 UseNumber，数字可能是 json.Number）
// ---------------------------------------------------------------------------

// decodeRequest 解析请求体，**保留数字原文**（UseNumber）。
// 不这么做的话，工具 schema 里的整数会被 float64 往返改写（例如 1e21 → 1e+21）。
func decodeRequest(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var in map[string]any
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象：%w", err)
	}
	if in == nil {
		return nil, errors.New("请求体是空 JSON 对象")
	}
	return in, nil
}

// jsonString 取字符串字段（非字符串/缺失 → 空串）。
func jsonString(v any) string {
	s, _ := v.(string)
	return s
}

// jsonFloat 取数值字段（json.Number / float64 / 整数都认）。
func jsonFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// jsonInt64 取整数值（非整数/缺失 → 0）。
func jsonInt64(v any) int64 {
	f, ok := jsonFloat(v)
	if !ok {
		return 0
	}
	return int64(f)
}

// trimSpace 取空白裁剪后的字符串（信封字段值的归一，薄封装便于一处修改）。
func trimSpace(s string) string { return strings.TrimSpace(s) }
