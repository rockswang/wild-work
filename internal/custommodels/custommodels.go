// Package custommodels 自定义模型代理的配置存储：两级结构（API 源 + 模型）。
//
// 语义：
//   - 两级结构：Source（base_url + api_key，可被多个模型共享）+ Model（对外裸模型名
//     → 所属源，可选 upstream_id 改写转发时的 model 字段）；
//   - 文件缺失 = 空表（首启）；解析失败仍返回可用空 Store 并附带 error——
//     自定义模型配置绝不能阻断服务启动，损坏文件在下次保存时被正常覆盖、自动自愈；
//   - 路由语义：/v1/chat/completions 的**裸模型名**命中启用中的模型（且所属源启用）时
//     直接转发第三方（优先于渠道默认/兼容映射兜底）；带渠道前缀（kind/model）的
//     请求永远走渠道，不受自定义模型影响。模型名不允许含 "/"——含前缀的名字
//     到不了直转分支，存进去只会造成「配置了却永远不生效」的假象，故在入库时拒绝。
package custommodels

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Source 一个第三方 OpenAI 兼容 API 源。Name 即唯一标识（源间互相引用、
// 模型绑定均用它），创建后不建议改名——改名需同步重建模型绑定关系。
type Source struct {
	Name    string `json:"name"`     // 唯一标识（非空、不含空白字符）
	BaseURL string `json:"base_url"` // 形如 https://api.deepseek.com 或 .../v1（存时归一）
	APIKey  string `json:"api_key"`  // 第三方 API key（完整明文存储；面板同 API-Key 同语义）
	Enabled bool   `json:"enabled"`  // false = 其下模型全部分流失效（仍保留配置）
}

// Model 一个对外自定义模型。Name 即客户端请求名（/v1/models 合并条目的 id）。
type Model struct {
	Name       string `json:"name"`                  // 对外裸模型名（唯一；非空、无空白、无 "/")
	Source     string `json:"source"`                // 所属 Source.Name（必填、须存在）
	UpstreamID string `json:"upstream_id,omitempty"` // 发给第三方的 model 字段；空 = 用 Name
	Enabled    bool   `json:"enabled"`               // false = 不分流（请求照旧走渠道路径）
	Note       string `json:"note,omitempty"`        // 备注（可选）
}

// fileData 持久化结构（data/custom-models.json）。
type fileData struct {
	Sources []Source `json:"sources"`
	Models  []Model  `json:"models"`
}

// Store 自定义模型配置存储。并发安全。
type Store struct {
	mu      sync.RWMutex
	file    string
	sources map[string]Source // key = Source.Name
	models  map[string]Model  // key = Model.Name
}

// Load 从 dataDir/custom-models.json 加载；文件缺失 = 空表（首启）。
// 解析失败时**仍返回可用的空 Store 并附带 error**（见包注释）。
func Load(dataDir string) (*Store, error) {
	s := &Store{
		file:    filepath.Join(dataDir, "custom-models.json"),
		sources: map[string]Source{},
		models:  map[string]Model{},
	}
	raw, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("读取 %s: %w", s.file, err)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // 容忍 UTF-8 BOM
	var fd fileData
	if err := json.Unmarshal(raw, &fd); err != nil {
		return s, fmt.Errorf("解析 %s: %w", s.file, err)
	}
	for _, src := range fd.Sources {
		if src.Name != "" {
			s.sources[src.Name] = src
		}
	}
	for _, m := range fd.Models {
		if m.Name != "" {
			s.models[m.Name] = m
		}
	}
	return s, nil
}

// Snapshot 返回源与模型的全量拷贝（均按 Name 升序，保证面板顺序稳定）。
func (s *Store) Snapshot() ([]Source, []Model) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	srcs := make([]Source, 0, len(s.sources))
	for _, v := range s.sources {
		srcs = append(srcs, v)
	}
	mods := make([]Model, 0, len(s.models))
	for _, v := range s.models {
		mods = append(mods, v)
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].Name < srcs[j].Name })
	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	return srcs, mods
}

// Target Resolve 的命中结果：转发一个自定义模型所需的全部信息。
type Target struct {
	Model    string // 对外模型名（记账/统计按它归行，与面板配置一一对应）
	Upstream string // 发给第三方的 model 字段（UpstreamID 非空则用之，否则 = Model）
	Source   string // 所属源名（日志用）
	BaseURL  string // 所属源 base_url（已归一）
	APIKey   string // 所属源 API key
}

