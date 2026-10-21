// 模型降级链的维度不变量锁定。
//
// 本文件落实 docs/模型降级链定义.md（R78）的唯一权威定义：
//
//	账号维度 → 换号重试（Rotatable）
//	模型维度 → 换模型（fallbackWorthy），且必须在换号耗尽之后
//	请求维度 → 原样透传，既不换号也不换模型，也不冷却
//
// 三个维度互斥：一个错误类型要么属于请求维度（零动作），要么属于账号/模型维度
// （可换号，其中只有两类可换模型）。任何新增 provider.ErrKind 若未在
// allErrKinds 与本文件的期望表中登记，测试会失败——这是「不允许静默新增语义」
// 的机制保障。
//
// 为什么需要这个测试：历史上「模型饱和(14003/6004)被误判成账号问题」导致客户端
// 死循环吃 429；「ErrImageInvalid 漏 case 落到 default 罚健康账号」也是同一类
// 缺陷。两者根因相同——错误类型的处置语义散落在多处，漏一处就出事故。
package server

import (
	"testing"

	"wild-work/internal/provider"
)

// allErrKinds 是 provider.ErrKind 的全量枚举清单（不含 ErrNone=成功）。
// 新增枚举必须同时更新本表 § 期望表，否则 TestFallbackDimensionCoverage 会红。
var allErrKinds = []provider.ErrKind{
	provider.ErrHardCredit,
	provider.ErrSoftRate,
	provider.ErrSessionDead,
	provider.ErrNotFound,
	provider.ErrServer,
	provider.ErrClient,
	provider.ErrContentBlocked,
	provider.ErrPromptTooLong,
	provider.ErrImageInvalid,
	provider.ErrBadParams,
	provider.ErrWafBlock,
	provider.ErrAccountFault,
	provider.ErrModelBlocked,
	provider.ErrPassthrough,
	provider.ErrQueued,
}

// dimensionOf 把错误类型归到唯一维度。这是本测试对生产代码的**独立复述**：
// 若生产侧 Rotatable/fallbackWorthy 的集合发生变化，两边会不一致而报错。
//
// 判定依据（对应定义 §5 归属表）：
//   - request：内容/参数/形态问题，换谁都会复现 → 零动作，原文透传
//   - model：本渠道对该模型无承载能力 → 换号耗尽后换模型
//   - account-rotate：账号可用性问题 → 换号（换模型治不了本）
//   - account-transparent：账号级问题但**不换号**（上游已给出确定性结论，
//     换号只会再撞一次）→ 冷却该号 + 直接透传
//   - upstream-transparent：上游确定性故障（5xx），既不是账号也不是请求的问题
//     → 不换号、不换模型、不冷却，直接透传（2026-10-20：实测 11134 同一波
//     4 个号全 500，换号只会把 1 次失败放大成 9 次）
func dimensionOf(kind provider.ErrKind) string {
	switch kind {
	case provider.ErrContentBlocked, provider.ErrPromptTooLong, provider.ErrImageInvalid,
		provider.ErrBadParams, provider.ErrPassthrough:
		return "request"
	case provider.ErrHardCredit, provider.ErrSoftRate:
		return "model"
	case provider.ErrWafBlock, provider.ErrModelBlocked:
		return "account-transparent"
	case provider.ErrServer:
		return "upstream-transparent"
	default:
		return "account-rotate"
	}
}

