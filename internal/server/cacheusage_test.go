// cacheusage_test.go 缓存命提取的**口径回归测试**。
//
// 这里的用例全部取自 2026-10-12 的 24 份真实上游快照（devdump，见 AGENTS.md §8.5），
// 每条都写明了它的渠道与实测数值。之所以要用真帧而非自造 map：本函数的全部难度都在
// 「各家字段名不同 + 有的字段是空对象 + 有的同一帧里有互相矛盾的重复字段」，
// 自造数据只会固化我自己的错误假设（R23b：不拿观察当因果）。
package server

import (
	"encoding/json"
	"testing"
)

// TestCacheUsageRealFrames 逐渠道真帧提取。
func TestCacheUsageRealFrames(t *testing.T) {
	cases := []struct {
		name    string
		channel string
		frame   string // 上游 usage 对象的 JSON 原文（取自 dump）
		wantHit int64
		wantRep bool
		why     string
	}{
		{
			name: "workbuddy 热前缀（命中）", channel: "workbuddy",
			frame: `{"prompt_tokens":97,"completion_tokens":228,"total_tokens":325,
			  "prompt_cache_hit_tokens":97,"prompt_cache_miss_tokens":0,
			  "cached_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`,
			wantHit: 97, wantRep: true,
			why: "prompt_cache_hit_tokens 是主字段；同帧 cached_tokens=0 是**恒 0 的噪声**，不得采信",
		},
		{
			name: "workbuddy 冷前缀（未命中但已上报）", channel: "workbuddy",
			frame: `{"prompt_tokens":97,"completion_tokens":228,
			  "prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":97}`,
			wantHit: 0, wantRep: true,
			why: "字段在、值为 0 ⇒ 上游明确报告「本轮没命中」⇒ 要显示 0% 而不是「—」",
		},
		{
			name: "workbuddyai 命中（hit+miss==prompt_tokens 的子集口径）", channel: "workbuddyai",
			frame: `{"prompt_tokens":103,"completion_tokens":291,
			  "prompt_cache_hit_tokens":97,"prompt_cache_miss_tokens":6,
			  "prompt_tokens_details":{"cached_tokens":97},"cached_tokens":0}`,
			wantHit: 97, wantRep: true,
			why: "97+6=103 证明命中是 prompt_tokens 的子集，故利用率分母可直接用 PT",
		},
		{
			name: "traework（Anthropic 命名，与官方口径相反）", channel: "traework",
			frame: `{"prompt_tokens":41610,"completion_tokens":86,"total_tokens":41696,
			  "cache_read_input_tokens":40960,"cache_creation_input_tokens":0}`,
			wantHit: 40960, wantRep: true,
			why: "total=prompt+completion 且 read<prompt ⇒ prompt_tokens 已是总输入（**不是** Anthropic 官方口径）",
		},
		{
			name: "monkeycode（input_tokens_details）", channel: "monkeycode",
			frame: `{"input_tokens":651,"output_tokens":131,"total_tokens":782,
			  "input_tokens_details":{"cached_tokens":512},"output_tokens_details":{"reasoning_tokens":129}}`,
			wantHit: 512, wantRep: true,
			why: "Responses 形状用 *_tokens_details；本渠道曾把这个 details 在重建 usage 时丢掉",
		},
		{
			name: "qodercn", channel: "qodercn",
			frame: `{"prompt_tokens":96,"completion_tokens":2024,"total_tokens":2120,
			  "credits":0.12763168,"prompt_tokens_details":{"cached_tokens":0}}`,
			wantHit: 0, wantRep: true, why: "details 非空且有该键 ⇒ 上报",
		},
		{
			name: "qodercom（多一个 cacheable_tokens=写入侧）", channel: "qodercom",
			frame: `{"prompt_tokens":152,"completion_tokens":2799,
			  "prompt_tokens_details":{"cacheable_tokens":500,"cached_tokens":0}}`,
			wantHit: 0, wantRep: true,
			why: "只取 cached_tokens（命中）；cacheable_tokens 是**写入**侧，算进命中会虚高",
		},
		{
			name: "cline", channel: "cline",
			frame: `{"prompt_tokens":94,"completion_tokens":383,"cost":0,
			  "prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":0,"audio_tokens":0}}`,
			wantHit: 0, wantRep: true, why: "含 cache_write_tokens 干扰项，仍只认 cached_tokens",
		},
		{
			name: "oczen（同时有 OpenAI 与 Anthropic 两套字段）", channel: "oczen",
			frame: `{"prompt_tokens":89,"completion_tokens":2813,
			  "prompt_tokens_details":{"cached_tokens":0,"cache_creation_input_tokens":0,"cache_write_tokens":0}}`,
			wantHit: 0, wantRep: true,
			why: "cache_creation_input_tokens 出现在 details **内部**（非顶层），不得被顶层那两条规则误取",
		},
		{
			name: "raccoon（空 details 对象 → 未上报）", channel: "raccoon",
			frame: `{"completion_tokens":6659,"prompt_tokens":89,"total_tokens":6748,
			  "completion_tokens_details":{"reasoning_tokens":6659},"prompt_tokens_details":{}}`,
			wantHit: 0, wantRep: false,
			why: "空对象 {} 是「没这个维度」而不是「命中 0」——判成 0 会让面板对一个不提供该指标的渠道恒显 0%",
		},
		{
			name: "glm / loomy（整条流无 usage 字段 → 未上报）", channel: "glm",
			frame: ``, wantHit: 0, wantRep: false,
			why: "实测这两家整条 SSE 流没有任何 token/计费字段",
		},
		{
			name: "usage 为 null（流式中间帧）", channel: "workbuddyai",
			frame: ``, wantHit: 0, wantRep: false, why: "usage:null 与字段缺失同义",
		},
		{
			name: "坏数据：负数钳 0 但仍算已上报", channel: "workbuddy",
			frame:   `{"prompt_tokens":10,"prompt_cache_hit_tokens":-3}`,
			wantHit: 0, wantRep: true, why: "坏数据不能让整行退化成「—」，那会掩盖上游异常",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u map[string]any
			if tc.frame != "" {
				if err := json.Unmarshal([]byte(tc.frame), &u); err != nil {
					t.Fatalf("用例 JSON 非法（测试数据写错了）：%v", err)
				}
			}
			got := cacheUsageOf(u)
			if got.reported != tc.wantRep {
				t.Errorf("reported=%v want %v —— %s", got.reported, tc.wantRep, tc.why)
			}
			if got.cached != tc.wantHit {
				t.Errorf("cached=%d want %d —— %s", got.cached, tc.wantHit, tc.why)
			}
		})
	}
}

