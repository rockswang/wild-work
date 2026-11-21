package stats

import (
	"sort"
	"testing"

	"wild-work/internal/provider"
)

// TestChannelKindCoversBuiltins 平台概览表的渠道白名单必须覆盖**全部内建渠道**。
//
// 为什么需要：漏登**不报错、不失败、面板也不提示** —— 该渠道的 token 与请求
// 静默不进平台表（doubao / lingxi 就靠人工发现才补上，R130 同款教训）。
// 判据用 provider.ChannelDisplayOrder（= 内建渠道全集的唯一真源，不含 ModelNamespaces
// 里那类「上游命名空间直通」条目如 cline-free —— 那些不是独立渠道），
// 新增渠道漏登即在此变红。
func TestChannelKindCoversBuiltins(t *testing.T) {
	var missing []string
	for _, k := range provider.ChannelDisplayOrder {
		if !IsKnownChannel(k.String()) {
			missing = append(missing, k.String())
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("stats.channelKind 漏了这些内建渠道（遗漏会让其 token/请求静默不进平台概览表）：%v", missing)
	}
}
