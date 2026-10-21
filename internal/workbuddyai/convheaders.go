// convheaders.go 会话头族（X-Conversation-*）的取值生成：国际版（www.workbuddy.ai）自持一份实现。
//
// 本仓渠道包之间不共享实现（R58 / R111 的既定纪律），WorkBuddy 两区虽同一族协议、
// 同一套语义，也各写一份——某一区要单独调形态（如上游最低版本口径差异）时，改动
// 不会牵连另一区。
//
// 语义（官方 5.8.4 抓包）：
//
//	X-Conversation-ID          会话级：同一次会话恒定（官方 = 会话的 session.id）
//	X-Conversation-Request-ID  轮级：一次 user send 一个值，工具回调/重试复用
//	X-Conversation-Message-ID  调用级：每次模型请求新值（= X-Request-ID）
//	X-Root-Request-ID          = 本轮的 X-Conversation-Request-ID
//
// 形态：会话 id 是带连字符的 UUID（v4/v7 均被上游接受，版本位不参与校验），其余三个
// 是 32 位无连字符 hex。上游对取值形态无校验（R71 实测 UUIDv4 / 32hex / 裸串都通过），
// 但「每请求现生成」会把一次多轮对话拆成上千个一次性会话，且 X-Conversation-Request-ID
// 是上游「一次 user send = 一次请求」的计数键（workbuddy2api-hub#381），故按会话稳定派生。
package workbuddyai

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// maxAnchorBody 超限不做锚点抽取：这种体积几乎都是内联图片，解析成本不值，
// 退回「每请求随机」的旧行为即可（缺锚点不影响上游校验，只影响像不像真人）。
const maxAnchorBody = 8 << 20

// conversationAnchors 从 OpenAI 形状的 chat 请求体里抽两个锚点：
//
//	conv = 首条 user 消息文本的指纹 —— 同一会话的多轮对话里恒定
//	turn = 末条 user 消息文本的指纹 —— 一次 user send 内恒定（工具回调追加的是
//	       assistant/tool 消息，末条 user 仍在，故一轮多步复用同一个 turn）
//
// 取不到（无 messages / 解析失败 / 无文本 / body 超限）时返回空串，调用方退回随机。
// 锚点只在本地参与哈希，原文不上送、不落盘。
func conversationAnchors(body []byte) (conv, turn string) {
	if len(body) == 0 || len(body) > maxAnchorBody {
		return "", ""
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return "", ""
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		text := convContentText(m.Content)
		if text == "" {
			continue
		}
		if conv == "" {
			conv = convFingerprint(text)
		}
		turn = convFingerprint(text)
	}
	if conv == "" {
		// 无 user 消息（少见的单轮 system/assistant 形态）：退化为首条消息当锚点。
		if text := convContentText(req.Messages[0].Content); text != "" {
			conv, turn = convFingerprint(text), convFingerprint(text)
		}
	}
	return conv, turn
}

// convFingerprint 锚点文本 → 16 位 hex（sha256 前 8 字节）。截断只为短，碰撞概率可忽略。
func convFingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// convContentText 提取消息文本：content 可能是裸字符串，也可能是分片数组
// （多模态 / 工具结果）。图片等非文本分片不参与锚点。
func convContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := ""
	for _, p := range parts {
		out += p.Text
	}
	return out
}

// conversationID 会话级 id：按「账号 + 会话锚点」稳定派生带连字符的 UUIDv4。
//
// 掺 uid 是刻意的：同一段对话被多个账号服务时不能共用同一个 id——那才是把会话
// 关联起来的键（hub 的注释同口径）。锚点为空时退回随机 v4（与旧行为等价）。
func conversationID(uid, anchor string) string {
	if anchor == "" {
		return randomUUIDv4()
	}
	sum := sha256.Sum256([]byte("wb-conv\x00" + uid + "\x00" + anchor))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

// turnIDOf 轮级 id：32 位 hex，按「账号 + 会话 + 本轮锚点」稳定派生。
// 重试、换号、工具回调在同一次 user send 内复用同一个值（上游按它计数一次 send）。
func turnIDOf(uid, conv, anchor string) string {
	if anchor == "" {
		return newMessageID()
	}
	sum := sha256.Sum256([]byte("wb-turn\x00" + uid + "\x00" + conv + "\x00" + anchor))
	return hex.EncodeToString(sum[:16])
}

// newMessageID 调用级 id：32 位 hex，每次上游请求新生成（crypto/rand）。
func newMessageID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand 失败时退回时间戳派生，保持 32 hex 形态。
		n := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(n >> (i * 8))
			b[15-i] = byte(n >> ((7 - i) * 8))
		}
	}
	return hex.EncodeToString(b[:])
}

func randomUUIDv4() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		n := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(n >> (i * 8))
			b[15-i] = byte(n >> ((7 - i) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hexdigits[v>>4]
		out[j+1] = hexdigits[v&0x0f]
		j += 2
	}
	return string(out)
}
