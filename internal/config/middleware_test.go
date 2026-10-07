package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMiddlewareDefaultOff 默认（零值）关闭 = 零行为变化（全部渠道直连）。
func TestMiddlewareDefaultOff(t *testing.T) {
	c := Default()
	if c.Middleware.Enabled {
		t.Error("默认应关闭中间层")
	}
	if c.Middleware.BaseURL != "" {
		t.Errorf("默认 base_url 应为空, got %q", c.Middleware.BaseURL)
	}
	// 旧配置无 middleware 段：Load 后保持关闭（向后兼容，零行为变化）
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":"127.0.0.1:9999","api_key":"k","region":"cn"}`), 0o600)
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Middleware.Enabled || c2.Middleware.BaseURL != "" {
		t.Errorf("旧配置加载后 middleware 应为零值, got %+v", c2.Middleware)
	}
}

// TestMiddlewareNormalize 校验判据：开启必须带 http/https 基址，基址去空白。
func TestMiddlewareNormalize(t *testing.T) {
	// 清洗：基址去首尾空白（尾部斜杠留给中间层归一）
	m := Middleware{Enabled: true, BaseURL: " http://127.0.0.1:8787/ "}
	if err := m.Normalize(); err != nil {
		t.Fatalf("合法配置应通过: %v", err)
	}
	if m.BaseURL != "http://127.0.0.1:8787/" {
		t.Errorf("base_url 应仅去首尾空白, got %q", m.BaseURL)
	}
	// 关闭状态：任何 base_url（含空）都合法
	if err := (&Middleware{}).Normalize(); err != nil {
		t.Errorf("零值应合法: %v", err)
	}
	// 开启但缺基址：错误（静默不生效会误导用户）
	if err := (&Middleware{Enabled: true}).Normalize(); err == nil {
		t.Error("开启但缺 base_url 应报错")
	}
	// 非 http/https 或无 host：错误
	for _, bad := range []string{"127.0.0.1:8787", "ftp://127.0.0.1:8787", "not a url", "http://"} {
		m := Middleware{Enabled: true, BaseURL: bad}
		if err := m.Normalize(); err == nil {
			t.Errorf("base_url=%q 应报错", bad)
		}
	}
	// 合法形态：https + 路径段
	if err := (&Middleware{Enabled: true, BaseURL: "https://mw.example.com/prefix"}).Normalize(); err != nil {
		t.Errorf("https 带路径段应合法: %v", err)
	}
}

// TestMiddlewareLoadSaveRoundtrip 配置文件往返：middleware 段保存后可原样读回，
// 且 Default() 的落盘形态包含该段（不变量 7：与 config.example.json 同步）。
func TestMiddlewareLoadSaveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	c := Default()
	c.Middleware = Middleware{Enabled: true, BaseURL: "http://127.0.0.1:8787"}
	if err := Save(c, fp); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Middleware.Enabled || c2.Middleware.BaseURL != "http://127.0.0.1:8787" {
		t.Errorf("往返后 middleware 异常: %+v", c2.Middleware)
	}

	// 非法 middleware 段：Load 必须报错（启动层 fatal 兜底）
	os.WriteFile(fp, []byte(`{"middleware":{"enabled":true,"base_url":"ftp://x"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Error("Load 对非法 middleware 段应报错")
	}
	// 合法段直接手写也能读；旧版遗留的 channels 键被自然忽略（全局开关无渠道维度）
	os.WriteFile(fp, []byte(`{"middleware":{"enabled":true,"base_url":"http://127.0.0.1:8787","channels":["glm"]}}`), 0o600)
	c3, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c3.Middleware.Enabled || c3.Middleware.BaseURL != "http://127.0.0.1:8787" {
		t.Errorf("手写段生效异常: %+v", c3.Middleware)
	}
}
