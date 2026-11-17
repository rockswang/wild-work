package doubao

import "encoding/json"

// jsonUnmarshalImpl 集中 encoding/json 的引用点（doubao.go 只暴露薄封装）。
func jsonUnmarshalImpl(b []byte, v any) error { return json.Unmarshal(b, v) }
