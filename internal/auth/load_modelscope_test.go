package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadModelscopeGlobBoundary 两个前缀是**两个独立服务区域、token 不通用**
// （modelscope / modelscopeai），加载器必须互不吞噬：
//
//   - `modelscope-*.json` 不得把 `modelscopeai-*.json` 也扫进来
//     （那会让国际站 token 混进国内站池，表现为「随机 401」）；
//   - `modelscopeai-*.json` 同理。
//
// 顺带锁住 loadPrefixed 的既有契约：Kind 与 FilePath 由加载器回填。
func TestLoadModelscopeGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"modelscope-u1.json":     `{"auth":{"accessToken":"ms-cn-1","expiresAt":4102444800},"account":{"uid":"u1"}}`,
		"modelscope-u2.json":     `{"auth":{"accessToken":"ms-cn-2","expiresAt":4102444800},"account":{"uid":"u2"}}`,
		"modelscopeai-u1.json":   `{"auth":{"accessToken":"ms-ai-1","expiresAt":4102444800},"account":{"uid":"a1"}}`,
		"workbuddy-u9.json":      `{"auth":{"accessToken":"at","expiresAt":4102444800},"account":{"uid":"u9"}}`,
		"modelscope-broken.json": `{`, // 损坏文件跳过而不整体失败（loadPrefixed 既有口径）
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cn, err := LoadModelscopeDir(dir)
	if err != nil {
		t.Fatalf("LoadModelscopeDir: %v", err)
	}
	if len(cn) != 2 {
		t.Fatalf("LoadModelscopeDir 命中 %d 个，期望 2（u1/u2，且不得吞 modelscopeai-*）", len(cn))
	}
	for _, a := range cn {
		if a.Kind != "modelscope" {
			t.Errorf("modelscope 账号 Kind = %q，期望 modelscope", a.Kind)
		}
		if a.FilePath == "" {
			t.Error("加载器必须回填 FilePath（SaveAtomic 依赖它）")
		}
		if len(a.AccessToken) < 5 || a.AccessToken[:5] != "ms-cn" {
			t.Errorf("扫到了非国内站 token（glob 破坏了？）")
		}
	}

	ai, err := LoadModelscopeAIDir(dir)
	if err != nil {
		t.Fatalf("LoadModelscopeAIDir: %v", err)
	}
	if len(ai) != 1 {
		t.Fatalf("LoadModelscopeAIDir 命中 %d 个，期望 1（不得吞 modelscope-*）", len(ai))
	}
	if ai[0].Kind != "modelscopeai" || ai[0].UID != "a1" {
		t.Errorf("国际站账号 = %+v，期望 Kind=modelscopeai uid=a1", ai[0])
	}
}
