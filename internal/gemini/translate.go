// translate.go 内层 OpenAI Chat 请求 → 上游 `contents` / `tools` 的纯函数翻译层。
//
// 与 minimax 的 messages.go 同一理由：本层只管形状翻译，与网络、凭据、账号池无关，
// 可用单测逐字节锁死（gemini_test.go 就吃这一层）。
//
// ## 三条必须保留的上游对齐细节（协议 §2.4）
//
//  1. **role 只有 `user` / `model`**（assistant → model，其余 → user）；
//  2. **历史里的 thinking / reasoning 块不回传**（签名无法回填，见包注释的未确认项）；
//  3. **`functionResponse.name` 必须来自对应 tool_call 的 name**（上游按 name 配对，不认 id），
//     故先扫全消息建映射；name 解析不出来时**整块丢弃**（空 name 会被上游 400，
//     丢了顶多少一轮工具上下文，不会打死整轮）。
//
// ⚠️ 与 Anthropic 面的差异（刻意为之）：Gemini 的 v1internal **不要求角色交替**，
// 参考实现也不合并相邻同角色 contents（gemini-messages.ts:214-280），故本包同样不合并。
// 别"顺手统一"成 minimax 那套相邻合并逻辑——那会改变发给上游的字节。
package gemini

import (
	"fmt"
	"strings"
)

// translateMessages 把 OpenAI 的 messages 翻成上游 contents，并把 system 文本单独交出。
//
// 返回值：contents、systemTexts、错误。system 走顶层 systemInstruction（见 buildInternalBody）——
// 本仓内层协议是 OpenAI Chat，system 恰恰**只在** messages 里；直接按参考实现「跳过 system」
// 会把系统提示词整段丢掉（与 minimax messages.go 规则 6 的同款偏差说明）。
func translateMessages(raw any) ([]content, []string, error) {
	list, _ := raw.([]any)

	// ---- 第一遍：全消息扫 tool_call_id → name 映射与配对集合 ----
	callNames := map[string]string{}
	resultIDs := map[string]bool{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(trimSpace(jsonString(m["role"]))) {
		case "assistant":
			for _, c := range toolCallsOf(m) {
				if id := toolCallIDOf(c); id != "" {
					callNames[id] = toolCallNameOf(c)
				}
			}
		case "tool", "function":
			if id := toolResultIDOf(m); id != "" {
				resultIDs[id] = true
			}
		}
	}
	// 配对判据：两侧 id 都在（参考实现 resolveToolPairing 的效果）。
	// 孤儿 tool_call / tool_result 上游同样会 400，故两侧都剔除。
	keepCall := func(id string) bool { return id != "" && resultIDs[id] }

	// ---- 第二遍：逐条消息构造 contents ----
	var (
		out      []content
		sysTexts []string
	)
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(trimSpace(jsonString(m["role"])))
		switch role {
		case "system", "developer":
			sysTexts = append(sysTexts, textOfContent(m["content"])...)
			continue
		}

		upstreamRole := "user"
		if role == "assistant" {
			upstreamRole = "model"
		}

		var parts []part
		if role == "tool" || role == "function" {
			// 工具结果：name 由第一遍的映射给出；解析不出 name ⇒ 整块丢弃
			// （空 name 会被上游 400，丢了顶多少一轮工具上下文）。
			name := callNames[toolResultIDOf(m)]
			if name == "" {
				continue
			}
			parts = append(parts, part{FunctionResponse: &functionResponse{
				Name: name,
				Response: respObject{
					Content: strings.Join(textOfContent(m["content"]), "\n"),
					Error:   isToolError(m),
				},
			}})
		} else {
			for _, b := range contentList(m["content"]) {
				switch v := b.(type) {
				case string:
					if v != "" {
						parts = append(parts, part{Text: v})
					}
				case map[string]any:
					if isImagePart(v) {
						p, err := imagePartOf(v)
						if err != nil {
							// 图片读不到**必须报错**，不静默丢图（否则用户以为模型看到了）。
							return nil, nil, err
						}
						if p != nil {
							parts = append(parts, *p)
						}
						continue
					}
					if t := jsonString(v["text"]); t != "" {
						parts = append(parts, part{Text: t})
					}
					// 其余块（reasoning / thinking / signature 等）一律丢弃：对齐细节 2。
				}
			}
		}
		if role == "assistant" {
			for _, c := range toolCallsOf(m) {
				id := toolCallIDOf(c)
				if !keepCall(id) {
					continue
				}
				if p, ok := functionCallPart(c); ok {
					parts = append(parts, p)
				}
			}
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, content{Role: upstreamRole, Parts: parts})
	}
	return out, sysTexts, nil
}

