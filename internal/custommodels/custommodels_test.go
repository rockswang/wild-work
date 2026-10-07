package custommodels

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mustStore 从临时目录建一个可用 Store（失败即 Fatal）。
func mustStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s, dir
}

// seedSource/mustUpsertModel 测试脚手架：塞一个启用中的源 + 模型。
func seedSource(t *testing.T, s *Store, name, baseURL string) {
	t.Helper()
	if err := s.UpsertSource(Source{Name: name, BaseURL: baseURL, APIKey: "dummy-key", Enabled: true}); err != nil {
		t.Fatalf("UpsertSource(%s): %v", name, err)
	}
}

func mustUpsertModel(t *testing.T, s *Store, m Model) {
	t.Helper()
	if err := s.UpsertModel(m); err != nil {
		t.Fatalf("UpsertModel(%s): %v", m.Name, err)
	}
}

// TestStoreCRUDAndPersistRoundTrip 增删改查 + 持久化往返：内存写入 → 磁盘 → 重新 Load 一致。
func TestStoreCRUDAndPersistRoundTrip(t *testing.T) {
	s, dir := mustStore(t)

	seedSource(t, s, "deepseek", "https://api.deepseek.com/")
	mustUpsertModel(t, s, Model{Name: "ds-chat", Source: "deepseek", Enabled: true, Note: "备注"})

	// 改：upsert 同名 = 更新（不是新增）
	if err := s.UpsertSource(Source{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1/", APIKey: "k2", Enabled: true}); err != nil {
		t.Fatalf("upsert 更新源: %v", err)
	}
	srcs, mods := s.Snapshot()
	if len(srcs) != 1 || len(mods) != 1 {
		t.Fatalf("upsert 同名应保持 1 源 1 模型，实际 %d/%d", len(srcs), len(mods))
	}
	if srcs[0].BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("base_url 归一（trim + 去尾 /）失败: %q", srcs[0].BaseURL)
	}

	// 持久化往返：重新 Load 同一目录，数据一致
	s2, err := Load(dir)
	if err != nil {
		t.Fatalf("重新 Load: %v", err)
	}
	srcs2, mods2 := s2.Snapshot()
	if len(srcs2) != 1 || len(mods2) != 1 {
		t.Fatalf("重载后应有 1 源 1 模型，实际 %d/%d", len(srcs2), len(mods2))
	}
	if mods2[0].Name != "ds-chat" || mods2[0].Source != "deepseek" || mods2[0].Note != "备注" {
		t.Fatalf("模型往返不一致: %+v", mods2[0])
	}

	// 删模型 → 删源（源被引用时必须先删模型）
	if err := s2.DeleteModel("ds-chat"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if err := s2.DeleteSource("deepseek"); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if _, mods3 := s2.Snapshot(); len(mods3) != 0 {
		t.Fatalf("删除后应无模型")
	}
	// 删不存在的 → 报错不 panic
	if err := s2.DeleteModel("nope"); err == nil {
		t.Fatal("删除不存在模型应报错")
	}
	if err := s2.DeleteSource("nope"); err == nil {
		t.Fatal("删除不存在源应报错")
	}
}

// TestDeleteSourceReferencedRejected 源仍被模型引用时删除必须被拒（引用不悬挂）。
func TestDeleteSourceReferencedRejected(t *testing.T) {
	s, _ := mustStore(t)
	seedSource(t, s, "src", "https://example.com")
	mustUpsertModel(t, s, Model{Name: "m", Source: "src", Enabled: true})
	err := s.DeleteSource("src")
	if err == nil {
		t.Fatal("仍被模型引用的源不应可删")
	}
	if !strings.Contains(err.Error(), "m") {
		t.Fatalf("错误应点名引用模型: %v", err)
	}
	_ = s.DeleteModel("m")
	if err := s.DeleteSource("src"); err != nil {
		t.Fatalf("解除引用后删除应成功: %v", err)
	}
}

// TestUpsertValidation 入库校验：名称空白、模型名含 "/"、模型缺源、源缺 base_url、坏 URL。
func TestUpsertValidation(t *testing.T) {
	s, _ := mustStore(t)
	seedSource(t, s, "src", "https://example.com")

	cases := []struct {
		what  string
		setup func() error
		want  string // 错误消息须含的子串
	}{
		{"源名为空", func() error {
			return s.UpsertSource(Source{Name: " ", BaseURL: "https://a.com"})
		}, "不能为空"},
		{"源名含空白", func() error {
			return s.UpsertSource(Source{Name: "a b", BaseURL: "https://a.com"})
		}, "空白"},
		{"源 base_url 为空", func() error {
			return s.UpsertSource(Source{Name: "x", BaseURL: ""})
		}, "base_url"},
		{"源 base_url 非 http", func() error {
			return s.UpsertSource(Source{Name: "x", BaseURL: "ftp://a.com"})
		}, "http/https"},
		{"模型名含斜杠", func() error {
			return s.UpsertModel(Model{Name: "a/b", Source: "src", Enabled: true})
		}, `"/"`},
		{"模型缺源", func() error {
			return s.UpsertModel(Model{Name: "m", Source: "", Enabled: true})
		}, "API 源"},
		{"模型引用不存在的源", func() error {
			return s.UpsertModel(Model{Name: "m", Source: "ghost", Enabled: true})
		}, "不存在"},
	}
	for _, c := range cases {
		err := c.setup()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 应报含 %q 的错误，实际 %v", c.what, c.want, err)
		}
	}
	// 校验失败的条目不得入库
	if _, mods := s.Snapshot(); len(mods) != 0 {
		t.Fatalf("校验失败的模型不应入库，实际 %d 个", len(mods))
	}
}

