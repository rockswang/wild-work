//go:build live

// live_gateway401_test.go 网关 HTML 401 的实盘探针（只读，不改任何账号文件）。
//
// 背景（2026-10-17 现场）：两个 WorkBuddyCN 账号的 refresh token 被上游吊销后，
// `www.codebuddy.cn` 的账单/目录/对话接口一律返回 APISIX 的 HTML 401 页。修复把
// 这种形态归成 ErrSessionDead（触发 refresh + 重试），并把无信息量的 HTML 换成
// 可行动文案。本探针用**真实账号**确认这两件事在真上游成立。
//
// ⚠️ 严格遵守 AGENTS.md §0.1：每个账号**只发 1 次**账单请求 + 至多 1 次 refresh，
// 不做消元矩阵、不做重复压测。本探针**不落盘**（不改 auth 文件）。
//
// 运行方式（仓库根目录）：
//
//	WILDWORK_AUTHDIR="$HOME/Library/Application Support/WildWork/auths" \
//	  go test -tags live ./internal/upstream/ -run TestLiveGateway401 -v -timeout 120s
//
// 指定单个账号：WILDWORK_LIVE_UID=<uid 前缀>
package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

func TestLiveGateway401(t *testing.T) {
	dir := os.Getenv("WILDWORK_AUTHDIR")
	if dir == "" {
		t.Skip("未设置 WILDWORK_AUTHDIR")
	}
	wantUID := os.Getenv("WILDWORK_LIVE_UID")
	files, _ := filepath.Glob(filepath.Join(dir, "workbuddy-*.json"))
	if len(files) == 0 {
		t.Skipf("目录下无 workbuddy 账号: %s", dir)
	}
	var picked *auth.Auth
	for _, f := range files {
		if wantUID != "" && !strings.Contains(filepath.Base(f), wantUID) {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		picked = a
		break
	}
	if picked == nil {
		t.Skipf("未找到匹配账号（WILDWORK_LIVE_UID=%q）", wantUID)
	}
	t.Logf("账号 %s（%s…）", picked.Nickname, picked.UID[:8])

	c := New()
	// ① 账单请求：只发 1 次
	_, _, err := c.UserResourceDetail(picked)
	if err != nil {
		var ue *Error
		ok := asProviderError(err, &ue)
		t.Logf("① 账单请求失败：%v", err)
		if !ok {
			t.Fatalf("账单请求的错误必须是 *upstream.Error（上层自愈依赖 errors.As），实得 %T", err)
		}
		t.Logf("   分类 = %s（status=%d）", ue.Kind, ue.Status)
		if strings.Contains(ue.Msg, "<html") {
			t.Errorf("错误文案仍含原始 HTML：%q", ue.Msg)
		}
		if ue.Kind == provider.ErrSessionDead && ue.Msg != GatewayAuthHint {
			t.Errorf("网关 401 的文案应为可行动提示，实得 %q", ue.Msg)
		}
		// ② refresh：只发 1 次（验证吊销形态确实救不回来）
		rerr := c.RefreshToken(picked)
		if rerr == nil {
			t.Logf("② refresh 成功 → 该账号本可自愈（本探针不落盘）")
		} else {
			t.Logf("② refresh 失败：%v", rerr)
			var re *Error
			if asProviderError(rerr, &re) && re.Kind != provider.ErrSessionDead {
				t.Errorf("refresh 的 invalid_grant 应归 session_dead，实得 %s", re.Kind)
			}
		}
		return
	}
	t.Logf("① 账单请求成功（账号健康，未复现 401）")
}

// asProviderError 是 errors.As 的小包装（避免在 live 探针里再 import errors）。
func asProviderError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
