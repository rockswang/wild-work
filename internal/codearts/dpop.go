// dpop.go DPoP（Demonstrating Proof-of-Possession）证明的 ES256 实现（协议 §2.2 末段）。
//
// 为什么 CodeArts 需要它：STS 换凭据端点的鉴权**不是** AK/SK 签名，而是 DPoP——
// 客户端持一把 ES256（P-256）私钥，每次请求带一枚自签的 JWS：
//
//	头：{"typ":"dpop+jwt","alg":"ES256","jwk":{公钥 kty/crv/x/y}}
//	载荷：{"htm":"POST","htu":"<完整 STS URL>","iat":<秒>,"jti":"<32 字节 hex>"}
//	签名：ECDSA-P256(SHA-256(头.载荷))，r||s 各 32 字节大端拼接后 base64url
//	发送：请求头 `DPoP: <jws>`
//
// 私钥以 JWK 形式持久化（落 auth.Auth.Extra 的 dpop_private_key_jwk），否则续期时
// 换不出新凭据（服务端会认成另一把 key）。没有 wasm / 第三方 JWT 库依赖：Go 标准库
// 的 crypto/ecdsa + crypto/elliptic 足够。
package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// DPoPKey 是 DPoP 证明用的 ES256 密钥对。
type DPoPKey struct {
	priv *ecdsa.PrivateKey
}

// NewDPoPKey 生成一把新的 P-256 私钥。
func NewDPoPKey() (*DPoPKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("codearts: 生成 DPoP 密钥失败: %w", err)
	}
	return &DPoPKey{priv: priv}, nil
}

// curve 曲线名。协议写死 P-256/ES256，故不做曲线协商。
const dpopCurve = "P-256"

// dpopAlg JWS 算法名。
const dpopAlg = "ES256"

// b64url 无填充 base64url（JWS 与 PKCE 都用它）。
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// pad32 把大端整数补足到 32 字节（P-256 坐标与 r/s 的定长要求）。
func pad32(v *big.Int) []byte {
	out := make([]byte, 32)
	b := v.Bytes()
	copy(out[32-len(b):], b)
	return out
}

// publicJWK 返回公钥 JWK 的字段表（JWS 头里内嵌）。
func (k *DPoPKey) publicJWK() map[string]string {
	return map[string]string{
		"kty": "EC",
		"crv": dpopCurve,
		"x":   b64url(pad32(k.priv.X)),
		"y":   b64url(pad32(k.priv.Y)),
	}
}

// JWK 返回**私钥** JWK 的 JSON 串（落 auth.Auth.Extra 持久化用）。
func (k *DPoPKey) JWK() (string, error) {
	jwk := map[string]string{
		"kty": "EC",
		"crv": dpopCurve,
		"x":   b64url(pad32(k.priv.X)),
		"y":   b64url(pad32(k.priv.Y)),
		"d":   b64url(pad32(k.priv.D)),
	}
	raw, err := json.Marshal(jwk)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ParseDPoPKey 从私钥 JWK JSON 串还原密钥。
func ParseDPoPKey(jwk string) (*DPoPKey, error) {
	s := strings.TrimSpace(jwk)
	if s == "" {
		return nil, fmt.Errorf("codearts: 空的 DPoP 私钥 JWK")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("codearts: DPoP 私钥 JWK 不是 JSON 对象: %w", err)
	}
	x, err := b64urlInt(m["x"])
	if err != nil {
		return nil, fmt.Errorf("codearts: DPoP JWK 的 x 非法: %w", err)
	}
	y, err := b64urlInt(m["y"])
	if err != nil {
		return nil, fmt.Errorf("codearts: DPoP JWK 的 y 非法: %w", err)
	}
	d, err := b64urlInt(m["d"])
	if err != nil {
		return nil, fmt.Errorf("codearts: DPoP JWK 的 d 非法: %w", err)
	}
	curve := elliptic.P256()
	if !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("codearts: DPoP JWK 的公钥不在 P-256 曲线上")
	}
	priv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         d,
	}
	return &DPoPKey{priv: priv}, nil
}

// b64urlInt 解码无填充 base64url 到大整数。
func b64urlInt(s string) (*big.Int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("缺字段")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// Proof 生成一枚 DPoP 证明（htm = HTTP 方法，htu = 完整请求 URL）。
//
// jti 取 32 字节随机数的 hex（协议 §2.2：`jti` = 32 字节 hex；DPoP 规范只要求唯一，
// 服务端不解析其结构）。
func (k *DPoPKey) Proof(htm, htu string, now time.Time) (string, error) {
	if k == nil || k.priv == nil {
		return "", fmt.Errorf("codearts: DPoP 密钥为空")
	}
	jtiRaw := make([]byte, 32)
	if _, err := rand.Read(jtiRaw); err != nil {
		return "", fmt.Errorf("codearts: 生成 DPoP jti 失败: %w", err)
	}
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": dpopAlg,
		"jwk": k.publicJWK(),
	}
	payload := map[string]any{
		"htm": strings.ToUpper(strings.TrimSpace(htm)),
		"htu": htu,
		"iat": now.Unix(),
		"jti": hex.EncodeToString(jtiRaw),
	}
	hRaw, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pRaw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hRaw) + "." + b64url(pRaw)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", fmt.Errorf("codearts: DPoP 签名失败: %w", err)
	}
	sig := append(pad32(r), pad32(s)...)
	return signingInput + "." + b64url(sig), nil
}
