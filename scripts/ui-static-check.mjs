// ui-static-check.mjs —— 前端静态验证：把假 state 喂给 app.js 的渲染函数，断言 HTML 输出。
//
// 为什么这样做（而不是起真实例）：
//   - 仓库无 JS 测试设施、也无浏览器环境；起真实例会连上游（签到/保活有副作用）；
//   - 我们只关心「函数拿到 state 会不会渲染出该有的 DOM」，纯函数级验证足够；
//   - app.js 顶层有 `let state = null`，故用「源码改写注入」把假 state 塞进去。
//
// 用法：node ui-static-check.mjs
import { readFileSync } from "node:fs";

const raw = readFileSync(new URL("../cmd/wild-work/web/app.js", import.meta.url), "utf8");

// 把 `let state = null;` 改成假数据，并暴露待测函数（app.js 是脚本，函数是顶层声明）。
const FAKE_STATE = {
  version: "2.5.5",
  accounts: [
    {
      uid: "1606848467182378",
      group: "traework",
      nickname: "中招号",
      credits: 2050,
      expiring_credits: 0,
      unusable_credits: 0,
      cooling: true,
      until: "10-03 09:12",
      until_ts: Math.floor(Date.now() / 1000) + 12 * 3600, // 24h 制歧义修复：带相对时长
      reason: "stream business error: solo error code=3004 …",
      disabled: false,
      last_checkin_at: "2026-10-02 09:00",
      last_checkin_ok: true,
    },
    {
      uid: "4298452267967801",
      group: "traework",
      nickname: "健康号",
      credits: 2686,
      expiring_credits: 100,
      unusable_credits: 0,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "2026-10-02 09:00",
      last_checkin_ok: true,
    },
    // OfficeAce 假数据（场景 1.5）：两张卡片只差「凭据是否缺 refresh_token」这一个字段，
    // 用来验证徽标**由该字段驱动**（而不是恒亮），否则徽标本身就是噪声。
    {
      uid: "01a149d8913679e5ba6de6275047897f:01a149d8913672db88d7a95fb3382d4d",
      group: "officeace",
      nickname: "缺凭据号",
      credits: 0,
      expiring_credits: 0,
      unusable_credits: 0,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "",
      last_checkin_ok: false,
      last_checkin_msg: "无临时凭据，需先续期/登录",
      refresh_token_missing: true,
      refresh_token_missing_reason: "缺 refresh_token：积分/签到不可用（测试用文案）",
    },
    // #134「登录态失效」假数据：2026-10-18 独立复核指出这条路**零覆盖**——
    // 把 needRelogin 里的 session_dead 去掉、或整块删掉 sessionDeadTag，UI 检查仍「全部通过」。
    // 故这里补一张 session_dead 卡片，锁住原行为（红徽标 + ⟳），与 officeace 的琥珀徽标并存。
    {
      uid: "qwenwork-session-dead-1",
      group: "qwenwork",
      nickname: "失效号",
      credits: 0,
      expiring_credits: 0,
      unusable_credits: 0,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "",
      last_checkin_ok: false,
      session_dead: true,
      session_dead_reason: "登录态已失效（测试用文案）",
    },
    {
      uid: "01a13f3d3ee6741d832a05b19fb0c028:01a13f3d3ee67da097f2cae2d1415761",
      group: "officeace",
      nickname: "正常号",
      credits: 5500,
      expiring_credits: 1000,
      unusable_credits: 0,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "2026-10-17 23:00",
      last_checkin_ok: true,
    },
  ],
  checkin_times: [],
  next_checkin: "-",
};

// ---- 最小 DOM stub ----
const nodes = {};
function mkNode(id) {
  const cls = new Set();
  return {
    id,
    innerHTML: "",
    textContent: "",
    value: "", // #149 起 updateEffDirty/renderFbList 会读输入框 .value.trim()
    style: {},
    classList: {
      add: (c) => cls.add(c),
      remove: (c) => cls.delete(c),
      contains: (c) => cls.has(c),
      _set: cls,
    },
    // 运行日志容器（renderAppLog 吸底判定需要；stub 下 pinned 恒 true，只验证 HTML 生成）
    scrollTop: 0,
    scrollHeight: 100,
    clientHeight: 50,
    onmouseenter: null,
    onmouseleave: null,
    // querySelector 供 renderModelTable 取 tbody；返回独立 stub（其 innerHTML
    // 会被断言读取，故必须与父节点区分开）。
    querySelector: (sel) => (nodes[id + sel] ||= mkNode(id + sel)),
    // querySelectorAll 供积分明细浮窗的翻页绑定使用。必须在 stub 上返回真实数组：
    // 旧 stub 靠「undefined.forEach 抛异常」掩盖了渲染成功与否，属侥幸通过，已改为显式空集合。
    querySelectorAll: () => [],
  };
}
const document = {
  getElementById: (id) => (nodes[id] ||= mkNode(id)),
  querySelectorAll: () => [],
  addEventListener: () => {},
  createElement: () => mkNode("stub-" + Math.random()),
  body: { appendChild: () => {} },
};
const window = { addEventListener: () => {}, location: { search: "" } };

// 源码改写：注入 state + 导出函数
const patched =
  raw.replace(/let state = null;/, "let state = " + JSON.stringify(FAKE_STATE) + ";")
  // 模型查找面板：预置一份费率缓存（正常由 loadFees 填充），供 renderModelSearch 过滤。
  + "\n; lastFees = " + JSON.stringify({ channels: [
    { channel: "qodercn", models: [
      { model: "qwen3.8-flash", priced: true, rate: 0.3, free: false, has_context: true, context_window: 180000, supports_images: false, supports_reasoning: false },
      { model: "glm-5.3", priced: true, rate: 0.5, free: false, has_context: true, context_window: 200000, supports_images: true, supports_reasoning: true,
        reasoning_efforts: ["low", "high", "max"], default_reasoning_effort: "high" },
    ] },
    { channel: "workbuddy", models: [
      { model: "deepseek-v4", priced: false, rate: 0, free: false, has_context: false },
      { model: "muse-free", priced: true, rate: 0, free: true, has_context: true, context_window: 128000 },
    ] },
    // 溢出场景：另 12 个 qwen* 模型，与 qodercn/qwen3.8-flash 合并共 13 条命中，
    // 用于验证「命中数超过旧上限 12」时仍全量渲染（结果不再截断，改由列表滚动展示）
    { channel: "loomy", models: Array.from({ length: 12 }, (_, i) =>
      ({ model: `qwen-bulk-${i}`, priced: true, rate: 0.1, free: false, has_context: false })) },
    // 上游 ID 自带命名空间的渠道（cline 免费档）：完整 ID 取服务端给的 id
    // （= 上游 ID 原样 `cline-free/*`，不再拼 `cline/`；决议 R59）
    { channel: "cline", models: [
      { model: "cline-free/mimo-v2.6-flash", id: "cline-free/mimo-v2.6-flash", priced: true, rate: 0, free: true, has_context: true, context_window: 1000000 },
    ] },
  ] }) + ";"
  + "\n; globalThis.__T = { renderAccounts, renderSummary, loadStats, renderStats, renderPlatformTable, renderRotation, renderExpiry, renderModels, renderLogs, renderAbnormal, renderFallbacks, renderEffList, renderAppLog, handleToasts, renderModelSearch, collectAllModels, cacheCell, renderUsageCacheCard, renderModelTable, renderUsage, renderFees"
  // 积分明细浮窗：函数 + 两个模块级 let 的注入器（detailState/detailCache 是 `let`，
  // 只能在源码作用域内赋值，故由改写注入 setter 而不是靠 globalThis）。
  // ⚠️ cache 必须按 uid 建键：真实代码是 detailCache[detailState.uid]，
  //    直接塞「响应对象」会让 detailCache[uid] 为 undefined（静默走空明细分支）。
  + ", renderCreditDetail, showCreditDetail, __setCreditDetailState: (st, resp) => { detailState = st; detailCache = { [st.uid]: resp }; }"
  + " };";

new Function("document", "window", "localStorage", "fetch", "location", "navigator", patched)(
  document,
  window,
  { getItem: () => null, setItem: () => {} },
  async () => ({ ok: true, json: async () => ({}) }),
  window.location,
  { userAgent: "node" }
);

const T = globalThis.__T;
const results = [];
const check = (name, pass, extra = "") => results.push([name, pass, extra]);

// ---- 场景 1：账号卡片的冷却可见性 ----
T.renderAccounts();
const cards = document.getElementById("acctList").innerHTML;
check("冷却标签出现（冷却至 …）", /冷却至/.test(cards));
check("冷却时间正确显示", cards.includes("10-03 09:12"));
check("冷却原因进 title（含 3004）", cards.includes("3004"));
check("冷却卡片带 cooling 类（左侧琥珀边）", /acct-card cooling/.test(cards));
check("冷却标签用 warn 样式", /tag warn/.test(cards));
check("冷却时长并列展示（剩 X 时 X 分，消除跨零点误读）", /（剩 \d+ 时 \d+ 分）/.test(cards));
check(
  "只有冷却账号带冷却标签（健康号不带）",
  (cards.match(/冷却至/g) || []).length === 1,
  `实际 ${(cards.match(/冷却至/g) || []).length} 个`
);
check("健康号仍在（未被过滤）", cards.includes("健康号"));

