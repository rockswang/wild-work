// authcode.go TraeWork AuthCode（PKCE 新流程）交换：登录回调返回 authCodeInfo，
// 用配对的 code_verifier + ECDSA 设备公钥换取 Cloud-IDE-JWT。
// 流程逆向自官方客户端（ai_agent.dll）与 ProjectEio/trae2api 的实现。
package traework

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"wild-work/internal/auth"
)

// EpAuthCodeExchange AuthCode 交换端点（首次换 token 无签名，对齐 codex-app-transfer 逆向实现）。
// 注意：此路径同时用于 refresh 续期（带 DeviceProof 签名），但首次 AuthCode 交换不需要签名。
const EpAuthCodeExchange = "/trae/api/v3/oauth/ExchangeToken"

// DeviceInfo 设备指纹（AuthCode 交换必需，官方客户端 bb() 注入）。
type DeviceInfo struct {
	DeviceID         string `json:"DeviceID"`
	MachineID        string `json:"MachineID"`
	PlatformCode     string `json:"PlatformCode"`
	DeviceType       string `json:"DeviceType"`
	DeviceName       string `json:"DeviceName"`
	DeviceModel      string `json:"DeviceModel"`
	ClientVersion    string `json:"ClientVersion"`
	DevicePublicKey  string `json:"DevicePublicKey"`
	DeviceBrand      string `json:"DeviceBrand"`
	DeviceCPU        string `json:"DeviceCPU"`
	OSInfo           string `json:"OSInfo"`
	OSVersion        string `json:"OSVersion"`
}

// AuthCodeResult AuthCode 交换响应。
type AuthCodeResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Host         string // 交换成功的 origin（后续 API 用）
}

// ExchangeAuthCode 用 AuthCode + CodeVerifier + 设备公钥交换 token。
// host 回调里的 login host（如 https://api.trae.com.cn）。
func (c *Client) ExchangeAuthCode(a *auth.Auth, authCode, codeVerifier string) (*AuthCodeResult, error) {
	if strings.TrimSpace(authCode) == "" {
		return nil, fmt.Errorf("no authCode")
	}
	if strings.TrimSpace(codeVerifier) == "" {
		return nil, fmt.Errorf("no codeVerifier")
	}
	pubPEM, err := devicePublicKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	di := DeviceInfo{
		DeviceID:        a.DeviceID,
		MachineID:       a.MachineID,
		PlatformCode:    "SOLO_PC", // 对齐抓包 GetPCAuthCode 的 PlatformCode
		DeviceType:      "PC",
		DeviceName:      deviceDisplayName(),
		DeviceModel:     DeviceBrand,
		ClientVersion:   IdeVersion,
		DevicePublicKey: pubPEM,
		DeviceBrand:     DeviceBrand,
		DeviceCPU:       "",
		OSInfo:          "windows",
		OSVersion:       OSVersion,
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   di,
		"IDEVersion":   IdeVersion,
	}
	raw, _ := json.Marshal(body)

	// 依次尝试候选 origin（回调 host 优先，再回退 trae.cn / trae.com.cn）
	var lastErr string
	for _, origin := range authCodeOrigins(a.ApiHost) {
		req, err := http.NewRequest(http.MethodPost, origin+EpAuthCodeExchange, bytes.NewReader(raw))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = origin + " => " + err.Error()
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Sprintf("%s => HTTP %d %s", origin, resp.StatusCode, truncate(string(data), 160))
			continue
		}
		res, err := parseAuthCodeResult(data)
		if err != nil {
			lastErr = origin + " => " + err.Error()
			continue
		}
		if res.AccessToken == "" {
			lastErr = origin + " => " + truncate(string(data), 160)
			continue
		}
		res.Host = origin
		log.Printf("traework authcode exchange success host=%s token_len=%d refresh=%t", origin, len(res.AccessToken), res.RefreshToken != "")
		return res, nil
	}
	return nil, fmt.Errorf("AuthCode ExchangeToken failed: %s", lastErr)
}

// authCodeOrigins 候选交换 origin：api_host（api.trae.cn）优先，再回退回调 host / trae.com.cn。
func authCodeOrigins(callbackHost string) []string {
	out := []string{}
	// 权威实现用 api.trae.cn（不带 com）
	for _, h := range []string{UgHost, strings.TrimSpace(callbackHost), "https://api.trae.com.cn"} {
		if h != "" && !containsString(out, h) {
			out = append(out, strings.TrimRight(h, "/"))
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// parseAuthCodeResult 解析交换响应：token 在 Result.Token/AccessToken，refresh 在 Result.RefreshToken。
func parseAuthCodeResult(data []byte) (*AuthCodeResult, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("authcode exchange parse: %w", err)
	}
	res := &AuthCodeResult{}
	for _, container := range []any{v["Result"], v["result"], v["data"], v} {
		m, ok := container.(map[string]any)
		if !ok {
			continue
		}
		res.AccessToken = firstNonEmptyStr(m, "AccessToken", "accessToken", "access_token", "Token", "token")
		if res.AccessToken != "" {
			res.RefreshToken = firstNonEmptyStr(m, "RefreshToken", "refreshToken", "refresh_token")
			res.ExpiresAt = firstInt64(m, "TokenExpireAt", "tokenExpireAt", "ExpiresAt", "expiresAt", "expiredAt")
			break
		}
	}
	if res.AccessToken == "" {
		return nil, fmt.Errorf("response missing token")
	}
	res.ExpiresAt = normalizeExpiresAt(res.ExpiresAt)
	return res, nil
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		}
	}
	return 0
}

// devicePublicKeyPEM 生成一次性 ECDSA P256 公私钥对，返回公钥 PEM（官方客户端持私钥用于后续鉴权）。
func devicePublicKeyPEM() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// deviceDisplayName 取主机名作设备名（官方客户端取 USER/USERNAME/HOSTNAME）。
func deviceDisplayName() string {
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "PC"
}

// GenPKCE 生成 PKCE code_verifier + code_challenge（S256），verifier 须与登录 URL 配对并保存。
func GenPKCE() (verifier, challenge string) {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}
