package chatgpt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"wild-work/internal/auth"
)

// 首页缓存：L1 是唯一「内容在几小时内不变」的步骤。
//
// 为什么只缓存首页：
//   - prepare_token / finalize token 都**一次性**，缓存即 403（见 tokencache.go）；
//   - PoW 与 VM 各只要 0.5~2.4ms，缓存没意义；
//   - 首页那份 HTML（含 data-build 与 script src）是几百 KB 的下载，是纯浪费。
//
// 缓存键含 base_url + UA（不同身份/不同镜像的首页不同），TTL 30 分钟。
// 缓存目录沿用 v2 数据目录惯例：`<data>/cache/home.json`（spec §8）。

// HomeTTL 是首页缓存的存活时间。data-build 变动频率是「上游发版」，远低于 30 分钟。
const HomeTTL = 30 * time.Minute

// cacheFileNameHome 是首页缓存文件名（`<data>/cache/` 下，渠道唯一占用者）。
const cacheFileNameHome = "home.json"

// cachedHome 是落盘形态。
type cachedHome struct {
	Base      string   `json:"base"`
	UA        string   `json:"ua"`
	DataBuild string   `json:"data_build"`
	Scripts   []string `json:"scripts"`
	SDKURL    string   `json:"sdk_url"`
	HTMLSize  int      `json:"html_size"`
	FetchedAt int64    `json:"fetched_at"` // Unix 秒
}

// fetchHomeCached 先看磁盘缓存，未命中才真抓首页。
//
// cached=true 表示这次没发 HTTP 请求（省掉一次几百 KB 下载）。
func (c *Client) fetchHomeCached(a *auth.Auth) (*HomeInfo, bool, error) {
	path := c.cachePath(cacheFileNameHome)
	if path != "" {
		ua, _ := c.cfgUA()
		if ch, ok := readHomeCache(path, c.baseURL(), ua); ok {
			return &HomeInfo{
				Status: 200, DataBuild: ch.DataBuild, Scripts: ch.Scripts,
				SDKURL: ch.SDKURL, HTMLSize: ch.HTMLSize,
			}, true, nil
		}
	}
	home, err := c.FetchHome(a)
	if err != nil {
		return home, false, err
	}
	if path != "" && home.Status == 200 && home.DataBuild != "" {
		ua, _ := c.cfgUA()
		writeHomeCache(path, home, c.baseURL(), ua)
	}
	return home, false, nil
}

func readHomeCache(path, base, ua string) (*cachedHome, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var ch cachedHome
	if err := json.Unmarshal(b, &ch); err != nil {
		return nil, false
	}
	// 缓存键必须匹配：换 base_url / 换 UA（身份族）都不能复用。
	if ch.Base != base || ch.UA != ua {
		return nil, false
	}
	if time.Since(time.Unix(ch.FetchedAt, 0)) > HomeTTL {
		return nil, false
	}
	// 只要求 data-build 非空：上游首页可能没有 script.src（全内联），
	// 此时 p token 的 scriptSources 为空数组也能过（实测）。
	if ch.DataBuild == "" {
		return nil, false
	}
	return &ch, true
}

func writeHomeCache(path string, home *HomeInfo, base, ua string) {
	ch := cachedHome{
		Base: base, UA: ua, DataBuild: home.DataBuild, Scripts: home.Scripts,
		SDKURL: home.SDKURL, HTMLSize: home.HTMLSize, FetchedAt: time.Now().Unix(),
	}
	b, err := json.Marshal(ch)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // 缓存不是凭据，但含身份指纹，仍按 0600
		return
	}
	_ = os.Rename(tmp, path)
}

// cachePath 返回 <data>/cache/<name>；dataDir 为空（纯内存模式）时返回空串。
func (c *Client) cachePath(name string) string {
	dir := c.dataDirValue()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "cache", name)
}
