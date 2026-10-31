package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatchAsset(t *testing.T) {
	assets := []AssetInfo{
		{Name: "wild-work-darwin-arm64", Size: 100, DownloadURL: "http://example.com/darwin-arm64"},
		{Name: "wild-work-windows-amd64.exe", Size: 200, DownloadURL: "http://example.com/win-amd64"},
		{Name: "wild-work-linux-amd64", Size: 300, DownloadURL: "http://example.com/linux-amd64"},
		{Name: "WildWork-2.9.4-arm64.dmg", Size: 400, DownloadURL: "http://example.com/dmg"},
	}

	matchWin, err := MatchAsset(assets, "windows", "amd64")
	if err != nil || matchWin.Name != "wild-work-windows-amd64.exe" {
		t.Fatalf("expected windows-amd64, got %v, err %v", matchWin, err)
	}

	matchMac, err := MatchAsset(assets, "darwin", "arm64")
	if err != nil || matchMac.Name != "wild-work-darwin-arm64" {
		t.Fatalf("expected darwin-arm64, got %v, err %v", matchMac, err)
	}

	matchLinux, err := MatchAsset(assets, "linux", "amd64")
	if err != nil || matchLinux.Name != "wild-work-linux-amd64" {
		t.Fatalf("expected linux-amd64, got %v, err %v", matchLinux, err)
	}

	_, err = MatchAsset(assets, "freebsd", "riscv64")
	if err == nil {
		t.Fatalf("expected error for unsupported platform, got nil")
	}
}

