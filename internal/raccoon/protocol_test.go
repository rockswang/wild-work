package raccoon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseCallbackURL 覆盖深链解析的各类形态：
// 正常回调、缺 code、scheme/host/path 不符、上游返回 error、空串。
func TestParseCallbackURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // 期望错误里包含的子串（空 = 期望成功）
		code    string
	}{
		{name: "正常回调", raw: "office-raccoon://auth/callback?code=abc123", code: "abc123"},
		{name: "带 state", raw: "office-raccoon://auth/callback?code=abc123&state=xyz", code: "abc123"},
		{name: "缺 code", raw: "office-raccoon://auth/callback", wantErr: "缺少 code"},
		{name: "scheme 不符", raw: "raccoon-work://auth/callback?code=abc", wantErr: "不是小浣熊登录回调"},
		{name: "host 不符", raw: "office-raccoon://other/callback?code=abc", wantErr: "不是小浣熊登录回调"},
		{name: "path 不符", raw: "office-raccoon://auth/other?code=abc", wantErr: "不是小浣熊登录回调"},
		{name: "上游拒绝", raw: "office-raccoon://auth/callback?error=access_denied", wantErr: "授权被拒绝"},
		{name: "空串", raw: "  ", wantErr: "深链为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParseCallbackURL(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望成功，实际 %v", err)
				}
				if p.Code != tc.code {
					t.Fatalf("code 期望 %q 实际 %q", tc.code, p.Code)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望失败，实际成功（code=%q）", p.Code)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误文案期望包含 %q，实际 %q", tc.wantErr, err.Error())
			}
		})
	}
}

// TestParseCallbackURLErrorNotLeakingCode 错误文案会被写进日志，不得回显授权码。
func TestParseCallbackURLErrorNotLeakingCode(t *testing.T) {
	_, err := ParseCallbackURL("office-raccoon://auth/callback?error=denied&code=SECRET123")
	if err == nil {
		t.Fatal("期望失败")
	}
	if strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("错误文案泄漏了授权码：%s", err.Error())
	}
}

// TestCallbackFileRoundTrip 覆盖回调载荷的落盘/读取/清理，
// 以及「落盘文件不保留原始深链」这条脱敏约定。
func TestCallbackFileRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// 未落盘时文件不存在
	if _, err := os.Stat(filepath.Join(dir, CallbackFileName)); !os.IsNotExist(err) {
		t.Fatalf("空目录期望无回调文件，实际 err=%v", err)
	}

	if err := SaveCallback(dir, "office-raccoon://auth/callback?code=c1&state=s1"); err != nil {
		t.Fatalf("SaveCallback: %v", err)
	}
	var p CallbackPayload
	raw, rerr := os.ReadFile(filepath.Join(dir, CallbackFileName))
	if rerr != nil {
		t.Fatalf("读回调文件：%v", rerr)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("回调文件应为合法 JSON：%v", err)
	}
	if p.Code != "c1" || p.State != "s1" {
		t.Fatalf("载荷不符：code=%q state=%q", p.Code, p.State)
	}
	if p.Err != "" {
		t.Fatalf("正常回调不该带 Err：%q", p.Err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, CallbackFileName))
	if err != nil {
		t.Fatalf("读回调文件：%v", err)
	}
	if strings.Contains(string(raw), "office-raccoon://") {
		t.Fatalf("回调文件不应保存原始深链：%s", raw)
	}

	if err := ClearCallback(dir); err != nil {
		t.Fatalf("ClearCallback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, CallbackFileName)); !os.IsNotExist(err) {
		t.Fatal("清理后回调文件仍在")
	}
	if err := ClearCallback(dir); err != nil {
		t.Fatalf("重复 ClearCallback 应无错：%v", err)
	}
}

// TestSaveCallbackKeepsParseError 解析失败也要落盘（带 Err）：
// 否则主进程只能等到 5 分钟超时才报错，注册表恢复也被无谓推迟。
func TestSaveCallbackKeepsParseError(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCallback(dir, "office-raccoon://auth/callback"); err != nil {
		t.Fatalf("SaveCallback: %v", err)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, CallbackFileName))
	if rerr != nil {
		t.Fatalf("期望落盘成功：%v", rerr)
	}
	var p CallbackPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("回调文件应为合法 JSON：%v", err)
	}
	if !strings.Contains(p.Err, "缺少 code") {
		t.Fatalf("期望 Err 记录解析失败原因，实际 %q", p.Err)
	}
}

// TestStateDirFallback state 目录为空时兜底为 data（与 config.Default 的 ./data/state.json 一致）。
func TestStateDirFallback(t *testing.T) {
	if got := StateDir("  "); got != "data" {
		t.Fatalf("StateDir 兜底期望 data，实际 %q", got)
	}
	if got := StateDir("C:\\x\\data"); got != "C:\\x\\data" {
		t.Fatalf("StateDir 应原样返回，实际 %q", got)
	}
}
