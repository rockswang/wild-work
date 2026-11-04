// response.go 上游响应里的工具名还原。
//
// 整形层把客户端工具改成了 `clint_<摘要>`，官方工具则用它们自己的名字。
// 客户端只认识自己声明的名字，故这里在**返回给调用方的那一层**把流式 SSE
// 逐行改写回去：`ChatStream` 包一次，`Stream`（直接透传）与 `Aggregate`
// （本地聚合成非流式）两条路径都自动吃到，不需要各自实现一遍。
//
// 还原规则由 {@link toolRenameMap.resolve} 决定：精确命中线上名 → 客户端名；
// 官方名（如 `read`）经折叠匹配落到客户端声明的 `Read`；名字只差大小写或
// 分隔符时同样折叠回去；有歧义时绝不猜。
package oczen

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// toolNameFieldRe 匹配 JSON 对象里的字符串 `name` 字段。
// 正则只吃「未转义的双引号」，故 tool_calls.arguments 里被转义的 `\"name\"`
// 不会被误改。
var toolNameFieldRe = regexp.MustCompile(`"name"\s*:\s*"([^"\\]*)"`)

// newToolNameReader 用一行行改写包装上游流。无改名时原样返回。
func newToolNameReader(src io.ReadCloser, renames *toolRenameMap) io.ReadCloser {
	if src == nil || renames == nil || renames.len() == 0 {
		return src
	}
	return &toolNameReader{src: src, br: bufio.NewReader(src), renames: renames}
}

// toolNameReader 按行（SSE 的 data 行）改写工具名，保留原始分帧与换行。
type toolNameReader struct {
	src     io.ReadCloser
	br      *bufio.Reader
	renames *toolRenameMap
	pending []byte
	done    bool
}

func (r *toolNameReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		line, err := r.br.ReadString('\n')
		if line != "" {
			r.pending = []byte(rewriteToolNamesLine(line, r.renames))
		}
		if err != nil {
			r.done = true
			if len(r.pending) == 0 {
				return 0, err
			}
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *toolNameReader) Close() error { return r.src.Close() }

// rewriteToolNamesLine 把一行里的工具名还原成调用方声明的名字。
// 不含 `"name"` 的行零分配原样返回。
func rewriteToolNamesLine(line string, renames *toolRenameMap) string {
	if renames == nil || renames.len() == 0 || !strings.Contains(line, `"name"`) {
		return line
	}
	changed := false
	out := toolNameFieldRe.ReplaceAllStringFunc(line, func(match string) string {
		sub := toolNameFieldRe.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		client := renames.resolve(sub[1])
		if client == "" || client == sub[1] {
			return match
		}
		encoded, err := json.Marshal(client)
		if err != nil {
			return match
		}
		changed = true
		return `"name":` + string(encoded)
	})
	if !changed {
		return line
	}
	return out
}
