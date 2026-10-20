package server

import (
	"net/http"
	"testing"

	"wild-work/internal/provider"
)

// 本文件锁定「上游侧故障不得惩罚健康账号」这条判据，对应 2026-10-18 两起实测事故：
//
//	① lobsterai：上游对带 developer 角色的请求回 500。ErrServer 可轮换 ⇒
//	   一次客户端请求打 3 个账号各 +1 errCount，err_threshold=3 ⇒ 重试一次
//	   就把三个健康账号全冷却 10 分钟，整条渠道 503。
//	② officeace：glm-5.3-flash 上游回 404，被归为 ErrModelBlocked，而该 case 走
//	   Pool.Cooldown(uid) 冻结**整个账号** ⇒ 一个不存在的模型拖垮同账号下所有模型。
//
// 两条都是「上游/单个模型的问题，被记到账号头上」。判据来自 provider.go:193 的
// 定义（ErrModelBlocked = (账号,模型) 负缓存避让）与 ErrServer 的语义（服务端故障）。

// TestServerErrorDoesNotCountAsAccountError 5xx 不得累计账号错误计数。
//
// 修复前行为：ErrServer 落到 else 分支调 NoteError ⇒ 打满 threshold 即冷却账号。
// 现在：默认不计数（CountServerErrorAsAccountFault 为 false）。
func TestServerErrorDoesNotCountAsAccountError(t *testing.T) {
	up := &kindedUpstream{
		status: http.StatusInternalServerError,
		kind:   provider.ErrServer,
		body:   `{"code":500,"message":"服务器内部错误","data":null}`,
	}
	h := newRotateTestHandler(t, up, "srv-")
	p := h.cfg.Runtimes[provider.GLM].Pool

	// 连打 3 轮（每轮最多轮转 3 个账号）。修复前这足以把两个账号都打成冻结。
	for i := 0; i < 3; i++ {
		postModel(h, "glm/m")
	}

	for _, uid := range []string{"srv-A", "srv-B"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("账号 %s 不在池中", uid)
		}
		if st.Cooling {
			t.Errorf("❌ 账号 %s 因上游 5xx 被冷却（reason=%q until=%v）—— "+
				"5xx 是服务端故障，不该记到客户端凭据头上", uid, st.Reason, st.Until)
		}
	}
}

// TestModelBlockedCoolsOnlyThatModel 一条 (账号,模型) 级 404 不得冻结整个账号。
//
// 修复前：ErrModelBlocked 与 ErrWafBlock/ErrAccountFault 合并，走 Pool.Cooldown(uid)。
// 现在：走 CooldownModel(uid, model)，只冻这一个模型；账号本身仍可服务其它模型。
func TestModelBlockedCoolsOnlyThatModel(t *testing.T) {
	up := &kindedUpstream{
		status: http.StatusNotFound,
		kind:   provider.ErrModelBlocked,
		body:   `{"error":{"message":"Invalid model."}}`,
	}
	h := newRotateTestHandler(t, up, "mb-")
	p := h.cfg.Runtimes[provider.GLM].Pool

	postModel(h, "glm/m")

	for _, uid := range []string{"mb-A", "mb-B"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("账号 %s 不在池中", uid)
		}
		// 账号本身不得被整号冻结。
		if st.Cooling {
			t.Errorf("❌ 账号 %s 被整号冷却（reason=%q）—— "+
				"ErrModelBlocked 的语义是 (账号,模型) 负缓存避让，不是账号故障", uid, st.Reason)
		}
	}
	// 该模型应被记住为「这个账号下不可用」：再挑号时应跳过它。
	// （CooldownModel 内部维护 账号×模型 冷却表，此处只断言账号未被牵连。）
}