func TestVerifyDigestStrict(t *testing.T) {
	content := []byte("hello wild-work auto update test suite")
	tmpFile, err := os.CreateTemp("", "test-digest-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	_, _ = tmpFile.Write(content)
	_ = tmpFile.Close()

	hasher := sha256.New()
	hasher.Write(content)
	expectedHex := hex.EncodeToString(hasher.Sum(nil))

	// 1. sha256:xxx 格式
	if err := VerifyDigest(tmpFile.Name(), "sha256:"+expectedHex); err != nil {
		t.Fatalf("VerifyDigest with prefix failed: %v", err)
	}

	// 2. 裸 hex 格式
	if err := VerifyDigest(tmpFile.Name(), expectedHex); err != nil {
		t.Fatalf("VerifyDigest bare hex failed: %v", err)
	}

	// 3. 大写前缀和字母
	if err := VerifyDigest(tmpFile.Name(), "SHA256:"+strings.ToUpper(expectedHex)); err != nil {
		t.Fatalf("VerifyDigest uppercase failed: %v", err)
	}

	// 4. 空摘要必须报错（严禁静默放行）
	if err := VerifyDigest(tmpFile.Name(), ""); err == nil {
		t.Fatalf("expected error for empty digest, got nil")
	}
	if err := VerifyDigest(tmpFile.Name(), "   "); err == nil {
		t.Fatalf("expected error for whitespace digest, got nil")
	}

	// 5. 长度非法摘要报错
	if err := VerifyDigest(tmpFile.Name(), "sha256:12345"); err == nil {
		t.Fatalf("expected error for invalid length digest, got nil")
	}

	// 6. 哈希不匹配报错
	wrongHex := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := VerifyDigest(tmpFile.Name(), "sha256:"+wrongHex); err == nil {
		t.Fatalf("expected digest mismatch error, got nil")
	}
}

func TestCheckSources(t *testing.T) {
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"tag_name": "v2.10.0",
			"name": "v2.10.0 发布",
			"body": "更新内容测试",
			"published_at": "2026-10-21T10:00:00Z",
			"assets": [
				{
					"name": "wild-work-darwin-arm64",
					"size": 12345,
					"digest": "sha256:11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff",
					"browser_download_url": "http://example.com/download/wild-work-darwin-arm64"
				}
			]
		}`)
	})

	server := httptest.NewServer(apiHandler)
	defer server.Close()

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			r := req.Clone(req.Context())
			r.URL.Scheme = "http"
			r.URL.Host = server.Listener.Addr().String()
			return http.DefaultTransport.RoundTrip(r)
		}),
	}

	rel, hasUpdate, err := CheckSources(context.Background(), client, "test/repo", "2.9.4", nil)
	if err != nil {
		t.Fatalf("CheckSources failed: %v", err)
	}
	if !hasUpdate {
		t.Fatalf("expected hasUpdate = true, got false")
	}
	if rel.Version != "2.10.0" {
		t.Fatalf("expected version 2.10.0, got %s", rel.Version)
	}
}

func TestDownloadAssetSlowStream(t *testing.T) {
	// 构建 256KB 测试流分块慢速返回，验证流式读取不会被提前 cancel 掐断
	chunk := bytes.Repeat([]byte("A"), 16*1024)
	totalChunks := 16
	var fullContent []byte
	for i := 0; i < totalChunks; i++ {
		fullContent = append(fullContent, chunk...)
	}

	hasher := sha256.New()
	hasher.Write(fullContent)
	expectedHex := hex.EncodeToString(hasher.Sum(nil))

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fullContent)))
		w.WriteHeader(http.StatusOK)

		for i := 0; i < totalChunks; i++ {
			_, _ = w.Write(chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Millisecond) // 模拟网络延迟流出
		}
	}))
	defer dlServer.Close()

	asset := &AssetInfo{
		Name:        "wild-work-test-slow",
		Size:        int64(len(fullContent)),
		Digest:      "sha256:" + expectedHex,
		DownloadURL: dlServer.URL + "/slow-binary",
	}

	var progressReports atomic.Int64
	res, err := DownloadAsset(context.Background(), http.DefaultClient, asset, nil, func(d, t, s int64, src string) {
		progressReports.Add(1)
	})
	if err != nil {
		t.Fatalf("DownloadAsset slow stream failed: %v", err)
	}
	defer os.Remove(res.FilePath)

	if progressReports.Load() == 0 {
		t.Errorf("expected progress reports during slow stream, got 0")
	}

	saved, err := os.ReadFile(res.FilePath)
	if err != nil || len(saved) != len(fullContent) {
		t.Fatalf("downloaded content length mismatch: got %d, want %d", len(saved), len(fullContent))
	}
}

func TestDownloadAssetRefuseEmptyDigest(t *testing.T) {
	asset := &AssetInfo{
		Name:        "wild-work-no-digest",
		Size:        100,
		Digest:      "",
		DownloadURL: "http://127.0.0.1:12345/binary",
	}

	_, err := DownloadAsset(context.Background(), http.DefaultClient, asset, nil, nil)
	if err == nil {
		t.Fatalf("expected error for asset without digest, got nil")
	}
}

func TestDownloadAssetProxyFallback(t *testing.T) {
	content := []byte("proxy fallback content")
	hasher := sha256.New()
	hasher.Write(content)
	expectedHex := hex.EncodeToString(hasher.Sum(nil))

	// 1. 直连服务（返回 500 失败）
	directServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer directServer.Close()

	// 2. 代理服务（正常返回）
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer proxyServer.Close()

	asset := &AssetInfo{
		Name:        "wild-work-test-fallback",
		Size:        int64(len(content)),
		Digest:      "sha256:" + expectedHex,
		DownloadURL: directServer.URL + "/direct-binary",
	}

	// 传入代理前缀
	proxyPrefix := proxyServer.URL + "/"
	res, err := DownloadAsset(context.Background(), http.DefaultClient, asset, []string{proxyPrefix}, nil)
	if err != nil {
		t.Fatalf("DownloadAsset fallback to proxy failed: %v", err)
	}
	defer os.Remove(res.FilePath)

	saved, err := os.ReadFile(res.FilePath)
	if err != nil || string(saved) != string(content) {
		t.Fatalf("downloaded content mismatch, got %s", string(saved))
	}
}

func TestApplyUpdate(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "wild-work-apply-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 模拟已安装目标二进制
	targetBin := filepath.Join(tempDir, "wild-work-app")
	oldContent := []byte("version 2.9.4 binary")
	if err := os.WriteFile(targetBin, oldContent, 0755); err != nil {
		t.Fatal(err)
	}

	// 模拟新下载的二进制
	newDownloaded := filepath.Join(tempDir, "downloaded.tmp")
	newContent := []byte("version 2.10.0 binary")
	if err := os.WriteFile(newDownloaded, newContent, 0644); err != nil {
		t.Fatal(err)
	}

	// 执行 ApplyUpdate 替换
	if err := ApplyUpdate(newDownloaded, targetBin); err != nil {
		t.Fatalf("ApplyUpdate failed: %v", err)
	}

	// 验证目标文件已更新为新内容
	updated, err := os.ReadFile(targetBin)
	if err != nil || string(updated) != string(newContent) {
		t.Fatalf("target binary content mismatch: got %s, want %s", string(updated), string(newContent))
	}

	// 验证下载临时文件已被清理
	if _, err := os.Stat(newDownloaded); !os.IsNotExist(err) {
		t.Errorf("expected downloaded.tmp to be cleaned up, but it exists")
	}

	// 验证可执行权限
	fi, err := os.Stat(targetBin)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0111 == 0 {
		t.Errorf("expected executable permission on %s, got %v", targetBin, fi.Mode().Perm())
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
