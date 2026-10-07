package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestMiddlewareDefaultOff 默认（零值）关闭 = 零行为变化：任何渠道都不适用。
func TestMiddlewareDefaultOff(t *testing.T) {
	c := Default()
	if c.Middleware.Enabled {
		t.Error("默认应关闭中间层")
	}
	if c.Middleware.AppliesTo("workbuddy") || c.Middleware.AppliesTo("traework") {
		t.Error("关闭状态下任何渠道都不得适用")
	}
	// 旧配置无 middleware 段：Load 后保持关闭（向后兼容，零行为变化）
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":"127.0.0.1:9999","api_key":"k","region":"cn"}`), 0o600)
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Middleware.Enabled || c2.Middleware.BaseURL != "" || len(c2.Middleware.Channels) != 0 {
		t.Errorf("旧配置加载后 middleware 应为零值, got %+v", c2.Middleware)
	}
}

// TestMiddlewareAppliesTo 启用后仅列表内渠道生效。
func TestMiddlewareAppliesTo(t *testing.T) {
	m := Middleware{Enabled: true, BaseURL: "http://127.0.0.1:8787/bili", Channels: []string{"workbuddy", "traework"}}
	if !m.AppliesTo("workbuddy") || !m.AppliesTo("traework") {
		t.Error("列表内渠道应适用")
	}
	if m.AppliesTo("qoder") || m.AppliesTo("") {
		t.Error("列表外渠道不得适用")
	}
	// 开关关闭或基址为空：即便在列表内也不适用
	off := m
	off.Enabled = false
	if off.AppliesTo("workbuddy") {
		t.Error("关闭状态下不得适用")
	}
	noBase := m
	noBase.BaseURL = "  "
	if noBase.AppliesTo("workbuddy") {
		t.Error("基址为空（纯空白）不得适用")
	}
}

// TestMiddlewareNormalize 校验判据：清洗 channels、开启必须带 http/https 基址。
func TestMiddlewareNormalize(t *testing.T) {
	// 清洗：空白渠道剔除、排序；基址去空白
	m := Middleware{Enabled: true, BaseURL: " http://127.0.0.1:8787/bili/ ",
		Channels: []string{"traework", " workbuddy ", "", "  "}}
	if err := m.Normalize(); err != nil {
		t.Fatalf("合法配置应通过: %v", err)
	}
	if m.BaseURL != "http://127.0.0.1:8787/bili/" {
		t.Errorf("base_url 应仅去首尾空白（尾部斜杠留给中间层归一）, got %q", m.BaseURL)
	}
	if !reflect.DeepEqual(m.Channels, []string{"traework", "workbuddy"}) {
		t.Errorf("channels 应去空白项并排序, got %v", m.Channels)
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
	c.Middleware = Middleware{Enabled: true, BaseURL: "http://127.0.0.1:8787/bili", Channels: []string{"workbuddy", " traework "}}
	if err := Save(c, fp); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Middleware.Enabled || c2.Middleware.BaseURL != "http://127.0.0.1:8787/bili" {
		t.Errorf("往返后 middleware 异常: %+v", c2.Middleware)
	}
	if !reflect.DeepEqual(c2.Middleware.Channels, []string{"traework", "workbuddy"}) {
		t.Errorf("往返后 channels 应已清洗排序: %v", c2.Middleware.Channels)
	}

	// 非法 middleware 段：Load 必须报错（启动层 fatal 兜底）
	os.WriteFile(fp, []byte(`{"middleware":{"enabled":true,"base_url":"ftp://x"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Error("Load 对非法 middleware 段应报错")
	}
	// 合法段直接手写也能读
	os.WriteFile(fp, []byte(`{"middleware":{"enabled":true,"base_url":"http://127.0.0.1:8787","channels":["glm"]}}`), 0o600)
	c3, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c3.Middleware.AppliesTo("glm") || c3.Middleware.AppliesTo("qoder") {
		t.Errorf("手写段生效异常: %+v", c3.Middleware)
	}
}
