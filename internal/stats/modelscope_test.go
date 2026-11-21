package stats

import "testing"

// TestChannelKindCoversModelscope channelKind 白名单必须含魔搭两个 kind：
// 漏了任一项 ⇒ 该区的 token 与请求都不进平台概览表，而工具本身不报错
// （R130 的 comate 漏项就是这个形态）。
func TestChannelKindCoversModelscope(t *testing.T) {
	for _, k := range []string{"modelscope", "modelscopeai"} {
		if !IsKnownChannel(k) {
			t.Errorf("channelKind 缺少 %s（会静默丢该渠道全部 token/请求统计）", k)
		}
	}
	// 反向护栏：前缀不含糊（不得出现 "modelscope-" 这类派生名混进白名单判定）。
	if IsKnownChannel("modelscope-x") {
		t.Error("IsKnownChannel 不应命中未登记的前缀")
	}
}