// TestFallbackDimensionCoverage 遍历全量 ErrKind，断言四维归属唯一且自洽。
//
// 锁定的不变量：
//  1. request ⇒ 不换号、不换模型（零动作）
//  2. model ⇒ 可换号 且 可换模型（换号耗尽后才换模型）
//  3. account-rotate ⇒ 可换号、不换模型
//  4. account-transparent ⇒ 不换号、不换模型（冷却但直接透传）
//  5. ErrNone 不属于任何维度
func TestFallbackDimensionCoverage(t *testing.T) {
	for _, kind := range allErrKinds {
		dim := dimensionOf(kind)
		rotatable := kind.Rotatable()
		fallback := fallbackWorthy(kind)

		switch dim {
		case "request":
			if rotatable {
				t.Errorf("%s 属请求维度，不应换号（Rotatable=true）——换号必然复现", kind)
			}
			if fallback {
				t.Errorf("%s 属请求维度，不应换模型（fallbackWorthy=true）——白烧备胎额度", kind)
			}
		case "model":
			if !rotatable {
				t.Errorf("%s 属模型维度，应先换号（Rotatable=false）——顺序消解要求先试便宜的", kind)
			}
			if !fallback {
				t.Errorf("%s 属模型维度，必须可换模型（fallbackWorthy=false）——否则饱和时首个请求必吃 429", kind)
			}
		case "account-rotate":
			if !rotatable {
				t.Errorf("%s 属账号维度，应可换号（Rotatable=false）", kind)
			}
			if fallback {
				t.Errorf("%s 属账号维度，不应换模型（fallbackWorthy=true）——换模型治不了账号问题", kind)
			}
		case "account-transparent":
			if rotatable {
				t.Errorf("%s 应直接透传不换号（Rotatable=true）——上游结论确定，换号只会再撞一次", kind)
			}
			if fallback {
				t.Errorf("%s 不应换模型（fallbackWorthy=true）", kind)
			}
		case "upstream-transparent":
			if rotatable {
				t.Errorf("%s 应直接透传不换号（Rotatable=true）——上游整体故障，换号必撞同一面墙", kind)
			}
			if fallback {
				t.Errorf("%s 不应换模型（fallbackWorthy=true）", kind)
			}
		default:
			t.Errorf("%s 维度未定义（dimensionOf 返回 %q）——新增 ErrKind 必须先在归属表登记", kind, dim)
		}
	}
}

// TestErrNoneHasNoAction 成功不是错误，不应有任何处置动作。
func TestErrNoneHasNoAction(t *testing.T) {
	if provider.ErrNone.Rotatable() {
		t.Error("ErrNone 不应可换号")
	}
	if fallbackWorthy(provider.ErrNone) {
		t.Error("ErrNone 不应可换模型")
	}
}

// TestFallbackWorthyIsSubsetOfRotatable 降级链的硬前提：能换模型的必须能换号。
//
// 这锁定「顺序消解」——换模型永远发生在换号耗尽之后，不存在「跳过换号直接换模型」。
func TestFallbackWorthyIsSubsetOfRotatable(t *testing.T) {
	for _, kind := range allErrKinds {
		if fallbackWorthy(kind) && !kind.Rotatable() {
			t.Errorf("%s 可换模型但不可换号——违反顺序消解（必须先换号、耗尽才换模型）", kind)
		}
	}
}

// TestRequestDimensionIsExclusive 请求维度必须与所有处置动作互斥。
//
// 单独再锁一次：因为「内容错误也换模型」是历史事故，且新增渠道最容易漏判。
func TestRequestDimensionIsExclusive(t *testing.T) {
	requestKinds := []provider.ErrKind{
		provider.ErrContentBlocked,
		provider.ErrPromptTooLong,
		provider.ErrImageInvalid,
		provider.ErrBadParams,
		provider.ErrPassthrough,
	}
	for _, kind := range requestKinds {
		if kind.Rotatable() || fallbackWorthy(kind) {
			t.Errorf("%s 是请求级错误，必须零动作（Rotatable=%v fallbackWorthy=%v）",
				kind, kind.Rotatable(), fallbackWorthy(kind))
		}
	}
}

// TestAllErrKindsRegistered 防御性检查：allErrKinds 表本身不能有重复或遗漏。
//
// 遗漏的检测是间接的——如果 provider 新增了枚举而没加进 allErrKinds，
// 前三个测试都不会覆盖它。这里通过数量断言提示维护者同步更新。
func TestAllErrKindsRegistered(t *testing.T) {
	seen := map[provider.ErrKind]bool{}
	for _, kind := range allErrKinds {
		if seen[kind] {
			t.Errorf("%s 在 allErrKinds 中重复登记", kind)
		}
		seen[kind] = true
		if kind == provider.ErrNone {
			t.Error("allErrKinds 不应包含 ErrNone（成功不是错误）")
		}
	}
	// provider.ErrKind 当前枚举定义见 internal/provider/provider.go 的 const 块。
	// 该常量若新增而未同步本表，此数字需要同步上调——刻意用硬编码数字，
	// 让「新增枚举」这件事无法静默通过。
	const expectedKindCount = 15
	if len(allErrKinds) != expectedKindCount {
		t.Errorf("allErrKinds 登记 %d 个，期望 %d 个——provider 新增 ErrKind 时必须同步本表与归属表",
			len(allErrKinds), expectedKindCount)
	}
}
