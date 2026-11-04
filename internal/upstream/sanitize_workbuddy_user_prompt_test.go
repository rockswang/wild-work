package upstream

// sanitize_workbuddy_user_prompt_test.go — WorkBuddy CN 渠道**非 system 角色**
// （user / assistant / tool / function）指纹探针。
//
// 与 sanitize_workbuddy_test.go（system 角色）互补。2026-11-03 实测结论：
// 上游只扫 system 与 assistant，**user / tool / function 均不扫**——
// user、tool、function 各 7 条活指纹全部 200；同一身份句放进 assistant 仍 400。
// 判据会漂移，本探针保留为角色维度的漂移哨兵；生产已同步跳过 user/tool/function 清洗。
//
// 26 例 = user/tool/function 各 1 控制组 + 7 条活指纹，assistant 控制组 + 身份句。
// 控制组非 200 视为「该角色形态不被上游接受」，跳过同角色其余用例，避免把
// 角色不支持误读成内容判据。
//
// 方法：system 固定无害句；tool/function 按协议构造合法配对（assistant 发起 →
// 结果消息），否则会被上游按 11148 配对失败拒收，污染内容判据。
// 判定口径与 §0.1 纪律同主探针：单账号单发、间隔 2s、控制组被拦即中止。
//
// 运行（显式选择专用小号）：
//
//	$env:WB_PROBE_UID="<小号uid>"; go test ./internal/upstream -run TestWorkBuddyUserPromptProbe -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// roleProbeCase 角色维度探针用例：fingerprint 原样放进 role 指定的消息。
// want 是截至 2026-11-03 的实测快照：user/tool/function 全放行、assistant 身份句仍拦。
type roleProbeCase struct {
	name        string
	fingerprint string
	want        string // "BLOCKED" | "OK" | "?"（无既有证据）
	role        string // user / assistant / tool / function
}

