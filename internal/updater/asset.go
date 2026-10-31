package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// MatchAsset 根据当前操作系统和架构，从 Release 资产列表中挑选对应的可执行文件。
func MatchAsset(assets []AssetInfo, goos, goarch string) (*AssetInfo, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("release assets list is empty")
	}

	var expectedName string
	switch goos {
	case "windows":
		expectedName = fmt.Sprintf("wild-work-windows-%s.exe", goarch)
	case "darwin":
		expectedName = fmt.Sprintf("wild-work-darwin-%s", goarch)
	case "linux":
		expectedName = fmt.Sprintf("wild-work-linux-%s", goarch)
	default:
		expectedName = fmt.Sprintf("wild-work-%s-%s", goos, goarch)
	}

	// 1. 严格名称匹配
	for _, a := range assets {
		if strings.EqualFold(a.Name, expectedName) {
			match := a
			return &match, nil
		}
	}

	// 2. 宽松前缀匹配（排除安装包文件如 .dmg, .zip, .tar.gz, .sha256）
	for _, a := range assets {
		lower := strings.ToLower(a.Name)
		if strings.HasSuffix(lower, ".dmg") || strings.HasSuffix(lower, ".zip") ||
			strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".sha256") {
			continue
		}
		if strings.Contains(lower, goos) && strings.Contains(lower, goarch) {
			match := a
			return &match, nil
		}
	}

	return nil, fmt.Errorf("no matching binary asset found for %s/%s in release", goos, goarch)
}

// CurrentPlatformAsset 从资产列表中寻找适合当前机器的二进制。
func CurrentPlatformAsset(assets []AssetInfo) (*AssetInfo, error) {
	return MatchAsset(assets, runtime.GOOS, runtime.GOARCH)
}

// VerifyDigest 校验目标文件的 SHA256 摘要。
// digest 格式通常形如 "sha256:d20311620dbe4307a3d5e5a17e5e2a77066a666db02e9d3115b79eb181f4ab36"。
// 严禁跳过摘要校验：若 digest 为空，直接返回错误，防止未受校验的二进制被安装。
func VerifyDigest(filePath, digest string) error {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return fmt.Errorf("digest is empty: sha256 digest is strictly required")
	}

	expectedHex := digest
	lower := strings.ToLower(digest)
	if strings.HasPrefix(lower, "sha256:") {
		expectedHex = digest[7:]
	}
	expectedHex = strings.TrimSpace(expectedHex)
	if len(expectedHex) != 64 {
		return fmt.Errorf("invalid sha256 digest length %d (expected 64 hex chars): %q", len(expectedHex), digest)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file for digest verification: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("calculate sha256: %w", err)
	}

	actualHex := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualHex, expectedHex) {
		return fmt.Errorf("sha256 mismatch: got %s, expected %s", actualHex, expectedHex)
	}
	return nil
}
