// e2e_modelscope_secrets_test.go spec 验收判据 T1-4 的后半句：「日志与面板**零 token 明文**」。
//
// 这一条最容易被「顺手 debug」破坏，而破坏后不会有任何报错——只会在用户把日志贴到
// issue 里时泄露凭据。故用**真实 token 串 → 全链路跑一遍 → 搜所有可见面**的方式兜住，
// 并额外断言「token 确实被用过」（否则"搜不到 token"可能只是因为整条链路没跑起来）。

package app

import (
	"net/http"
	"strings"
	"testing"
)

func TestE2E_ModelScope_NoTokenLeakInLogsOrPanel(t *testing.T) {
	const tok = "ms-e2e-SECRET-do-not-leak-9f3a"

	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{tok}})

	// 一次成功 + 一次凭据被拒（失败路径最容易顺手把请求头打出来）。
	if resp, raw := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen3.5-122B-A10B", false)); resp.StatusCode != http.StatusOK {
		t.Fatalf("成功用例对话 = %d：%s", resp.StatusCode, truncateForLog(raw))
	}
	cn.setChatStatus(tok, http.StatusUnauthorized, `{"error":{"message":"invalid token"}}`)
	if resp, _ := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen3.5-122B-A10B", false)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("失败用例 status = %d，want 401", resp.StatusCode)
	}

	// 非空洞前提：上游确实收到了这枚 token（否则「搜不到」毫无意义）。
	if len(cn.reqsByPath("/v1/chat/completions")) == 0 {
		t.Fatal("上游零请求：链路没跑起来，后续『不泄露』断言无意义")
	}

	surfaces := map[string]string{
		"data/app.log": env.appLog(),
	}
	if _, raw := env.get("/api/state"); true {
		surfaces["/api/state"] = string(raw)
	}
	if _, raw := env.get("/api/usage?days=1"); true {
		surfaces["/api/usage"] = string(raw)
	}
	if _, raw := env.get("/api/logs"); true {
		surfaces["/api/logs"] = string(raw)
	}

	for name, body := range surfaces {
		if strings.Contains(body, tok) {
			t.Errorf("%s 出现了 token 明文（零 token 落盘纪律）", name)
		}
		if strings.Contains(body, "Bearer "+tok) {
			t.Errorf("%s 出现了 `Bearer <token>`（同上）", name)
		}
	}

	// 日志本身要有内容（防止「日志路径写错 → 永远是空文件 → 断言恒真」）。
	if logs := surfaces["data/app.log"]; len(logs) < 20 {
		t.Fatalf("app.log 内容过少（%d 字节），无法证明零 token：换路径了？", len(logs))
	}
}