// TestResolveSemantics 路由判定：停用/缺源/命中后 Target 字段。
func TestResolveSemantics(t *testing.T) {
	s, _ := mustStore(t)
	seedSource(t, s, "src", "https://example.com")
	mustUpsertModel(t, s, Model{Name: "on", Source: "src", Enabled: true})
	mustUpsertModel(t, s, Model{Name: "off", Source: "src", Enabled: false})
	mustUpsertModel(t, s, Model{Name: "mapped", Source: "src", UpstreamID: "real-model", Enabled: true})
	seedSource(t, s, "disabled-src", "https://example.org")
	mustUpsertModel(t, s, Model{Name: "src-off", Source: "disabled-src", Enabled: true})
	if err := s.UpsertSource(Source{Name: "disabled-src", BaseURL: "https://example.org", Enabled: false}); err != nil {
		t.Fatalf("停用源: %v", err)
	}

	if _, ok := s.Resolve("on"); !ok {
		t.Fatal("启用模型 + 启用源应命中")
	}
	if _, ok := s.Resolve("off"); ok {
		t.Fatal("停用模型不应命中")
	}
	if _, ok := s.Resolve("src-off"); ok {
		t.Fatal("源停用后其模型不应命中")
	}
	if _, ok := s.Resolve("missing"); ok {
		t.Fatal("未配置的裸名不应命中")
	}
	if _, ok := s.Resolve("  "); ok {
		t.Fatal("空名不应命中")
	}

	// upstream_id 非空 → 转发改写值；空 → 用模型名原样
	tgt, ok := s.Resolve("mapped")
	if !ok || tgt.Upstream != "real-model" || tgt.Model != "mapped" {
		t.Fatalf("upstream_id 映射错误: %+v", tgt)
	}
	tgt, ok = s.Resolve("on")
	if !ok || tgt.Upstream != "on" || tgt.BaseURL != "https://example.com" || tgt.APIKey != "dummy-key" {
		t.Fatalf("无 upstream_id 应用模型名原样: %+v", tgt)
	}
}

// TestEnabledIDs /v1/models 合并清单：只列「模型启用 且 源启用」的，升序。
func TestEnabledIDs(t *testing.T) {
	s, _ := mustStore(t)
	seedSource(t, s, "src", "https://example.com")
	mustUpsertModel(t, s, Model{Name: "b", Source: "src", Enabled: true})
	mustUpsertModel(t, s, Model{Name: "a", Source: "src", Enabled: true})
	mustUpsertModel(t, s, Model{Name: "c", Source: "src", Enabled: false})
	if ids := s.EnabledIDs(); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("应只列启用的 a,b 升序，实际 %v", ids)
	}
}

