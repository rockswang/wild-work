// 降级记账归属锁定（2026-10-16 口径变更）：降级命中后，谁实际工作、谁实际
// 消耗 token/积分，就记在谁名下——
//   - statRow.Model / stats.AddUsage → CanonicalModelID(fbID)（备胎规范对外 ID）；
//   - ledger.AppendUsage.Model → UpstreamModelOf(fbID)（备胎上游模型名）；
//   - Ch 同步为备胎渠道（Ch 本就取降级后 rt.Kind，改后 Model 与 Ch 自洽）。
// 无降级时保持原口径不变（canonModel / 客户端请求名的上游模型名）。
// 原请求名→备胎的对照由日志行 + 降级记录区块 + X-Model-Fallback 头承担。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/oczen"
	"wild-work/internal/provider"
	"wild-work/internal/stats"
)

// acctUpstream 假上游：非流式 Aggregate 回填 model 字段并上报 usage，
// 使 handler 走到 ledger/stats 记账分支。
type acctUpstream struct{}

func (u *acctUpstream) RefreshToken(*auth.Auth) error                                  { return nil }
func (u *acctUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error)           { return nil, nil }
func (u *acctUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (u *acctUpstream) UserResource(*auth.Auth) (int64, error)                         { return 0, nil }
func (u *acctUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *acctUpstream) DailyCheckin(*auth.Auth) error         { return nil }
func (u *acctUpstream) Classify(int, string) provider.ErrKind { return provider.ErrServer }
func (u *acctUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), 200, nil, nil
}
func (u *acctUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *acctUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	// usage 数值用 float64：numField 按该类型断言（json.Unmarshal 的原生形态）
	return map[string]any{
		"model":   model,
		"usage":   map[string]any{"prompt_tokens": float64(11), "completion_tokens": float64(7)},
		"choices": []any{},
	}, nil
}

// readUsageToday 读 ledger 目录下当月 usage 文件的全部行（测试专用；
// 每个用例独立 TempDir，文件内只有本用例写入的行）。
func readUsageToday(t *testing.T, dir string) []ledger.UsageEntry {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.UsageEntry
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "usage-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var ue ledger.UsageEntry
			if err := json.Unmarshal([]byte(line), &ue); err != nil {
				t.Fatal(err)
			}
			out = append(out, ue)
		}
	}
	return out
}

// TestFallbackUsageBookedUnderActualModel 降级命中的请求：请求日志行、今日模型
// 消耗、token 流水全部归属**备胎模型**，主模型名不出现在任何记账处。
func TestFallbackUsageBookedUnderActualModel(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, pa, _ := newFbHandler(t, &acctUpstream{}, &acctUpstream{}, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	ledgerDir := t.TempDir()
	ld, err := ledger.New(ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	h.stats, h.ledger = st, ld

	// ① 健康基线：无降级 → 记账归主模型（锁定原口径不被误伤）
	callModel(h, "oczen/big-pickle")
	ld.Flush() // ledger 缓冲写：须显式落盘后才能读到流水
	rows := st.Logs(10)
	if len(rows) != 1 || rows[0].Model != "oczen/big-pickle" {
		t.Fatalf("无降级时请求日志 Model 应为主模型 oczen/big-pickle，实际 %+v", rows)
	}
	ues := readUsageToday(t, ledgerDir)
	if len(ues) != 1 || ues[0].Model != "big-pickle" || ues[0].Ch != "oczen" {
		t.Fatalf("无降级时流水应为 Model=big-pickle Ch=oczen，实际 %+v", ues)
	}
	if ms := st.Snapshot(time.Now()).Models; len(ms) != 1 || ms[0].Model != "oczen/big-pickle" {
		t.Fatalf("无降级时模型消耗应归主模型，实际 %+v", ms)
	}

	// ② 降级命中 → 三处记账全部归备胎
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	callModel(h, "oczen/big-pickle")
	ld.Flush()

	// 请求日志（面板「请求日志最近15条」）：Model = 备胎规范对外 ID
	rows = st.Logs(10)
	if len(rows) != 2 || rows[1].Model != "workbuddy/glm-free" {
		t.Fatalf("降级后请求日志 Model 应为备胎 workbuddy/glm-free，实际 %+v", rows)
	}
	// token 流水：Model = 备胎上游模型名，Ch = 备胎渠道（自洽）
	ues = readUsageToday(t, ledgerDir)
	if len(ues) != 2 || ues[1].Model != "glm-free" || ues[1].Ch != "workbuddy" {
		t.Fatalf("降级后流水应为 Model=glm-free Ch=workbuddy，实际 %+v", ues)
	}
	// 今日模型消耗：归备胎；主模型行不得新增消耗
	found := map[string]int64{}
	for _, m := range st.Snapshot(time.Now()).Models {
		found[m.Model] = m.Tokens
	}
	if found["workbuddy/glm-free"] != 18 { // 11 prompt + 7 completion
		t.Fatalf("备胎行 tokens 应为 18，实际 %v", found)
	}
	if found["oczen/big-pickle"] != 18 { // ①健康基线那笔，未被②重复计入
		t.Fatalf("主模型行 tokens 应保持基线 18（降级请求不得计入主模型），实际 %v", found)
	}
	ld.Close() // Windows：关闭文件句柄，否则 t.TempDir 清理失败
}
