// cosy.go 实现 QoderCN 的 COSY 请求签名：RSA 包裹 AES 会话密钥 +
// AES-128-CBC 加密身份 + MD5 请求签名。
// 代码级复制自 internal/qoder/cosy.go 后按 qoder2api 参数改造：
//   - cosyVersion: 0.1.43 → 1.0.10（qoder2api internal/cosy/session.go Version）
//   - 头集合改为 qoder2api bridge/client.go buildHeaders 的 18 头形态：
//     多 cosy-scene/cosy-business-product/cosy-business-type/login-version，
//     去 cosy-clientip；data-policy 小写 agree；accept 由调用方指定
//   - identity 的 user_type 由调用方传实测值（来自 /api/v1/userinfo）
package qodercn

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
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// cosyVersion COSY 协议版本（qoder2api internal/cosy/session.go Version）。
const cosyVersion = "1.0.10"

// serverPubKeyPEM Qoder RSA 公钥（桌面客户端硬编码，与 qoderwork 渠道同钥）。
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
		panic("bad server pubkey PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	serverPubKey = k.(*rsa.PublicKey)
}

// CosySession 每账号的签名状态。
type CosySession struct {
	MachineID    string
	MachineToken string
	MachineType  string
	TempKey      []byte // 16 字节
	CosyKey      string // base64(RSA(tempKey))
	Info         string // base64(AES-CBC(identity))
}

// NewCosySession 用持久机器指纹 + 当前 dt/drt 构建签名会话。
// dt/drt 变化（refresh）时需要重建（identity 内嵌这两个 token）。
// tempKey 是 16 个 ASCII 字符（与桌面端一致），不是随机字节。
// userType 为账号实测档位（/api/v1/userinfo 的 userType，缺省 personal_standard）。
func NewCosySession(machineID, machineToken, machineType, nickname, uid, dt, drt, userType string) (*CosySession, error) {
	if machineID == "" || machineToken == "" || machineType == "" {
		return nil, fmt.Errorf("missing machine fingerprint (need MachineID/Token/Type)")
	}
	if userType == "" {
		userType = "personal_standard" // qoder2api StrValDefault(userInfo, "userType", "personal_standard")
	}
	tempKey := []byte(hexShort(16)) // 16 ASCII chars, AES-128 key
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(wrapped)
	identity := map[string]string{
		"name":                 nickname,
		"aid":                  uid,
		"uid":                  uid,
		"yx_uid":               "",
		"organization_id":      "",
		"organization_name":    "",
		"user_type":            userType,
		"security_oauth_token": dt,
		"refresh_token":        drt,
	}
	infoPlain := jsonSortedCompact(identity)
	infoCipher, err := aesCBCEncrypt(infoPlain, tempKey)
	if err != nil {
		return nil, err
	}
	return &CosySession{
		MachineID:    machineID,
		MachineToken: machineToken,
		MachineType:  machineType,
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         base64.StdEncoding.EncodeToString(infoCipher),
	}, nil
}

// jsonSortedCompact 按 key 排序、无空白序列化。
func jsonSortedCompact(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String())
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

// nowUnix 便于测试注入（签名时间戳与 cosy-date 头必须同源）。
var nowUnix = func() int64 { return time.Now().Unix() }

// AuthHeader 计算单次请求的 Authorization 头，并返回签名所用的时间戳。
//
// 返回的 date 必须原样用于 cosy-date 头：上游拿 cosy-date 重算签名。签名串里含
// 整个 body，长会话（数 MB 上下文）时 md5 + 字符串拼接要耗掉毫秒级时间；若签名与
// 头各自取一次时间，两次取值可能跨秒，使签名内的时间戳与头里的不一致 → 上游回
// {"code":"101","message":"Signature invalid"}（长会话里偶发，body 越大越频繁）。
func (s *CosySession) AuthHeader(body, rawURL, uid string) (auth, date string, err error) {
	payload := map[string]string{
		"cosyVersion": cosyVersion,
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   uuid4(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(jsonSortedCompact(payload))

	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", "", perr
	}
	pathSig := strings.TrimPrefix(u.Path, "/algo")
	date = fmt.Sprintf("%d", nowUnix())
	sigInput := payloadB64 + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + pathSig
	sum := md5.Sum([]byte(sigInput))
	sig := hex.EncodeToString(sum[:])
	return "Bearer COSY." + payloadB64 + "." + sig, date, nil
}

// ApplyHeaders 把 qoder2api 形态的 COSY 头全部设置到 req。
// accept 由调用方传入（GET 查询 application/json；SSE 流 text/event-stream）；
// sse 仅控制 cache-control。extra 允许追加头（签到路径的 origin 等）。
func (s *CosySession) ApplyHeaders(req *http.Request, body, rawURL, uid, accept string, sse bool, modelKey string) error {
	auth, date, err := s.AuthHeader(body, rawURL, uid)
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("cosy-data-policy", "agree") // qoder2api 小写
	h.Set("content-type", "application/json")
	h.Set("cosy-machinetype", s.MachineType)
	h.Set("cosy-clienttype", "5")
	h.Set("cosy-date", date)
	h.Set("cosy-user", uid)
	h.Set("cosy-key", s.CosyKey)
	h.Set("cache-control", "no-cache")
	h.Set("accept", accept)
	h.Set("authorization", auth)
	h.Set("cosy-version", cosyVersion)
	h.Set("cosy-machineid", s.MachineID)
	h.Set("cosy-machinetoken", s.MachineToken)
	h.Set("login-version", "v2")
	h.Set("user-agent", clientUA)
	// qoder2api 新增的三条业务头（bridge/client.go buildHeaders）
	h.Set("cosy-scene", "assistant")
	h.Set("cosy-business-product", "ide")
	h.Set("cosy-business-type", "agent")
	if modelKey != "" {
		h.Set("x-model-key", modelKey)
		h.Set("x-model-source", "system")
	}
	return nil
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

// hexShort 生成 n 字符的随机 hex（用于 tempKey，与桌面端 uuid.hex[:16] 对齐）。
func hexShort(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