// roleFingerprints 7 条活指纹（system 角色 2026-11-02 实测仍拦），逐条放进各角色。
var roleFingerprints = []struct{ name, text string }{
	{"DSH身份句", "You are an AI agent powered by DeepSeek Harness."},
	{"DSH跨句路径", "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."},
	{"CC身份句", "You are Claude Code, Anthropic's official CLI for Claude."},
	{"MainBranch", "Main branch (you will usually use this for PRs)"},
	{"Codex首段", "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."},
	{"反馈句", "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
	{"billing键值", "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"},
}

// roleProbeCases 每个角色先控制组（验证角色形态本身可用），再 7 条活指纹；
// assistant 只复测身份句（其余 6 条不在本次范围）。
var roleProbeCases = buildRoleProbeCases()

func buildRoleProbeCases() []roleProbeCase {
	const harmless = "请帮我总结这段日志的要点。"
	cases := make([]roleProbeCase, 0, 26)
	for _, role := range []string{"user", "tool", "function"} {
		cases = append(cases, roleProbeCase{"control_" + role + "无害文本", harmless, "OK", role})
		for _, fp := range roleFingerprints {
			cases = append(cases, roleProbeCase{"fp_" + fp.name + "@" + role, fp.text, "OK", role})
		}
	}
	return append(cases,
		roleProbeCase{"control_assistant无害文本", harmless, "OK", "assistant"},
		roleProbeCase{"fp_DSH身份句@assistant", "You are an AI agent powered by DeepSeek Harness.", "BLOCKED", "assistant"},
	)
}

// roleProbeBody 按角色构造合法消息序列：tool 需 assistant.tool_calls 配对，
// function 需旧版 function_call / functions 配对，否则上游按 11148 拒收、污染判据。
func roleProbeBody(model string, tc roleProbeCase) ([]byte, error) {
	sys := map[string]any{"role": "system", "content": "You are a helpful assistant."}
	var msgs []any
	switch tc.role {
	case "assistant":
		msgs = []any{sys,
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": tc.fingerprint},
			map[string]any{"role": "user", "content": "ok"},
		}
	case "tool":
		msgs = []any{sys,
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "probe_tool", "arguments": "{}",
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": tc.fingerprint},
			map[string]any{"role": "user", "content": "ok"},
		}
	case "function":
		msgs = []any{sys,
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "function_call": map[string]any{
				"name": "probe_fn", "arguments": "{}",
			}},
			map[string]any{"role": "function", "name": "probe_fn", "content": tc.fingerprint},
			map[string]any{"role": "user", "content": "ok"},
		}
	default: // user
		msgs = []any{sys, map[string]any{"role": "user", "content": tc.fingerprint}}
	}
	obj := map[string]any{
		"model": model, "stream": true, "max_tokens": 32, "messages": msgs,
	}
	switch tc.role {
	case "tool":
		obj["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "probe_tool", "description": "probe",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}
	case "function":
		obj["functions"] = []any{map[string]any{
			"name": "probe_fn", "description": "probe",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}}
	}
	return json.Marshal(obj)
}

func TestWorkBuddyUserPromptProbe(t *testing.T) {
	uid := strings.TrimSpace(os.Getenv("WB_PROBE_UID"))
	if uid == "" {
		t.Skip("user 角色探针未启用：设置 WB_PROBE_UID 后运行（§0.1：禁止对主力账号探测）")
	}
	authDir := os.Getenv("WB_PROBE_AUTH_DIR")
	if authDir == "" {
		authDir = "auths"
	}
	model := os.Getenv("WB_PROBE_MODEL")
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	auths, err := auth.LoadWorkBuddyDir(filepath.Clean(authDir), "cn")
	if err != nil {
		t.Fatalf("加载凭证（dir=%s）: %v", authDir, err)
	}
	var acct *auth.Auth
	for _, a := range auths {
		if a.UID == uid {
			acct = a
			break
		}
	}
	if acct == nil {
		t.Fatalf("auths 里没有 uid=%s 的 CN 账号", uid)
	}
	c := New()
	hc := &http.Client{Timeout: 60 * time.Second, Transport: newTransport()}

	t.Logf("角色维度探针: %d 例（user/tool/function/assistant），模型 %s，间隔 2s，账号 uid=%s", len(roleProbeCases), model, uid)

	type sentVerdict struct {
		tc roleProbeCase
		v  probeVerdict
	}
	var sent []sentVerdict
	skipRole := map[string]string{}
	for _, tc := range roleProbeCases {
		if reason, ok := skipRole[tc.role]; ok {
			t.Logf("%-32s SKIP（%s，不再发请求）", tc.name, reason)
			continue
		}
		if len(sent) > 0 {
			time.Sleep(2 * time.Second)
		}
		body, err := roleProbeBody(model, tc)
		if err != nil {
			t.Fatalf("构造 body: %v", err)
		}
		v := probeRaw(t, c, hc, acct, tc.name, body)
		sent = append(sent, sentVerdict{tc, v})
		// user 控制组被拦 ⇒ 账号已被风控标记，后续结果全部作废（§0.1）。
		if tc.name == "control_user无害文本" && v.blocked {
			t.Fatalf("控制组（user 无害文本）即被拦——账号 %s 已被风控，探针中止，结论作废: %+v", uid, v)
		}
		// 角色控制组非 200 ⇒ 该角色形态（配对/字段）本身不被接受，跳过同角色后续用例。
		if strings.HasPrefix(tc.name, "control_") && v.status != 200 {
			skipRole[tc.role] = fmt.Sprintf("控制组 status=%d kind=%s", v.status, v.kind)
			t.Logf("%-32s 控制组非 200 ⇒ %s 角色形态不可用，跳过同角色其余用例", tc.name, tc.role)
		}
	}

	t.Logf("%-32s %-8s %-8s %s", "用例", "预期", "status", "判定")
	for _, sv := range sent {
		v := sv.v
		mark := "OK"
		if v.blocked {
			mark = "BLOCKED"
		} else if v.status != 200 {
			mark = "OTHER(" + v.kind.String() + ")"
		}
		got := "OK"
		if v.blocked {
			got = "BLOCKED"
		} else if v.status != 200 {
			got = "OTHER"
		}
		flip := ""
		if sv.tc.want != got {
			flip = "  ◀ 预期被推翻"
		}
		t.Logf("%-32s %-8s %-8d %s%s  %s", v.name, sv.tc.want, v.status, mark, flip, v.snippet)
	}

	// 汇总：按角色分别计数；被跳过的角色 total=0，不参与结论。
	blocked, total := map[string]int{}, map[string]int{}
	for _, sv := range sent {
		total[sv.tc.role]++
		if sv.v.blocked {
			blocked[sv.tc.role]++
		}
	}
	t.Logf("汇总: user 被拦 %d/%d，tool 被拦 %d/%d，function 被拦 %d/%d，assistant 被拦 %d/%d —— %s",
		blocked["user"], total["user"], blocked["tool"], total["tool"],
		blocked["function"], total["function"], blocked["assistant"], total["assistant"],
		func() string {
			toolKnown, fnKnown := total["tool"] > 0, total["function"] > 0
			switch {
			case !toolKnown && !fnKnown:
				return "tool/function 角色形态本身未被接受，未取得内容判据"
			case blocked["tool"] > 0 || blocked["function"] > 0:
				return "上游扫描 tool/function 的 content：清洗必须覆盖"
			case !toolKnown || !fnKnown:
				return "部分工具角色形态未被接受，其余放行；判据不完整"
			default:
				return "tool/function 全放行：工具返回内容无需清洗（生产已同步跳过）"
			}
		}())
}