// ---- 场景 1.5：OfficeAce「缺 refresh_token」持久徽标（2026-10-17 用户实测要求）----
// 为什么要有这个徽标：客户端导入只拿得到不过期的网关 Basic，拿不到换临时凭据所需的
// refresh_token ⇒ 积分/签到永久不可用，且**不会自愈**（不是 401 那种能被 refresh 救回的
// 形态）。原先只在导入那一刻弹一次提示文字，事后无从复查；「需重新登录」徽标又只认
// session_dead ⇒ 这种账号在面板上看起来一切正常。以下断言锁住「该亮则亮、不该亮则不亮」。
check("缺凭据号出现徽标「缺续期凭据」", cards.includes("缺续期凭据"));
check("徽标带 warn 样式（与「需重新登录」的 bad 区分）", /tag warn"[^>]*>缺续期凭据/.test(cards));
check("徽标 title 带补救路径提示", cards.includes("缺 refresh_token：积分/签到不可用"));
check("缺凭据号给出一键重新登录入口（⟳ + startLogin('officeace')）",
  /onclick="startLogin\('officeace'\)"/.test(cards));
check("重新登录入口的 title 点明缺 refresh_token 场景",
  cards.includes("本账号缺 refresh_token，积分/签到不可用"));
// ⟳ 的**渠道分派**必须存在：OfficeAce 是两步流程（云端轮询捕获），不能打
// `POST /api/login/start`（后端 StartLoginFor 不会为它拉起轮询）——2026-10-18 独立复核
// 抓到的死按钮就是这个。这条断言把「卡片 ⟳ → startLogin → 分派到 OfficeAce 专属入口」
// 这段接线锁住（真正的端到端对齐在 Go 侧 TestReloginChannelSetMatchesBackend）。
{
  const appSrc = readFileSync(new URL("../cmd/wild-work/web/app.js", import.meta.url), "utf8");
  check("startLogin 对 officeace 分派到专属入口（防死按钮复发）",
    /if \(channel === "officeace"\) return startOfficeAceWebLogin\(\);/.test(appSrc));
  check("OfficeAce 专属入口打的是 officeace_auto 端点（而非 /api/login/start）",
    appSrc.includes('api("/api/login/officeace_auto"'));
}
// 反例（防「徽标恒亮」这种假通过）：同渠道、只差该字段的账号不得带徽标与 ⟳。
// 按 `<div class="acct-card` 切分取**该账号自己那张卡**，避免跨卡片误判。
{
  const cardOf = (name) =>
    cards.split('<div class="acct-card').find((seg) => seg.includes(name)) || "";
  const missingCard = cardOf("缺凭据号");
  const normalCard = cardOf("正常号");
  check("两张 OfficeAce 卡片都渲染出来了", missingCard !== "" && normalCard !== "");
  check("缺凭据那张卡带徽标", missingCard.includes("缺续期凭据"));
  check("缺凭据那张卡带 ⟳ 入口", missingCard.includes("startLogin('officeace')"));
  check("正常号那张卡不带徽标（防徽标恒亮）", !normalCard.includes("缺续期凭据"));
  check("正常号那张卡不带 ⟳ 重新登录入口", !normalCard.includes("startLogin('officeace')"));
}
check("非 officeace 账号不带「缺续期凭据」（渠道限定，避免误标）",
  (cards.match(/缺续期凭据/g) || []).length === 1,
  `实际 ${(cards.match(/缺续期凭据/g) || []).length} 个`);

// ---- 场景 1.6：#134「登录态失效」原行为（2026-10-18 补，此前零覆盖）----
// 独立复核用两个破坏性变异证明这条路没被守住：把 needRelogin 里的 session_dead 去掉、
// 或整块删掉 sessionDeadTag，UI 检查都仍「全部通过」。故补这张 session_dead 卡片与断言。
{
  const cardOf = (name) =>
    cards.split('<div class="acct-card').find((seg) => seg.includes(name)) || "";
  const deadCard = cardOf("失效号");
  const healthyCard = cardOf("健康号");
  check("登录态失效账号渲染出来了", deadCard !== "");
  check("失效号带「需重新登录」徽标（#134 不回归）", deadCard.includes("需重新登录"));
  check("失效号徽标用 bad 样式（与缺凭据的 warn 区分）", /tag bad"[^>]*>需重新登录/.test(deadCard));
  check("失效号徽标 title 用 session_dead_reason", deadCard.includes("登录态已失效（测试用文案）"));
  check("失效号带 ⟳ 一键重新登录（#134 不回归）", deadCard.includes("startLogin('qwenwork')"));
  check("失效号不带「缺续期凭据」徽标（两种状态互不串味）", !deadCard.includes("缺续期凭据"));
  check("普通健康号既无「需重新登录」也无 ⟳", !healthyCard.includes("需重新登录") && !healthyCard.includes("startLogin("));
  check("「需重新登录」全文只出现一次（不误标他人）",
    (cards.match(/需重新登录/g) || []).length === 1,
    `实际 ${(cards.match(/需重新登录/g) || []).length} 个`);
}

// ---- 场景 2：渠道汇总条 ----
T.renderSummary();
const sum = document.getElementById("creditSummary").innerHTML;
check("汇总条渲染了 traework", sum.includes("TraeWork"));
check("汇总可用 = 2050+2686 = 4736", sum.includes("4,736"), sum.slice(0, 160));
check("汇总临期 = 100", sum.includes("临期100"));
check("汇总含合计", sum.includes("合计"));

// ---- 场景 2.5：模型查找面板（纯前端过滤，全量渲染 + 列表滚动） ----
// 假缓存共 17 个模型：qodercn 2 + workbuddy 2 + loomy 12 + cline 1（含 13 个 qwen* 用于滚动断言）
check("collectAllModels 展平全渠道 17 个模型", T.collectAllModels().length === 17, `实际 ${T.collectAllModels().length}`);
check("collectAllModels 完整 ID = 渠道/裸名", T.collectAllModels()[0].full === "qodercn/qwen3.8-flash");
// 上游 ID 自带命名空间（cline-free/*）时完整 ID = 上游 ID 原样（服务端 m.id 同源），
// 不再拼 `cline/`（R59）；旧的 cline/cline-free/* 仍可路由（兼容断言在 Go 侧）
check("上游命名空间 ID 直通上游前缀（cline-free/*）",
  T.collectAllModels().some((x) => x.full === "cline-free/mimo-v2.6-flash"),
  JSON.stringify(T.collectAllModels().filter((x) => x.ch === "cline")));
T.renderModelSearch("glm");
let msHtml = document.getElementById("modelSearchList").innerHTML;
check("搜索 glm 命中 1 条", (msHtml.match(/class="ms-row"/g) || []).length === 1, msHtml.slice(0, 120));
check("命中项 data-mid 为完整 ID", msHtml.includes('data-mid="qodercn/glm-5.3"'));
check("命中项显示渠道徽章与费率", msHtml.includes("QoderCN") && msHtml.includes("x0.50"));
check("命中项显示思考档位（issue #96）", msHtml.includes('class="ms-eff"') && msHtml.includes("low·high·max"), msHtml.slice(0, 300));
check("档位 title 含默认档", msHtml.includes("默认 high"));
check("底部提示共 N 个匹配", /共 1 个匹配/.test(document.getElementById("modelSearchFoot").textContent));
T.renderModelSearch("");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("空关键字全量列出 17 条（不再截断）", (msHtml.match(/class="ms-row"/g) || []).length === 17, `实际 ${(msHtml.match(/class="ms-row"/g) || []).length}`);
check("空关键字提示共 N 个匹配", /共 17 个匹配/.test(document.getElementById("modelSearchFoot").textContent), document.getElementById("modelSearchFoot").textContent);
check("免费模型显示 Free", msHtml.includes("Free"));
check("未定价模型显示 unknown", msHtml.includes("unknown"));
T.renderModelSearch("zzz-none");
check("无匹配时提示无匹配", /无匹配/.test(document.getElementById("modelSearchList").innerHTML));
// 全量：13 个 qwen* 命中全部渲染（列表可滚动查看，不再截断到 12 条）
T.renderModelSearch("qwen");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("超 12 条时全量渲染 13 条", (msHtml.match(/class="ms-row"/g) || []).length === 13, `实际 ${(msHtml.match(/class="ms-row"/g) || []).length}`);
check("命中数超出时底部提示共 N 个匹配", /共 13 个匹配/.test(document.getElementById("modelSearchFoot").textContent), document.getElementById("modelSearchFoot").textContent);

// ---- 场景 2.7：费率表思考档位标记（issue #96，与 /v1/models + /api/fees 同源） ----
T.renderFees({ channels: [
  { channel: "qodercn", models: [
    { model: "glm-5.3", priced: true, rate: 0.5, free: false, has_context: true, context_window: 200000,
      supports_reasoning: true, reasoning_efforts: ["low", "high", "max"], default_reasoning_effort: "high" },
    { model: "qwen3.8-flash", priced: true, rate: 0.3, free: false, has_context: true, context_window: 180000, supports_reasoning: false },
  ] },
  // 上游 ID 自带命名空间（cline 免费档）：完整 ID = 上游 ID 原样
  { channel: "cline", models: [
    { model: "cline-free/mimo-v2.6-flash", id: "cline-free/mimo-v2.6-flash", priced: true, rate: 0, free: true, has_context: true, context_window: 1000000 },
  ] },
] });
const feesHtml = document.getElementById("feesBox").innerHTML;
check("费率表档位小字标出现（[low·high·max]）", feesHtml.includes("effort-tag") && feesHtml.includes("[low·high·max]"), feesHtml.slice(0, 200));
check("档位 title 含默认档与调档提示", feesHtml.includes("默认 high") && feesHtml.includes("reasoning_effort"));
check("无档位声明的模型不显示档位标", (feesHtml.match(/effort-tag/g) || []).length === 1);
// 完整 ID（R59）：上游命名空间 ID 直接透传（服务端 m.id 同源），复制出去即可用
// （旧的 cline/cline-free/* 仍被服务端路由兼容，但对外只广告新形态）
check("费率表上游命名空间 ID 直通（cline-free/*）",
  feesHtml.includes('data-mid="cline-free/mimo-v2.6-flash"'), feesHtml.slice(0, 300));

// ---- 场景 2.75：模型查找面板无档位声明时不显示档位 ----
T.renderModelSearch("deepseek");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("未声明档位的模型行无 ms-eff", !msHtml.includes("ms-eff"), msHtml.slice(0, 200));

// ---- 场景 3：运行统计（/api/stats 载荷 → 各区块渲染） ----
// 载荷形状 = internal/stats 引擎 Snapshot 的 JSON（与 sidecar /capi/stats 对齐）
const FAKE_STATS = {
  version: 1, today: "2026-10-06",
  total_credits: 4736, total_accounts: 2,
  credit_in: 200, credit_out: 150, credit_out_exact: 150.5, credit_expired: 100, credit_used: 50,
  tokens: 12345, reqs: 10,
  token_usage: { today: 9000, days7: 50000, days30: 120000, all: 200000, req_today: 8, req_all: 90, days: 12 },
  expiring_7d: 300, expiring_7d_accts: 1, expire_today: 100, expire_tomorrow: 200,
  platforms: [{
    group: "traework", accounts: 2, credits: 4736, credit_in: 200, credit_out: 150,
    credit_expired: 100, credit_used: 50, tokens: 12345, expire_7d: 300, expire_today: 100, expire_tomorrow: 0,
  }],
  models: [{ model: "traework/DeepSeek-V4", reqs: 3, tokens: 5000, credit: 12.5, avg_ttfb_ms: 800, avg_tok_per_sec: 45.2, perf_samples: 3 }],
  expiry: [{ uid: "4298452267967801", nickname: "健康号", group: "traework", expire_at: "2026-10-08", days_left: 2, amount: 100, today_out: 30, burn_forecast: 45, risk: true }],
  rotation: [{
    group: "traework",
    current: { uid: "1606848467182378", nickname: "中招号", group: "traework", credits: 2050, expiring: 0, sticky: "12/50" },
    next: [{ uid: "4298452267967801", nickname: "健康号", group: "traework", credits: 2686, expiring: 100 }],
  }],
  abnormal: [
    { uid: "1606848467182378", nickname: "中招号", group: "traework", level: "cool", reason: "stream business error: code=3004", until: "10-08 09:12", err_count: 0, credits: 2050, updated_at: "2026-10-06 12:00:00" },
    { uid: "4298452267967801", nickname: "健康号", group: "traework", level: "err", reason: "", until: "", err_count: 3, credits: 2686, updated_at: "2026-10-06 12:00:00" },
  ],
  milestone_total: 100, milestone_count: 1,
  toasts: [], // 空：本场景不触 Notification/面板 toast 路径
};

T.renderStats(FAKE_STATS);
const stSum = document.getElementById("stSum").innerHTML;
check("统计汇总条渲染总积分（4,736）", stSum.includes("4,736"));
check("汇总条含账号数注解", stSum.includes("2 个账号"));
check("今日作废仅 >0 时出现", stSum.includes("今日作废"));
check("今日到期仅 >0 时出现", stSum.includes("今日到期"));
check("里程碑计数正确", document.getElementById("stMilestone").textContent === "里程碑 1×100分");
const stToken = document.getElementById("stToken").innerHTML;
check("Token 多天行含今日/7日/已记录", stToken.includes("Token 今日") && stToken.includes("7日") && stToken.includes("已记录") && stToken.includes("12 天"));

const stPlat = document.getElementById("stPlat").innerHTML;
check("平台表渲染渠道徽章（traework）", /badge trae"/.test(stPlat));
check("平台表含合计行（全池口径 4,736）", stPlat.includes("合计") && stPlat.includes("4,736"));
check("平台表零值列置灰（st-plat-zero）", stPlat.includes("st-plat-zero"));

const stRotation = document.getElementById("stRotation").innerHTML;
check("接棒顺序渲染使用中账号", stRotation.includes("使用中 <b>中招号</b>"));
check("接棒顺序含 ① 候补", stRotation.includes("① 健康号"));

const stExpiry = document.getElementById("stExpiry").innerHTML;
check("临期风险行高亮（st-risk）", stExpiry.includes("st-ex-row st-risk"));
check("临期剩余天数文案", stExpiry.includes("2 天后到期"));
check("预计作废量 = 额度−外推消耗（55分）", stExpiry.includes("预计作废 55分"));

const stModels = document.getElementById("stModels").innerHTML;
check("Trae 系模型积分为估算（≈ 前缀）", stModels.includes("≈12.50"));
check("模型平均速度含 tok/s 与 TTFB", stModels.includes("45.2 tok/s") && stModels.includes("800ms"));

T.renderAbnormal(FAKE_STATS.abnormal);
const stAbnormal = document.getElementById("stAbnormal").innerHTML;
check("异常账号渲染整号冷却标签", stAbnormal.includes("整号冷却"));
check("错误累计显示连续次数", stAbnormal.includes("连续错误 3 次"));

// ---- 场景 4：请求日志（降序 / 状态标红 / tok/s 占位） ----
const FAKE_LOGS = [
  { seq: 1, time: "10-06 12:00:00", model: "traework/x", mode: "流", status: 200, ttfb_ms: 500, tok: 100, tok_per_sec: 20.5, total_sec: 3.2, credit: 1.5 },
  { seq: 2, time: "10-06 12:01:00", model: "qodercn/y", mode: "非流", status: 500, ttfb_ms: 800, tok: 0, tok_per_sec: 0, total_sec: 0, credit: 0 },
];
T.renderLogs(FAKE_LOGS);
const stLogs = document.getElementById("stLogs").innerHTML;
check("请求日志降序（最新 seq 在前）", stLogs.indexOf("<td>2</td>") < stLogs.indexOf("<td>1</td>"));
check("≥400 状态标红（st-bad）", stLogs.includes("st-bad"));
check("非流式 tok/s 显示「—」", stLogs.includes("—"));

// ---- 场景 5：运行日志（级别配色 / 时间前缀剥离） ----
const FAKE_APPLOG = [
  "2026/10/06 12:00:00.123 checkin platform=traework ok=false code=9074 msg=已签到",
  "2026/10/06 12:00:01.000 批量签到完成：total=20 ok=19 failed=1",
];
T.renderAppLog(FAKE_APPLOG);
const stApplog = document.getElementById("stApplog").innerHTML;
check("运行日志剥离年份前缀", stApplog.includes("10/06 12:00:00") && !stApplog.includes("2026/10/06"));
check("code=9074/ok=false 行标红", (stApplog.match(/st-log-err/g) || []).length === 1);
check("汇总成功行（含 failed=N）按 info 放行", (stApplog.match(/st-log-info/g) || []).length === 1);

// ---- 场景 6：里程碑 toast 去重（同 id 二次轮询不重复弹） ----
// 主 stub 的 localStorage 无状态，去重断言需带状态的存储：
// 用同一份 patched 源码再实例化一次（隔离作用域），以「已读标记写入次数」断言——
// 两次 handleToasts 同 id → 第一次写 1 次标记，第二次读到已读跳过 → 共 1 次写入。
let toastWrites = 0;
const lsStore = {};
const lsStub = {
  getItem: (k) => (k in lsStore ? lsStore[k] : null),
  setItem: (k, v) => { lsStore[k] = v; toastWrites++; },
};
new Function("document", "window", "localStorage", "fetch", "location", "navigator",
  patched + "\n; globalThis.__DedupT = handleToasts;")(
  document, window, lsStub, async () => ({ ok: true, json: async () => ({}) }), window.location, { userAgent: "node" });
globalThis.__DedupT([{ id: "2026-10-06-m1", title: "里程碑", body: "100分" }]);
globalThis.__DedupT([{ id: "2026-10-06-m1", title: "里程碑", body: "100分" }]);
check("里程碑 toast 同 id 去重（只写一次已读标记）", toastWrites === 1, `实际写入 ${toastWrites} 次`);

// ---- 静态结构断言：index.html / style.css 的 stats 骨架 ----
const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
check('第三个 tab 按钮存在（data-mtab="stats"）', /data-mtab="stats"/.test(html));
check("#mtabStats body 存在且默认隐藏", /class="mtab-body hidden" id="mtabStats"/.test(html));
check("运行统计区块骨架存在（stSum/stPlat/stApplog）", /id="stSum"/.test(html) && /id="stPlat"/.test(html) && /id="stApplog"/.test(html));
check("更新角标存在（id=stUpdated，拉取失败可感知）", /id="stUpdated"/.test(html));
// ---- 第四个 tab：模型列表和费率（原独立 section 移入主 tab 组）----
check('第四个 tab 按钮存在（data-mtab="fees"）', /data-mtab="fees"/.test(html));
check("#mtabFees body 存在且默认隐藏", /class="mtab-body hidden" id="mtabFees"/.test(html));
check("模型费率区块骨架存在（feesBox/btnRefreshFees）", /id="feesBox"/.test(html) && /id="btnRefreshFees"/.test(html));
check("模型费率区块在 section 内（不再是独立 section）", /<div class="mtab-body hidden" id="mtabFees">[\s\S]*?<\/section>/.test(html) && !/<!-- 模型列表和费率 -->\s*<section class="panel">/.test(html));
const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
check("style.css 含运行统计样式段（#mtabStats .st-sum 等）", /#mtabStats \.st-sum/.test(css) && /#mtabStats \.st-plat-row/.test(css));
check("更新角标失败警示样式存在（#stUpdated.st-stale）", /#stUpdated\.st-stale/.test(css));
// ---- 用量 tab 五卡同行（5 列 grid，不换行）----
check("用量卡片 5 列同行（.usage-cards repeat(5)）", /\.usage-cards \{[^}]*grid-template-columns: repeat\(5, 1fr\)/.test(css));

// ---- 场景 3.5：缓存利用率的**三态降级**（本列的核心就是不能把「不知道」显示成 0%） ----
// 语义务必与 internal/server/cacheusage.go + internal/ledger 的 CR/CRC 保持一致：
//   cr_cov > 0 → 百分比（允许 0%，那是真未命中）
//   cr_cov = 0 → 「—」（上游不提供该维度：glm / raccoon）
{
  const T2 = globalThis.__T;
  // ① 有上报样本：命中 97 / 分母 103 → 94.17%
  const withHit = T2.cacheCell({ cr: 97, cr_cov: 103, cr_reqs: 2 });
  check("有上报样本时显示百分比（97/103 → 94.2%）", /94\.2%/.test(withHit), withHit);
  check("百分比带样本量 title", /2 次上报请求/.test(withHit), withHit);
  // ② 上报了但命中为 0：显示 0.00%，**不是** 「—」（这两者必须区分）
  const zeroHit = T2.cacheCell({ cr: 0, cr_cov: 500, cr_reqs: 3 });
  check("上报但未命中显示 0%（不显示「—」）", /0\.00%/.test(zeroHit) && !/—/.test(zeroHit), zeroHit);
  // ③ 未上报（glm / raccoon）：显示「—」，且**绝不能**出现百分比
  const noReport = T2.cacheCell({ cr: 0, cr_cov: 0, cr_reqs: 0 });
  check("未上报时显示「—」而非 0%", noReport.includes("—") && !/%/.test(noReport), noReport);
  const missing = T2.cacheCell({});
  check("字段整体缺失（老流水）也显示「—」", missing.includes("—") && !/%/.test(missing), missing);
  // ④ 汇总卡同款三态
  T2.renderUsageCacheCard({ cr: 50, cr_cov: 200, cr_reqs: 4 });
  check("汇总卡有样本时显示 25.0%", document.getElementById("ucCache").textContent === "25.0%", document.getElementById("ucCache").textContent);
  T2.renderUsageCacheCard({});
  check("汇总卡无样本时显示「—」", document.getElementById("ucCache").textContent === "—", document.getElementById("ucCache").textContent);
  // ⑤ 模型表真渲染一列（列数从 5 → 6，占位行 colspan 同步）
  T2.renderModelTable([
    { model: "glm-5.3", channel: "workbuddy", requests: 2, pt: 200, ct: 100, cr: 150, cr_cov: 200, cr_reqs: 2 },
    { model: "chatglm", channel: "glm", requests: 1, pt: 50, ct: 10 },
  ], 1);
  const rowsHtml = document.getElementById("tblModelstbody").innerHTML;
  check("模型表渲染缓存列（命中行 75.0%）", /75\.0%/.test(rowsHtml), rowsHtml.slice(0, 200));
  check("模型表对不上报渠道显示「—」", rowsHtml.includes("—"));
  check("模型表渲染平均速度列（无速度样本显示「—」）", (rowsHtml.match(/—/g) || []).length >= 2, rowsHtml.slice(0, 300));
  T2.renderModelTable([], 1);
  check("模型表空态占位行跨 7 列", document.getElementById("tblModelstbody").innerHTML.includes('colspan="7"'));
}

// ---- 输出 ----
let failed = 0;
for (const [name, pass, extra] of results) {
  console.log(`  ${pass ? "✅" : "❌"} ${name}${extra && !pass ? "  → " + extra : ""}`);
  if (!pass) failed++;
}

// ---- 静态结构断言：同名顶层函数/常量不得重复定义 ----
// 起因（2026-10-03 复核发现）：renderSummary 曾被定义两次，前者成死代码；
// node --check 与功能断言都发现不了（后者只测生效的那版）。
// 一旦有人"修好"死的那版，面板数字会静默变化。
const uniq = [
  ["function renderSummary", /^\s*function renderSummary\s*\(/gm],
  ["const CH_ORDER", /^\s*const CH_ORDER\s*=/gm],
  ["const CH_SUMMARY_ORDER", /^\s*const CH_SUMMARY_ORDER\s*=/gm],
  ["function renderAccounts", /^\s*function renderAccounts\s*\(/gm],
  // 顶层 = 行首无缩进。内层同名函数（如 attachModelPicker 内的 render()、
  // attachEffortPicker 内的 render()）是合法的局部闭包，不得计入。
  ["function render()", /^function render\s*\(/gm],
  // 运行统计区块（防「同一提交里写两版」的非预期重复，同 renderSummary 教训）
  ["function loadStats", /^\s*async function loadStats\s*\(/gm],
  ["function renderStats", /^\s*function renderStats\s*\(/gm],
  ["function renderPlatformTable", /^\s*function renderPlatformTable\s*\(/gm],
  ["function renderRotation", /^\s*function renderRotation\s*\(/gm],
  ["function renderExpiry", /^\s*function renderExpiry\s*\(/gm],
  ["function renderModels", /^\s*function renderModels\s*\(/gm],
  ["function renderLogs", /^\s*function renderLogs\s*\(/gm],
  ["function renderAbnormal", /^\s*function renderAbnormal\s*\(/gm],
  ["function renderAppLog", /^\s*function renderAppLog\s*\(/gm],
  ["function handleToasts", /^\s*function handleToasts\s*\(/gm],
];
for (const [label, re] of uniq) {
  const n = (raw.match(re) || []).length;
  // 判定「不得 >1」而非「必须 ==1」：某些定义（如 CH_ORDER）本就不该存在，
  // 删掉后是 0 次也正常；这里防的是"同一提交里写了两版"这种非预期重复。
  const pass = n <= 1;
  console.log(`  ${pass ? "✅" : "❌"} 顶层定义不重复：${label}（${n} 次）`);
  if (!pass) failed++;
}

// ---- 渠道接线完整性（cline）----
// 面板新增一个渠道最容易漏的是「按钮 ↔ 弹层 ↔ 提交函数」这条链：
// 漏了不报错，只表现为「渠道能选到但没入口」或「弹层弹出来但点没反应」——
// 与 R34/R41 同族的「声明了却没接线」，故在此静态锁住。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    ["index.html 有 Cline 添加按钮", html.includes('id="btnAddCline"')],
    ["index.html 有 API Key 粘贴弹层", html.includes('id="clineOverlay"')],
    ["弹层含 Key 输入框", html.includes('id="clineKeyInput"')],
    ["弹层写明申请办法（app.cline.bot）", html.includes("app.cline.bot")],
    ["弹层警告「必须多个独立账号」", /多个独立账号|多个不同.*账号/.test(html)],
    ["app.js 接线了 btnAddCline", raw.includes('$("btnAddCline").onclick')],
    ["app.js 接线了提交/取消按钮", raw.includes('$("btnClineSubmit").onclick') && raw.includes('$("btnClineCancel").onclick')],
    ["app.js 调 /api/login/cline_key", raw.includes("/api/login/cline_key")],
    ["cline 在渠道名/样式表里登记", /cline: "Cline"/.test(raw) && /cline: "cline"/.test(raw)],
    ["cline 归入无签到渠道", /NO_EXPLICIT_CHECKIN[^)]*"cline"/.test(raw)],
    ["cline 无余额概念→积分显示不适用", /NO_CREDITS[^)]*"cline"/.test(raw)],
    ["备用上游通道入口在位（单一输入 + 生效位回显 + 保存走 /api/config/proxy）",
      html.includes('id="backupProxyInput"') && html.includes('id="backupProxyState"')
        && raw.includes('"/api/config/proxy"') && raw.includes("backup_proxy_active")],
    // issue #117：面板「测试」按钮原先直连上游（实际对话走备用通道），用户配了代理点测试
    // 仍看到 403 且无从判断代理是否生效。现已与真实对话同源，并把出口（直连/备用）如实展示。
    ["OC Zen 测试按钮展示出口（直连/备用）——后端同源编排 + 前端读 via_backup",
      raw.includes("via_backup") && raw.includes("经备用通道")],
    ["hint 写明测试与真实对话同源（否则用户仍会误读 403）",
      html.includes("与真实对话同源") && html.includes("自动改走备用通道")],
    ["旧「每渠道代理」输入表已清除（R57：不再有 PROXY_CHANNELS / proxyList）",
      !raw.includes("PROXY_CHANNELS") && !html.includes('id="proxyList"') && !raw.includes("/api/config/proxies")],
    ["style.css 有 cline 渠道色", css.includes(".badge.cline") && css.includes(".btn.primary.alt.cline")],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 小浣熊签到接线（R65）----
// 与 R34/R41 同族：签到接入横跨「渠道实现 + 调度器配置 + noExplicitCheckin + 前端集合」四处，
// 漏任意一处都**不报错**——只表现为「按钮是灰的」或「定时不跑」，极难发现，故在此静态锁住。
// （渠道实现与调度器配置由 Go 侧锁：internal/raccoon/checkin_test.go、internal/app 的 noExplicitCheckin 断言。）
{
  const need = [
    ["raccoon 不在无签到集合（否则手动按钮永久置灰、批量签到静默跳过）",
      !/NO_EXPLICIT_CHECKIN[^)]*"raccoon"/.test(raw)],
    ["raccoon 仍保留渠道名/样式登记（防止误删）",
      /raccoon: "小浣熊"/.test(raw) && /raccoon: "raccoon"/.test(raw)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- MonkeyCode 签到 + Loomy 浏览器登录/签到（2026-10-17）----
// 与上面 raccoon 同族：漏改任意一处都**不报错**，只表现为「按钮是灰的」或「引导页打不开」。
// Go 侧另有 internal/app/{monkeycode,loomy}_checkin_test.go 与 app.go 的 noExplicitCheckin 断言。
{
  const need = [
    ["monkeycode 不在无签到集合（已实装签到，见 internal/monkeycode/checkin.go）",
      !/NO_EXPLICIT_CHECKIN[^)]*"monkeycode"/.test(raw)],
    ["loomy 不在无签到集合（已实装宠物打工领奖，见 internal/loomy/checkin.go）",
      !/NO_EXPLICIT_CHECKIN[^)]*"loomy"/.test(raw)],
    ["loomy 在网页版登录集合里（否则「＋Loomy」不弹登录框）",
      /WEB_LOGIN_CHANNELS = new Set\(\[[^\]]*"loomy"/.test(raw)],
    // Loomy 与 raccoon 相反：后端不拉浏览器，必须由前端 window.open 打开引导页。
    ["loomy 的登录分支有 window.open（否则引导页打不开）",
      /channel === "loomy"[\s\S]{0,600}?window\.open\(url/.test(raw)],
    ["loomy 的弹窗文案不再套用「互踢/自动续期」（Loomy 无 refresh 端点）",
      (() => {
        // 只检查**用户可见的文案**（lcMsg 赋值段），并先剥掉注释 ——
        // 否则分支里的说明性注释（「不存在互踢 / 自动续期」）会被自己的断言命中。
        const i = raw.indexOf('channel === "loomy"');
        if (i < 0) return false;
        const seg = raw
          .slice(i, raw.indexOf("return;", i) + 7)
          .replace(/^\s*\/\/.*$/gm, "");
        return !/自动续期|互踢/.test(seg);
      })()],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- fmtTokens 只能有一个定义 + Token 额度必须用可读单位（2026-10-17 冒烟发现）----
// 背景：Token 计数面板新增了一个「万/亿」版 fmtTokens，与上游原有的「k/M」版**同名**，
// 且后者在文件更靠后 ⇒ 静默覆盖。后果：Token 卡片把 94539300 显示成 `94539k`
//（用户读不出来的数字），而模型上下文窗口又确实需要 k/M 形态。
// 修法：额度用 fmtTokens（万/亿），模型元数据用 fmtTokensCompact（k/M）。
// 这里锁住三件事，防止再次互相覆盖：
{
  const decls = raw.match(/^\s*function\s+fmtTokens\s*\(/gm) || [];
  const compact = raw.match(/^\s*function\s+fmtTokensCompact\s*\(/gm) || [];
  const need = [
    ["fmtTokens 只有一处定义（同名互相覆盖会让额度显示成 94539k）", decls.length === 1],
    ["fmtTokensCompact 存在且唯一（模型元数据用 k/M，与上游口径一致）", compact.length === 1],
    // 直接跑真实函数：额度数字必须落在「万/亿」口径上。
    ["fmtTokens(94539300) 用万/亿（不是 94539k）", (() => {
      const m = raw.match(/function\s+fmtTokens\s*\(n\)\s*\{[\s\S]*?\n\}/);
      if (!m) return false;
      try {
        const fn = new Function("return (" + m[0] + ")")();
        const out = String(fn(94539300));
        return /万|亿/.test(out);
      } catch { return false; }
    })()],
    ["fmtTokens(0) 不得是 '-'（那是紧凑版的占位，额度 0 要如实显示）", (() => {
      const m = raw.match(/function\s+fmtTokens\s*\(n\)\s*\{[\s\S]*?\n\}/);
      if (!m) return false;
      try {
        const fn = new Function("return (" + m[0] + ")")();
        return String(fn(0)) !== "-";
      } catch { return false; }
    })()],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 积分明细浮窗点击式 + 手机竖屏响应式 ----
// 与 R34/R41 同族：这两处都是「改了交互/样式但没接线/被后写规则覆盖」就静默失效，
// 且宽度断点一旦被人调大就会重新弄坏桌面端（本次就是靠像素级 diff 才发现过一次），故在此锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    // 积分明细：点击式。hover 版在「浮窗开着时后台 loadState() 重建卡片 DOM」会让
    // mouseleave 不再派发，浮窗永久残留（用户反馈的「经常不能自动关闭」），触屏更是关不掉。
    ["明细标签已改为点击触发（不再用 onmouseenter）", raw.includes('onclick="showCreditDetail(event,')],
    ["明细标签不再残留 hover 处理器", !raw.includes("onmouseenter=")],
    ["hover 关闭函数与定时器已删除（防回退；注释提及不算）", !/\bdetailTimer\b/.test(raw.replace(/^\/\/.*$/gm, "")) && !raw.includes("hideCreditDetail")],
    ["浮窗渲染右上角关闭按钮", raw.includes('data-close-credit-tip="1"')],
    ["存在统一关闭入口 closeCreditDetail", /function closeCreditDetail\(/.test(raw)],
    ["存在序号竞态守卫 detailOpenSeq（连点两账号只让最后一次上屏）", /let detailOpenSeq = 0;/.test(raw) && raw.includes("seq !== detailOpenSeq")],
    ["外部关闭路径已绑定一次", raw.includes("bindCreditDetailDismiss()")],
    ["关闭判定用 composedPath（syncCreditsFromDetail 会在派发途中重写积分格，e.target 会变 detached）",
      /bindCreditDetailDismiss[\s\S]{0,900}composedPath/.test(raw) && !/closest\("\.acct-credits"\)/.test(raw)],
    ["切主 tab 仍会收起浮窗", /切换主 tab 强制收起积分明细浮窗[\s\S]{0,80}closeCreditDetail\(\)/.test(raw)],
    [".credit-tip 不再重复定义（原两块，后者静默覆盖前者）", (css.match(/^\.credit-tip \{/gm) || []).length === 1],
    ["浮窗宽度自适应视口（手机不再溢出）", /width: min\(420px, calc\(100vw - 24px\)\)/.test(css)],
    // 响应式：断点是按实测溢出阈值卡位的，调宽会破坏桌面端逐像素一致性，调窄则手机仍溢出。
    ["渠道按钮基础为 6 列（PC 默认宽度 6+5 两行，消除第三行孤行）", css.includes("repeat(6, auto)")],
    ["渠道按钮摆位全归 CSS（index.html 不再内联 grid-column，否则窄屏无法重排）", !/btnAdd\w+" style="grid-column/.test(html)],
    ["渠道按钮 DOM 顺序 = 列配对（同列上下 = 同渠道国内/国际版；Cline 占第一行末位）",
      /btnAddWB\b[\s\S]*?btnAddQoderCN\b[\s\S]*?btnAddTrae\b[\s\S]*?btnAddMonkeyCode\b[\s\S]*?btnAddLoomy\b[\s\S]*?btnAddCline\b[\s\S]*?btnAddWBAI\b[\s\S]*?btnAddQoderCOM\b[\s\S]*?btnAddQwen\b[\s\S]*?btnAddRaccoon\b[\s\S]*?btnAddGLM\b/.test(html)],
    ["窄屏渠道按钮自适应列（auto-fit，118px 可在 320px 竖屏排出两列）", css.includes("repeat(auto-fit, minmax(118px, 1fr))")],
    ["按钮紧凑档字号（12px，PC 一行放下 6 个）", /\.panel-actions-wrap \.btn \{ font-size: 12px;/.test(css)],
    // Cline 实测元数据链路：Go 侧（measuredCaps 表 / ContextFromAPI=false / solar 不支持图像）
    // 由 internal/cline 的单测锁住（TestFetchModelsAppliesMeasuredCaps），此处只管前端文案。
    ["添加窗口含实测参数表且标注仅供参考",
      /免费模型参数（本工具实测验证值，仅供参考）/.test(html) && /仅作参考/.test(html)],
    ["添加窗口说明 muse 靠备用上游通道解地区限制",
      /地区限制/.test(html) && /备用上游通道/.test(html)],
    ["顶栏 1fr 下界解除（minmax(0,1fr)）", /\.top-info \{[^}]*grid-template-columns: minmax\(0, 1fr\)/.test(css)],
    ["账号卡片允许收缩（minmax(0,1fr)）", /\.acct-grid \{ grid-template-columns: minmax\(0, 1fr\); \}/.test(css)],
    ["费率表窄屏隐藏第 3/4 列（4 列布局手机必溢出）", /\.fees table th:nth-child\(n\+3\)[\s\S]*?display: none/.test(css)],
    ["运行统计宽表给横向滚动容器", /#mtabStats \.st-plat,\s*\n\s*#mtabStats \.st-ex-scroll,\s*\n\s*#mtabStats \.st-ab-scroll,\s*\n\s*#mtabStats \.st-scroll \{ max-width: 100%; overflow-x: auto; \}/.test(css)],
    ["触控目标仅在粗指针设备放大（桌面鼠标不吃）", /@media \(hover: none\) and \(pointer: coarse\)/.test(css)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 模型降级链（PR #106）：面板接线 + 降级记录渲染 ----
// 与 R34/R41 同族：「后端有端点、面板没接线」不报错不崩溃，只表现为
// 配不了降级链 / 配了不生效，故把接线与渲染一并锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  T.renderFallbacks([
    { from: "workbuddyai/deepseek-v4.1-flash", to: "workbuddy/glm-5.2", at: "09:12:03", at_unix: 1760000000, active: true },
    { from: "oczen/big-pickle", to: "workbuddy/glm-free", at: "08:01:00", at_unix: 1759990000, recovered: "08:05:12", dur_sec: 252, active: false },
  ]);
  const fbHtml = nodes["stFbs"].innerHTML;
  const need = [
    ["设置弹层有降级链编辑区", html.includes('id="fbList"') && html.includes('id="fbFromInput"') && html.includes('id="fbToInput"')],
    ["app.js 接线添加/保存按钮", raw.includes('$("btnFbAdd").onclick') && raw.includes('$("btnFbSave").onclick')],
    ["app.js 调 POST /api/config/model_fallback", raw.includes('"/api/config/model_fallback"')],
    ["openSettings 从 state.model_fallback 初始化编辑态", /fbRules = JSON\.parse\(JSON\.stringify\(state\.model_fallback \|\| \[\]\)\)/.test(raw)],
    ["运行统计有降级记录区块且 loadStats 渲染它", html.includes('id="stFbBlock"') && raw.includes("renderFallbacks(s.fallbacks")],
    ["进行中事件渲染「降级中」+ 实时计时锚点 data-fb-epoch", fbHtml.includes("降级中") && fbHtml.includes('data-fb-epoch="1760000000"')],
    ["已恢复事件渲染恢复时刻与持续时长", fbHtml.includes("08:05:12") && fbHtml.includes("4 分 12 秒")],
    ["原/备胎模型与箭头均在行内", fbHtml.includes("workbuddyai/deepseek-v4.1-flash") && fbHtml.includes("→")],
    ["空事件常显占位说明（不整块隐藏）", (T.renderFallbacks([]), nodes["stFbs"].innerHTML.includes("暂无降级记录"))],
    ["行内主模型 ID 是可编辑输入框（fb-from-edit + data-edit-idx）", raw.includes('fb-from-edit') && raw.includes('data-edit-idx')],
    ["行内主模型编辑有 change 提交接线（校验+回滚）", raw.includes('classList.contains("fb-from-edit")') && raw.includes("已有规则（每个主模型只允许一条）")],
    // PR #127（R54 触发判据扩展）：文案必须覆盖「整渠道全灭」而非只说「模型全冷」，
    // 否则用户在渠道全灭时看到 503 会以为降级链没配好。
    ["降级链 hint 写明两种触发形态（模型全冷 / 整渠道全灭）",
      /无账号可承载[\s\S]{0,120}整渠道/.test(html)],
    ["降级记录区块标题不再写死「主模型全冷」", !html.includes("主模型全冷切备胎")],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 思考强度注入（issue #96）：设置弹层接线与三态语义文案 ----
// 与降级链同族：「后端有端点、面板没接线」不报错不崩溃，只表现为配不了
// 档位 / 配了不生效，故把编辑区、建议集与接线一并锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const need = [
    ["设置弹层有思考强度编辑区（全局默认行 + 映射列表 + 输入行）",
      html.includes('id="effGlobalInput"') && html.includes('id="effList"') && html.includes('id="effModelInput"') && html.includes('id="effEffortInput"') && html.includes('id="btnEffAdd"') && html.includes('id="btnEffSave"')],
    // #149 把 datalist 换成了 combobox 下拉（attachEffortPicker），候选集在 app.js 的
    // PK_EFFORTS；仍可自由输入（哑管道不校验档位取值）。
    ["档位建议集 none/low/medium/high/max（哑管道：输入框仍可自由输入）",
      /const PK_EFFORTS = \["none", "low", "medium", "high",[^\]]*"max"\]/.test(raw)
      && raw.includes("attachEffortPicker($(\"effEffortInput\")")],
    ["hint 含三态语义（未带才注入 / 显式传不覆盖 / 回退全局默认）",
      html.includes("客户端请求<b>未带</b>") && html.includes("一律不覆盖") && html.includes("精确未命中回退全局默认")],
    ["hint 含 channel/model 格式与 TraeWork 无效说明",
      html.includes("channel/model") && html.includes("TraeWork 渠道协议无 reasoning 注入点")],
    ["app.js 接线添加/保存按钮", raw.includes('$("btnEffAdd").onclick') && raw.includes('$("btnEffSave").onclick')],
    ["app.js 调 POST /api/config/reasoning_efforts", raw.includes('"/api/config/reasoning_efforts"')],
    ["openSettings 从 state.reasoning_efforts 初始化且 `*` 分离到全局默认行",
      /effRules = Object\.entries\(effs\)\.filter\(\(\[k\]\) => k !== "\*"\)/.test(raw) && raw.includes('effs["*"] || ""')],
    ["重复模型拒绝（每个模型只允许一条）", raw.includes("已有档位（每个模型只允许一条）")],
    ["模型行拒绝 `*`（全局默认专用键另有入口）", raw.includes("* 是全局默认专用键")],
    ["renderEffList 空态占位常显（未配置 = 不注入，不整块隐藏）",
      (T.renderEffList(), nodes["effList"].innerHTML.includes("暂无模型档位"))],
    // #149 起映射行改为卡片（.fb-map-card），“模型 ID 列不被 .lbl.wide 的 90px 定宽压掉”
    // 的约束转移到了 style.css 的 .fb-map-card .fb-map-model（flex 自适应 + 省略号）。
    ["renderEffList 模型 ID 列可伸缩不被定宽压掉",
      /class="fb-map-model" title=/.test(raw)
      && /\.fb-map-card \.fb-map-model \{[^}]*flex: 1 1 200px/.test(
        readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8"))],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 设置弹层内的模型查找入口（模型降级链 / 思考强度标题右侧）----
// 与 R34/R41 同族：「按钮存在但没接线」不报错，只表现为点了没反应，故静态锁住。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    ["两处 cfg-heading 各带一个 data-model-search 查找按钮",
      (html.match(/data-model-search/g) || []).length === 2],
    // 文案带「🔍 查询模型」：设置内两处 + 模型列表标题行一处（#139 把首页纯图标入口也改成了带字文案）。
    // 只断言数量是为了防“改回纯图标”回退：纯图标入口在触屏/小字号下不可发现。
    ["按钮文案含「🔍 查询模型」（设置内两处 + 模型列表标题行一处）",
      (html.match(/🔍 查询模型/g) || []).length === 3],
    ["模型降级链标题右侧有查找入口", /模型降级链[\s\S]{0,80}data-model-search/.test(html)],
    ["思考强度标题右侧有查找入口", /思考强度[\s\S]{0,80}data-model-search/.test(html)],
    ["app.js 统一接线 data-model-search → openModelSearch",
      /querySelectorAll\("\[data-model-search\]"\)/.test(raw) && raw.includes("el.onclick = openModelSearch")],
    ["style.css 有 .cfg-search-btn 样式（宽度自适应，与首页 search-btn 同源）",
      css.includes(".cfg-heading .cfg-search-btn")],
    // 只断言选择器存在会给假绿：本按钮带 .icon-btn（width:30px 纯图标尺寸），
    // 规则里漏写 width 时标签会挣出边框盒（实测 30px 盒 / 68px 文字 / overflow 默认 visible）。
    ["该规则显式声明 width:auto（覆盖 .icon-btn 的 30px）",
      /\.cfg-heading \.cfg-search-btn \{[^}]*\bwidth:\s*auto/.test(css)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 自定义渠道（R66）：设置面板接线与样式 ----
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    ["设置弹层有自定义渠道编辑区", html.includes('id="customChList"') && html.includes('id="inCustomName"') && html.includes('id="inCustomURL"')],
    ["app.js 接线自定义渠道测试/添加/保存按钮", raw.includes('$("btnCustomTest").onclick') && raw.includes('$("btnCustomAdd").onclick') && raw.includes('$("btnCustomSave").onclick')],
    ["app.js 调 POST /api/config/custom_channels", raw.includes('"/api/config/custom_channels"')],
    ["app.js 调 POST /api/config/custom_channel_test", raw.includes('"/api/config/custom_channel_test"')],
    ["openSettings 从 state.custom_channels 初始化编辑态", /editCustomChannels = JSON\.parse\(JSON\.stringify\(state\.custom_channels \|\| \[\]\)\)/.test(raw)],
    // 内建同名拒绝（R66）：前后端各自守一道——前端给即时反馈，后端 Validate 会拒。
    ["前端有内建渠道名保留集（防与内建同名）", raw.includes("RESERVED_CHANNEL_NAMES") && raw.includes('"workbuddy"') && raw.includes('"cline-free"')],
    ["后端拒绝内建同名的文案已在 hint 写清", html.includes("不得与内建渠道同名")],
    // 测试按钮必须带 name（否则脱敏回显态的 api_key 无法在后端找回真实密钥，恒 401）
    ["自定义渠道测试请求携带 name（后端子据以找回被脱敏的真实 api_key）",
      /custom_channel_test[\s\S]{0,90}name[:,]/.test(raw)],
    ["style.css 有自定义渠道徽章颜色", css.includes(".badge.custom")],
    // 悬空 DOM 引用检测（本轮实测踩坑）：app.js 里 $("x") 指向的 id 必须在 index.html 存在，
    // 否则 openSettings() 直接抛 TypeError: Cannot set properties of null 导致弹层打不开——
    // 不报错到日志、静态检查也全绿，属 R34/R41 家族的「声明了却没接线」。
    // 例外：creditTip 由 JS 动态创建（见 newCreditTip），故白名单放行。
    (() => {
      const dyn = new Set(["creditTip"]);
      const ids = new Set([...html.matchAll(/id="([A-Za-z0-9_-]+)"/g)].map((m) => m[1]));
      const missing = [...new Set([...raw.matchAll(/\$\("([A-Za-z0-9_-]+)"\)/g)].map((m) => m[1]))]
        .filter((id) => !ids.has(id) && !dyn.has(id));
      return ["app.js 的 $() DOM 引用无悬空（防 openSettings 抛 TypeError 打不开弹层）", missing.length === 0];
    })(),
    // ---- 跟进项（PR #121：自定义渠道 UI 打磨，R66 延续）----
    ["模型添加按钮为 ➕ emoji（紧凑单行）", raw.includes('data-add-model="${i}"') && raw.includes("➕")],
    ["上下文/输出输入支持 k/M 单位（parseTokens）", raw.includes("function parseTokens(") && raw.includes("function formatTokenCount(")],
    ["上下文/输出默认值使用 1M / 64k 单位串", raw.includes('value="1M"') && raw.includes('value="64k"')],
    ["模型行含工具能力勾选（supports_tools）", raw.includes('data-m-tools="${i}"') && raw.includes("supports_tools: hasTools")],
    ["模型表格有固定列宽 colgroup（对齐）", raw.includes('<colgroup>') && css.includes("table-layout: fixed")],
    ["openSettings 不再引用已移除的 inCustomModels", !raw.includes('$("inCustomModels")')],
    ["模型添加行回车快捷提交", raw.includes("模型添加输入框回车快捷添加")],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- overlay 嵌套完整性（issue #125）----
// 起因：v2.7.3 的「思考强度」cfg-section 少写一个 </div> ⇒ settingsOverlay 提前“吃掉”
// 其后的 rename/help/about/modelSearch/**adminLogin** 五个弹层。DOM 上 JS 已去掉 hidden 且
// display:flex，但父级 settingsOverlay.hidden 为 display:none，子树布局恒 0×0 ⇒ 用户开了
// admin_password 却看不到登录弹层、点任何按钮无反应，而 JS/后端全都正常（零报错零崩溃）。
// 与 R34/R41 同族「声明了却没接线」，只是这次是 HTML 结构层面的静默失效，故静态锁住。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const opens = (html.match(/<div\b/g) || []).length;
  const closes = (html.match(/<\/div>/g) || []).length;
  // 从任一 overlay 起点按花括号/标签配平找到它的匹配 </div>（返回闭合处的下标，找不到返回 -1）。
  const matchClose = (from) => {
    let depth = 0;
    const re = /<div\b|<\/div>/g;
    re.lastIndex = from;
    for (let m; (m = re.exec(html)); ) {
      if (m[0] === "</div>") {
        if (--depth === 0) return m.index;
      } else depth++;
    }
    return -1;
  };
  const start = html.indexOf('id="settingsOverlay"');
  const closeAt = start < 0 ? -1 : matchClose(html.lastIndexOf("<", start));
  const adminAt = html.indexOf('id="adminLoginOverlay"');
  const need2 = [
    ["index.html 的 <div> 开闭数量配平（少一个 </div> 会把后续弹层全卷进上一层）", opens === closes],
    ["settingsOverlay 有匹配闭合标签（未闭合即 issue #125 复现）", closeAt >= 0],
    ["adminLoginOverlay 位于 settingsOverlay **之外**（登录层必须在 body 直系，否则被 hidden 父级压成 0×0）",
      closeAt > 0 && adminAt > closeAt],
  ];
  for (const [label, pass] of need2) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 局域网/无头部署的剪贴板降级（issue #142 / PR #143）----
// 明文 http:// 是非安全上下文，navigator.clipboard 被浏览器直接禁用。这类缺陷在桌面
// localhost（安全上下文）**测不出来**，只在局域网部署暴露，故静态锁住：copyUrl 必须
// 走带 execCommand 降级的 copyText，且不得裸调 navigator.clipboard。
{
  const body = (raw.match(/function copyUrl\(\)[\s\S]*?\n}/) || [""])[0]
    .replace(/\/\/.*$/gm, ""); // 去行注释：注释里会提到 navigator.clipboard（不得误判）
  const need3 = [
    ["copyUrl 函数存在", body.length > 0],
    ["copyUrl 走 copyText（含 execCommand 降级）", /copyText\(/.test(body)],
    ["copyUrl 不得裸调 navigator.clipboard（局域网 http 下静默失效）", !/navigator\.clipboard/.test(body)],
    ["copyText 内部有 execCommand 降级分支", /document\.execCommand\("copy"\)/.test(raw)],
    // noopener 下 window.open 返回恒为 null（MDN），拿返回值判拦截会每次误报。
    ["OfficeAce 打开授权页不得用 noopener 的返回值判弹窗拦截",
      !/window\.open\([^)]*noopener[^)]*\)\s*\)/.test(raw)],
  ];
  for (const [label, pass] of need3) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 积分明细浮窗（发布级回归：点积分数字什么都不出现）----
// 起因（2026-10-17 用户报告「账号面板里点积分数字，积分明细浮窗不显示」）：
// Token 计数面板那次改动把 `uid` 从 renderCreditDetail 的作用域里带走了 ——
// renderCreditDetail() 内部写成 `state.accounts.find((x) => x.uid === uid)`，
// 而 `uid` 只是 showCreditDetail(e, uid) 的形参。ReferenceError 抛在
// `tip.innerHTML = html` 之前 ⇒ 浮窗 display:block 但 innerHTML 是空串，用户看到「什么都没发生」。
//
// 这一族缺陷的共同点是「运行期才炸 / 静默 return」，纯 HTML 断言抓不到：
//   ① 未定义标识符（ReferenceError，只在有 items 的真实响应上才会走到）；
//   ② catch 静默 return（接口 400/网络错时零提示）；
//   ③ 空 items 静默 return（渠道没下发明细时零提示）。
// 故这里同时做「源码作用域检查」与「真跑一遍渲染」两层，避免只测到表面。
{
  const need3 = [];
  const check3 = (label, pass, extra) => need3.push([label, pass, extra]);

  // ---- ① renderCreditDetail 内不得出现裸 uid ----
  // 取函数体：从 `function renderCreditDetail(` 起到下一个顶层 `}` 单独成行的位置。
  const fnBody = (name) => {
    const at = raw.indexOf(`function ${name}(`);
    if (at < 0) return "";
    const brace = raw.indexOf("{", at);
    let depth = 0;
    for (let i = brace; i < raw.length; i++) {
      if (raw[i] === "{") depth++;
      else if (raw[i] === "}" && --depth === 0) return raw.slice(brace, i + 1);
    }
    return "";
  };
  // 去注释与字符串/模板串，只留代码标识符（避免注释里写 `uid` 造成误判）。
  const codeOnly = (s) => s
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/\/\/[^\n]*/g, " ")
    .replace(/`(?:[^`\\]|\\.)*`/g, '""')
    .replace(/"(?:[^"\\]|\\.)*"/g, '""')
    .replace(/'(?:[^'\\]|\\.)*'/g, '""');

  const rcd = fnBody("renderCreditDetail");
  check3("renderCreditDetail 函数体可定位", rcd.length > 200, `len=${rcd.length}`);
  const rcdCode = codeOnly(rcd);
  // 裸 uid = 前面不是 `.`（成员访问）也不是标识符字符；排除 `uid=` 这类形参声明也不该有。
  // 允许的出现形式只有 detailState.uid / xxx.uid。
  const bareUidMatches = [...rcdCode.matchAll(/(^|[^.\w$])uid\b/g)].map((m) => m.index);
  check3(
    "renderCreditDetail 内无裸 uid（ReferenceError: uid is not defined 的根因）",
    bareUidMatches.length === 0,
    bareUidMatches.length ? `命中 ${bareUidMatches.length} 处：${rcdCode.slice(Math.max(0, bareUidMatches[0] - 40), bareUidMatches[0] + 40)}` : ""
  );
  check3(
    "renderCreditDetail 用 detailState.uid 取账号（单一来源）",
    /detailState\.uid/.test(rcdCode)
  );

  // ---- ② 真跑一遍 renderCreditDetail ----
  // 用「有 items 的正常响应」喂进去：这正是用户点有积分账号时走到的路径。
  // 旧实现会在 329 行抛 ReferenceError（此时 innerHTML 仍为空串）⇒ 断言失败；修复后应正常上屏。
  // ⚠️ 必须断言**明细正文**（detail-table / 小计），不能只断言「innerHTML 非空」：
  //    修复后失败路径会渲染提示条（也是非空），只判长度会放过「渲染了但渲染的是错误提示」。
  {
    const tipNode = document.getElementById("creditTip");
    tipNode.innerHTML = "";
    // 通过注入的 setter 直接摆好 detailState + detailCache（两者是 app.js 顶层 let）。
    T.__setCreditDetailState({ uid: "4298452267967801", page: 0 }, {
      items: [
        { name: "每日签到", total: 2000, used: 100, remain: 1900, usable: true, expire_at: "2026-12-01" },
        { name: "奖励积分", total: 800, used: 0, remain: 786, usable: true },
      ],
      usable_remain: 2686, unusable_remain: 0, expiring_remain: 100,
    });
    let threw = "";
    try { T.renderCreditDetail(); } catch (e) { threw = `${e.constructor.name}: ${e.message}`; }
    const html = tipNode.innerHTML;
    check3("renderCreditDetail 在有明细时不抛异常", threw === "", threw);
    check3("明细表格已上屏（detail-table）", html.includes("detail-table"), `htmlLen=${html.length}`);
    check3("浮窗含明细标题「积分明细」", html.includes("积分明细"));
    check3("浮窗含明细行（每日签到 / 奖励积分）", html.includes("每日签到") && html.includes("奖励积分"));
    check3("浮窗含可用小计 2686", html.includes("2686"), html.slice(0, 200));
    check3("明细渲染时不得退化成提示条", !html.includes("detail-empty"), html.slice(0, 200));
  }

  // ---- ②b 真跑一遍空明细分支 ----
  // 空 items 以前是静默 return（用户点了没反应）；现在必须在浮窗里给出可见解释。
  {
    const tipNode = document.getElementById("creditTip");
    tipNode.innerHTML = "";
    T.__setCreditDetailState({ uid: "4298452267967801", page: 0 }, { items: [], usable_remain: 0, unusable_remain: 0 });
    let threw = "";
    try { T.renderCreditDetail(); } catch (e) { threw = `${e.constructor.name}: ${e.message}`; }
    const html = tipNode.innerHTML;
    check3("空明细：renderCreditDetail 不抛异常", threw === "", threw);
    check3("空明细：浮窗显示可见提示（detail-empty）", html.includes("detail-empty"), html.slice(0, 160));
    check3("空明细：提示文案说明「暂无…明细」", html.includes("暂无") && html.includes("明细"), html.slice(0, 160));
  }

  // ---- ③ showCreditDetail 的 catch 不得静默 return ----
  // 接口报错（400/网络错）时必须 toast 出原因，否则用户点了永远「没反应」。
  const scd = fnBody("showCreditDetail");
  check3("showCreditDetail 函数体可定位", scd.length > 100, `len=${scd.length}`);
  const scdCode = codeOnly(scd);
  const catchIdx = scdCode.indexOf("catch");
  check3("showCreditDetail 有 catch 分支", catchIdx >= 0);
  if (catchIdx >= 0) {
    // 取 catch 之后到函数体末尾的片段，断言里面有可见提示调用。
    const tail = scdCode.slice(catchIdx);
    check3("showCreditDetail 的 catch 不再静默 return（含 toast/可见提示）", /toast\(/.test(tail), tail.slice(0, 120));
  }

  // ---- ④ 空明细 / 无条目时也不能静默 ----
  // 后端对 oczen/kilo/custom 直接 400，其余渠道若 items 为空也必须让用户看到解释。
  // 判据用函数定义（不是正则字面量）：/creditTipNotice/ 那种写法会匹配到本文件自己的正则。
  check3("空明细提示函数存在（renderCreditTipNotice）", /function renderCreditTipNotice\(/.test(raw));
  check3("showCreditDetail 空 items 分支也给出提示（不再静默）",
    /if \(!d \|\| !d\.items \|\| !d\.items\.length\) \{\s*renderCreditTipNotice\(/.test(raw));
  check3(".detail-empty 有样式（提示条可读）", readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8").includes(".credit-tip .detail-empty"));

  for (const [label, pass, extra] of need3) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}${!pass && extra ? `  ← ${extra}` : ""}`);
    if (!pass) failed++;
  }
}

// ---- Qoder 双区签到：结果必须按 msg 区分「领到了」与「还没发」（2026-10-18）----
// 背景：campaigns 路径有三个都是 ok=true 的结果（真领到 / 今日已领 / 上游还没发可领活动），
// 旧 toast 一律写「签到成功：…」，把 no_campaign 也说成成功 —— 这是 10-17「签到没实现」
// 误判的来源之一。断言 checkinToast 存在，且 checkin() 走它而不是内联三元表达式。
{
  const need = [
    ["checkinToast 存在（按 msg 区分签到结果）", /function checkinToast\(/.test(raw)],
    ["checkin 走 checkinToast（不再内联「签到成功：」）",
      /toast\(checkinToast\(r\)\)/.test(raw) && !/toast\(r\.ok \? `签到成功：/.test(raw)],
    ["checkinToast 区分「今日已领过」", /今日已领过/.test(raw)],
    ["checkinToast 区分「暂未领到」", /暂未领到/.test(raw)],
    // 独立复核（2026-10-18）抓到：ok=true 但 msg 是「领取失败 http 400」时旧实现仍说「暂未领到」，
    // 等于换了说法的假成功。断言失败判据存在且排在「含 +N」之前。
    ["checkinToast 有失败档（ok=true 的真失败不得说成「暂未领到」）", /签到失败：/.test(raw) && /CHECKIN_FAIL_RE = \/.+\/i;/.test(raw)],
    // 2026-10-18 用户实测定性：活动资格由上游按账号授予，不是每个号都有。
    // 无资格的 msg 既不含「失败」也不含错误码，若不单独成档会落到兜底被说成「暂未领到」，
    // 再叠加「窗口内会自动重试」⇒ 用户白等一个永远不来的结果（「以为工具没实现」的最后来源）。
    ["无领取资格档存在（该状态不得说成「暂未领到」）",
      /CHECKIN_NO_ELIGIBILITY_RE = \/.+\;/.test(raw) && /无领取资格：/.test(raw)],
    ["无资格判据先于失败判据（否则会先被兜底/失败分支吃掉）",
      raw.indexOf('CHECKIN_NO_ELIGIBILITY_RE.test(msg)') > 0 &&
      raw.indexOf('CHECKIN_NO_ELIGIBILITY_RE.test(msg)') < raw.indexOf('CHECKIN_FAIL_RE.test(msg)')],
    ["checkin() 对无资格不再说「会自动重试」（先判无资格，再判 retryable）",
      raw.indexOf('CHECKIN_NO_ELIGIBILITY_RE.test(r.msg') > 0 &&
      raw.indexOf('CHECKIN_NO_ELIGIBILITY_RE.test(r.msg') < raw.indexOf('!r.ok && r.retryable')],
    ["失败判据先于 \\+N 判据（否则「领取失败 http 400」会被数字命中）",
      raw.indexOf('CHECKIN_FAIL_RE.test(msg)') > 0 && raw.indexOf('CHECKIN_FAIL_RE.test(msg)') < raw.indexOf('/\\+-?\\d+/.test(msg)')],
    ["qodercom 不在无签到集合（campaigns 活动路径已实装）", !/NO_EXPLICIT_CHECKIN[^)]*"qodercom"/.test(raw)],
    ["qodercn 不在无签到集合", !/NO_EXPLICIT_CHECKIN[^)]*"qodercn"/.test(raw)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

console.log(failed === 0 ? "\n全部通过" : `\n${failed} 项失败`);
process.exit(failed === 0 ? 0 : 1);

