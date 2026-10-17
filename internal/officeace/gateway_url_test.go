package officeace

import (
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// 网关 base 的字段语义锁定（issue #152）：
//
// 同一个 `model_api_url_base` 被两类端点复用，而它们的前缀要求**相反**——
//   - 模型/对话：{host}/v2/chat/completions、{host}/v2/models（带 /v2）
//   - 额度/签到：{host}/v1/subscription（不带 /v2）
//
// 上游在不同账号/不同网关版本下**可能下发带 /v2 也可能不带**（issue #152 现场是裸 host，
// 而仓库里早先的样本带 /v2）。故：存盘一律存裸 host（bareGateway 剥 /v2），
// 各端点按需自行拼前缀。本文件锁住「两种输入 → 同一套正确 URL」这个不变量。
func TestGatewayURLPrefixContract(t *testing.T) {
	const host = "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com"
	inputs := []string{host, host + "/v2", host + "/v2/", host + "/"}
	for _, in := range inputs {
		ac := &Account{BaseURL: bareGateway(in)}
		if ac.BaseURL != host {
			t.Fatalf("bareGateway(%q) = %q，want %q（必须剥掉结尾 /v2 与斜杠）", in, ac.BaseURL, host)
		}
		// 模型/对话：带 /v2
		if got, want := ac.modelsURL(), host+"/v2/models"; got != want {
			t.Errorf("输入 %q → modelsURL() = %q，want %q（不带 /v2 会 APIG.0101 404）", in, got, want)
		}
		if got, want := ac.chatURL(), host+"/v2/chat/completions"; got != want {
			t.Errorf("输入 %q → chatURL() = %q，want %q（不带 /v2 会 APIG.0101 404）", in, got, want)
		}
		// 额度/签到：不得带 /v2（带了会 APIG.0303 app not found）
		c := &Client{}
		gw, err := c.gatewayBase(ac)
		if err != nil {
			t.Fatal(err)
		}
		if gw != host {
			t.Fatalf("输入 %q → gatewayBase() = %q，want %q", in, gw, host)
		}
		if u := trimSlash(gw) + "/v1/subscription"; strings.Contains(u, "/v2") {
			t.Errorf("输入 %q → 额度端点 %q 含 /v2（会 401 APIG.0303）", in, u)
		}
	}
}

// 旧凭据（Extra.base_url 带 /v2）读回来必须自动归一，用户无需重新登录。
func TestAccountFromAuthStripsV2(t *testing.T) {
	const host = "https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com"
	a := &auth.Auth{
		Kind:        "officeace",
		UID:         "acct:user",
		ApiHost:     host + "/v2",
		AccessToken: "Basic xxx",
		Extra:       map[string]string{"base_url": host + "/v2"},
	}
	ac := accountFromAuth(a)
	if ac.BaseURL != host {
		t.Fatalf("accountFromAuth 应把带 /v2 的旧凭据归一为裸 host，实际 %q", ac.BaseURL)
	}
	if got := ac.chatURL(); got != host+"/v2/chat/completions" {
		t.Fatalf("chatURL() = %q", got)
	}
}

// 只有 ApiHost（Extra 无 base_url）的老凭据同样要归一。
func TestAccountFromAuthApiHostFallback(t *testing.T) {
	const host = "https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com"
	ac := accountFromAuth(&auth.Auth{Kind: "officeace", UID: "a:u", ApiHost: host + "/v2/"})
	if ac.BaseURL != host {
		t.Fatalf("ApiHost 兜底路径未归一，实际 %q", ac.BaseURL)
	}
}