// joinSystemTexts 把多条 system/developer 文本拼成一条 systemInstruction。
// 用空行分隔：多条 system 在本仓可能来自 developer→system 归一（不变量 1）与客户端
// 自带 system，拼接顺序即消息顺序，语义不丢。
func joinSystemTexts(texts []string) string {
	kept := make([]string, 0, len(texts))
	for _, t := range texts {
		if t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n\n")
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

// collectTools 归一工具声明为上游形状 `[{functionDeclarations:[{name,description,parameters}]}]`。
//
// 同时兼容两种入参形态：OpenAI 的 `{"type":"function","function":{…}}` 与已扁平的 `{name,…}`。
// 返回 ok=false 表示没有任何可用工具（此时不产出 tools / toolConfig 两个键）。
func collectTools(raw any) ([]toolGroup, bool) {
	list, _ := raw.([]any)
	decls := make([]functionDeclaration, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		src := t
		if fn, ok := t["function"].(map[string]any); ok {
			src = fn
		}
		name := jsonString(src["name"])
		if name == "" {
			continue
		}
		params, err := sanitizeParameters(src["parameters"])
		if err != nil {
			// schema 不可解析：**整个键退成 `{}`**，而不是让整轮请求失败。
			// 理由：上游对「没有参数声明的工具」是接受的，而对未知 schema 键是硬 400；
			// 一个畸形 schema 不该打死整轮对话（与 minimax 的容错口径一致）。
			params = alphaJSON("{}")
		}
		decls = append(decls, functionDeclaration{
			Name:        name,
			Description: jsonString(src["description"]),
			Parameters:  params,
		})
	}
	if len(decls) == 0 {
		return nil, false
	}
	return []toolGroup{{FunctionDeclarations: decls}}, true
}

// sanitizeParameters 清洗工具参数 schema 并做字母序规范化。
func sanitizeParameters(v any) (alphaJSON, error) {
	schema, err := schemaObject(v)
	if err != nil {
		return nil, err
	}
	if schema == nil {
		return alphaJSON("{}"), nil
	}
	return canonicalRaw(sanitizeSchema(schema))
}

// schemaObject 把入参归一成 schema 对象（对象原样；JSON 字符串则解析；其余 nil）。
func schemaObject(v any) (map[string]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return t, nil
	case string:
		if trimSpace(t) == "" {
			return nil, nil
		}
		obj, err := decodeRequest([]byte(t))
		if err != nil {
			return nil, err
		}
		return obj, nil
	}
	return nil, nil
}

// geminiSchemaKeys 上游 `FunctionDeclaration.parameters` 接受的键白名单
// （gemini.ts:583-603，逐字照抄）。
//
// ⚠️ 白名单**之外**的键（`$schema` / `additionalProperties` / `default` / `examples` /
// `$ref` …）必须**整键删除**——上游对未知键是**硬 400**，而各客户端的工具 schema 里
// 出现这些键是常态。
var geminiSchemaKeys = map[string]bool{
	"type": true, "format": true, "description": true, "nullable": true, "enum": true,
	"items": true, "minItems": true, "maxItems": true, "properties": true, "required": true,
	"minProperties": true, "maxProperties": true, "minLength": true, "maxLength": true,
	"pattern": true, "anyOf": true, "propertyOrdering": true, "minimum": true, "maximum": true,
}

// sanitizeSchema 递归清洗工具参数 schema（gemini.ts:615-681）：
//  1. 白名单外的键整键删除；
//  2. `properties` / `items` / `anyOf` 递归清洗（`items` 可能是对象或数组）；
//  3. `type` 为数组形态（如 `["string","null"]`）时收敛成单个 type 并补 `nullable: true`；
//     `enum` 只要含**任何非字符串**值就整删（Gemini 只接受字符串枚举）。
func sanitizeSchema(in map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		if !geminiSchemaKeys[key] {
			continue
		}
		switch key {
		case "properties":
			obj, ok := value.(map[string]any)
			if !ok {
				continue
			}
			props := map[string]any{}
			for name, child := range obj {
				cm, ok := child.(map[string]any)
				if !ok {
					continue
				}
				props[name] = sanitizeSchema(cm)
			}
			out[key] = props
		case "items":
			switch t := value.(type) {
			case []any:
				arr := make([]any, 0, len(t))
				for _, it := range t {
					if m, ok := it.(map[string]any); ok {
						arr = append(arr, sanitizeSchema(m))
					}
				}
				out[key] = arr
			case map[string]any:
				out[key] = sanitizeSchema(t)
			}
		case "anyOf":
			arr, ok := value.([]any)
			if !ok {
				continue
			}
			items := make([]any, 0, len(arr))
			for _, it := range arr {
				if m, ok := it.(map[string]any); ok {
					items = append(items, sanitizeSchema(m))
				}
			}
			out[key] = items
		case "type":
			switch t := value.(type) {
			case string:
				out[key] = t
			case []any:
				var nonNull []string
				hasNull := false
				for _, it := range t {
					s, ok := it.(string)
					if !ok {
						continue
					}
					if s == "null" {
						hasNull = true
						continue
					}
					nonNull = append(nonNull, s)
				}
				if len(nonNull) > 0 {
					out["type"] = nonNull[0]
				}
				if hasNull {
					out["nullable"] = true
				}
			}
		case "enum":
			arr, ok := value.([]any)
			if !ok {
				continue
			}
			allStrings := true
			for _, it := range arr {
				if _, ok := it.(string); !ok {
					allStrings = false
					break
				}
			}
			if allStrings {
				out[key] = append([]any(nil), arr...)
			}
		default:
			out[key] = value
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 消息形状小工具
// ---------------------------------------------------------------------------

// contentList 把 content 归一成块数组（字符串 → 单元素；数组原样；其余空）。
func contentList(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []any{t}
	case []any:
		return t
	case map[string]any:
		// 客户端偶尔直接给一个块对象（非数组）。
		return []any{t}
	}
	return nil
}

// textOfContent 抽取 content 里的全部文本（跳过非文本块）。
func textOfContent(v any) []string {
	var out []string
	for _, b := range contentList(v) {
		switch t := b.(type) {
		case string:
			if t != "" {
				out = append(out, t)
			}
		case map[string]any:
			if s := jsonString(t["text"]); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// toolCallsOf 取 assistant 消息的 tool_calls 数组。
func toolCallsOf(m map[string]any) []map[string]any {
	list, _ := m["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		if cm, ok := c.(map[string]any); ok {
			out = append(out, cm)
		}
	}
	return out
}

// toolCallIDOf 取 tool_call 的 id（顶层 `id`）。
func toolCallIDOf(c map[string]any) string { return jsonString(c["id"]) }

// toolCallNameOf 取 tool_call 的函数名（`function.name` 优先，退到顶层 `name`）。
func toolCallNameOf(c map[string]any) string {
	if fn, ok := c["function"].(map[string]any); ok {
		if name := jsonString(fn["name"]); name != "" {
			return name
		}
	}
	return jsonString(c["name"])
}

// toolResultIDOf 取工具结果消息对应的 tool_call id。
// 两种形态都认：OpenAI 的 `tool_call_id` 与部分客户端的驼峰 `toolCallId`。
func toolResultIDOf(m map[string]any) string {
	if id := jsonString(m["tool_call_id"]); id != "" {
		return id
	}
	return jsonString(m["toolCallId"])
}

// isToolError 判定工具结果是否为失败（OpenAI 无标准字段，客户端惯用 `is_error`/`isError`）。
func isToolError(m map[string]any) bool {
	for _, key := range []string{"is_error", "isError"} {
		if b, ok := m[key].(bool); ok && b {
			return true
		}
	}
	return false
}

// functionCallPart 把一条 tool_call 转成 functionCall part。
func functionCallPart(c map[string]any) (part, bool) {
	name := toolCallNameOf(c)
	if name == "" {
		return part{}, false
	}
	args, err := canonicalRaw(parseArguments(c))
	if err != nil {
		args = alphaJSON("{}")
	}
	return part{FunctionCall: &functionCall{Name: name, Args: args}}, true
}

// parseArguments 解析 tool_call 的参数：`function.arguments`（JSON **字符串**，OpenAI 惯例）
// 或已经是对象的形态。解析失败 / 非对象一律退化为 `{}`（**不编造参数**）。
func parseArguments(c map[string]any) map[string]any {
	var raw any
	if fn, ok := c["function"].(map[string]any); ok {
		raw = fn["arguments"]
	}
	if raw == nil {
		raw = c["arguments"]
	}
	switch t := raw.(type) {
	case string:
		if trimSpace(t) == "" {
			return map[string]any{}
		}
		obj, err := decodeRequest([]byte(t))
		if err != nil {
			return map[string]any{}
		}
		return obj
	case map[string]any:
		return t
	}
	return map[string]any{}
}

// ---------------------------------------------------------------------------
// 图片
// ---------------------------------------------------------------------------

// isImagePart 判定内容块是否为图片块（三种 OpenAI 生态写法都认）。
func isImagePart(v map[string]any) bool {
	switch jsonString(v["type"]) {
	case "image_url", "image", "input_image":
		return true
	}
	return false
}

// imagePartOf 把图片内容块转成 `inlineData`。
//
// ⚠️ 只支持 **data: URL** 形态（`data:image/png;base64,<裸 base64>`）：上游要的就是裸
// base64，而 http(s) 图片需要本地抓取+转码，本包不做（也就**不会**静默丢图——直接报错）。
func imagePartOf(v map[string]any) (*part, error) {
	url, ok := imageURLOf(v)
	if !ok || url == "" {
		return nil, fmt.Errorf("图片块缺少 URL（只支持 data: URL 形态的 base64 图片）")
	}
	mime, data, ok := splitDataURL(url)
	if !ok {
		return nil, fmt.Errorf("图片不是 data: URL 形态（本包不代抓 http(s) 图片，避免静默丢图）")
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("图片 mimeType %q 不是 image/*", mime)
	}
	clean := normalizeBase64(data)
	if clean == "" {
		return nil, fmt.Errorf("图片 base64 不可用（长度非 4 的倍数或含非法字符）")
	}
	return &part{InlineData: &inlineData{MimeType: mime, Data: clean}}, nil
}

// imageURLOf 取图片块里的 URL（`image_url` 可能是字符串或 `{url:…}` 对象）。
func imageURLOf(v map[string]any) (string, bool) {
	for _, key := range []string{"image_url", "image", "input_image", "url"} {
		raw, has := v[key]
		if !has {
			continue
		}
		switch t := raw.(type) {
		case string:
			return t, true
		case map[string]any:
			if u := jsonString(t["url"]); u != "" {
				return u, true
			}
		}
	}
	return "", false
}

// splitDataURL 拆 `data:<mime>;base64,<payload>`；非 data URL / 非 base64 时 ok=false。
func splitDataURL(u string) (mime, payload string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	i := strings.IndexByte(rest, ',')
	if i < 0 {
		return "", "", false
	}
	meta, body := rest[:i], rest[i+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mime = strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return mime, body, true
}

// normalizeBase64 宽松清洗并校验 base64（协议 §2.4：上游接受带换行的 base64，
// 故先去掉 `\n`/`\r`/空白再校验；本地校验若不容忍换行会把**合法**的图片判死）。
func normalizeBase64(data string) string {
	var b strings.Builder
	for i := 0; i < len(data); i++ {
		switch c := data[i]; c {
		case '\n', '\r', ' ', '\t':
			continue
		default:
			b.WriteByte(c)
		}
	}
	clean := b.String()
	if clean == "" || len(clean)%4 != 0 {
		return ""
	}
	pad := 0
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			if pad > 0 {
				return "" // 填充符后不得再有数据字符
			}
		case c == '=':
			pad++
			if pad > 2 {
				return ""
			}
		default:
			return ""
		}
	}
	return clean
}
