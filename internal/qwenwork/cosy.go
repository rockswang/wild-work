// cosy.go 千问办公 COSY 请求签名：RSA_PKCS1 包 16 字符 AES 会话密钥 +
// AES-128-CBC 加密身份（info）+ MD5 请求签名。与 internal/qoder/cosy.go 同构
// （同一把 RSA 公钥、同一签名串格式），差异仅在：
//   - info 明文为 5 字段 {uid,aid,name,email,security_oauth_token}（qoder 是 9 字段排序形）
//   - ideVersion/cosyVersion 取值不同（实测网关对版本号不校验）
//   - 不需要机器指纹（machineid/machinetoken 实测可省）
package qwenwork

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// serverPubKeyPEM Qoder 系共享 RSA 公钥（桌面客户端硬编码，与 qoder 渠道逐字节相同）。
const serverPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var serverPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(serverPubKeyPEM))
	if block == nil {
		panic("qwenwork: bad server pubkey PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("qwenwork: parse pubkey: " + err.Error())
	}
	serverPubKey = k.(*rsa.PublicKey)
}

// CosySession 单次请求的签名材料（每次对话/模型请求新建，成本低：一次 RSA + 一次 AES）。
type CosySession struct {
	TempKey []byte // 16 个 ASCII hex 字符（与桌面端 uuid.hex[:16] 对齐）
	CosyKey string // base64(RSA_PKCS1(tempKey))
	Info    string // base64(AES-128-CBC(key=iv=tempKey, userInfo))
}

// NewCosySession 构建签名材料。identity 内嵌 access token，token 刷新后必须重建。
func NewCosySession(uid, name, email, accessToken string) (*CosySession, error) {
	tempKey := []byte(hexShort(16)) // 16 个 ASCII hex 字符
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, tempKey)
	if err != nil {
		return nil, err
	}
	// info 明文：与官方 asar encryptUserInfo 一致的 5 字段紧凑 JSON
	// （顺序不重要——AES 密文服务端按 key 解，不做字节级比对）。
	infoPlain, _ := json.Marshal(map[string]string{
		"uid":                  uid,
		"aid":                  "",
		"name":                 name,
		"email":                email,
		"security_oauth_token": accessToken,
	})
	infoCipher, err := aesCBCEncrypt(infoPlain, tempKey)
	if err != nil {
		return nil, err
	}
	return &CosySession{
		TempKey: tempKey,
		CosyKey: base64.StdEncoding.EncodeToString(wrapped),
		Info:    base64.StdEncoding.EncodeToString(infoCipher),
	}, nil
}

// aesCBCEncrypt AES-128-CBC，key=iv=tempKey（16 字节），PKCS7 padding。
func aesCBCEncrypt(plain, tempKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(tempKey)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, tempKey).CryptBlocks(out, padded)
	return out, nil
}

// nowUnix 便于测试注入（签名时间戳与 Cosy-Date 头必须同源）。
var nowUnix = func() int64 { return time.Now().Unix() }

// AuthHeader 计算单次请求的 Authorization 头，并返回签名所用的时间戳。
// 签名串：base64(header)\n cosyKey \n ts \n body \n path（path 去 /algo 前缀）。
//
// 返回的 date 必须原样用于 Cosy-Date 头：上游拿它重算签名。签名串里含整个 body，
// 长会话（数 MB 上下文）时 md5 + 字符串拼接要耗掉毫秒级时间；若签名与头各自取一次
// 时间，两次取值可能跨秒 → 上游回 101 Signature invalid（长会话里偶发）。
func (s *CosySession) AuthHeader(body, rawURL string) (auth, date string, err error) {
	header := map[string]string{
		"version":     "v1",
		"requestId":   uuid4(),
		"info":        s.Info,
		"cosyVersion": "1.1.18",
		"ideVersion":  "0.1.8",
	}
	headerB64 := base64.StdEncoding.EncodeToString(mustJSON(header))

	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", "", perr
	}
	pathSig := strings.TrimPrefix(u.Path, "/algo")
	date = fmt.Sprintf("%d", nowUnix())
	sigInput := headerB64 + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + pathSig
	sum := md5.Sum([]byte(sigInput))
	return "Bearer COSY." + headerB64 + "." + hex.EncodeToString(sum[:]), date, nil
}

// ApplyHeadersWithUID 设置推理/模型请求的最小头集。
// 实测：Cosy-* 业务头、版本头、UA 全部不校验，可全部省略；
// 强校验的只有 Authorization / Cosy-Key / Cosy-User / Cosy-Date 四件套。
// 少传头可规避未来版本漂移（见备忘 §2.4 头容差矩阵）。
func (s *CosySession) ApplyHeadersWithUID(h map[string]string, body, rawURL, uid string) error {
	auth, date, err := s.AuthHeader(body, rawURL)
	if err != nil {
		return err
	}
	h["Authorization"] = auth
	h["Cosy-Key"] = s.CosyKey
	h["Cosy-User"] = uid
	h["Cosy-Date"] = date
	h["Content-Type"] = "application/json"
	h["Accept"] = "text/event-stream"
	return nil
}

// mustJSON 紧凑序列化（map 遍历顺序无关：签名串是自包含的，服务端按收到值校验）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// uuid4 简单 UUIDv4。
func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hexShort 生成 n 字符随机 hex（tempKey 用，对齐桌面端 uuid.hex[:16]）。
func hexShort(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