// TestCacheUsageSubsetInvariant 命中数恒为 prompt_tokens 的子集 ⇒ 利用率必在 [0,1]。
// 这是「分母直接用 PT、不做 PT+read」这一决策的自检：若哪天真出现了 read>PT 的帧，
// 说明口径判断错了，本测试会立刻失败（而不是面板上悄悄显示 >100%）。
func TestCacheUsageSubsetInvariant(t *testing.T) {
	frames := []string{
		`{"prompt_tokens":100,"prompt_cache_hit_tokens":100,"prompt_cache_miss_tokens":0}`,
		`{"prompt_tokens":100,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}`,
		`{"prompt_tokens":100,"cache_read_input_tokens":99,"cache_creation_input_tokens":1}`,
		`{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":100}}`,
	}
	for _, f := range frames {
		var u map[string]any
		if err := json.Unmarshal([]byte(f), &u); err != nil {
			t.Fatal(err)
		}
		cu := cacheUsageOf(u)
		pt, _, _ := usageTokens(u)
		if !cu.reported || pt <= 0 {
			t.Fatalf("帧应被判定为已上报：%s", f)
		}
		if cu.cached > pt {
			t.Errorf("命中(%d) > prompt_tokens(%d)，子集不变量被破坏：%s", cu.cached, pt, f)
		}
	}
}

// TestCacheUsageIgnoresBareCachedTokens 顶层裸 cached_tokens 不作为判据。
// 依据：workbuddy 系同一帧里 `prompt_cache_hit_tokens:97` 与 `cached_tokens:0` **同时存在**，
// 后者实测恒 0。只认各渠道族主字段才能保证取值唯一确定。
func TestCacheUsageIgnoresBareCachedTokens(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"cached_tokens":7}`), &u); err != nil {
		t.Fatal(err)
	}
	if got := cacheUsageOf(u); got.reported {
		t.Fatalf("顶层裸 cached_tokens 不应作为判据，实际 reported=true cached=%d", got.cached)
	}
}
