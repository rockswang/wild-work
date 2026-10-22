package updater

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ProgressCallback 下载进度回调。
type ProgressCallback func(downloaded, total int64, speedBytesPerSec int64, source string)

// DownloadResult 下载完成结果。
type DownloadResult struct {
	FilePath string
	Source   string
}

// DownloadAsset 执行自动下载：直连优先 + 代理降级兜底 + 强制 SHA256 完整性校验。
// 遵循 R35 纪律：流式传输必须使用独立的、不设 Client.Timeout 的 Client。
func DownloadAsset(
	ctx context.Context,
	client *http.Client,
	asset *AssetInfo,
	proxies []string,
	onProgress ProgressCallback,
) (*DownloadResult, error) {
	if asset == nil || asset.DownloadURL == "" {
		return nil, fmt.Errorf("invalid asset: empty download url")
	}
	if strings.TrimSpace(asset.Digest) == "" {
		return nil, fmt.Errorf("download refused: asset has no SHA256 digest")
	}

	// 保证 client 不设全局 Timeout（R35 纪律）
	if client == nil {
		client = &http.Client{}
	} else if client.Timeout != 0 {
		// 克隆并移除全局超时，交由 Context 控制
		cloned := *client
		cloned.Timeout = 0
		client = &cloned
	}

	directURL := asset.DownloadURL

	type candidate struct {
		name string
		url  string
	}
	var candidates []candidate
	// 1. 直连官方源
	candidates = append(candidates, candidate{name: "github-direct", url: directURL})

	// 2. 配置的代理镜像源
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		candidates = append(candidates, candidate{
			name: p,
			url:  p + directURL,
		})
	}

	var lastErr error

	// 依次尝试候选源（直连优先，失败则自动切换加速代理）
	for _, c := range candidates {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		res, err := downloadFromSource(ctx, client, c.name, c.url, asset, onProgress)
		if err == nil {
			return res, nil
		}

		log.Printf("[updater] source %s failed: %v", c.name, err)
		lastErr = fmt.Errorf("source %s: %w", c.name, err)
	}

	return nil, fmt.Errorf("all download sources failed; last error: %w", lastErr)
}

// downloadFromSource 从指定源完整流式下载并校验，期间 context 保持存活，直到读取完成并校验通过。
func downloadFromSource(
	ctx context.Context,
	client *http.Client,
	sourceName string,
	url string,
	asset *AssetInfo,
	onProgress ProgressCallback,
) (*DownloadResult, error) {
	// 创建独立下载请求上下文，继承调用方 ctx
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "wild-work-updater")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", sourceName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d from %s", resp.StatusCode, sourceName)
	}

	// 创建临时接收文件
	tmpFile, err := os.CreateTemp("", "wild-work-update-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
	}()

	total := resp.ContentLength
	if total <= 0 && asset.Size > 0 {
		total = asset.Size
	}

	var downloaded atomic.Int64
	startTime := time.Now()
	var lastTickTime time.Time
	var lastTickBytes int64

	doneCh := make(chan struct{})
	defer close(doneCh)

	if onProgress != nil {
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-doneCh:
					return
				case <-ticker.C:
					now := time.Now()
					curr := downloaded.Load()
					var speed int64
					if !lastTickTime.IsZero() {
						elapsedSec := now.Sub(lastTickTime).Seconds()
						if elapsedSec > 0 {
							speed = int64(float64(curr-lastTickBytes) / elapsedSec)
						}
					} else {
						elapsedSec := now.Sub(startTime).Seconds()
						if elapsedSec > 0 {
							speed = int64(float64(curr) / elapsedSec)
						}
					}
					lastTickTime = now
					lastTickBytes = curr
					onProgress(curr, total, speed, sourceName)
				}
			}
		}()
	}

	// 采用带有看门狗超时检查的流式读取：60s 无字节读取视为断流
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(tmpPath)
			return nil, ctx.Err()
		default:
		}

		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := tmpFile.Write(buf[:n]); wErr != nil {
				_ = os.Remove(tmpPath)
				return nil, fmt.Errorf("write temp file: %w", wErr)
			}
			downloaded.Add(int64(n))
		}

		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("read stream: %w", rErr)
		}
	}

	// 关闭临时文件句柄，准备校验 SHA256
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("close temp file: %w", err)
	}

	if onProgress != nil {
		onProgress(downloaded.Load(), total, 0, sourceName)
	}

	// 强制异源 SHA256 校验
	if err := VerifyDigest(tmpPath, asset.Digest); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("digest verification failed on %s: %w", sourceName, err)
	}

	return &DownloadResult{
		FilePath: tmpPath,
		Source:   sourceName,
	}, nil
}
