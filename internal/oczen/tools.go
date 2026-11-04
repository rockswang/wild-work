// tools.go 客户端工具名的「不透明改名」与响应侧还原。
//
// 免费档 v2 闸门会拒收**仍携带其他客户端工具词汇**的请求体（实测：Claude Code
// 的 ReportFindings + ScheduleWakeup 同时出现即被拒，去掉任一即放行），故无论
// 调用方是谁，客户端工具名一律改写成 `clint_<sha256 前 12 位 hex>`：
//   - 摘要把同一个客户端工具在多次请求间固定到同一个线上名（prompt cache 亲和）；
//   - 撞名（理论上）追加 `_2`、`_3`… 直到不撞（工具名上限 64 字符）；
//   - 改名可逆：响应侧按 {@link toolRenameMap.resolve} 还原。
//
// 移植自 opencode2api v1.4.6 `internal/protocol/tools.go`（经参考实现
// dsh-our-free-model `src/o2a/tool-renames.js` 逐条对齐）。
package oczen

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	// clientToolPrefix 标记每一个被整形层改名的客户端工具（ClientToolPrefix）。
	clientToolPrefix = "clint_"
	// clientToolDigestLength 线上名携带的摘要长度（clientToolDigestLength）。
	clientToolDigestLength = 12
	// toolWireNameLimit 上游接受的工具名长度上限（toolWireNameLimit）。
	toolWireNameLimit = 64
)

// foldToolName 把工具名按「agent 们惯用的分隔符」折叠成形状键：
// todo_write / todo-write / todo.write / todowrite 折叠成同一个 key。
func foldToolName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		switch r {
		case '_', '.', '-', ' ', '\t', '\r', '\n':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// clientToolWireName 为 `name` 分配一个不透明的线上名（ClientToolWireName）。
// 摘要稳定，撞名时按 `_2`、`_3`… 顺延。
func clientToolWireName(name string, used map[string]bool) string {
	sum := sha256.Sum256([]byte(name))
	digest := hex.EncodeToString(sum[:])[:clientToolDigestLength]
	base := clientToolPrefix + digest
	if !used[base] {
		used[base] = true
		return base
	}
	for suffix := 2; ; suffix++ {
		tag := "_" + strconv.Itoa(suffix)
		cut := toolWireNameLimit - len(tag)
		if cut > len(base) {
			cut = len(base)
		}
		candidate := base[:cut] + tag
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// toolRenameMap 是一次整形请求的改名集合（ToolRenames）。
// 以「线上名 → 客户端名」为主表（与参考实现的 Map 方向一致），并额外维护
// 正向表与两张折叠表供响应侧还原使用。
type toolRenameMap struct {
	toClient map[string]string // 线上名 → 客户端名
	toWire   map[string]string // 客户端名 → 线上名
	byCase   map[string]string // 小写客户端名 → 客户端名（"" = 有歧义）
	byShape  map[string]string // 折叠客户端名 → 客户端名（"" = 有歧义）
}

func newToolRenameMap() *toolRenameMap {
	return &toolRenameMap{
		toClient: map[string]string{},
		toWire:   map[string]string{},
		byCase:   map[string]string{},
		byShape:  map[string]string{},
	}
}

// len 返回已登记的改名条数。
func (m *toolRenameMap) len() int {
	if m == nil {
		return 0
	}
	return len(m.toClient)
}

// add 登记一条「客户端名 → 线上名」映射（Add）。冲突保留首次分配。
func (m *toolRenameMap) add(client, wire string) {
	if m == nil || client == "" || wire == "" || client == wire {
		return
	}
	if _, ok := m.toWire[client]; ok {
		return
	}
	if _, ok := m.toClient[wire]; ok {
		return
	}
	m.toWire[client] = wire
	m.toClient[wire] = client
	bindFolded(m.byCase, strings.ToLower(client), client)
	bindFolded(m.byShape, foldToolName(client), client)
}

// wireOf 返回某客户端名已登记的线上名（Wire）。
func (m *toolRenameMap) wireOf(client string) (string, bool) {
	if m == nil {
		return "", false
	}
	wire, ok := m.toWire[client]
	return wire, ok
}

// resolve 把上游回报的名字还原成调用方声明的名字（Resolve）：
//   - 精确命中线上名 → 直接还原；
//   - `clint_` 前缀按尽力而为剥掉；
//   - 仅大小写不同、或大小写 + 分隔符都不同的名字折叠到唯一声明的那个工具；
//   - 两个工具折叠到同一 key 时绝不猜（返回折叠后的名字）。
//
// 官方工具名（read/shell/…）只要客户端声明过形状相同的工具就会被正确折叠回去。
func (m *toolRenameMap) resolve(wire string) string {
	if m == nil || wire == "" {
		return wire
	}
	if client, ok := m.toClient[wire]; ok {
		return client
	}
	client := wire
	if strings.HasPrefix(client, clientToolPrefix) {
		if rest := client[len(clientToolPrefix):]; rest != "" {
			client = rest
		}
	}
	if _, ok := m.toWire[client]; ok {
		return client
	}
	if folded, ok := m.byCase[strings.ToLower(client)]; ok && folded != "" {
		return folded
	}
	if folded, ok := m.byShape[foldToolName(client)]; ok && folded != "" {
		return folded
	}
	return client
}

// bindFolded 在 `folded[key]` 记录 `client`；同一 key 被两个客户端名命中时标为歧义（""）。
func bindFolded(folded map[string]string, key, client string) {
	if key == "" {
		return
	}
	existing, ok := folded[key]
	if !ok {
		folded[key] = client
		return
	}
	if existing != client {
		folded[key] = ""
	}
}
