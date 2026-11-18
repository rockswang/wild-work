package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// TestRequestLevelErrorIsLogged 请求级 4xx（内容拦截 / 上下文超限 / 图片无效 /
// 出站 body 不合格 / 上游请求级拒绝）**必须留日志**。
//
// 2026-11-17 实测事故：小浣熊（raccoon）上游按 content_filter 拒绝请求时，
// 网关侧 data/app.log 全量零命中——请求确实失败了，但没有任何痕迹，只能靠
// 反查客户端会话记录才定住「这个 400 是上游报的还是网关报的」。
//
// 本用例锁死这条路径的**可观测性**：必须写出 platform / uid / model / status /
// kind / 上游原文摘要，并标注「不罚号、不轮换」以免运维误判账号被惩罚。
//
// 只断言「有日志」，不断言轮转行为——后者由 TestContentErrorDoesNotRotate 负责，
// 两者互补：本条治可观测性，那条治轮转语义。
func TestRequestLevelErrorIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOut)

	up := &kindedUpstream{
		status: http.StatusBadRequest,
		kind:   provider.ErrContentBlocked,
		body:   `{"code":"content_filter","message":"Inappropriate input/output rejected for security reasons","type":"invalid_request_error"}`,
	}
	h := newRotateTestHandler(t, up, "log")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400 原文透传", rec.Code)
	}

	got := buf.String()
	if !strings.Contains(got, "upstream error") {
		t.Fatalf("请求级 4xx 必须写出 upstream error 日志，实际日志:\n%s", got)
	}
	// 关键字段：没有 status 就无法分辨 400/413/422；没有 kind 就看不出是审核拦截
	// 还是上下文超限；没有 body 摘要就看不到上游原始错误码（如 content_filter）。
	for _, want := range []string{"status=400", "kind=", "content_filter", "platform=", "uid="} {
		if !strings.Contains(got, want) {
			t.Errorf("日志缺少关键字段 %q，实际日志:\n%s", want, got)
		}
	}
	// 必须明确标注「不罚号/不轮换」，否则运维会误以为账号被惩罚了。
	if !strings.Contains(got, "不罚号") || !strings.Contains(got, "不轮换") {
		t.Errorf("日志应标注「不罚号、不轮换」语义，实际日志:\n%s", got)
	}
}

// TestRequestLevelErrorLogCoversAllKinds 同一分支上的五种请求级错误都必须被记录，
// 防止以后有人只给 ErrContentBlocked 单独加日志、其余分支继续静默。
func TestRequestLevelErrorLogCoversAllKinds(t *testing.T) {
	kinds := []provider.ErrKind{
		provider.ErrContentBlocked,
		provider.ErrPromptTooLong,
		provider.ErrImageInvalid,
		provider.ErrBadParams,
		provider.ErrPassthrough,
	}
	for _, k := range kinds {
		var buf bytes.Buffer
		prevOut := log.Writer()
		log.SetOutput(&buf)

		up := &kindedUpstream{status: http.StatusBadRequest, kind: k, body: `{"error":"x"}`}
		h := newRotateTestHandler(t, up, "k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
		got := buf.String()
		log.SetOutput(prevOut)

		if !strings.Contains(got, "upstream error") || !strings.Contains(got, "status=400") {
			t.Errorf("%v 分支缺少 upstream error 日志，实际日志:\n%s", k, got)
		}
	}
}
