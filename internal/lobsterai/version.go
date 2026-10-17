// version.go 客户端版本号解析器（协议 §2.5）。
//
// 为什么需要它：X-LobsterAI-Client-Version 是**动态真值**（对话、模型列表、签到、
// 续期都要带），而它的来源是一个第三方域的查询接口。流程（逐条对齐协议 §2.5）：
//
//	进程内缓存（12h TTL）→ GET VersionURL → 取 data.value.version → 正则校验
//	→ 失败回退常量 FallbackClientVersion，且**失败不缓存兜底值**。
//
// 「失败不缓存」是协议明确要求（`:663-666`），含义是：一次网络抖动不得让整个进程
// 在接下来 12 小时里一直用兜底版本号——下次请求要重新尝试真实取值。
// 代价是版本接口长期不可用时会每次请求都试一次；这里照抄上游语义，不做负缓存。
package lobsterai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// clientVersionRe 版本号格式校验正则（协议 §2.5 逐字照抄）：
// `^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$` —— 接受 `2026.9.4`、`1.2.3` 与
// `1.2.3-beta.1` 这类带预发布后缀的形态，拒绝空串/任意文本。
var clientVersionRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$`)

// versionCache 进程内版本号缓存（12h TTL）。**只缓存成功取到的真值**，兜底值不入缓存。
var versionCache struct {
	mu     sync.Mutex
	value  string
	expiry time.Time
}

// cachedClientVersion 取未过期的缓存值；空串表示需要重新查询。
func cachedClientVersion() string {
	versionCache.mu.Lock()
	defer versionCache.mu.Unlock()
	if versionCache.value != "" && time.Now().Before(versionCache.expiry) {
		return versionCache.value
	}
	return ""
}

// storeClientVersion 写入成功取到的版本号。
func storeClientVersion(v string) {
	versionCache.mu.Lock()
	versionCache.value = v
	versionCache.expiry = time.Now().Add(ClientVersionTTL)
	versionCache.mu.Unlock()
}

// ClientVersion 返回客户端版本号（包级入口）。
//
// 供**登录包**（internal/login_lobsterai 的 exchange 请求体）与本包各出站请求共用；
// 两者共享同一个进程内缓存，因此登录后紧接的对话请求不会再多打一次版本接口。
func ClientVersion() string { return defaultClient.ClientVersion() }

// ClientVersion 返回客户端版本号（实例入口，测试可经 c.Version 注入假上游）。
//
// 返回空串是**不可能**的：取不到真值时返回 FallbackClientVersion。
func (c *Client) ClientVersion() string {
	if v := cachedClientVersion(); v != "" {
		return v
	}
	v, err := c.fetchClientVersion()
	if err != nil || v == "" {
		// 失败回退常量，且**不缓存**：下次调用会重新尝试真值（协议 §2.5）。
		return FallbackClientVersion
	}
	storeClientVersion(v)
	return v
}

// fetchClientVersion 查询版本号接口并解析 data.value.version。
func (c *Client) fetchClientVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.versionURL(), nil)
	if err != nil {
		return "", err
	}
	// 版本号查询按协议 §2.1 末行：**不带 Authorization**，只要 Accept 与 User-Agent。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("lobsterai：版本号查询 HTTP %d", resp.StatusCode)
	}
	return parseClientVersion(raw)
}

// parseClientVersion 解析版本号响应：取 data.value.version 并做格式校验。
//
// 形状（协议 §2.5）：`{"data":{"value":{"version":"2026.9.4"}}}`。
// 容忍 `data.value` 直接被省略的形态（视为失败，不猜）。
func parseClientVersion(raw []byte) (string, error) {
	var doc struct {
		Data struct {
			Value struct {
				Version string `json:"version"`
			} `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	v := strings.TrimSpace(doc.Data.Value.Version)
	if v == "" {
		return "", errors.New("lobsterai：版本号响应没有 data.value.version")
	}
	if !clientVersionRe.MatchString(v) {
		return "", errors.New("lobsterai：版本号格式非法")
	}
	// 正则的第二个捕获组被刻意保留为「非捕获」写法（协议原文如此）：这里只需要整体匹配。
	return v, nil
}

// versionURL 生效的版本号接口地址（测试经 Client.Version 注入假上游）。
func (c *Client) versionURL() string {
	if s := strings.TrimSpace(c.Version); s != "" {
		return s
	}
	return VersionURL
}
