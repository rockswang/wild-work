package officeace

import (
	"bufio"
	"bytes"
	"io"
)

// rewriteModelReader 把上游 SSE 里的 "model":"..." 回填为客户端请求模型名。
// （上游常返回裸模型名或 "auto"，需回填成客户端看到的 <渠道>/<模型>。）
type modelRewriter struct {
	src     *bufio.Reader
	pending []byte
	model   string
}

func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteModelLine(line, m.model)
		}
		if err != nil {
			if len(m.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model":"`)
	idx := bytes.Index(line, needle)
	if idx < 0 {
		return line
	}
	start := idx + len(needle)
	end := bytes.IndexByte(line[start:], '"')
	if end < 0 {
		return line
	}
	end += start
	var out bytes.Buffer
	out.Grow(len(line) + len(model))
	out.Write(line[:start])
	out.WriteString(model)
	out.Write(line[end:])
	return out.Bytes()
}
