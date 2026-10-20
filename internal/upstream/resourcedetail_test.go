package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// TestUserResourceDetailCarriesExpiryAndUsable 明细须带到期日与可用标记。
// 到期判据是 CycleEndTime（上游不下发 PackageEndTime），且按 UTC+8 墙钟解释。
func TestUserResourceDetailCarriesExpiryAndUsable(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"周期包","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200,"CycleEndTime":"2026-09-30 23:59:59"},
			{"PackageName":"无到期包","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0,"CycleEndTime":""}
		]}}}}`), nil
	})
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 400 {
		t.Errorf("remain=%d want 400", remain)
	}
	if len(items) != 2 {
		t.Fatalf("items=%d want 2", len(items))
	}
	if items[0].ExpireAt != "2026-09-30T23:59:59+08:00" {
		t.Errorf("ExpireAt=%q want 2026-09-30T23:59:59+08:00（保留时分秒，供剩余小时分桶）", items[0].ExpireAt)
	}
	if items[1].ExpireAt != "" {
		t.Errorf("无到期包 ExpireAt=%q want 空串", items[1].ExpireAt)
	}
	for _, it := range items {
		if !it.Usable {
			t.Errorf("%s 国内版无端点分区，应标记可用", it.Name)
		}
	}
}

// TestUserResourceDetailMatchesUserResource 两条路径必须同口径，
// 否则面板显示与 pool 选号依据会不一致。
func TestUserResourceDetailMatchesUserResource(t *testing.T) {
	body := `{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageName":"a","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200},
		{"PackageName":"b","CapacitySize":1000,"CapacityRemain":700,"CapacityUsed":300},
		{"PackageName":"c","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
	]}}}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, body), nil
	})
	a := &auth.Auth{AccessToken: "at"}
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	detailRemain, items, err := c.UserResourceDetail(a)
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != detailRemain {
		t.Errorf("UserResource=%d 与 UserResourceDetail=%d 口径不一致", remain, detailRemain)
	}
	var sum int64
	for _, it := range items {
		sum += it.Remain
	}
	if sum != remain {
		t.Errorf("明细剩余合计=%d 与 remain=%d 不一致", sum, remain)
	}
	// 负剩余条目必须钳 0，且不出现在合计里
	if items[2].Remain != 0 {
		t.Errorf("脏数据剩余=%d want 0（钳负）", items[2].Remain)
	}
}

// TestExpireAtRejectsMalformed 不可解析的时间串不得被当成有效到期日。
//
// 返回值是 **RFC3339 精确时刻**（旧版返回 "YYYY-MM-DD"）：面板按「N 小时内到期」
// 分桶需要时分秒，否则今晚失效的包会被误报成「明日到期」（2026-10-19 实测）。
// 同时验证：纯日期串 "2026-09-30" 不在可解析列表里（上游不会下发纯日期，
// 混进来说明形状变了，宁可当无到期也不猜）。
func TestExpireAtRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"   ":                 "",
		"2026-09-30":          "",
		"not-a-time":          "",
		"2026-09-30 23:59:59": "2026-09-30T23:59:59+08:00",
		"2026-01-01 00:00:00": "2026-01-01T00:00:00+08:00",
	}
	for in, want := range cases {
		r := resourceAccount{CycleEndTime: in}
		if got := r.expireAt(); got != want {
			t.Errorf("expireAt(%q)=%q want %q", in, got, want)
		}
	}
}

// TestExpireDayIsDateOnlyForLedgerKey 守门：伪键必须只含日期，不含时刻。
//
// 为什么这条必须存在：ExpireAt 从日期改成 RFC3339 后，如果伪键跟着变，
// 升级瞬间每个包会产出一对 expire+earn 假流水（workbuddy 单账号 28 个包
// ⇒ 56 条，把「今日作废」刷成虚高）。expireDay() 就是为这个而存在的。
func TestExpireDayIsDateOnlyForLedgerKey(t *testing.T) {
	r := resourceAccount{CycleEndTime: "2026-09-30 23:59:59"}
	if got := r.expireDay(); got != "2026-09-30" {
		t.Fatalf("expireDay()=%q want 2026-09-30（伪键不得含时刻）", got)
	}
	if got := r.expireAt(); got == r.expireDay() {
		t.Fatalf("expireAt()=%q 与 expireDay() 相同，时刻信息丢了", got)
	}
	// 空值：两者都空，不得造出 "|" 之类的畸形键片段
	r2 := resourceAccount{CycleEndTime: ""}
	if r2.expireDay() != "" || r2.expireAt() != "" {
		t.Fatalf("空 CycleEndTime: expireDay=%q expireAt=%q", r2.expireDay(), r2.expireAt())
	}
}

// TestGetUserResourceSendsProductCode 请求体契约：必须带 ProductCode=p_tcaca。
func TestGetUserResourceSendsProductCode(t *testing.T) {
	var seen string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		seen = string(raw)
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})
	if _, err := c.UserResource(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if !strings.Contains(seen, `"ProductCode":"p_tcaca"`) {
		t.Errorf("请求体缺 ProductCode: %s", seen)
	}
}
