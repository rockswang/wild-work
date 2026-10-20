package traework

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// entUsageBody 构造一份 web_user_ent_usage 响应：
//   - product_id=221 每月登录（可用，500 限额已用 40）
//   - product_id=208 每日签到 150 档（可用，150 限额未用）
//   - product_id=209 每日签到 200 档（可用：2026-09-26 起升级为通用积分，issue #44）
//   - ep=1 每日签到（不可用：available_endpoint 历史兜底判据）
//   - 限额 0 的免费包（应被明细过滤掉）
//
// expire_time 用固定值便于断言日期格式化。
const entUsageBody = `{"is_credits_billing":true,"user_entitlement_pack_list":[
 {"display_desc":"每月登录赠送","group_name":"每月登录积分","group_type":3,"expire_time":1790783999,
  "entitlement_base_info":{"available_endpoint":0,"product_id":221,"quota":{"credits_limit":500}},
  "usage":{"credits_amount":40}},
 {"display_desc":"签到奖励","group_name":"每日签到","group_type":1,"expire_time":1789779374,
  "entitlement_base_info":{"available_endpoint":0,"product_id":208,"quota":{"credits_limit":150}},
  "usage":{}},
 {"display_desc":"签到奖励","group_name":"每日签到","group_type":1,"expire_time":1789779374,
  "entitlement_base_info":{"available_endpoint":0,"product_id":209,"quota":{"credits_limit":200}},
  "usage":{}},
 {"display_desc":"签到奖励","group_name":"每日签到","group_type":1,"expire_time":1789779374,
  "entitlement_base_info":{"available_endpoint":1,"product_id":208,"quota":{"credits_limit":300}},
  "usage":{}},
 {"display_desc":"免费","group_type":0,"expire_time":1790783999,
  "entitlement_base_info":{"available_endpoint":0,"product_id":0,"quota":{"credits_limit":0}},
  "usage":{}}
]}`

func entUsageClient(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpEntUsage {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	return c
}

// TestUserResourceExcludesOfficialClientPool 可消耗余额只算 usable()：
// ep=1 必须排除，否则 pool 按虚高余额选号；pid=209 已升级为通用积分，计入（issue #44）。
func TestUserResourceExcludesOfficialClientPool(t *testing.T) {
	c := entUsageClient(t, entUsageBody)
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	// 460 (每月登录) + 150 (pid=208 签到) + 200 (pid=209 签到，已通用) = 810；不含 ep=1 的 300。
	if remain != 810 {
		t.Errorf("remain=%d want 810 (仅 ep=1 不应计入)", remain)
	}
}

// TestUserResourceDetailMarksUsableAndExpiry 明细须带可用性标记与到期日。
func TestUserResourceDetailMarksUsableAndExpiry(t *testing.T) {
	c := entUsageClient(t, entUsageBody)
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 810 {
		t.Errorf("remain=%d want 810", remain)
	}
	// 限额 0 的免费包应被过滤，剩 4 条
	if len(items) != 4 {
		t.Fatalf("items=%d want 4（限额 0 的包应被过滤）: %+v", len(items), items)
	}

	usable, unusable := provider.Summarize(items)
	if usable != 810 || unusable != 300 {
		t.Errorf("usable=%d unusable=%d, want 810/300", usable, unusable)
	}

	byName := map[string][]item{}
	for _, it := range items {
		byName[it.Name] = append(byName[it.Name], item{it.Remain, it.ExpireAt, it.Usable})
	}
	month := byName["每月登录积分"]
	if len(month) != 1 || month[0].remain != 460 || !month[0].usable {
		t.Errorf("每月登录积分应为 460 且可用: %+v", month)
	}
	if month[0].expire != "2026-09-30T23:59:59+08:00" {
		t.Errorf("每月登录积分到期时刻=%q want 2026-09-30T23:59:59+08:00（保留时分秒）", month[0].expire)
	}
	// 同名「每日签到」按判据区分可用性：208(可用150) / 209(可用200，已通用) / ep=1(不可用300)
	checkins := byName["每日签到"]
	if len(checkins) != 3 {
		t.Fatalf("每日签到应有 3 条: %+v", checkins)
	}
	var avail150, avail209, unavailEp1 bool
	for _, ci := range checkins {
		if ci.usable && ci.remain == 150 {
			avail150 = true
		}
		if ci.usable && ci.remain == 200 {
			avail209 = true
		}
		if !ci.usable && ci.remain == 300 {
			unavailEp1 = true
		}
	}
	if !avail150 || !avail209 || !unavailEp1 {
		t.Errorf("每日签到应为可用150/可用200/不可用ep1: %+v", checkins)
	}
}

// TestEntPackageUsable 直接钉住可用性判据：仅 ep=1 不可用（issue #44 撤回 pid==209 判据）。
func TestEntPackageUsable(t *testing.T) {
	cases := []struct {
		name string
		ep   int
		pid  int
		want bool
	}{
		{"每月登录221", 0, 221, true},
		{"150签到208", 0, 208, true},
		{"200签到209已通用", 0, 209, true},
		{"ep1历史兜底", 1, 208, false},
		{"免费订阅0", 0, 0, true},
	}
	for _, c := range cases {
		p := entPackage{}
		p.EntitlementBaseInfo.AvailableEndpoint = c.ep
		p.EntitlementBaseInfo.ProductID = c.pid
		if got := p.usable(); got != c.want {
			t.Errorf("%s usable()=%v want %v", c.name, got, c.want)
		}
	}
}

// item 测试内使用的明细投影。
type item struct {
	remain int64
	expire string
	usable bool
}

// TestUserResourceDetailNoExpiryWhenUpstreamOmits 上游未下发 expire_time 时，
// ExpireAt 必须为空串——前端据此隐藏有效期列，不得用零值冒充「永不过期」。
func TestUserResourceDetailNoExpiryWhenUpstreamOmits(t *testing.T) {
	c := entUsageClient(t, `{"user_entitlement_pack_list":[
	 {"group_name":"无到期包","group_type":4,
	  "entitlement_base_info":{"available_endpoint":0,"product_id":221,"quota":{"credits_limit":100}},
	  "usage":{"credits_amount":10}}
	]}`)
	_, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	if items[0].ExpireAt != "" {
		t.Errorf("ExpireAt=%q want 空串（上游未下发）", items[0].ExpireAt)
	}
	if items[0].Remain != 90 {
		t.Errorf("Remain=%d want 90", items[0].Remain)
	}
}

// TestPackRemainClampsNegative 上游脏数据（used > limit）剩余应钳 0，不得为负。
func TestPackRemainClampsNegative(t *testing.T) {
	p := entPackage{}
	p.EntitlementBaseInfo.Quota.CreditsLimit = 100
	p.Usage.CreditsAmount = 150
	if got := packRemain(p); got != 0 {
		t.Errorf("packRemain=%d want 0 (钳负)", got)
	}
}
