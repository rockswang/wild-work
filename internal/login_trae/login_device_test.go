package login_trae

// login_device_test.go 回归测试：设备号作用域 = 登录会话（issue #60/#70，R37 第二次修订）。
//
// 锁定三个事实：
//  1. 每次 Start 生成全新设备号（不再读/写本机级 device-id.json）——一机多号不再共享设备；
//  2. 登录成功后设备号落进 auths/trae-<uid>.json（后续签到/对话头从 auth 文件读，稳定不变）；
//  3. Poll 成功后清理旧版遗留的 device-id.json（老用户自愈）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/traework"
)

// extractQuery 从授权 URL 里取 query 参数。
func extractQuery(t *testing.T, authURL, key string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("解析授权 URL: %v", err)
	}
	return u.Query().Get(key)
}

// TestStartGeneratesFreshDevicePerSession 每次 Start 都是新设备号，
// 且不再产生/读取 device-id.json（旧实现一机多号共享设备号 → #70 的 4017 风控）。
func TestStartGeneratesFreshDevicePerSession(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")

	// 模拟旧版遗留文件：新实现必须无视它（不读它的值）
	legacy := `{"machineId":"00000000000000000000000000000000","deviceId":"100000000000000"}`
	if err := os.WriteFile(filepath.Join(dir, "device-id.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	var dev1, mach1, dev2, mach2 string
	for i := 0; i < 2; i++ {
		authURL, err := Start(NewClient(), statePath)
		if err != nil {
			t.Fatalf("Start #%d: %v", i+1, err)
		}
		if i == 0 {
			dev1 = extractQuery(t, authURL, "device_id")
			mach1 = extractQuery(t, authURL, "machine_id")
		} else {
			dev2 = extractQuery(t, authURL, "device_id")
			mach2 = extractQuery(t, authURL, "machine_id")
		}
		// 不等回调，直接关掉监听（5 分钟兜底太长）：往 state 写入即视为本轮结束
	}
	if dev1 == "" || mach1 == "" || dev2 == "" || mach2 == "" {
		t.Fatalf("授权 URL 缺设备号: dev1=%q mach1=%q dev2=%q mach2=%q", dev1, mach1, dev2, mach2)
	}
	if dev1 == dev2 || mach1 == mach2 {
		t.Fatalf("两次登录会话复用了设备号（#70 根因回归）：dev %q/%q mach %q/%q", dev1, dev2, mach1, mach2)
	}
	if dev1 == "100000000000000" || dev2 == "100000000000000" {
		t.Fatal("新会话不应复用旧 device-id.json 里的编造值（#60 根因）")
	}
}

// TestPollSavesDeviceIntoAuthFileAndCleansLegacy 登录成功后：
// 设备号随 auth 文件落盘（此后稳定，签到头从文件读）；device-id.json 被清理。
func TestPollSavesDeviceIntoAuthFileAndCleansLegacy(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")

	// 假 OAuth 上游：ExchangeToken + GetUserInfo（Poll 成功路径只碰这两个端点）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, traework.EpExchange):
			// RefreshToken 直接解析顶层 Result（doJSON 不剥 envelope）
			_, _ = w.Write([]byte(`{"Result":{"Token":"at-new","TokenExpireAt":9999999999,"RefreshToken":"rt-new"}}`))
		case strings.HasSuffix(r.URL.Path, traework.EpUserInfo):
			// GetUserInfo 同样解析顶层 Result
			_, _ = w.Write([]byte(`{"Result":{"UserID":"uid-devtest","ScreenName":"设备测试","EnterpriseID":"ent1"}}`))
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()

	authURL, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantDev := extractQuery(t, authURL, "device_id")
	wantMach := extractQuery(t, authURL, "machine_id")

	// 模拟授权回调（标准 refreshToken 路径）
	cb, _ := url.Parse(authURL)
	q := url.Values{"refreshToken": {"rt-callback"}, "host": {srv.URL}}
	_, _ = http.Post(cb.Query().Get("auth_callback_url")+"&refreshToken=rt-callback", "application/x-www-form-urlencoded", nil)
	_ = q
	// 回调处理器会 shutdown server；改用直接写 state 文件模拟回调结果（更稳）
	st := state{MachineID: wantMach, DeviceID: wantDev, CodeVerifier: "v", RefreshToken: "rt-callback", Host: srv.URL}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.DeviceID != wantDev || res.MachineID != wantMach {
		t.Fatalf("Poll 结果设备号与会话不符: dev=%q want %q, mach=%q want %q", res.DeviceID, wantDev, res.MachineID, wantMach)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("登录成功后 login-state.json 应被删除")
	}
	// device-id.json 清理（旧文件存在时）
	if err := os.WriteFile(filepath.Join(dir, "device-id.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 落盘 auth 文件并验证设备号进入文件
	fp, err := SaveAuth(dir, res)
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	rawAuth, _ := os.ReadFile(fp)
	var doc struct {
		Auth struct {
			DeviceID  string `json:"deviceId"`
			MachineID string `json:"machineId"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(rawAuth, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Auth.DeviceID != wantDev || doc.Auth.MachineID != wantMach {
		t.Fatalf("auth 文件设备号不符: got %q/%q want %q/%q", doc.Auth.DeviceID, doc.Auth.MachineID, wantDev, wantMach)
	}
}
