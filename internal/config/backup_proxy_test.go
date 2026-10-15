package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 旧「每渠道代理」→ 全局备用通道的一次性迁移（决议 R57）。
// 锁三条：迁移取值优先级、脏值不得把启动拉死、旧段必须消失（不再被任何消费方读取）。

func loadWith(t *testing.T, json string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	return c
}

// cline 有值时优先取它（当前唯一有备用通道需求的渠道）。
func TestMigrateLegacyProxiesPrefersCline(t *testing.T) {
	c := loadWith(t, `{"proxies":{"workbuddy":"http://127.0.0.1:7890","cline":"socks5://127.0.0.1:1080","oczen":"http://127.0.0.1:7891"}}`)
	if c.BackupProxy != "socks5://127.0.0.1:1080" {
		t.Errorf("应迁移 cline 的地址，实得 %q", c.BackupProxy)
	}
	if c.LegacyProxies != nil {
		t.Error("迁移后旧 proxies 段必须清空（否则下次保存又写回去）")
	}
	if !strings.Contains(c.LegacyProxyNote, "cline") || !strings.Contains(c.LegacyProxyNote, "oczen") {
		t.Errorf("迁移说明必须点名取用来源与被丢弃的渠道：%q", c.LegacyProxyNote)
	}
}

// 没有 cline 时按渠道名排序取第一个非空值（确定性：map 遍历顺序不可依赖）。
func TestMigrateLegacyProxiesDeterministic(t *testing.T) {
	c := loadWith(t, `{"proxies":{"qwenwork":"http://127.0.0.1:1","workbuddy":"http://127.0.0.1:2"}}`)
	if c.BackupProxy != "http://127.0.0.1:1" {
		t.Errorf("应按渠道名排序取首个，实得 %q", c.BackupProxy)
	}
	if !strings.Contains(c.LegacyProxyNote, "workbuddy") {
		t.Errorf("被丢弃的渠道要在说明里列出：%q", c.LegacyProxyNote)
	}
}

// backup_proxy 已有值时不被旧段覆盖。
func TestMigrateKeepsExplicitBackupProxy(t *testing.T) {
	c := loadWith(t, `{"backup_proxy":"http://127.0.0.1:9","proxies":{"cline":"http://127.0.0.1:8"}}`)
	if c.BackupProxy != "http://127.0.0.1:9" {
		t.Errorf("现配的 backup_proxy 应优先，实得 %q", c.BackupProxy)
	}
}

// 脏值（多年前手写的非法地址）不得让启动 fatal —— 只记日志丢弃。
func TestMigrateIgnoresDirtyLegacyValue(t *testing.T) {
	c := loadWith(t, `{"proxies":{"cline":"localhost:8118"}}`)
	if c.BackupProxy != "" {
		t.Errorf("非法旧值不应被迁移：%q", c.BackupProxy)
	}
	if !strings.Contains(c.LegacyProxyNote, "不合法") {
		t.Errorf("应说明未迁移的原因：%q", c.LegacyProxyNote)
	}
}

// 新字段自身的校验必须 fail-early（面板保存与启动走同一判据）。
func TestBackupProxyInvalidRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"backup_proxy":"ftp://127.0.0.1:21"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "备用上游代理") {
		t.Fatalf("非法备用代理地址应报错，实得 %v", err)
	}
}

// 保存后旧段必须从文件里消失（omitempty + 已置 nil）。
func TestSaveDropsLegacyProxies(t *testing.T) {
	c := loadWith(t, `{"proxies":{"cline":"socks5://127.0.0.1:1080"}}`)
	p := filepath.Join(t.TempDir(), "out.json")
	if err := Save(c, p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"proxies"`) {
		t.Errorf("旧段应已被移除：%s", raw)
	}
	if !strings.Contains(string(raw), `"backup_proxy"`) {
		t.Errorf("迁移结果应落盘：%s", raw)
	}
}
