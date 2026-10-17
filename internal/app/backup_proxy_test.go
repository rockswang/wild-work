package app

import (
	"encoding/json"
	"os"
	"testing"

	"wild-work/internal/backuplink"
)

// 面板侧「备用上游通道」的保存/回显回归（决议 R57）。
// 这里只验配置面与 API 契约；真正的取用编排由 internal/cline/backup_proxy_test.go 锁死。

func TestSaveBackupProxyTakesEffectImmediately(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)

	w := doReq(mux, "POST", "/api/config/proxy", `{"proxy":"socks5://127.0.0.1:1080"}`, nil)
	if w.Code != 200 {
		t.Fatalf("保存应 200：%d %s", w.Code, w.Body.String())
	}
	// 即时生效：不重启、不重建任何渠道 Transport
	if !backuplink.Enabled() || a.cfg.BackupProxy != "socks5://127.0.0.1:1080" {
		t.Errorf("保存后应立即生效：enabled=%v cfg=%q", backuplink.Enabled(), a.cfg.BackupProxy)
	}
	if a.cfg.BackupProxy != "socks5://127.0.0.1:1080" {
		t.Errorf("配置未落字段：%q", a.cfg.BackupProxy)
	}
	raw, err := os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		BackupProxy string `json:"backup_proxy"`
		Proxies     any    `json:"proxies"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil { // config.Save 是缩进格式，故按字段断言而非子串
		t.Fatal(err)
	}
	if onDisk.BackupProxy != "socks5://127.0.0.1:1080" {
		t.Errorf("配置文件的 backup_proxy 未落盘：%s", raw)
	}
	if onDisk.Proxies != nil {
		t.Errorf("旧 proxies 段不该再被写回：%v", onDisk.Proxies)
	}

	// /api/state 必须回显（面板要能填回输入框）
	var st State
	if err := json.Unmarshal(doReq(mux, "GET", "/api/state", "", nil).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.BackupProxy != "socks5://127.0.0.1:1080" || !st.BackupProxyActive {
		t.Errorf("state 回显错误：%q active=%v", st.BackupProxy, st.BackupProxyActive)
	}

	// 空串 = 关闭
	if w := doReq(mux, "POST", "/api/config/proxy", `{"proxy":""}`, nil); w.Code != 200 {
		t.Fatalf("关闭应 200：%d", w.Code)
	}
	if backuplink.Enabled() {
		t.Error("空串应关闭备用通道")
	}
}

// 非法地址必须在保存前被拒（且不得污染已生效的链路）。
func TestBackupProxyRejectsBadAddress(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)
	if w := doReq(mux, "POST", "/api/config/proxy", `{"proxy":"socks5://127.0.0.1:1080"}`, nil); w.Code != 200 {
		t.Fatalf("先配一个合法值：%d", w.Code)
	}

	w := doReq(mux, "POST", "/api/config/proxy", `{"proxy":"ftp://127.0.0.1:21"}`, nil)
	if w.Code != 400 {
		t.Fatalf("非法协议应 400，实得 %d %s", w.Code, w.Body.String())
	}
	if !backuplink.Enabled() || a.cfg.BackupProxy != "socks5://127.0.0.1:1080" {
		t.Error("被拒的保存不得改动现生效配置")
	}
}

// 旧端点必须彻底下线（面板与后端同版本发布，留别名只会让「渠道级代理」借尸还魂）。
func TestLegacyProxiesEndpointGone(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	w := doReq(panelMux(a), "POST", "/api/config/proxies", `{"proxies":{"cline":"http://127.0.0.1:1"}}`, nil)
	if w.Code != 404 && w.Code != 405 {
		t.Fatalf("旧 /api/config/proxies 应已下线，实得 %d %s", w.Code, w.Body.String())
	}
	if backuplink.Enabled() {
		t.Error("旧端点不得还能改动备用通道配置")
	}
}
