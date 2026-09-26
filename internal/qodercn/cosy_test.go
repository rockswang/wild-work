package qodercn

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// TestCosyDateMatchesSignature 锁定签名契约：cosy-date 头必须就是签名串里使用的时间戳。
//
// 上游拿 cosy-date 重算签名。签名串里含整个 body，长会话（数 MB 上下文）时
// md5 + 字符串拼接要耗掉毫秒级时间；若签名与头各自取一次时间，两次取值可能跨秒，
// 使签名内的时间戳与头里的不一致 → 上游回
// {"code":"101","message":"Signature invalid"}（长会话里偶发，body 越大越频繁）。
//
// 用例注入「每次取值都前进 1 秒」的时钟，让这种错位必然发生：修复前签名取 t1、
// 头取 t2（≠t1），本用例即红。
func TestCosyDateMatchesSignature(t *testing.T) {
	old := nowUnix
	n := int64(1700000000)
	nowUnix = func() int64 { n++; return n }
	defer func() { nowUnix = old }()

	s := &CosySession{
		MachineID:    "mid",
		MachineToken: "mtok",
		MachineType:  "mtype",
		TempKey:      []byte("0123456789abcdef"),
		CosyKey:      "cosykey",
		Info:         "info",
	}
	const body = `{"hello":"world"}`
	const rawURL = "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result"
	const wantPath = "/api/v2/service/pro/sse/agent_chat_generation"

	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyHeaders(req, body, rawURL, "uid", "text/event-stream", false, ""); err != nil {
		t.Fatal(err)
	}
	// 注入时钟的第一次取值。修复前签名与头各自取 time.Now()，注入时钟根本不被使用，
	// 头会拿到真实时间（≠ 本值）；若改回 nowUnix() 却各取一次，头会拿到第二个值。
	// 两种情况都由本断言直接抓住。
	const wantDate = "1700000001"
	date := req.Header.Get("cosy-date")
	if date != wantDate {
		t.Fatalf("cosy-date=%s，应为签名所用的 %s（头另取时间源就会与签名跨秒不同源 → 上游回 101 Signature invalid）", date, wantDate)
	}
	auth := req.Header.Get("authorization")
	parts := strings.SplitN(auth, ".", 3)
	if len(parts) != 3 || parts[0] != "Bearer COSY" {
		t.Fatalf("authorization 格式非法: %q", auth)
	}
	// 与上游同法校验：用 cosy-date 复算签名，必须与 authorization 里的 sig 一致。
	sum := md5.Sum([]byte(parts[1] + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + wantPath))
	if got := hex.EncodeToString(sum[:]); got != parts[2] {
		t.Fatalf("cosy-date 与签名不同源（上游会回 101 Signature invalid）：用 cosy-date=%s 复算得 %s，authorization 里是 %s", date, got, parts[2])
	}
}
