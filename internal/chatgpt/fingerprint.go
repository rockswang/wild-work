package chatgpt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"wild-work/internal/auth"
)

// 升级预警：把「上游对抗算法是否变了」变成可自动判定的门禁。
//
// 为什么需要它：上游改对抗算法时**不会报"算法变了"**，而是回
//
//	403 {"detail":"Unusual activity has been detected from your device"}（封号前兆）
//	404 {"detail":"Model not found"}（伪装成模型不存在）
//
// 也就是说「第一次失败」往往已经是一次可疑请求。本文件的目的是**在发高敏感请求之前**
// 就发现漂移，把失败前移到零配额的 prepare/finalize 阶段。
//
// 观测点（全部来自零配额请求，不消耗对话/生图配额）：
//
//	data_build     首页 data-build        前端重新发布会变（最常见，且通常无害）
//	prepare_keys   prepare 响应键集合      上游加/删字段 = 契约变更（危险）
//	finalize_keys  finalize 响应键集合     同上
//	difficulty     PoW 难度位宽            从 6 位变 7 位 = 成本翻 16 倍（危险）
//	so_present     prepare 是否带 so 块    变 False 可能意味着换算法（危险）
//	dx_unknown     Go VM 不认识的 opcode   非空 = VM 一定算错（致命，直接拦）
//	dx_ops         dx 指令数              小幅波动正常（每次不同），**故意不比**（比它只会误报）
//
// 分级：
//   - 致命（dx_unknown 非空）→ 直接拒绝，绝不发业务请求；
//   - 结构漂移（keys / so / difficulty 位宽）→ 拒绝；
//   - 构建漂移（仅 data_build 变）→ 只警告，因为这是日常发版。
//
// 落盘：<data>/cache/fingerprint-<key>.json（账号级）。

// Fingerprint 是一次 sentinel 的观测快照（用于跨轮对比）。
type Fingerprint struct {
	Key          string   `json:"key"`
	Base         string   `json:"base"`
	DataBuild    string   `json:"data_build"`
	PrepareKeys  []string `json:"prepare_keys"`
	FinalizeKeys []string `json:"finalize_keys"`
	Difficulty   string   `json:"difficulty"`
	SoPresent    bool     `json:"so_present"`
	DXOps        int      `json:"dx_ops"`
	SeenAt       int64    `json:"seen_at"`
}

// Drift 是一次漂移判定结果。
type Drift struct {
	// Level: "" 无漂移 / "warn" 构建号变了 / "block" 结构变了 / "fatal" VM 不认识了
	Level  string   `json:"level"`
	Reason []string `json:"reason"`
}

// fingerprintPath 返回 <data>/cache/fingerprint-<key>.json。
func (c *Client) fingerprintPath(a *auth.Auth) string {
	key := sanitizeKey(accountKey(a))
	if key == "" {
		return ""
	}
	return c.cachePath("fingerprint-" + key + ".json")
}

// loadFingerprint 读上次快照（无则 nil）。
func (c *Client) loadFingerprint(a *auth.Auth) *Fingerprint {
	p := c.fingerprintPath(a)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var f Fingerprint
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	if f.Base != c.baseURL() {
		return nil // 换了基址，旧快照无意义
	}
	return &f
}

// saveFingerprint 落盘本次快照（原子写）。
func (c *Client) saveFingerprint(a *auth.Auth, f *Fingerprint) {
	p := c.fingerprintPath(a)
	if p == "" || f == nil {
		return
	}
	f.Key, f.Base, f.SeenAt = sanitizeKey(accountKey(a)), c.baseURL(), time.Now().Unix()
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// fpOf 从一次 sentinel 结果提取快照。
func fpOf(s *Sentinel) *Fingerprint {
	f := &Fingerprint{
		PrepareKeys:  s.PrepareKeys,
		FinalizeKeys: s.FinalizeKeys,
		Difficulty:   s.Difficulty,
		SoPresent:    s.SoPresent,
		DXOps:        s.DXOps,
	}
	if s.Home != nil {
		f.DataBuild = s.Home.DataBuild
	}
	return f
}

// diffFingerprint 对比新旧快照，给出漂移分级。
//
// 注意**故意不比较 dx_ops**：dx 程序每次都不一样（实测 91~99 波动），
// 比它只会误报。VM 是否还认得上游程序由 dx_unknown 判定（更准）。
func diffFingerprint(old, cur *Fingerprint, unknown []string) *Drift {
	d := &Drift{}
	// 1 致命：VM 不认识 opcode —— 现在发业务请求必然算出错 token。
	if len(unknown) > 0 {
		d.Level = "fatal"
		d.Reason = append(d.Reason, "dx 出现 Go VM 不认识的 opcode "+joinStrings(unknown))
		return d
	}
	if old == nil {
		return d // 首轮无基线，只记录不判定
	}
	// 2 结构漂移：字段集合 / so 块 / 难度位宽。
	if !sameStrings(old.PrepareKeys, cur.PrepareKeys) {
		d.Reason = append(d.Reason, "prepare 响应键变了: "+joinStrings(old.PrepareKeys)+" → "+joinStrings(cur.PrepareKeys))
	}
	if !sameStrings(old.FinalizeKeys, cur.FinalizeKeys) {
		d.Reason = append(d.Reason, "finalize 响应键变了: "+joinStrings(old.FinalizeKeys)+" → "+joinStrings(cur.FinalizeKeys))
	}
	if old.SoPresent != cur.SoPresent {
		d.Reason = append(d.Reason, "prepare 的 so 块存在性变了（可能换算法）")
	}
	if len(old.Difficulty) != len(cur.Difficulty) && old.Difficulty != "" && cur.Difficulty != "" {
		d.Reason = append(d.Reason, "PoW 难度位宽变了: "+old.Difficulty+" → "+cur.Difficulty)
	}
	if len(d.Reason) > 0 {
		d.Level = "block"
		return d
	}
	// 3 构建漂移：日常发版，只警告。
	if old.DataBuild != cur.DataBuild && old.DataBuild != "" {
		d.Level = "warn"
		d.Reason = append(d.Reason, "data-build 变了: "+old.DataBuild+" → "+cur.DataBuild+"（前端发版，通常无害）")
	}
	return d
}

// joinStrings 用 ", " 连接（避免每处都 import strings 只为 Join）。
func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	if out == "" {
		return "(空)"
	}
	return out
}

// sameStrings 判断两个字符串切片是否完全一致（顺序敏感）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FingerprintMaxAge 是指纹快照的可用年龄（秒）。
//
// 比 TokenMaxAge 宽一点：一次预取会同时刷新 token 与指纹，消费 token 时
// 指纹最多比 token 新。留 2 倍余量，免得时钟抖动造成误判。
const FingerprintMaxAge = 2 * TokenMaxAge

// fingerprintFresh 判断是否有新鲜的指纹快照（= 刚刚真跑过一次完整 sentinel）。
//
// 这是预取缓存的安全前提：**不允许在没做过升级校验的情况下消费缓存 token**。
func (c *Client) fingerprintFresh(a *auth.Auth) bool {
	p := c.fingerprintPath(a)
	if p == "" {
		return true // 未启用指纹（dataDir 为空），不阻塞
	}
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return time.Since(st.ModTime()).Seconds() <= float64(FingerprintMaxAge)
}
