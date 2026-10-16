package officeace

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"time"
)

// DPoP proof JWT（ES256）——华为 STS 换令牌/续期时随请求头 DPoP 发送。
// 私钥只留本地，JWT 头只带公钥 jwk（不含 d）。签名为 raw R||S 各 32 字节（共 64），
// 不是 ASN.1 DER —— Go 的 ecdsa.SignASN1 不可直接用。

// DPoPKey 一对 P-256 密钥的 JWK 表示。
type DPoPKey struct {
	Private *ecdsa.PrivateKey
}

// NewDPoPKey 生成新的 P-256 DPoP 密钥。
func NewDPoPKey() (*DPoPKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &DPoPKey{Private: k}, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64urlDecode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func (k *DPoPKey) coordBytes(v *big.Int) []byte {
	b := make([]byte, 32)
	v.FillBytes(b)
	return b
}

// publicJWK 返回只含公钥的 JWK 映射。
func (k *DPoPKey) publicJWK() map[string]string {
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   b64url(k.coordBytes(k.Private.X)),
		"y":   b64url(k.coordBytes(k.Private.Y)),
	}
}

// privateJWK 返回含私钥 d 的 JWK（用于落盘）。
func (k *DPoPKey) privateJWK() map[string]string {
	m := k.publicJWK()
	m["d"] = b64url(k.coordBytes(k.Private.D))
	return m
}

// DPoPProof 生成 htm/htu 对应的 DPoP proof JWT。
func (k *DPoPKey) DPoPProof(htm, htu string, now time.Time) (string, error) {
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": k.publicJWK(),
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	payload := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": now.Unix(),
		"jti": fmt.Sprintf("%x", jti),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(pb)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64url(sig), nil
}

// dpopKeyFromJWK 从落盘的私钥 JWK 还原 DPoP 密钥。
func dpopKeyFromJWK(jwk map[string]string) (*DPoPKey, error) {
	xb, err := b64urlDecode(jwk["x"])
	if err != nil {
		return nil, err
	}
	yb, err := b64urlDecode(jwk["y"])
	if err != nil {
		return nil, err
	}
	db, err := b64urlDecode(jwk["d"])
	if err != nil {
		return nil, err
	}
	priv := new(ecdsa.PrivateKey)
	priv.PublicKey.Curve = elliptic.P256()
	priv.PublicKey.X = new(big.Int).SetBytes(xb)
	priv.PublicKey.Y = new(big.Int).SetBytes(yb)
	priv.D = new(big.Int).SetBytes(db)
	return &DPoPKey{Private: priv}, nil
}