// Resolve 按客户端请求名精确匹配启用中的自定义模型。
// ok=false 的全部情形（未命中/模型停用/源缺失/源停用）调用方一律走原有渠道路径。
func (s *Store) Resolve(model string) (Target, bool) {
	m := strings.TrimSpace(model)
	if m == "" {
		return Target{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	md, hit := s.models[m]
	if !hit || !md.Enabled {
		return Target{}, false
	}
	src, hit := s.sources[md.Source]
	if !hit || !src.Enabled {
		return Target{}, false
	}
	upstream := md.UpstreamID
	if upstream == "" {
		upstream = md.Name
	}
	return Target{
		Model:    md.Name,
		Upstream: upstream,
		Source:   src.Name,
		BaseURL:  src.BaseURL,
		APIKey:   src.APIKey,
	}, true
}

// EnabledIDs 返回全部生效模型的对外裸名（/v1/models 合并用），升序。
// 生效 = 模型启用 且 所属源存在且启用。
func (s *Store) EnabledIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.models))
	for _, m := range s.models {
		if !m.Enabled {
			continue
		}
		src, ok := s.sources[m.Source]
		if !ok || !src.Enabled {
			continue
		}
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// UpsertSource 新建或更新 API 源（按 Name），校验通过后原子落盘。
// 落盘失败**回滚内存改动**，保证内存/磁盘/面板三者一致（仿 config.Save 的 tmp+rename）。
func (s *Store) UpsertSource(src Source) error {
	src.Name = strings.TrimSpace(src.Name)
	src.BaseURL = normalizeBaseURL(src.BaseURL)
	src.APIKey = strings.TrimSpace(src.APIKey)
	if err := validID(src.Name, "API 源名称"); err != nil {
		return err
	}
	if err := validBaseURL(src.BaseURL); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, existed := s.sources[src.Name]
	s.sources[src.Name] = src
	if err := s.flushLocked(); err != nil {
		if existed {
			s.sources[src.Name] = old
		} else {
			delete(s.sources, src.Name)
		}
		return err
	}
	return nil
}

// DeleteSource 删除 API 源；仍被模型引用时拒绝（引用关系不悬挂）。
func (s *Store) DeleteSource(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.sources[name]
	if !ok {
		return fmt.Errorf("API 源 %q 不存在", name)
	}
	for _, m := range s.models {
		if m.Source == name {
			return fmt.Errorf("API 源 %q 仍被模型 %q 引用，请先删除或改绑该模型", name, m.Name)
		}
	}
	delete(s.sources, name)
	if err := s.flushLocked(); err != nil {
		s.sources[name] = old // 落盘失败回滚
		return err
	}
	return nil
}

// UpsertModel 新建或更新自定义模型（按 Name），校验 source 引用存在后原子落盘。
// 落盘失败**回滚内存改动**（同 UpsertSource）。
func (s *Store) UpsertModel(m Model) error {
	m.Name = strings.TrimSpace(m.Name)
	m.Source = strings.TrimSpace(m.Source)
	m.UpstreamID = strings.TrimSpace(m.UpstreamID)
	m.Note = strings.TrimSpace(m.Note)
	if err := validID(m.Name, "模型名称"); err != nil {
		return err
	}
	if strings.Contains(m.Name, "/") {
		return fmt.Errorf("模型名称 %q 不能含 \"/\"：带渠道前缀的请求永远走渠道，此类名称永远无法直转", m.Name)
	}
	if m.Source == "" {
		return fmt.Errorf("模型 %q 必须选择 API 源", m.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sources[m.Source]; !ok {
		return fmt.Errorf("API 源 %q 不存在", m.Source)
	}
	old, existed := s.models[m.Name]
	s.models[m.Name] = m
	if err := s.flushLocked(); err != nil {
		if existed {
			s.models[m.Name] = old
		} else {
			delete(s.models, m.Name)
		}
		return err
	}
	return nil
}

// DeleteModel 删除自定义模型。
func (s *Store) DeleteModel(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.models[name]
	if !ok {
		return fmt.Errorf("模型 %q 不存在", name)
	}
	delete(s.models, name)
	if err := s.flushLocked(); err != nil {
		s.models[name] = old // 落盘失败回滚
		return err
	}
	return nil
}

// validID 名称非空且不含空白字符（空白会让日志/URL/表格解析混乱）。
func validID(id, what string) error {
	if id == "" {
		return fmt.Errorf("%s 不能为空", what)
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return fmt.Errorf("%s 不能含空白字符: %q", what, id)
	}
	return nil
}

// validBaseURL 必须是 http/https 绝对 URL。
func validBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("base_url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("base_url 须为 http/https 绝对地址: %q", raw)
	}
	return nil
}

// normalizeBaseURL 存储前归一：trim + 去尾部 "/"。
// 兼容用户填 https://api.deepseek.com 与 .../v1 两种习惯——不在这里强制 /v1，
// 由转发侧拼路径时判断（见 server.customChatURL）。
func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// flushLocked 落盘（tmp+rename 原子写，写法对齐 config.Save；调用方须持锁）。
func (s *Store) flushLocked() error {
	fd := fileData{
		Sources: make([]Source, 0, len(s.sources)),
		Models:  make([]Model, 0, len(s.models)),
	}
	for _, v := range s.sources {
		fd.Sources = append(fd.Sources, v)
	}
	for _, v := range s.models {
		fd.Models = append(fd.Models, v)
	}
	sort.Slice(fd.Sources, func(i, j int) bool { return fd.Sources[i].Name < fd.Sources[j].Name })
	sort.Slice(fd.Models, func(i, j int) bool { return fd.Models[i].Name < fd.Models[j].Name })
	raw, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		return fmt.Errorf("落盘 %s: %w", s.file, err)
	}
	return nil
}
