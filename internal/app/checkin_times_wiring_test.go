// checkin_times_wiring_test.go 锁住 issue #210 的装配面：
// 「签到时刻用全局配置」与「声明 FollowGlobalCheckin」必须成对出现。
//
// 为什么需要静态断言：漏掉 FollowGlobalCheckin 不会编译报错、不会 panic、不会写日志，
// 只表现为「用户在面板改签到时间后，这条渠道不再跟随」（比红标更隐蔽）。反过来
// 给 []int{} 的渠道加上它，就是 issue #210 本体的复发（每天空跑必然失败的签到）。
package app

import (
	"os"
	"strings"
	"testing"
)

// schedulerConfigBlocks 从 main.go 里抽出每个 scheduler.New(scheduler.Config{...}) 的字面量。
// 用花括号计数而不是正则：Config 字面量里有 `[]int{...}`，正则的 `[^}]*` 会提前截断。
func schedulerConfigBlocks(src string) []string {
	const anchor = "scheduler.New(scheduler.Config{"
	var out []string
	for i := 0; ; {
		j := strings.Index(src[i:], anchor)
		if j < 0 {
			return out
		}
		start := i + j + len(anchor) - 1 // 指向 '{'
		depth := 0
		k := start
		for ; k < len(src); k++ {
			switch src[k] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		out = append(out, src[start:k+1])
		i = k + 1
	}
}

// TestMainGoFollowGlobalCheckinWiring 逐块核对标记与签到时刻的一致性。
func TestMainGoFollowGlobalCheckinWiring(t *testing.T) {
	mainSrc, err := os.ReadFile("../../cmd/wild-work/main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	blocks := schedulerConfigBlocks(string(mainSrc))
	if len(blocks) < 20 {
		t.Fatalf("只解析到 %d 个 scheduler.Config 字面量，解析器可能已失效（应 >= 20）", len(blocks))
	}

	var followers, skipped int
	for _, b := range blocks {
		name := ""
		if i := strings.Index(b, `Name: "`); i >= 0 {
			rest := b[i+len(`Name: "`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				name = rest[:j]
			}
		}
		if name == "" {
			t.Errorf("无法从调度器配置里取到渠道名：%s", b)
			continue
		}
		follows := strings.Contains(b, "FollowGlobalCheckin: true")
		usesGlobal := strings.Contains(b, "CheckinMinutes: checkinMinutes")
		// 三种「不跟随」的合法形态：显式空切片（无签到任务）、固定时刻（Qoder 双区窗口）、
		// 以及 keepalive-only 渠道（gemini：CheckinMinutes 空、只开保活）。
		noCheckin := strings.Contains(b, "CheckinMinutes: []int{}")
		fixedMinute := strings.Contains(b, "CheckinMinutes: []int{qoderCheckinMinute}")

		switch {
		case usesGlobal && !follows:
			t.Errorf("❌ %s 用全局签到时刻却没声明 FollowGlobalCheckin: true —— "+
				"用户在面板改签到时间后这条渠道不会跟着变（静默失效）", name)
		case follows && !usesGlobal:
			t.Errorf("❌ %s 声明了 FollowGlobalCheckin: true 但签到时刻不是全局配置 —— "+
				"推送会把它已有的固定/空时刻覆盖掉（issue #210 复发）", name)
		case !usesGlobal && !noCheckin && !fixedMinute:
			t.Errorf("❌ %s 的 CheckinMinutes 既不是全局配置、也不是显式空切片或固定窗口：%s", name, b)
		}
		if follows {
			followers++
		} else {
			skipped++
		}
	}
	// 两个方向都得有样本，否则断言可能因为「全是同一种」而空转。
	if followers == 0 || skipped == 0 {
		t.Fatalf("跟随/不跟随两侧必须都有渠道（followers=%d skipped=%d）", followers, skipped)
	}
	// 点名守住最容易踩的几个：无签到任务的渠道绝不跟随，Qoder 双区绝不跟随。
	for _, name := range []string{"qwenwork", "cline", "comate", "oczen", "kilo", "zcode", "gemini", "qodercn", "qodercom"} {
		for _, b := range blocks {
			if !strings.Contains(b, `Name: "`+name+`"`) {
				continue
			}
			if strings.Contains(b, "FollowGlobalCheckin: true") {
				t.Errorf("❌ %s 不得跟随全局签到时刻（无签到任务/固定窗口）", name)
			}
		}
	}
	// 有签到活动的渠道必须跟随（漏了就静默失效）。
	for _, name := range []string{"workbuddy", "traework", "monkeycode", "raccoon", "loomy", "lingxi", "codearts", "glm"} {
		found := false
		for _, b := range blocks {
			if !strings.Contains(b, `Name: "`+name+`"`) {
				continue
			}
			found = true
			if !strings.Contains(b, "FollowGlobalCheckin: true") {
				t.Errorf("❌ %s 应跟随全局签到时刻", name)
			}
		}
		if !found {
			t.Errorf("❌ main.go 里找不到 %s 的调度器配置", name)
		}
	}
}
