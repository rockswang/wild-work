package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var tagRegex = regexp.MustCompile(`releases/tag/(v?[0-9]+.[0-9]+(.[0-9]+)?[a-zA-Z0-9._-]*)`)

// ghReleasePayload GitHub API /releases/latest 返回的 JSON 结构。
type ghReleasePayload struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// CheckSources 并行查询多源（直连 GitHub API + 加速代理），取最高版本。
func CheckSources(ctx context.Context, client *http.Client, repo, currentVer string, proxies []string) (*ReleaseInfo, bool, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		repo = "rockswang/wild-work"
	}

	type queryTarget struct {
		name  string
		url   string
		isAPI bool
	}

	var targets []queryTarget
	// 1. 直连 GitHub 官方 API（权威元数据与 SHA256 来源）
	targets = append(targets, queryTarget{
		name:  "github-direct",
		url:   fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo),
		isAPI: true,
	})

	// 2. 经配置各代理的 GitHub API 镜像（提高版本感知成功率）
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		targets = append(targets, queryTarget{
			name:  p + "api",
			url:   fmt.Sprintf("%shttps://api.github.com/repos/%s/releases/latest", p, repo),
			isAPI: true,
		})
		// 3. HTML 页面 302 探针（仅作版本感知，无 digest 不直接走自动下载）
		targets = append(targets, queryTarget{
			name:  p + "web",
			url:   fmt.Sprintf("%shttps://github.com/%s/releases/latest", p, repo),
			isAPI: false,
		})
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var bestRelease *ReleaseInfo
	var errorsList []string

	var wg sync.WaitGroup
	for _, tgt := range targets {
		wg.Add(1)
		go func(t queryTarget) {
			defer wg.Done()
			rel, err := fetchReleaseFromTarget(ctxTimeout, client, t.url, t.isAPI)
			if err != nil {
				mu.Lock()
				errorsList = append(errorsList, fmt.Sprintf("[%s] %v", t.name, err))
				mu.Unlock()
				return
			}
			if rel == nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()
			if bestRelease == nil {
				bestRelease = rel
			} else {
				cmp := CompareVersions(rel.Version, bestRelease.Version)
				// 优先更高版本；若版本相同，优先选择包含合法 Asset 和 Digest 的权威元数据
				if cmp > 0 {
					bestRelease = rel
				} else if cmp == 0 {
					if hasValidAsset(rel) && !hasValidAsset(bestRelease) {
						bestRelease = rel
					}
				}
			}
		}(tgt)
	}

	wg.Wait()

	if bestRelease == nil {
		return nil, false, fmt.Errorf("all update sources failed: %s", strings.Join(errorsList, "; "))
	}

	// 匹配当前平台二进制资产
	if len(bestRelease.AllAssets) > 0 {
		if asset, err := CurrentPlatformAsset(bestRelease.AllAssets); err == nil {
			bestRelease.Asset = asset
		}
	}

	hasUpdate := CompareVersions(bestRelease.Version, currentVer) > 0
	return bestRelease, hasUpdate, nil
}

func hasValidAsset(rel *ReleaseInfo) bool {
	if rel == nil || len(rel.AllAssets) == 0 {
		return false
	}
	for _, a := range rel.AllAssets {
		if a.Digest != "" {
			return true
		}
	}
	return false
}

func fetchReleaseFromTarget(ctx context.Context, client *http.Client, targetURL string, isAPI bool) (*ReleaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wild-work-updater")
	if isAPI {
		req.Header.Set("Accept", "application/vnd.github.v3+json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	if isAPI {
		var payload ghReleasePayload
		dec := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024))
		if err := dec.Decode(&payload); err != nil {
			return nil, fmt.Errorf("decode json: %w", err)
		}
		if payload.TagName == "" {
			return nil, fmt.Errorf("empty tag_name")
		}

		rel := &ReleaseInfo{
			TagName:     payload.TagName,
			Version:     CleanVersion(payload.TagName),
			Title:       payload.Name,
			Changelog:   payload.Body,
			PublishedAt: payload.PublishedAt,
			HTMLURL:     payload.HTMLURL,
		}
		for _, a := range payload.Assets {
			rel.AllAssets = append(rel.AllAssets, AssetInfo{
				Name:        a.Name,
				Size:        a.Size,
				Digest:      a.Digest,
				DownloadURL: a.BrowserDownloadURL,
			})
		}
		return rel, nil
	}

	// 网页 / 302 页面探测
	loc := resp.Header.Get("Location")
	if loc != "" {
		if m := tagRegex.FindStringSubmatch(loc); len(m) > 1 {
			tag := m[1]
			return &ReleaseInfo{
				TagName: tag,
				Version: CleanVersion(tag),
				HTMLURL: loc,
			}, nil
		}
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err == nil {
		if m := tagRegex.FindSubmatch(bodyBytes); len(m) > 1 {
			tag := string(m[1])
			return &ReleaseInfo{
				TagName: tag,
				Version: CleanVersion(tag),
			}, nil
		}
	}

	return nil, fmt.Errorf("could not extract release tag from html")
}