// TestFlushRollbackOnWriteError 落盘失败必须回滚内存（内存/磁盘一致）。
// 制造失败：加载后删掉数据目录 → tmp 写入失败。
func TestFlushRollbackOnWriteError(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("删目录: %v", err)
	}
	if err := s.UpsertSource(Source{Name: "ghost", BaseURL: "https://example.com", Enabled: true}); err == nil {
		t.Fatal("目录已删，落盘应失败")
	}
	if srcs, _ := s.Snapshot(); len(srcs) != 0 {
		t.Fatalf("落盘失败应回滚内存，实际残留 %d 个源", len(srcs))
	}
}

// TestLoadCorruptFileNotFatal 损坏文件：返回可用空 Store + error（不阻断启动）。
func TestLoadCorruptFileNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom-models.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err == nil {
		t.Fatal("损坏文件应附带 error")
	}
	if s == nil {
		t.Fatal("损坏文件仍须返回可用空 Store")
	}
	if srcs, mods := s.Snapshot(); len(srcs) != 0 || len(mods) != 0 {
		t.Fatalf("损坏文件应视为空表")
	}
	// 空表可用：下次保存覆盖损坏文件，自动自愈
	if err := s.UpsertSource(Source{Name: "fresh", BaseURL: "https://example.com", Enabled: true}); err != nil {
		t.Fatalf("空表应可写入（自愈）: %v", err)
	}
}

// TestLoadBOMTolerance UTF-8 BOM 容忍（PowerShell Set-Content 会写 BOM）。
func TestLoadBOMTolerance(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("\xef\xbb\xbf" + `{"sources":[{"name":"s","base_url":"https://example.com","api_key":"k","enabled":true}],"models":[{"name":"m","source":"s","enabled":true}]}`)
	if err := os.WriteFile(filepath.Join(dir, "custom-models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("BOM 文件应可解析: %v", err)
	}
	if _, ok := s.Resolve("m"); !ok {
		t.Fatal("BOM 容忍后模型应命中")
	}
}

// TestResolveDanglingModelMisses 悬挂引用防御：手工编辑文件可造出
// 「模型引用不存在的源」的中间态（CRUD 侧已校验拦截，文件直改绕过校验），
// 此时 Resolve 必须判未命中（调用方走原渠道路径），不得 panic。
func TestResolveDanglingModelMisses(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"sources":[{"name":"s","base_url":"https://example.com","api_key":"k","enabled":true}],` +
		`"models":[{"name":"dangling","source":"ghost","enabled":true}]}`)
	if err := os.WriteFile(filepath.Join(dir, "custom-models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("悬挂引用文件应可加载: %v", err)
	}
	if _, ok := s.Resolve("dangling"); ok {
		t.Fatal("引用缺失源的模型不得命中（防御：交调用方走原路径）")
	}
	if ids := s.EnabledIDs(); len(ids) != 0 {
		t.Fatalf("悬挂模型不应进 /v1/models，实际 %v", ids)
	}
}

// TestConcurrentUpsertsAtomicSave 并发安全抽例：多 goroutine 同时 upsert 不同源，
// 全部落盘成功且重载后一个不少（每轮写走 tmp+rename 原子替换，读者永远读到完整文件）。
func TestConcurrentUpsertsAtomicSave(t *testing.T) {
	const n = 16
	s, dir := mustStore(t)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.UpsertSource(Source{
				Name:    "src-" + strings.Repeat("x", i+1), // 长度递增避免 upsert 互相覆盖
				BaseURL: "https://example.com",
				APIKey:  "dummy-key",
				Enabled: true,
			}); err != nil {
				t.Errorf("并发 upsert #%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	s2, err := Load(dir)
	if err != nil {
		t.Fatalf("并发写后重载: %v", err)
	}
	srcs, _ := s2.Snapshot()
	if len(srcs) != n {
		t.Fatalf("并发 upsert 后应 persisted %d 个源，实际 %d", n, len(srcs))
	}
}
