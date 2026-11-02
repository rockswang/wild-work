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
    // 虚拟账号（场景 1.7）：oczen/kilo 这类「没有账号概念」的通道，账号不是用户添加的，
    // 卡片上一串「不适用」纯噪声，故默认收起。**收起判据是「虚拟账号」而不是 credits_na**：
    // credits_na 只表达「上游没有余额接口」，与「该不该收起」正交 —— 用户亲手粘凭据
    // 加进来的 cline/comate/自定义渠道账号同样 credits_na，但必须立即可见
    // （曾按 credits_na 收起，表现为「贴完 wps_sid 账号没刷出来」）。
    {
      uid: "oczen-anon",
      group: "oczen",
      nickname: "匿名",
      credits: 0,
      expiring_credits: 0,
      unusable_credits: 0,
      credits_na: true,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "",
      last_checkin_ok: false,
    },
    // 有真实积分的渠道（R136）：comate 一份 wps_sid = 一个 WPS 账号，
    // 额度来自 /llmproxy/v1/user/token-usage（每日额度 2000，剩余 1857.37），
    // 故 credits_na 为 false、卡片主数字是真实剩余，与 cline（真查不到）区分开。
    {
      uid: "sid-0123456789abcdef",
      group: "comate",
      nickname: "粘错sid的号",
      credits: 1857,
      expiring_credits: 0,
      unusable_credits: 0,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "",
      last_checkin_ok: false,
    },
    // 真·无积分但用户添加的账号（cline）：上游确无余额端点 ⇒ credits_na=true。
    // 留着它才能持续验证「credits_na 不决定收起、也不决定固定账号」（R130/R131 的护栏）。
    {
      uid: "cline-abcdef123456",
      group: "cline",
      nickname: "cline无积分号",
      credits: 0,
      expiring_credits: 0,
      unusable_credits: 0,
      credits_na: true,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "",
      last_checkin_ok: false,
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
      // toggle 供 applyAcctFilter / 「不适用」收起提示使用；缺了它会在 stub 里
      // 抛 TypeError，让整份检查在第一个 toggle 处就中断（不是断言失败，是崩）。
      toggle: (c, force) => {
        const on = force === undefined ? !cls.has(c) : Boolean(force);
        if (on) cls.add(c); else cls.delete(c);
        return on;
      },
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
  // 降级链别名（R129）前端即时校验：fbRules / fbAliasValidate 是 `let` 与模块内函数，
  // 由改写注入 setter + 校验入口，供「硬拒分支 + 软警告不阻止保存」的断言直接调用。
  + ", fbAliasValidate, __setFbRules: (rs) => { fbRules = rs; }"
  + ", renderCreditDetail, showCreditDetail, __setCreditDetailState: (st, resp) => { detailState = st; detailCache = { [st.uid]: resp }; }"
  // 「不适用」默认收起（R103）：showNoCredit / acctFilter 都是 `let`，toggle 也要直接调
  + ", applyAcctFilter, toggleNoCredit, __setShowNoCredit: (v) => { showNoCredit = v; }, __setAcctFilter: (v) => { acctFilter = v; }"
  // 停用原因徽标（R135）：需要临时把一条 fixture 账号改成「已停用 + 指定原因」再重渲染。
  + ", __setAccount: (uid, patch) => { const a = (state.accounts || []).find((x) => x.uid === uid); if (a) Object.assign(a, patch); }"
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

// ---- 场景 1.8：停用原因徽标（R135）----
// 为什么需要：账号被「自动停用」时（R135 的典型场景——这个 WPS 账号在 Comate 侧
// 一个模型都没有），卡片原先只有灰底 + ▶，用户既不知道为什么被停，也不知道点 ▶ 有没有用。
// 变异点：删掉 disabledTag 的模板拼接，本节立刻变红。
T.__setAccount("1606848467182378", {
  cooling: false, until: "", until_ts: 0,
  disabled: true, reason: "该账号没有可用模型（未开通 Comate），已自动停用",
});
T.renderAccounts();
const disCards = document.getElementById("acctList").innerHTML;
check("停用账号卡片显示「已停用 · 原因」徽标", /已停用 · 该账号没有可用模型/.test(disCards));
check("停用原因完整写进 title（不被截断）",
  disCards.includes('title="该账号没有可用模型（未开通 Comate），已自动停用"'));
check("停用徽标只出现在被停用的账号上（健康号不带）",
  (disCards.match(/已停用 · /g) || []).length === 1,
  `实际 ${(disCards.match(/已停用 · /g) || []).length} 个`);
T.__setAccount("1606848467182378", { disabled: false, reason: "" });
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

// ---- 场景 1.75：真实账号可删可停 + 有积分才有「刷新积分」（R130/R136，comate/cline）----
// 独立缺陷：旧判据 `fixedAcct = noCredits && !isCustomGroup(...)` 把 comate 当成
// 「固定账号」⇒ 卡片只剩 🔒，无删除/停用/改名入口；而 comate 是真多账号池
// （一份 wps_sid = 一个 WPS 账号，后端 RemoveAccount/DisableAccount 只硬拒 oczen/kilo）
// ⇒ 粘错 sid 就永久卡死。这里锁住「无积分≠固定账号」两个维度必须分开。
// R136 起 comate 有真实额度端点，「无积分」这一侧改由 cline 承担护栏。
{
  const cardOf = (name) =>
    cards.split('<div class="acct-card').find((seg) => seg.includes(name)) || "";
  const comateCard = cardOf("粘错sid的号");
  check("comate 卡片渲染出来了", comateCard !== "");
  check("comate 卡片**不得**是固定账号（无 🔒）", !comateCard.includes("固定账号，不可停用/删除"), comateCard.slice(0, 200));
  check("comate 卡片有删除入口（粘错 sid 能删）", /onclick="removeAcct\('sid-/.test(comateCard), comateCard.slice(0, 400));
  check("comate 卡片有停用入口", /onclick="toggleDisable\('sid-/.test(comateCard));
  check("comate 卡片名字可改（非固定账号才可编辑）", /class="acct-name editable"/.test(comateCard));
  // R136：comate 额度查得到（token-usage）⇒ 必须给「刷新积分」入口，不再是「不适用」。
  check("comate 有积分 ⇒ 给「刷新积分」按钮", comateCard.includes("refreshOne('sid-"));
  // 反向护栏：真查不到余额的 cline 仍不得给刷新入口（点下去只会报错）。
  const clineCard = cardOf("cline无积分号");
  check("cline 卡片渲染出来了", clineCard !== "");
  check("cline 无积分 ⇒ 不给「刷新积分」按钮", !clineCard.includes("refreshOne('cline-"));
  check("cline 同样**不得**是固定账号（无积分≠固定账号）",
    !clineCard.includes("固定账号，不可停用/删除") && /onclick="removeAcct\('cline-/.test(clineCard),
    clineCard.slice(0, 300));
  // 反向护栏：虚拟账号（oczen）必须仍然只有 🔒（否则「解耦」就解过头了）
  const oczenCard = cardOf("匿名");
  check("oczen 仍是固定账号（🔒 + 无删除入口）",
    oczenCard.includes("固定账号，不可停用/删除") && !oczenCard.includes("removeAcct('oczen"));
  check("VIRTUAL_ACCOUNTS 只含 oczen/kilo（与无积分名单是两个维度）",
    /const VIRTUAL_ACCOUNTS = new Set\(\["oczen", "kilo"\]\)/.test(raw));
}

// ---- 场景 1.7：「不适用」账号默认收起 + 一键展开（R103，2026-10-19 用户要求）----
// 需求原话：「把不适用的默认隐藏掉，但是有时候又得看一下」。
// 难点不在隐藏，而在「默认值 + 可逆 + 状态不丢」三件事同时成立：
//   ① 首帧就必须是收起态（不能先闪一排「不适用」再消失）；
//   ② 展开入口不能藏在设置里（否则「有时候得看一下」变成要翻两层）；
//   ③ 收起后不能把用户卡住（若正筛选着某不适用渠道，得自动清筛选，否则网格空白且
//      连「全部」chip 都随汇总条消失）。
{
  const naCardOf = () => cards.split('<div class="acct-card').find((seg) => seg.includes("匿名")) || "";
  const healthyCardOf = () => cards.split('<div class="acct-card').find((seg) => seg.includes("健康号")) || "";
  // 注意：按 '<div class="acct-card' 切分后，段内已不含 `class="acct-card`，
  // 故断言从 `hidden" data-group=` 这一段开始匹配（直接拿 class= 开头会永远不命中）。
  check("默认收起：虚拟账号卡片带 hidden 且 data-na=1",
    /^ hidden" data-group="oczen" data-na="1"/.test(naCardOf()),
    naCardOf().slice(0, 120));
  check("默认收起：有余额的卡片不带 hidden（防「一刀切全收起」）",
    !/^[^"]*hidden/.test(healthyCardOf()), healthyCardOf().slice(0, 120));
  // 收回合判据必须是「虚拟账号」：曾用 credits_na，导致用户粘完凭据（cline/comate/
  // 自定义渠道）后卡片带 hidden，表现为「账号没刷出来」（R131）。
  check("data-na 由虚拟账号名单驱动（isVirtualAcct），不是 credits_na",
    /data-na="\$\{isVirtualAcct\(a\) \? 1 : 0\}"/.test(raw)
      && !/naHiddenClass = \(!showNoCredit && isNoCreditAcct\(a\)\)/.test(raw));
  {
    // 反例护栏：fixture 里的 cline 账号（credits_na=true，用户亲手粘 sk_ key 加的）
    // 必须默认可见 —— 这就是用户报的「贴进去 wps_sid 后账号没刷出来」的同款形态。
    const clineCard = cards.split('<div class="acct-card').find((seg) => seg.includes("cline无积分号")) || "";
    const head = clineCard.slice(0, clineCard.indexOf(">"));
    check("用户添加的无积分账号（cline）默认不收起",
      clineCard !== "" && !head.includes("hidden") && head.includes('data-na="0"'),
      head.slice(0, 160));
    // R136：有真实额度的 comate 同样默认可见。
    const comateCard2 = cards.split('<div class="acct-card').find((seg) => seg.includes("粘错sid的号")) || "";
    const head2 = comateCard2.slice(0, comateCard2.indexOf(">"));
    check("comate 账号默认不收起", comateCard2 !== "" && !head2.includes("hidden"), head2.slice(0, 160));
  }

  // 汇总条：收起态下不渲染 oczen 的「不适用」chip，只留一枚可点的开关。
  T.__setShowNoCredit(false);
  T.renderSummary();
  const sumOff = document.getElementById("creditSummary").innerHTML;
  check("收起时汇总条不出现 oczen 的渠道 chip", !/data-wg="oczen"/.test(sumOff), sumOff.slice(0, 200));
  check("收起时汇总条保留「不适用」开关 chip（入口在原地）",
    /data-na-toggle="1"/.test(sumOff) && sumOff.includes("不适用"));
  // 被收起的只有虚拟账号（oczen 匿名）→ 1 号。comate 虽同样 credits_na，
  // 但它是用户添加的真实账号，不进「被收起」计数（否则数字会误导人去「展开找回」）。
  check("开关 chip 只统计被收起的虚拟账号（1号）", /<span class="cs-num">1号<\/span>/.test(sumOff), sumOff.slice(0, 300));
  // 反向护栏：用户添加的无积分渠道 chip 常驻汇总条（贴完就能看见渠道 N 号）。
  check("收起时 comate 渠道 chip 仍在（用户添加的渠道不被收起）",
    /data-wg="comate"/.test(sumOff), sumOff.slice(0, 400));
  check("收起态开关不亮 on 高亮", !/cs-chip clickable cs-na on"/.test(sumOff));

  // 展开态：chip 回来、开关变 on、账号卡片不再 hidden。
  T.__setShowNoCredit(true);
  T.renderSummary();
  const sumOn = document.getElementById("creditSummary").innerHTML;
  check("展开时 oczen 渠道 chip 回来", /data-wg="oczen"/.test(sumOn), sumOn.slice(0, 200));
  check("展开态开关带 on 高亮", /cs-chip clickable cs-na on"/.test(sumOn));
  check("展开时开关文案变「收起」", sumOn.includes("收起"));

  // toggleNoCredit 的副作用：持久化 + 重渲染（不能只改内存变量）。
  T.__setShowNoCredit(false);
  T.toggleNoCredit();
  check("toggleNoCredit 一次即展开（内存状态翻转）", /data-wg="oczen"/.test(document.getElementById("creditSummary").innerHTML));
  check("toggleNoCredit 会重渲染账号卡片（不是只改汇总条）",
    !/^ hidden" data-group="oczen"/.test(
      document.getElementById("acctList").innerHTML
        .split('<div class="acct-card').find((seg) => seg.includes("匿名")) || "hidden"));
  T.toggleNoCredit();
  check("再点一次收起（可逆）", !/data-wg="oczen"/.test(document.getElementById("creditSummary").innerHTML));

  // 卡住场景：筛选着一个「不适用」渠道时收起，必须自动清筛选（否则网格空白）。
  T.__setShowNoCredit(true);
  T.__setAcctFilter("oczen");
  T.toggleNoCredit(); // → 收起，应顺手把 acctFilter 清掉
  T.renderSummary();
  const sumAfter = document.getElementById("creditSummary").innerHTML;
  check("收起时自动清掉指向「不适用」渠道的筛选（不留白网格）",
    !/cs-all/.test(sumAfter), "「全部」chip 仍在 = 筛选未清");

  // 静态接线：开关必须绑在汇总条事件委派里，且默认值是 false（默认收起）。
  check("汇总条点击委派里有 data-na-toggle 分支",
    /if \(target\.closest\("\[data-na-toggle\]"\)\) \{ toggleNoCredit\(\); return; \}/.test(raw));
  check("默认收起：showNoCredit 初始为 false（仅 localStorage=1 才展开）",
    /let showNoCredit = false;/.test(raw) && raw.includes('localStorage.getItem(SHOW_NA_KEY) === "1"'));
  check("展开选择持久化到 localStorage", raw.includes("localStorage.setItem(SHOW_NA_KEY"));
  check("全部被收起时给出可执行提示（不留空白页）",
    /id="acctHiddenTip"/.test(
      readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8"))
    && raw.includes('$("acctHiddenTip")'));

  // 复原，避免影响后续场景的汇总条断言。
  T.__setShowNoCredit(false);
  T.__setAcctFilter(null);
  T.renderAccounts();
  T.renderSummary();
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
  // expire_today(77) 故意与 credit_expired(100) 取不同值：断言才能分辨「今日作废」
  // 是否被误接成「今日到期」（两者语义相反，混列会把作废说成还没发生）。
  // 三个到期列 77/200/300 互不相同：任一列接错（如 7 日内接成今日+明日）
  // 都会让断言变红——同值时测不出来（R102 的假绿教训）。
  expiring_7d: 300, expiring_7d_accts: 1, expire_today: 77, expire_tomorrow: 200,
  platforms: [{
    group: "traework", accounts: 2, credits: 4736, credit_in: 200, credit_out: 150,
    credit_expired: 100, credit_used: 50, tokens: 12345, expire_7d: 300, expire_today: 77, expire_tomorrow: 200,
  }, {
    // 全零渠道：守「零值列置灰」断言。R103 把上面那行的 expire_tomorrow 从 0 改成 200
    // 后，原本靠它提供的零值没了，故显式补一行零值渠道（也顺便覆盖「小渠道整行剔除」之外
    // 的渲染路径）。
    group: "lobsterai", accounts: 0, credits: 0, credit_in: 0, credit_out: 0,
    credit_expired: 0, credit_used: 0, tokens: 0, expire_7d: 0, expire_today: 0, expire_tomorrow: 0,
  }],
  models: [{ model: "traework/DeepSeek-V4", reqs: 3, tokens: 5000, credit: 12.5, avg_ttfb_ms: 800, tok_per_sec: 45.2, tput_window_sec: 60, perf_samples: 3 }],
  // hours_left 与 days_left 故意不同步（5.7h ⇒ days_left=0）：断言「剩余」列优先用
  // 精确小时数。若接成 days_left，会渲染成「今日到期」而看不出还剩 5.7 小时。
  // 两行：① 能外推（forecast=45，预计作废 55）；② **无法外推**（forecast=-1，
  // 今日没消耗）——后者必须按「全部额度作废」显示，不能出「预计作废 -1分」反数。
  // 三行覆盖三档配色（urgency）：
  //   ① today → 红；② tomorrow → 黄；③ later → 不上色。
  // 注意 ③ 的 risk 故意给 true：颜色必须跟 urgency 走，不跟 risk 走——
  // 此前用 risk 上色导致 7 天内的行全红（135h 后到期的也红，用户实测反馈）。
  expiry: [
    { uid: "4298452267967801", nickname: "健康号", group: "traework", expire_at: "2026-10-20T00:00:00+08:00", hours_left: 5.7, days_left: 0, amount: 100, today_out: 30, burn_forecast: 45, risk: true, urgency: "today" },
    { uid: "4298452267967802", nickname: "闲置号", group: "qwenwork", expire_at: "2026-10-20T08:45:21+08:00", hours_left: 4.7, days_left: 0, amount: 100, today_out: 0, burn_forecast: -1, risk: true, urgency: "today" },
    { uid: "4298452267967803", nickname: "明日号", group: "traework", expire_at: "2026-10-21T09:32:32+08:00", hours_left: 19.5, days_left: 1, amount: 115, today_out: 10, burn_forecast: 400, risk: false, urgency: "tomorrow" },
    { uid: "4298452267967804", nickname: "远期号", group: "workbuddyai", expire_at: "2026-10-25T12:06:19+08:00", hours_left: 135.7, days_left: 5, amount: 249, today_out: 0, burn_forecast: -1, risk: true, urgency: "later" },
  ],
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
const htmlEarly = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
check("汇总横条已删（容器与渲染函数都不存在，信息由合计行承接）",
  !raw.includes("function stSumItem") && !raw.includes('$("stSum")') && !/id="stSum"/.test(htmlEarly));
check("合计行消耗列带精确小注（承接旧汇总条唯一独有信息）",
  /st-plat-sub">\(精 /.test(raw) && raw.includes("credit_out_exact"));
check("里程碑计数正确", document.getElementById("stMilestone").textContent === "里程碑 1×100分");
const stToken = document.getElementById("stToken").innerHTML;
check("Token 多天行含今日/7日/已记录", stToken.includes("Token 今日") && stToken.includes("7日") && stToken.includes("已记录") && stToken.includes("12 天"));

const stPlat = document.getElementById("stPlat").innerHTML;
check("平台表渲染渠道徽章（traework）", /badge trae"/.test(stPlat));
check("平台表含合计行（全池口径 4,736）", stPlat.includes("合计") && stPlat.includes("4,736"));
check("平台表零值列置灰（st-plat-zero）", stPlat.includes("st-plat-zero"));
// 「今日作废」独立成列（R101 配套）：此前只藏在合计行 title 里看不见。
// 与「今日到期」是相反方向的口径（往回看已没了 / 往前看今晚会没），不能合列。
check("平台表有今日作废列（表头 + 逐渠道取值）",
  /st-plat-expd[^>]*>今日作废/.test(stPlat) &&
  /st-plat-expd">100</.test(stPlat) /* 逐渠道 credit_expired=100，而 expire_today=77 */);
check("合计行今日作废取 credit_expired（非 expire_today；两者不同源）",
  /st-plat-sum-row[\s\S]*?st-plat-expd">100</.test(stPlat) &&
  !/st-plat-expd">77</.test(stPlat));
// 到期三列（R103）：按精确时刻 + 自然日分桶。三列取值 77/200/300 互不相同，
// 任一列接错（7 日内接成 今日+明日、或三列串位）都会变红。
check("平台表三列到期取值各自独立（77/200/300，未串位）",
  /st-plat-exp">77分</.test(stPlat) &&
  /st-plat-exp">200分</.test(stPlat) &&
  /st-plat-exp">300分</.test(stPlat));
// 「7 日内」是含今日+明日的合计口径，后端已算好总量（300），前端不得自己再相加。
// 若误写成 today+tomorrow+7d 会得到 577。
check("7 日内到期直接取后端合计（300，不是今日+明日+7日 的 577）",
  !stPlat.includes(">577分<") && stPlat.includes(">300分<"));

const stRotation = document.getElementById("stRotation").innerHTML;
check("接棒顺序渲染使用中账号", stRotation.includes("使用中 <b>中招号</b>"));
check("接棒顺序含 ① 候补", stRotation.includes("① 健康号"));

const stExpiry = document.getElementById("stExpiry").innerHTML;
// 配色按紧迫度分档（2026-10-19 用户定义）：今天红、明天黄、更远不上色。
// 关键：远期号 risk=true 但 urgency=later，必须**不上色**——若前端拿 risk 上色会变红。
check("临期配色：今天到期标红（st-risk）", /st-ex-row st-risk[^>]*>[\s\S]*?健康号/.test(stExpiry));
check("临期配色：明天到期标黄（st-warn）", /st-ex-row st-warn[^>]*>[\s\S]*?明日号/.test(stExpiry));
// 远期号 risk=true（按今日速度花不完）但 urgency=later：必须不上色。
// 这条专门锁「颜色跟 urgency 走、不跟 risk 走」——若退回按 risk 上色，
// 远期号会变红，这条立刻红。
check("临期配色：5.7 天后到期不上色（即使 risk=true）",
  (() => {
    // 按行切分后逐行找，不能用 [\s\S]*? 跨行匹配（会串到下一行）
    const rows = stExpiry.split('<div class="st-ex-row').slice(1);
    const far = rows.find((r) => r.includes("远期号"));
    if (!far) return false;
    const cls = (far.match(/^([^"]*)"/) || [])[1] || "";
    return cls.trim() === "";
  })());
// R103：剩余列优先用精确小时（hours_left=5.7）而非自然日差（days_left=0）。
// 若接成 days_left 会渲染「今日到期」，就看不出「只剩 5.7 小时」。
check("临期剩余时长用精确小时（5.7 小时后，非 days_left 文案）",
  stExpiry.includes("5.7 小时后") && !stExpiry.includes("今日到期"));
// 到期时刻必须渲染成完整标准时间「YYYY-MM-DD HH:MM:SS」：
// 后端下发 RFC3339（2026-10-20T00:00:00+08:00），直接显示又长又被列宽截断；
// 秒级信息是判断「今晚几点没」的关键，不能丢。
check("到期时刻渲染为完整标准时间（含时分秒，无 T、无时区后缀）",
  stExpiry.includes("2026-10-20 00:00:00") &&
  !stExpiry.includes("2026-10-20T00:00:00"));
// forecast<0（今日没消耗、无法外推）时按「全部额度作废」显示，
// 不能出现「预计作废 -1分」这种反数。
// forecast<0 的号（今日没消耗）必须显示「预计作废 100分」（全部额度），
// 而不是「预计作废 -1分」这种反数。
check("预计作废不为负（forecast<0 按全部额度）",
  stExpiry.includes("预计作废 100分") &&
  !/预计作废\s*-\d/.test(stExpiry));
check("预计作废量 = 额度−外推消耗（55分）", stExpiry.includes("预计作废 55分"));

const stModels = document.getElementById("stModels").innerHTML;
check("Trae 系模型积分为估算（≈ 前缀）", stModels.includes("≈12.50"));
check("模型当前速度含 tok/s 与 TTFB（滑动窗口口径）", stModels.includes("45.2 tok/s") && stModels.includes("800ms"));
// R147：窗口内无投递（tok_per_sec=0）必须显示「—」，绝不允许渲染 "0 tok/s"
// ——0 会被读成「模型卡死」，而实际含义是「最近窗口内没有流式请求」。
T.renderModels([{ model: "x/y", reqs: 1, tokens: 10, credit: 0, avg_ttfb_ms: 0, tok_per_sec: 0, tput_window_sec: 60, perf_samples: 1 }]);
const stModelsEmpty = document.getElementById("stModels").innerHTML;
check("窗口无样本显示「—」而非 0 tok/s", stModelsEmpty.includes("—") && !stModelsEmpty.includes("0 tok/s"), stModelsEmpty.slice(0, 300));

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
check("运行统计区块骨架存在（stPlat/stApplog）", /id="stPlat"/.test(html) && /id="stApplog"/.test(html));
check("更新角标存在（id=stUpdated，拉取失败可感知）", /id="stUpdated"/.test(html));
// ---- 第四个 tab：模型列表和费率（原独立 section 移入主 tab 组）----
check('第四个 tab 按钮存在（data-mtab="fees"）', /data-mtab="fees"/.test(html));
check("#mtabFees body 存在且默认隐藏", /class="mtab-body hidden" id="mtabFees"/.test(html));
check("模型费率区块骨架存在（feesBox/btnRefreshFees）", /id="feesBox"/.test(html) && /id="btnRefreshFees"/.test(html));
check("模型费率区块在 section 内（不再是独立 section）", /<div class="mtab-body hidden" id="mtabFees">[\s\S]*?<\/section>/.test(html) && !/<!-- 模型列表和费率 -->\s*<section class="panel">/.test(html));
const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
check("style.css 含运行统计样式段（#mtabStats .st-plat-row 等）", /#mtabStats \.st-token/.test(css) && /#mtabStats \.st-plat-row/.test(css));
// ---- Token 多天行单行：不许回退成 wrap（wrap 会在内容长度临界时抖动换行）----
check("Token 多天行 st-token 单行不折行（nowrap，极窄整条截尾）",
  /#mtabStats \.st-token \{[^}]*flex-wrap: nowrap/.test(css) &&
  /#mtabStats \.st-token \{[^}]*overflow: hidden/.test(css) &&
  !/#mtabStats \.st-token \{[^}]*flex-wrap: wrap/.test(css));
// 合计行配色：资产紫/琥珀，锁住防漂移
// 平台表动态列宽：JS 实测经 --plat-cols 应用（固定 px 遇大额数字会截断）
check("表格剩余宽度由平台列吸收（minmax(实测,1fr)），末列既不吸收也不压缩",
  /if \(i === 1\) return `minmax\(\$\{w \+ 2\}px, 1fr\)`/.test(raw) &&
  !/max\.length - 1 \? "max-content"/.test(raw));
check("平台表列宽动态化（--plat-cols 变量 + fitPlatCols 实测），静态兑底含 96px 精确小注位与 72px 作废位",
  /#mtabStats \.st-plat-row \{\n  display: grid;[\s\S]*?grid-template-columns: var\(--plat-cols, 28px minmax\(150px, 1fr\) 52px 100px 80px 96px 72px 112px 84px 84px 84px\)/.test(css) &&
  raw.includes("function fitPlatCols") && raw.includes('style.setProperty("--plat-cols"') &&
  raw.includes("offsetParent") /* 隐藏时跳过 */ &&
  /b\.dataset\.mtab === "stats"[\s\S]{0,200}fitPlatCols\(\)/.test(raw) /* 切回时重测 */ &&
  /\$\("stPlat"\)\.innerHTML = html;\s*fitPlatCols\(\);/.test(raw) /* 渲染后实测，别只留 tab 切换那处 */);
check("合计行当前积分=资产紫、Token=琥珀（与汇总条同色系）",
  /#mtabStats \.st-plat-row\.st-plat-sum-row \.st-plat-credits \{ color: #7c3aed/.test(css) &&
  /#mtabStats \.st-plat-row\.st-plat-sum-row \.st-plat-tokens \{ color: var\(--warn\)/.test(css));
// 今日作废用 danger 色，与「今日消耗」的 warn 色分开（语义相反：真花掉 / 没花就没了）
check("今日作废列配色为 danger（与今日消耗的 warn 区分）",
  /#mtabStats \.st-plat-expd \{ color: var\(--danger\)/.test(css) &&
  !/#mtabStats \.st-plat-out[^\n]*\.st-plat-expd/.test(css));
check("Token 多天行 st-token 单行不折行（nowrap，极窄整条截尾）",
  /#mtabStats \.st-token \{[^}]*flex-wrap: nowrap/.test(css) &&
  /#mtabStats \.st-token \{[^}]*overflow: hidden/.test(css) &&
  !/#mtabStats \.st-token \{[^}]*flex-wrap: wrap/.test(css));
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

const regexOk = (re) => re.test(raw);

// ---- 场景 1.8：Comate 登录三条路径（R132）----
// 起因：分支构建部署到 NAS 后，用户点「一键登录」什么也没发生 —— CDP 拉起的是
// **运行 wild-work 那台机器**的浏览器，而 NAS 没有桌面。结论不是把报错写漂亮，
// 而是换一条与部署环境无关的登录路径：服务端申请二维码（扫码），外加可选的
// 「接管本机浏览器」（用户自己开调试端口时才有）。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  check("弹层带扫码区（服务端申请二维码）", html.includes('id="comateQrImg"') && html.includes('id="btnComateQrStart"'));
  check("弹层带「用本机浏览器登录」区（默认隐藏，由能力探测决定是否显示）",
    html.includes('id="comateBrowser"') && /<div class="cfg-section hidden" id="comateBrowser"/.test(html));
  check("浏览器入口按钮由前端轮询后端状态", regexOk(/btnComateBrowserStart"\).onclick[\s\S]{0,120}startComateBrowser\(\)/));
  check("打开弹层先探测浏览器能力，而不是先发登录请求",
    /async function loadComateBrowserInfo/.test(raw) && /openComateDialog[\s\S]{0,400}loadComateBrowserInfo/.test(raw));
  check("未配置调试端口时整块隐藏（不摆点了必失败的按钮）",
    /function loadComateBrowserInfo[\s\S]{0,900}!info\.configured\) return/.test(raw));
  check("配置了但端口不通时给出原因并禁用按钮",
    /info\.available[\s\S]{0,300}disabled = !info\.available/.test(raw));
  check("扫码轮询走后端状态接口", /comatePollPath = "\/api\/login\/comate_qr_status"/.test(raw));
  check("关闭弹层同时取消扫码与浏览器两条会话",
    /cancelComateLogin[\s\S]{0,400}comate_qr_cancel[\s\S]{0,200}comate_browser_cancel/.test(raw));
  check("手工兜底仍在（整段 Cookie 也能粘）", html.includes('id="comateSidInput"') && html.includes('id="btnComateToggleManual"'));
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
    ["渠道按钮基础为 6 列（消除孤行，按钮数变动时按行自然流）", css.includes("repeat(6, auto)")],
    ["渠道按钮摆位全归 CSS（index.html 不再内联 grid-column，否则窄屏无法重排）", !/btnAdd\w+" style="grid-column/.test(html)],
    // R142（2026-10-30 用户拍板）：渠道展示序唯一规则 = 内建渠道按**显示名的 UTF-8 字节序**升序，
    // 自定义渠道 / 自定义模型**硬编码钉在最末**。三处（面板按钮 / 汇总条 / 后端费率 tab）
    // 必须同序，否则「账号卡片看到的顺序」与「费率 tab 的顺序」互相打脸。
    // 下面不再硬编码一串按钮 id（旧的「列配对」写法与字母序规则冲突，已删除）：
    // 把按钮的 `alt <短类名>` 还原成渠道 key，再断言「它确实按显示名字节序升序 + 自定义垫底」。
    ["渠道按钮 DOM 顺序 = 按渠道显示名字节序升序，自定义模型垫底（R142）",
      (() => {
        const cls2keyOf = (() => {
          const src = raw.match(/const CH_CLASS = (\{[^}]*\});/);
          const m = src ? new Function("return (" + src[1] + ")")() : {};
          const inv = {};
          for (const [k, c] of Object.entries(m)) inv[c] = k;
          return inv;
        })();
        const labelsOf = (() => {
          const src = raw.match(/const CH_LABEL = (\{[^}]*\});/);
          return src ? new Function("return (" + src[1] + ")")() : {};
        })();
        const btns = [...html.matchAll(/<button class="btn primary alt (\S+)" id="(btnAdd\w+|btnOpenCustomModels)"/g)]
          .map((m) => ({ cls: m[1], id: m[2] }));
        const keys = btns.filter((b) => b.id !== "btnOpenCustomModels").map((b) => cls2keyOf[b.cls]);
        const sorted = keys.slice().sort((a, b) => (labelsOf[a] < labelsOf[b] ? -1 : labelsOf[a] > labelsOf[b] ? 1 : 0));
        return btns.length > 0 && btns[btns.length - 1].id === "btnOpenCustomModels" && !keys.includes(undefined) &&
          keys.join("|") === sorted.join("|");
      })()],
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

// ---- 降级链别名（R129）：head 行内输入框 + 只读 chip + 复制 + ? 说明 + 前端校验 ----
// 与上面同族：别名是「后端有字段、面板没接线」就完全不生效的功能，故把接线一并锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const need = [
    // 输入框必须在规则 head 行内（不额外占一行）——渲染模板里 .fb-rule-head 内出现 fb-name-edit
    ["别名输入框渲染在规则 head 行内（不额外占行）",
      /class="fb-rule-head">[\s\S]{0,600}fb-name-edit[\s\S]{0,400}fb-from-edit/.test(raw)],
    ["别名输入框带 data-name-idx 与可选占位提示",
      raw.includes("data-name-idx=") && raw.includes("别名（可选，如 fast）")],
    ["卡片正文有只读别名 chip（「客户端可直接请求」）",
      raw.includes("fb-alias-chip") && raw.includes("客户端可直接请求")],
    ["chip 带复制按钮且接线到 copyText",
      raw.includes("data-copy-alias") && /data-copy-alias\][\s\S]{0,200}copyText\(/.test(raw)],
    ["无别名时 chip 不渲染（条件模板）",
      /\$\{r\.name \?/.test(raw)],
    ["? 说明写明别名语义（等同于主力模型 + 不参与备胎链 + 大小写敏感）",
      html.includes("别名</b>是主力模型的另一个名字") && html.includes("别名不参与备胎链")
      && html.includes("大小写敏感")],
    ["别名输入框 change 提交接线（校验 + 回滚）",
      raw.includes('classList.contains("fb-name-edit")') && raw.includes("不能含 / 、空白、逗号或 *")],
    ["前端即时校验覆盖硬拒分支（斜杠 / 空白逗号星号 / 重名 / model_map / 渠道名）",
      raw.includes("function fbAliasValidate()") && raw.includes("compat.model_map 的键重名")
      && raw.includes("与渠道名重名") && raw.includes("别名不可重复")],
    ["软警告（与真实裸名重合）只提示、不阻止保存",
      html.includes('id="fbAliasWarn"') && /bareNames\.has\(nm\)[\s\S]{0,120}warns\.push/.test(raw)
      && /const \[hardErr\] = fbAliasValidate\(\);[\s\S]{0,120}if \(hardErr\)/.test(raw)],
    ["保存后端硬拒别名含 /（唯一真相在服务端）",
      readFileSync(new URL("../internal/app/app.go", import.meta.url), "utf8").includes("不能含 /（别名是裸名")],
    // 前端即时校验的**行为**断言（不只看代码里有几行）：逐条硬拒必须报错，
    // 裸名重合必须只出软警告（否则用户被硬拦或根本不知情）。
    ["前端即时校验：六条硬拒各一例都报错",
      (() => {
        const cases = [
          [{ from: "a/b", to: ["c/d"], name: "x/y" }],
          [{ from: "a/b", to: ["c/d"], name: "x y" }],
          [{ from: "a/b", to: ["c/d"], name: "x,y" }],
          [{ from: "a/b", to: ["c/d"], name: "x*" }],
          [{ from: "a/b", to: ["c/d"], name: "dup" }, { from: "a/e", to: ["c/d"], name: "dup" }],
          [{ from: "a/b", to: ["c/d"], name: "workbuddy" }],
        ];
        return cases.every((rs) => { T.__setFbRules(rs); return T.fbAliasValidate()[0] !== ""; });
      })()],
    ["前端即时校验：空别名合法（不报错不警告）",
      (() => { T.__setFbRules([{ from: "a/b", to: ["c/d"], name: "" }]);
        const [h, w] = T.fbAliasValidate(); return h === "" && w === ""; })()],
    ["前端即时校验：与真实裸名重合只出软警告（不硬拦）",
      (() => { T.__setFbRules([{ from: "a/b", to: ["c/d"], name: "glm-5.3" }]);
        const [h, w] = T.fbAliasValidate(); return h === "" && w.includes("裸名重合"); })()],
    ["软警告不阻止保存（saveModelFallback 只硬拦 hardErr）",
      raw.includes(`if (hardErr) { $("fbErr").textContent = hardErr; return; }`)],
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
    ["自定义模型是独立弹层（R85：设置里只留一个入口按钮）",
      html.includes('id="customModelsOverlay"') && html.includes('id="customChList"') &&
      html.includes('id="btnOpenCustomModels"')],
    ["主面板的自定义模型入口在「＋ CodeArts」右侧（R86）",
      html.includes('id="btnAddCodeArts"') && html.includes('id="btnOpenCustomModels"') &&
      html.indexOf('id="btnAddCodeArts"') < html.indexOf('id="btnOpenCustomModels"')],
    ["app.js 不再引用 btnCustomChannels", !raw.includes('btnCustomChannels')],
    ["openSettings 不再初始化自定义渠道（职责归 openCustomModels）", (() => {
      const at = raw.indexOf("function openSettings()");
      if (at < 0) return false;
      const brace = raw.indexOf("{", at);
      let depth = 0;
      for (let i = brace; i < raw.length; i++) {
        if (raw[i] === "{") depth++;
        else if (raw[i] === "}" && --depth === 0) return !raw.slice(brace, i).includes("editCustomChannels");
      }
      return false;
    })()],
    ["openCustomModels 从 state.custom_channels 初始化编辑态",
      /function openCustomModels\(\)[\s\S]{0,400}?editCustomChannels = JSON\.parse\(JSON\.stringify\(state\.custom_channels \|\| \[\]\)\)/.test(raw)],
    ["openCustomChannels 已移除（不再有独立弹层）", !raw.includes("function openCustomChannels")],
    ["app.js 接线自定义模型的打开/新增/保存按钮",
      raw.includes('$("btnOpenCustomModels").onclick') && raw.includes('$("btnCustomAdd").onclick') && raw.includes('$("btnCustomSave").onclick')],
    ["app.js 调 POST /api/config/custom_channels", raw.includes('"/api/config/custom_channels"')],
    ["app.js 调 POST /api/config/custom_channel_test", raw.includes('"/api/config/custom_channel_test"')],

    // 内建同名拒绝（R66）：前后端各自守一道——前端给即时反馈，后端 Validate 会拒。
    ["前端有内建渠道名保留集（防与内建同名）", raw.includes("RESERVED_CHANNEL_NAMES") && raw.includes('"workbuddy"') && raw.includes('"cline-free"')],
    ["后端拒绝内建同名的文案已在 hint 写清", html.includes("不得与内建渠道同名") ||
      raw.includes("不得与内建渠道同名")],
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
    ["模型新增走弹窗（R82：列表里不再内嵌添加行；R122 改成 tab panel 的手填页签）", raw.includes('$("btnMeSave").onclick') && raw.includes("function openModelEditor(")],
    ["上下文/输出输入支持 k/M 单位（parseTokens）", raw.includes("function parseTokens(") && raw.includes("function formatTokenCount(")],
    // R119 推翻旧口径：输入框不再预填 1M/64k 这类猜测值——留空就是「未探测」，
    // 面板绝不能把猜的冒充上游声明（同 R49「只透传声明」）。
    ["模型编辑弹窗不预填猜测默认值（留空=未探测，R119）",
      html.includes('id="meCtx"') && !/id="meCtx"[^>]*value="1M"/.test(html) && !/id="meOut"[^>]*value="64k"/.test(html)],
    ["模型编辑弹窗含工具能力勾选（supports_tools）", html.includes('id="meTools"') && raw.includes('supports_tools: $("meTools").checked')],
    ["渠道与模型列表都用一行式 flex 行（R82）", css.includes(".custom-ch-row {") && css.includes(".model-mgr-row {") && css.includes("white-space: nowrap")],
    ["openSettings 不再引用已移除的 inCustomModels", !raw.includes('$("inCustomModels")')],
    ["模型编辑弹窗回车快捷提交", /meId"\)\.onkeydown = \(e\) => \{ if \(e\.key === "Enter"\) saveModelEditor\(\)/.test(raw)],
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

// ---- 场景 4：折线图不得画「全 0 系列」----
// 背景（用户实测）：某渠道只在一个小时点有量、其余点全 0，图上却有一条贯穿
// 到底的贴 0 轴长线，既白占图例又把 Y 轴视觉压扁。修法是按「区间总量 > 0」
// 过滤系列（与 tooltip 过滤 0 值系列同口径）。
{
  const need4 = [];
  const check4 = (name, pass, extra = "") => need4.push([name, pass, extra]);
  // 同 fnBody（上面那个是局部作用域内的，这里需要独立定义）
  const bodyOf = (name) => {
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

  // 函数体源码断言（图表依赖 echarts，stub 环境跑不了真实渲染，
  // 但「是否按总量过滤」是纯源码性质，可静态锁死）。
  const usageBody = bodyOf("renderUsageChart");
  check4("renderUsageChart 函数体可定位", usageBody.length > 200, `len=${usageBody.length}`);
  check4("token 图按总量过滤掉 0 渠道（active 变量）",
    /const active = chans\.filter\(\(ch\) => \(totalOf\[ch\] \|\| 0\) > 0\)/.test(usageBody),
    usageBody.slice(0, 200));
  check4("token 图的 series 用过滤后的 active（不再直接用 chans）",
    /const series = active\.map/.test(usageBody) && !/const series = chans\.map/.test(usageBody));
  check4("token 图的 color 也用过滤后的 active",
    /color: active\.map\(\(ch\) => chColor\(ch\)\)/.test(usageBody));
  // 核心：0 值必须画成 null（断点），不能画成 0 —— 否则每个"没跑"的时刻都落一个
  // 0 点、被直线串成一条贴 X 轴的长线（用户实测的"底部总有一条线"）。
  check4("token 图把 0 画成 null 断点（不再落 0 点）",
    /return v > 0 \? v : null;/.test(usageBody),
    usageBody.slice(usageBody.indexOf("data: byDay.map"), usageBody.indexOf("data: byDay.map") + 120));
  check4("token 图显式关掉 connectNulls（否则 null 会被跨过连成假线）",
    /connectNulls: false/.test(usageBody));
  check4("token 图不再用「|| 0」兜底成 0 点",
    !/d\.by_channel\[ch\] \|\| 0/.test(usageBody));

  const creditBody = bodyOf("renderCreditChart");
  check4("renderCreditChart 函数体可定位", creditBody.length > 200, `len=${creditBody.length}`);
  check4("积分图按总量过滤掉 0 账号",
    /\.filter\(\(uid\) => \(totals\[uid\] \|\| 0\) > 0\)/.test(creditBody),
    creditBody.slice(0, 200));
  check4("积分图同样把 0 画成 null 断点",
    /return v > 0 \? v : null;/.test(creditBody));
  check4("积分图同样关掉 connectNulls",
    /connectNulls: false/.test(creditBody));
  check4("积分图不再用「|| 0」兜底成 0 点",
    !/byAcct\[uid\]\[d\] \|\| 0/.test(creditBody));

  for (const [label, pass, extra] of need4) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}${!pass && extra ? `  ← ${extra}` : ""}`);
    if (!pass) failed++;
  }
}

// ---- 场景 4.5：横轴固定整窗刻度（R98）----
// 背景（用户实测）：今日视图只画到当前小时（截图里 11 点就只剩 00:00~11:00），
// 横轴长度随时刻变化、刻度本身也在变，无法横向比较。修法是后端按窗口全枚举
// 补空桶，前端只消费不再自算（口径单一来源），积分图也用同一份刻度。
{
  const need45 = [];
  const check45 = (name, pass, extra = "") => need45.push([name, pass, extra]);
  const bodyOf = (name) => {
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

  const axisBody = bodyOf("usageAxisKeys");
  check45("usageAxisKeys 函数体可定位", axisBody.length > 100, `len=${axisBody.length}`);
  check45("今日视图固定 24 个小时桶（与「当前几点」无关）",
    /for \(let h = 0; h < 24; h\+\+\)/.test(axisBody), axisBody.slice(0, 160));
  check45("多日视图按窗口天数逐日枚举（缺数据的日子也有刻度）",
    /for \(let i = days - 1; i >= 0; i--\)/.test(axisBody), axisBody.slice(0, 160));

  const usageBody = bodyOf("renderUsageChart");
  check45("token 图横轴直接用后端下发的 by_day（不在前端另算）",
    /const dates = byDay\.map\(\(d\) => d\.date\)/.test(usageBody));

  const creditBody = bodyOf("renderCreditChart");
  check45("积分图横轴用 usageAxisKeys（与 token 图同刻度）",
    /const dates = usageAxisKeys\(usageDays\)/.test(creditBody), creditBody.slice(0, 300));
  check45("积分图不再用「只含有消耗的时段」建横轴",
    !/new Set\(spends\.map\(\(e\) => fmtDay\(e\.ts\)\)\)/.test(creditBody));
  check45("积分图今日视图按小时归桶（fmtHour）",
    /usageDays === 1 \? fmtHour\(ts\) : fmtDay\(ts\)/.test(creditBody));

  for (const [label, pass, extra] of need45) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}${!pass && extra ? `  ← ${extra}` : ""}`);
    if (!pass) failed++;
  }
}

// ---- 场景 5：自定义渠道弹层独立 + 模型一键导入（R81）----
{
  const need5 = [];
  const check5 = (name, pass, extra = "") => need5.push([name, pass, extra]);

  check5("主面板渠道按钮回到 17 个（R84：自定义渠道入口收敛到设置）",
    !/btnCustomChannels/.test(html));
  check5("弹层保留完整动作（列表 + 新增 Provider + 保存 + 提示）",
    ['customChList', 'btnCustomAdd', 'btnCustomSave', 'customChErr', 'customChOk']
      .every((id) => html.includes(`id="${id}"`)));
  // R85：Provider 一行 + 其下每个模型各一行，任何一行都不换行
  check5("Provider 行与其模型行都不换行（white-space: nowrap）",
    /\.cm-provider-row, \.cm-model-row \{[^}]*white-space: nowrap/.test(css) &&
    /\.cm-info \{[^}]*white-space: nowrap/.test(css));
  check5("每个 Provider 一个区块、其下每个模型各占一行",
    /class="cm-provider"/.test(raw) && /class="cm-model-row"/.test(raw));
  check5("Provider 行功能完整（编辑/＋模型/删除；R122 导入已并进「＋ 模型」）",
    ['data-edit-custom', 'data-add-model-in', 'data-del-custom']
      .every((a) => raw.includes(a)) && !raw.includes('data-import-custom') && !raw.includes('data-test-custom'));
  check5("每行按钮组整体右对齐（信息区 flex:1 + 按钮组 margin-left:auto）",
    /\.cm-provider-row, \.cm-model-row \{[^}]*display: flex/.test(css) &&
    /\.cm-info \{[^}]*flex: 1 1 auto/.test(css) &&
    /\.cm-btn-edit \{ margin-left: auto/.test(css));
  check5("自定义模型弹层与设置同宽（modal-wide 720px）", (() => {
    const a = html.indexOf('id="customModelsOverlay"');
    if (a < 0) return false;
    const seg = html.slice(a, a + 300);   // 只看开标签附近，避免跨弹层误匹配
    return /class="modal modal-wide"/.test(seg) && !/modal-xwide/.test(seg);
  })());
  check5("模型行功能完整（编辑/删除都在同一行）",
    /data-edit-model-in=/.test(raw) && /data-del-model-in=/.test(raw));
  // R82：一行展示 + 增改走弹窗
  check5("Provider 行 URL 用省略号收窄（不撑破布局）",
    /\.cm-url \{[^}]*text-overflow: ellipsis/.test(css));
  // R83：模型与渠道同一行展示，模型 chip 是可点的文字按钮
  check5("模型行可点编辑（打开模型面板的手填页签，不打上游）",
    /data-edit-model-in\]"?\)[\s\S]{0,200}?openModelPick\(ci, "manual", mi\)/.test(raw));
  // R83：删除改为文字按钮，不再用 🗑 / × 图标
  check5("删除渠道是文字按钮（不是 🗑 emoji）",
    /data-del-custom="\$\{i\}"[^>]*>删除</.test(raw) && !/data-del-custom="\$\{i\}"[^>]*>🗑/.test(raw));
  check5("删除模型是文字按钮（不是 × 图标）",
    /data-del-model-in="\$\{i\}:\$\{j\}"[^>]*>删除</.test(raw));
  check5("危险按钮有 danger 样式",
    /\.btn\.danger \{/.test(css));
  // R83：停用功能整体移除（前端按钮 + 后端 enabled 字段）
  check5("自定义渠道不再有停用按钮", !raw.includes('data-toggle-custom'));
  check5("后端 ChannelConfig 不再有 Enabled 字段",
    !readFileSync(new URL("../internal/custom/config.go", import.meta.url), "utf8").includes("Enabled *bool"));
  check5("Provider 行有编辑入口", /data-edit-custom=/.test(raw));
  // R122：三个弹窗（模型管理/单模型编辑/导入勾选）合成一个 tab panel。
  check5("渠道编辑弹层 + 模型面板弹层存在，旧三个弹层已删除",
    html.includes('id="channelEditorOverlay"') && html.includes('id="modelPickOverlay"') &&
    !html.includes('id="modelManagerOverlay"') && !html.includes('id="modelEditorOverlay"') &&
    !html.includes('id="modelImportOverlay"'));
  check5("模型面板两个页签齐备（上游列表 / 手动添加）",
    /data-pick-tab="api"/.test(html) && /data-pick-tab="manual"/.test(html) &&
    html.includes('id="pickPaneApi"') && html.includes('id="pickPaneManual"') &&
    /function switchPickTab\(tab\)/.test(raw));
  // 渠道标识从「禁改」放开：用户要求能改，代价（旧前缀 400、历史流水留旧名）由前端
  // 弹确认告知 + 后端级联 config 引用；不写 renames 后端无法区分改名与「删+增」。
  check5("渠道标识可改且提交 renames 映射（R122：反 R82 的 disabled）",
    /\$\("ceName"\)\.disabled = false/.test(raw) && !/\$\("ceName"\)\.disabled = !!ch/.test(raw) &&
    /pendingRenames\.push\(\{ new: name, old: ch\.name \}\)/.test(raw) &&
    /api\/config\/custom_channels", \{ channels: editCustomChannels, renames: pendingRenames \}/.test(raw));
  check5("新增渠道不再依赖底部输入行（旧输入框已移除）",
    !html.includes('id="inCustomName"') && !raw.includes('addPendingCustomInputs'));
  // 下拉框自绘箭头（原生箭头又小又贴边）
  // 判定方式：截取 `.select, select.input {` 到下一个 `}` 的块，断言块内含 appearance:none
  // （用跨块正则容易误匹配到别处的 appearance，测不出回退）。
  check5("select 统一 appearance:none + 自绘箭头", (() => {
    const at = css.indexOf(".select, select.input {");
    if (at < 0) return false;
    const end = css.indexOf("}", at);
    const block = css.slice(at, end);
    return /appearance:\s*none/.test(block) && /background-image:\s*linear-gradient\(45deg/.test(block);
  })());
  check5("设置里不再有自定义模型配置 UI（R86：只从主工具栏入口进）", (() => {
    const a = html.indexOf('id="settingsOverlay"');
    const b = html.indexOf('id="customModelsOverlay"');
    if (a < 0 || b < 0 || b < a) return false;
    // 设置弹层这一段里不能出现自定义模型的配置元素
    const seg = html.slice(a, b);
    return !seg.includes('id="customChList"') && !seg.includes('id="btnCustomAdd"');
  })());
  // 宽度规范：设置保持原样，用户明确要求「宽度也不改」
  check5("设置弹窗保持原来的 modal-wide（720px，R86 用户要求不动宽度）",
    /<div class="overlay hidden" id="settingsOverlay">\s*<div class="modal modal-wide">/.test(html));
  check5("弹窗宽度只有三档（.modal / .modal-wide / .modal-xwide）",
    (css.match(/^\.modal(?:\.modal-(?:wide|xwide|search))? \{/gm) || []).length <= 5 &&
    /\.modal\.modal-xwide \{[^}]*width: 1180px/.test(css));

  // 一键导入→逐行 ＋/−（R122：勾完还要点「导入选中」再点「保存」，中间那步没有意义）
  check5("不再有独立的「导入模型」按钮（已并进「＋ 模型」）",
    !/data-import-custom=/.test(raw) && !/btnImportAll|btnImportNone|btnImportConfirm|btnImportCancel/.test(html));
  check5("模型面板带筛选与「重新拉取」（不预置全选/全不选/导入选中）",
    html.includes('id="importFilter"') && html.includes('id="btnImportAgain"') && html.includes('id="importList"'));
  check5("上游清单走既有 custom_channel_test 端点（不新增拉模型接口）",
    /function probeCustomChannel\(ch\)[\s\S]{0,400}?\/api\/config\/custom_channel_test/.test(raw) &&
    /async function loadUpstreamModels\(\)[\s\S]{0,700}?probeCustomChannel\(/.test(raw));
  check5("已添加的行给「－ 移除」与「更新能力」，未添加的行给「＋」（R122）",
    /data-pick-toggle="\$\{i\}"[^>]*>－<|>－<\/button>[\s\S]{0,200}?data-apply-cap="\$\{i\}"/.test(raw) &&
    /data-pick-toggle="\$\{i\}"[^>]*>＋</.test(raw));
  check5("点 ＋/− 只改编辑态、仍需「保存/关闭」落盘（不隐式写盘）",
    /function togglePickModel\(i\)[\s\S]{0,1200}?点「保存」生效/.test(raw));
  // R119：旧断言要求导入时写死 1M/64k + 三能力全开，那是**编造**上游没说的话，
  // 已整体推翻。现在要求的是反面：拉到什么写什么，拉不到只存 ID。
  check5("导入不再编造默认值（R119：无 1000000/64000 硬编码）",
    !/context_window: 1000000/.test(raw) && !/supports_images: true, supports_reasoning: true, supports_tools: true/.test(raw));
  check5("加入按 cap 写配置、无 cap 只存 ID（R119/R122）",
    /ch\.models\.push\(row\.cap \? capToModelConfig\(row\.cap\) : \{ id: row\.id \}\)/.test(raw));
  check5("caps 字段直接进配置且带 cap_source（R119）",
    /function capToModelConfig\(c\)[\s\S]{0,700}?rec\.cap_source = c\.cap_source/.test(raw));
  check5("导入行展示上游能力摘要，拿不到标「未探测」（R119）",
    /function capSummaryHtml\(c\)/.test(raw) && raw.includes("cap-unknown") && /未探测/.test(raw));
  check5("已存在的模型可「更新能力」覆写（R119：把旧版编造值洗成上游真值）",
    /data-apply-cap="\$\{i\}"/.test(raw) && /function applyImportedCap\(i\)/.test(raw));
  check5("能力来源徽章三态齐备（上游/手填/未探测，R119）",
    /function capSourceTag\(src\)/.test(raw) && raw.includes('class="cm-meta cap-api"') &&
    raw.includes('cap-user') && raw.includes('cap-unknown') && css.includes(".cap-api") && css.includes(".cap-unknown"));
  check5("费率页自定义渠道显示接入点与脱敏 key（R119）",
    /function feeChannelAccessHtml\(ch\)/.test(raw) && /fee-copy-url/.test(raw) && /fee-copy-key/.test(raw) &&
    css.includes(".fee-access") && css.includes(".fee-access-v"));
  check5("api-key 明文只在点复制/点眼睛时按需取（R119/R122：面板不得缓存明文）",
    /async function copyCustomKey\(name\)/.test(raw) &&
    /api\("\/api\/config\/custom_channel_key", \{ name \}\)/.test(raw));
  check5("列表/费率载荷里的 key 不得当 title 明文悬浮显示（R119）",
    !/title="\$\{esc\(c\.api_key\)\}"/.test(raw));
  check5("新建渠道后自动拉一次上游模型（R119：endpoint+key 填完即可勾选）",
    /editCustomChannels\.push\(\{ name, base_url: url, api_key: key, models: \[\] \}\)[\s\S]{0,600}?openModelPick\(idx, "api"\)/.test(raw));
  check5("费率页上下文/能力措辞按来源区分（R119：手填/实测不冒充上游）",
    /CAP_SRC_TEXT/.test(raw) && raw.includes("面板手填值") && raw.includes("上游接口返回"));
  // R121：接入点规范化 + 端点定位。面板不得自己拼 /v1（只有上游能回答「哪个根是真的」），
  // 但必须把上游纠正后的根回写进配置，否则用户下次还得重粘一遍死地址。
  check5("渠道编辑器有「识别端点」入口并绑到 sniffFromEditor（R121）",
    html.includes('id="btnCeSniff"') && /function sniffFromEditor\(\)/.test(raw) &&
    /\$\("btnCeSniff"\)\.onclick[\s\S]{0,60}sniffFromEditor\(\)/.test(raw));
  check5("探测结论回写真实根（R121：res.base_url 覆盖 ch.base_url）",
    /function applyProbeResult\(res, target, setHint\)[\s\S]{0,900}?target\.base_url = res\.base_url/.test(raw));
  check5("保存渠道时不再硬拦「缺协议头」（R121：交给后端规范化，内网补 http）", (() => {
    const at = raw.indexOf("function saveChannelEditor");
    if (at < 0) return false;
    const seg = raw.slice(at, at + 3000);
    return /looksLikeEndpoint\(url\)/.test(seg) && !/\/\^https\?:/.test(seg);
  })());
  check5("前端不再对 api-key 做二次脱敏（R121：后端已给出长头+四星+短尾，再切一遍会切坏）",
    /const keyMask = c\.api_key \|\| "无密钥"/.test(raw) && !/c\.api_key\.slice\(0, 3\)/.test(raw));
  check5("前端脱敏判据认新的四星标记且兼容旧 …/••（R121：与后端 isMaskedKey 同步）",
    raw.includes('const isMaskedKey = (k)') && raw.includes('k.includes("****")') &&
    raw.includes('k.includes("…")') && raw.includes('k.includes("••")'));
  check5("地址合法性只看形状、不在前端猜协议（R121：补 http/https 是后端的活）",
    /function looksLikeEndpoint\(u\)/.test(raw) && /\/\^https\?:\\\/\\\/\/i\.test\(u\)/.test(raw));
  check5("探测失败要列出逐条 tries 而不是只一句「连接失败」（R121）",
    /res\.tries[\s\S]{0,400}?→/.test(raw) && /function applyProbeResult/.test(raw));
  check5("渠道编辑器留了结论回显位（R121）", html.includes('id="ceHint"'));
  // R122：识别详情从 API Key 行里搬出来单独成行（端点结论是一长串：根 + 试过哪几个 +
  // 各回什么），和输入框同行会把框挤没。判定：ceHint 不得在 cfg-row 内部。
  check5("识别详情/错误独立成行，不挤在 API Key 行里（R122）", (() => {
    const a = html.indexOf('id="channelEditorOverlay"');
    const b = html.indexOf('id="modelPickOverlay"');
    if (a < 0 || b < a) return false;
    const seg = html.slice(a, b);
    const k = seg.indexOf('API Key'), h = seg.indexOf('id="ceHint"');
    // ceHint 必须在 API Key 那一行已经收尾（</div></div>）之后，否则它还在 cfg-row 里
    return k > 0 && h > k && /<\/div><\/div>[\s\S]{0,400}?id="ceHint"/.test(seg.slice(k));
  })());
  // R122：API Key 旁有眼睛，且明文只活在本弹窗（关窗即清）
  check5("API Key 行有眼睛按钮，取明文与关窗即清逻辑齐备（R122）",
    html.includes('id="btnCeEye"') && /\$\("btnCeEye"\)\.onclick[\s\S]{0,60}toggleCeKeyEye\(\)/.test(raw) &&
    /async function toggleCeKeyEye\(\)/.test(raw) &&
    /function closeChannelEditor\(\)[\s\S]{0,400}?ceKeyPlain = ""/.test(raw) &&
    /\$\("ceKey"\)\.type = "password"/.test(raw));
  check5("列表/费率载荷里的 key 不得当 title 明文悬浮显示（R119）",
    !/title="\$\{esc\(c\.api_key\)\}"/.test(raw));
  check5("导入列表有滚动容器样式", /\.import-list \{ max-height/.test(css));
  // 模型面板叠在自定义渠道弹层之上：.overlay 基类 z-index=100，
  // 写比它小的值会被压在下面（实测：坐标点模型名落到下层卡片上，点不动）。
  check5("模型面板 z-index 高于 .overlay 基类（100），否则会被下层弹层遮挡",
    /id="modelPickOverlay" style="z-index:(1[1-9]\d|[2-9]\d\d+)"/.test(html));
  // 不得用 label 包裹 input：点 label 会「label 转发 + 冒泡」双重切换（旧勾选框实测 bug）。
  // R122 彻底去掉勾选框：行尾就是 ＋/− 按钮，没有中间态。
  check5("导入行不再用 checkbox/label 包裹（改成行尾 ＋/− 按钮）",
    !/data-import-pick/.test(raw) && !/<label class="import-row"/.test(raw) &&
    /<div class="import-row/.test(raw));
  check5("面板切换页签时手填表单重置为新建（不残留上一个模型的值）",
    /t\.dataset\.pickTab === "manual"\) openModelEditor\(-1\)/.test(raw));
  // R122：主面板「关闭」必须有保存语义，否则用户「＋ 模型」加完点关闭会静默丢弃。
  check5("自定义渠道主面板「关闭」带保存语义（脏则先存，存败不关窗）",
    /async function closeCustomModels\(\)[\s\S]{0,400}?await saveCustomChannels\(\)[\s\S]{0,120}?if \(!ok\) return/.test(raw) &&
    /\$\("btnCustomClose"\)\.onclick = \(\) => closeCustomModels\(\)/.test(raw));
  check5("保存后立即刷新模型列表与费率页（R122：新 provider 当场出现在模型列表）",
    /async function saveCustomChannels\(\)[\s\S]{0,900}?await Promise\.all\(\[loadState\(\), loadFees\(\)\]\)/.test(raw));
  // R122 需求8：费率页接入点行后的「显示全部模型」开关。打开必须**现拉**一次上游，
  // 不得读本地配置（本地只有已添加的那几个），也不得把拉到的写进配置。
  check5("费率页接入点行后有「显示全部模型」开关（默认关）",
    /data-allmodels="\$\{esc\(ch\.channel\)\}"/.test(raw) && /feeShowAll\.has\(ch\.channel\)[\s\S]{0,40}\? " checked"/.test(raw) &&
    css.includes(".fee-all-wrap") && css.includes(".fee-all-box"));
  check5("开关走 change 委托（label 包 input 时 click 会双重切换）、打开才拉上游",
    /input\[data-allmodels\]/.test(raw) && /async function toggleFeeAllModels\(chName, on\)/.test(raw) &&
    !/data-allmodels[\s\S]{0,200}?addEventListener\("click"/.test(raw));
  check5("「显示全部模型」只展示不写盘（R122：拉到的全量不进 editCustomChannels）",
    /function feeAllBoxHtml\(chName\)/.test(raw) &&
    !/toggleFeeAllModels[\s\S]{0,900}?editCustomChannels/.test(raw));
  check5("拉取失败时给出可见错误（不静默）",
    /拉取失败：/.test(raw) && /\("importErr"\)\.textContent = /.test(raw));
  // R122 后端：/models 响应上限与超时。OpenRouter 一类目录 3~4MiB，旧 1MiB 截断后
  // JSON 解不动，表现为「点导入不是每次都能列出列表」——用户实测报的 5 号缺陷。
  check5("探测响应上限放宽到 8MiB、超时 45s（R122：1MiB 截断会把大目录打成「非法 JSON」）", (() => {
    const src = readFileSync(new URL("../internal/custom/probe.go", import.meta.url), "utf8");
    return /probeBodyCap = 8 << 20/.test(src) && /probeTimeout = 45 \* time\.Second/.test(src) &&
      !/io\.LimitReader\(resp\.Body, 1 << 20\)/.test(src);
  })());
  // 用户粘的常是目录地址 {root}/models，本包内部一律拼 {BaseURL}/models ⇒ 会变成
  // {root}/models/models，看着像「上游没有 /models」实为自己拼错。规范化必须剔掉这条尾巴。
  check5("规范化会剔掉 /models 尾巴（R122：不得拼出 /models/models）", (() => {
    const src = readFileSync(new URL("../internal/custom/endpoint.go", import.meta.url), "utf8");
    return /"\/models"/.test(src) && !/chatTails/.test(src);
  })());

  for (const [label, pass, extra] of need5) {
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

// ---- 手动回调兜底（CodeArts 异地浏览器，issue #160 / R96）----
// 背景：CodeArts 的授权回调由 portal 写死成 http://127.0.0.1:<port>/oauth/callback，
// 只有「浏览器与网关同机」时授权码才会自动回到网关。从局域网/远程打开面板时，浏览器
// 跳到的是用户自己机器的回环地址（连接被拒），流程只能等到超时。修法是补一条人工通道。
// 这里锁住三件事：① 输入框/按钮存在；② 按后端 manual_callback 显隐（不是恒显示，
// 否则同机用户会被要求粘贴一条本来就能自动回来的地址）；③ 失败时**保留输入**并就地
// 显示错误——端口粘错是很常见的可恢复错误，关掉弹层等于把输入丢了。
{
  const needCb = [];
  const checkCb = (name, pass, extra = "") => needCb.push([name, pass, extra]);

  checkCb("登录弹层里有「粘贴回调地址」输入框与提交按钮",
    html.includes('id="lcCallbackUrl"') && html.includes('id="btnSubmitCallback"'));
  checkCb("兜底行默认隐藏（同机场景不该被要求手动粘贴）",
    /<div class="hidden" id="lcCallbackRow">/.test(html));
  checkCb("按后端 manual_callback 显隐该行（不是恒显示）",
    raw.includes("if (r.manual_callback)") &&
    raw.includes('$("lcCallbackRow").classList.remove("hidden")') &&
    raw.includes('$("lcCallbackRow").classList.add("hidden")'));
  checkCb("非手动回调渠道会清空上次残留的输入（避免误提交上一条地址）",
    raw.includes('$("lcCallbackUrl").value = ""'));
  checkCb("提交走 POST /api/login/callback",
    raw.includes('await api("/api/login/callback", { url })'));
  checkCb("提交失败保留输入并就地显示错误（不 toast 后关弹层）",
    raw.includes('$("lcCallbackHint").textContent = "提交失败："'));
  checkCb("输入框与按钮用项目既有样式令牌（.input/.btn），不用内联字号宽高",
    html.includes('<input class="input grow" type="text" id="lcCallbackUrl"') &&
    !/id="lcCallbackUrl"[^>]*style=/.test(html));

  for (const [label, pass, extra] of needCb) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}${!pass && extra ? `  ← ${extra}` : ""}`);
    if (!pass) failed++;
  }
}

// ---- 渠道配色单一来源（R99）----
// 背景：折线图 chColor() 读 --ch-<类名> 变量取色，但 Kilo/MiniMax/LobsterAI/
// ZCode/Gemini/CodeArts 六个渠道只在 .badge.x 里写死 hex、没有对应变量，
// chColor() 读不到就全部回退 --ch-custom，图上这 6 条线同色且与列表徽章对不上。
// 这里锁住三件事：① 每个渠道都有 --ch-* 变量且徽章走变量；② 徽章统一经
// chBadge() 取色（与折线同源）；③ 自定义渠道色按黄金角均分、数量再多也不撞色。
{
  const needPal = [];
  const checkPal = (name, pass, extra = "") => needPal.push([name, pass, extra]);
  // 渠道短类名从 app.js 的 CH_CLASS 真源派生，不再手抄一份（手抄的名单漏了 custom，
  // 导致 .badge.custom 改回硬编码 hex 也能全绿）。
  const CH_CLASS_SRC = raw.match(/const CH_CLASS = (\{[^}]*\});/);
  const CH_CLASS = CH_CLASS_SRC ? new Function("return (" + CH_CLASS_SRC[1] + ")")() : {};
  const CH = [...new Set(Object.values(CH_CLASS)), "custom"];
  const BUILTIN_CH = Object.values(CH_CLASS);

  checkPal(`${BUILTIN_CH.length} 个内建渠道都有 --ch-* 颜色变量`,
    BUILTIN_CH.every((c) => new RegExp(`--ch-${c}:\\s*#[0-9a-fA-F]{6}`).test(css)));

  // .badge.custom 也必须在扫描范围内（它是兜底色，同样不许写死 hex）。
  checkPal(".badge.x 全部走 var(--ch-x)，CSS 里不再硬编码渠道色",
    CH.every((c) => new RegExp(`\\.badge\\.${c}\\s*\\{\\s*background:\\s*var\\(--ch-${c}\\);`).test(css)),
    CH.filter((c) => !new RegExp(`\\.badge\\.${c}\\s*\\{\\s*background:\\s*var\\(--ch-${c}\\);`).test(css)).join(", "));

  // 徽章只有 chBadge() 一个出口：任何手写的 `<span class="badge ...">` 都会让
  // 自定义渠道退回 .badge.custom 的单色、并让卡片筛选失效（账号卡片曾长期手拼）。
  // 旧断言只查 `class="badge ${chClass(` —— 那写法根本不存在，等于空断言。
  // 先剥注释（chBadge 的说明文字里就举了反例写法），再排除 chBadge 自身的实现行。
  const rawNoComment = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  const handBadge = [...rawNoComment.matchAll(/<span class="badge[ "`]/g)]
    .filter((m) => !/style="background:\$\{chColor\(k[^)]*\)\}"/.test(rawNoComment.slice(m.index, m.index + 120)));
  checkPal("徽章统一经 chBadge() 渲染（底色 = chColor()，与折线同源）",
    /function chBadge\(k, list\) \{/.test(raw) &&
    raw.includes("style=\"background:${chColor(k, list)}\"") &&
    handBadge.length === 0,
    handBadge.length ? `仍有 ${handBadge.length} 处手拼徽章` : "");

  checkPal("自定义渠道色按序号黄金角均分，不用固定色池取模",
    raw.includes("const CUSTOM_GOLDEN_ANGLE = 137.508") &&
    raw.includes("customChannelColor(k, list)") &&
    !raw.includes("CUSTOM_PALETTE["));

  // 自定义渠道取色缓存必须带序号：只按渠道名缓存时，删/调序后同名渠道会返回
  // 过期颜色（与列表序号对不上，且可能与其他渠道撞色）。
  checkPal("自定义渠道色缓存键含序号（删/调序后不会返回过期颜色）",
    raw.includes("const ck = `${k}#${idx}`") && raw.includes("_customColorCache[ck]"));

  // 账号卡片的筛选靠 data-group（渠道 key），不靠徽章类名：自定义渠道的短类名
  // 一律是 custom，拿类名反查会把它们全归到同一个 key 上（且 CLS_KIND 里没有 custom）。
  checkPal("账号卡片带 data-group（渠道 key），筛选不靠徽章类名反查",
    /<div class="acct-card[^"]*" data-group="\$\{esc\(a\.group/.test(raw) &&
    raw.includes("card.dataset.group") &&
    !raw.includes("CLS_KIND"));

  checkPal("自定义渠道色白字对比度自动达标（黄/绿色相压暗到 ≥4.5）",
    raw.includes("for (let l = 42; l >= 22; l--)") &&
    raw.includes("1.05 / (relLuminance(hex) + 0.05) >= 4.5"));

  // 内建渠道色两两可辨：完全同色会导致图上两条线重叠（原 zcode/qwenwork、
  // gemini/wb、codearts/qoder、lobsterai/loomy 四对完全相同）
  // 数量从 CH_CLASS 派生，不写死：写死时新增渠道（comate 是第 22 个）会让标签与实际脱节。
  const hexOf = {};
  for (const c of CH) {
    const m = css.match(new RegExp(`--ch-${c}:\\s*(#[0-9a-fA-F]{6})`));
    if (m) hexOf[c] = m[1].toLowerCase();
  }
  const dup = [];
  const names = Object.keys(hexOf);
  for (let i = 0; i < names.length; i++)
    for (let j = i + 1; j < names.length; j++)
      if (hexOf[names[i]] === hexOf[names[j]]) dup.push(`${names[i]}=${names[j]}`);
  // 计数只报内建（hexOf 含 custom 兜底色，它不是一条渠道）
  const builtinColorCount = Object.keys(hexOf).filter((k) => k !== "custom").length;
  checkPal(`${builtinColorCount} 个内建渠道色两两不同（无完全同色）`, dup.length === 0, dup.join(", "));

  checkPal("渠道按钮配色全部走 var(--ch-*)，CSS 里无硬编码 hex",
    !/\.btn\.primary\.alt[^{]*\{[^}]*#[0-9a-fA-F]{6}/.test(css));

  checkPal("每个渠道按钮都有专属 .btn.primary.alt.<类名> 规则",
    CH.every((c) => new RegExp(`\\.btn\\.primary\\.alt\\.${c}\\s*\\{`).test(css)),
    CH.filter((c) => !new RegExp(`\\.btn\\.primary\\.alt\\.${c}\\s*\\{`).test(css)).join(", "));

  // 渠道按钮必须带 `alt <渠道短类名>`。两种漏法都要拦：
  //   ① 完全裸的 `btn primary`（btnAddWB 曾如此，靠 --primary 恰好等于 --ch-wb 蒙对）；
  //   ② `btn primary alt` 但没跟渠道类名（落到兜底色）。
  {
    const chBtn = [...html.matchAll(/<button class="btn primary([^"]*)" id="(btnAdd\w+|btnOpenCustomModels)"/g)]
      .map((m) => ({ id: m[2], cls: m[1].trim() }));
    const badCls = chBtn.filter((b) => {
      if (b.id === "btnOpenCustomModels") return !/\balt custom\b/.test(b.cls);
      const m = b.cls.match(/\balt\s+(\S+)/);
      return !m || !CH.includes(m[1]);
    });
    checkPal("index.html 的渠道按钮都带 alt <渠道短类名> 修饰类（裸 btn primary 会落到 --primary）",
      chBtn.length > 0 && badCls.length === 0,
      badCls.map((b) => `${b.id}=「btn primary ${b.cls}」`).join(", "));
  }

  // 全仓扫描：渠道类名出现在任何 CSS 规则里时，颜色属性必须走 var(--ch-*)。
  // 历史坑：.badge.oczen 被重复定义两次（后一条写死 #475569 覆盖变量版）、
  // .fees .ch-header.x 七处仍是调整前的旧 hex，改色板时会静默不生效。
  {
    const chSel = new RegExp(`\\.(?:badge|ch-header|btn\\.primary\\.alt|btn\\.alt)\\.(${CH.join("|")})\\b[^{]*\\{([^}]*)\\}`, "g");
    const bad = [];
    for (const m of css.matchAll(chSel)) {
      const body = m[2];
      // 允许 #fff/#ffffff（白字）；其余 hex 一律视为硬编码渠道色
      for (const hex of body.match(/#[0-9a-fA-F]{3,8}/g) || []) {
        if (!/^#(?:fff|ffffff)$/i.test(hex)) bad.push(`.${m[1]}=${hex}`);
      }
    }
    checkPal("渠道相关 CSS 规则里没有硬编码颜色（白字除外）", bad.length === 0, bad.join(", "));

    const dupSel = [];
    for (const c of CH) {
      for (const kind of ["badge", "btn.primary.alt"]) {
        const re = new RegExp(`^\\.${kind.replace(".", "\\.")}\\.${c}\\s*\\{`, "gm");
        const n = (css.match(re) || []).length;
        if (n > 1) dupSel.push(`${kind}.${c}×${n}`);
      }
    }
    checkPal("渠道 CSS 规则无重复定义（重复会互相覆盖）", dupSel.length === 0, dupSel.join(", "));

    // .ch-header 是费率表改 tab 布局前的表头行，前端已不再渲染（app.js 无 ch-header）。
    // 保留 23 条规则只会让人误以为费率表头还吃渠道色 —— R99 曾把它们从 hex 改成
    // var(--ch-*) 却没生效过。这里锁住「有规则必有渲染」。
    const cssNoComment = css.replace(/\/\*[\s\S]*?\*\//g, "");
    const rawNoComment2 = raw.replace(/^\s*\/\/.*$/gm, "");
    checkPal("CSS 里的渠道配色选择器都有对应的渲染点（无死规则）",
      !/\.ch-header/.test(cssNoComment) && !/class="ch-header/.test(rawNoComment2),
      /\.ch-header/.test(cssNoComment) ? "style.css 仍有 .ch-header 死规则" : "app.js 仍在渲染 ch-header");
  }

  // ---- 渠道名单一致性（同一份渠道集合在前后端各写一份，漂移会静默丢渠道）----
  {
    // 1) 前端保留名 vs 后端 BuiltinKinds()：前端漏一个 → 不拦同名、后端才报错。
    const reservedSrc = raw.match(/const RESERVED_CHANNEL_NAMES = new Set\(\[([\s\S]*?)\]\)/);
    const reserved = reservedSrc ? new Function("return [" + reservedSrc[1] + "]")() : [];
    const goKinds = [...readFileSync(new URL("../internal/provider/provider.go", import.meta.url), "utf8")
      .matchAll(/^\t[A-Za-z]+\s+Kind = "([a-z]+)"/gm)].map((m) => m[1]);
    const missingReserved = goKinds.filter((k) => !reserved.includes(k));
    checkPal("前端 RESERVED_CHANNEL_NAMES 覆盖全部内建渠道（与 provider.BuiltinKinds 同集）",
      missingReserved.length === 0, `缺 ${missingReserved.join(", ")}`);

    const labelSrc = raw.match(/const CH_LABEL = (\{[^}]*\});/);
    const labelMap = labelSrc ? new Function("return (" + labelSrc[1] + ")")() : {};

    // 2) R142 起汇总条顺序**不再手写数组**：手写清单每加一个渠道都要人工同步一份，
    //    漏改是静默降级（该渠道被垫到最末）。断言它确是由 CH_LABEL 派生 + 按显示名字节序排序。
    const derivedOrder = /const CH_SUMMARY_ORDER = Object\.keys\(CH_LABEL\)[\s\S]{0,300}?chLabel\(/.test(raw);
    const handWritten = /const CH_SUMMARY_ORDER = \[/.test(raw);
    checkPal("CH_SUMMARY_ORDER 由 CH_LABEL 派生（不再手写渠道数组，漏改会静默垫到最末）",
      derivedOrder && !handWritten, handWritten ? "仍是手写数组" : "未按 CH_LABEL 派生");

    // 2b) 跨语言同序：JS 派生出的汇总条顺序必须与 Go 的 ChannelDisplayOrder 逐项相同。
    //     Go 侧顺序 = builtinKindList 按 DisplayNames 的字节序（provider.sortedKindsByDisplayName
    //     现算），这里按同一规则模拟一遍再比 —— 任一侧换了排序口径（例如 JS 用 localeCompare、
    //     Go 换比较函数）都会红。
    const labelOf = (k) => labelMap[k] || k;
    const order = Object.keys(labelMap).sort((a, b) => (labelOf(a) < labelOf(b) ? -1 : labelOf(a) > labelOf(b) ? 1 : 0));
    {
      const goSrc = readFileSync(new URL("../internal/provider/provider.go", import.meta.url), "utf8");
      const kindListSrc = goSrc.match(/var builtinKindList = \[\]Kind\{([\s\S]*?)\n\}/);
      const kindList = kindListSrc
        ? [...kindListSrc[1].replace(/\/[\/].*$/gm, "").matchAll(/\b([A-Z][A-Za-z]+)\b/g)].map((m) => m[1])
        : [];
      const constOf = {};
      for (const m of goSrc.matchAll(/^\t([A-Za-z]+)\s+Kind = "([^"]+)"/gm)) constOf[m[1]] = m[2];
      const goDisp = {};
      for (const m of goSrc.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "")
        .matchAll(/^\t([A-Za-z]+):\s*"([^"]*)",$/gm)) goDisp[m[1]] = m[2];
      const goOrder = kindList.map((cn) => [constOf[cn], goDisp[cn]]).filter((p) => p[0] && p[1])
        .sort((a, b) => (a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0)).map((p) => p[0]);
      const diff = [];
      if (goOrder.length !== order.length) diff.push(`Go 内建渠道 ${goOrder.length} 个，JS CH_LABEL ${order.length} 个`);
      for (let i = 0; i < Math.min(goOrder.length, order.length); i++)
        if (goOrder[i] !== order[i]) diff.push(`第 ${i} 位 JS=${order[i]} Go=${goOrder[i]}`);
      checkPal("汇总条顺序（JS 由 CH_LABEL 派生）与后端 ChannelDisplayOrder（Go 由 DisplayNames 派生）逐项同序（R142）",
        diff.length === 0, diff.join("; "));
    }

    // 2c) Go/JS 显示名必须逐项一致（R142 起排序键就是显示名，漂移会让前后端排出两个顺序）。
    //     provider.DisplayNames 是 Go 侧真源，CH_LABEL 是 JS 侧副本，两边任一改动都要同步。
    {
      const goSrc = readFileSync(new URL("../internal/provider/provider.go", import.meta.url), "utf8");
      // 表形如：WorkBuddy: "WorkBuddyCN",  注释与空行不参与
      const goDisp = {};
      for (const m of goSrc.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "")
        .matchAll(/^\t([A-Za-z]+):\s*"([^"]*)",$/gm)) goDisp[m[1]] = m[2];
      // Go 常量名 → kind 字符串（常量定义行形如 `Cline Kind = "cline"`）
      const constOf = {};
      for (const m of goSrc.matchAll(/^\t([A-Za-z]+)\s+Kind = "([^"]+)"/gm)) constOf[m[1]] = m[2];
      const drift = [];
      for (const [cn, name] of Object.entries(goDisp)) {
        const kindStr = constOf[cn];
        if (!kindStr) { drift.push(`Go 常量 ${cn} 找不到对应 kind`); continue; }
        if (labelMap[kindStr] !== name) drift.push(`${kindStr}: JS=${labelMap[kindStr] ?? "(无)"} Go=${name}`);
      }
      const missInGo = Object.keys(labelMap).filter((k) => !Object.values(goDisp).includes(labelMap[k]));
      if (missInGo.length) drift.push(`Go 缺显示名: ${missInGo.join(", ")}`);
      checkPal("渠道显示名 Go(provider.DisplayNames) 与 JS(CH_LABEL) 逐项一致（排序键漂移会让前后端排出两个顺序）",
        Object.keys(goDisp).length > 0 && drift.length === 0, drift.join("; "));
    }

    // 3) 无签到渠道必须有无签到文案：落兜底「无签到」会把「自动领日活」说错。
    const noExpSrc = raw.match(/const NO_EXPLICIT_CHECKIN = new Set\(\[([^\]]*)\]\)/);
    const noExp = noExpSrc ? new Function("return [" + noExpSrc[1] + "]")() : [];
    const tagSrc = raw.match(/const NO_CHECKIN_TAG = (\{[^}]*\});/);
    const tags = tagSrc ? new Function("return (" + tagSrc[1] + ")")() : {};
    const missingTag = noExp.filter((k) => !(k in tags));
    checkPal("NO_EXPLICIT_CHECKIN 的每个渠道都有专属状态文案（否则落兜底「无签到」）",
      missingTag.length === 0, `缺 ${missingTag.join(", ")}`);

    // 4) 渠道显示名只有一个来源：CH_NAMES 这类别名会让图例与徽章各走一份。
    checkPal("渠道显示名只有一个来源（不再有 CH_NAMES 之类的别名表）",
      !/const CH_NAMES\b/.test(raw) && /const chLabel = \(k\) => CH_LABEL\[k\]/.test(raw));

    // 5) 配色规范文档必须与 CSS 实际色值一致（R99 文档化）。
    // 为什么需要：docs/渠道配色规范.md 把全部内建色值当「规范基线」公布，一旦有人改
    // CSS 不改文档，文档就变成错误信息（比没文档更糟——后来者会按错的基线去改）。
    // 这里逐行比对 §3 色值表的 hex 与 :root 的 --ch-*，任一不一致即失败。
    {
      const docPath = new URL("../docs/渠道配色规范.md", import.meta.url);
      let doc = "";
      try { doc = readFileSync(docPath, "utf8"); } catch (e) { /* 缺文档下面会报 */ }
      const rootHex = {};
      for (const m of css.matchAll(/--ch-([a-z]+):\s*(#[0-9a-fA-F]{6})/g)) rootHex[m[1]] = m[2].toLowerCase();
      // 表格行形如：| 1 | `traecode` | `traecode` | TraeCode | `#0284c7` | 4.10 | ...
      // 第 2 格是 **CSS 短类名**（对应 `--ch-<类名>`），第 3 格是渠道 key。
      // 按格切分而不是写死整行形状：表头列数以后还会变（加指标列），
      // 写死正则会让“只是加了列”也变成测试失败（假失败同样是噪声）。
      const docHex = {};
      for (const line of doc.split("\n")) {
        if (!/^\|\s*\d+\s*\|/.test(line)) continue;
        const cells = line.split("|").map((c) => c.trim());
        const cls = (cells[2] || "").replace(/`/g, "");
        const hex = (line.match(/`(#[0-9a-fA-F]{6})`/) || [])[1];
        if (cls && hex) docHex[cls.toLowerCase()] = hex.toLowerCase();
      }
      const keys = Object.keys(rootHex).filter((k) => k !== "custom");
      const missing = keys.filter((k) => !docHex[k]);
      const drift = keys.filter((k) => docHex[k] && docHex[k] !== rootHex[k])
        .map((k) => `${k}: CSS ${rootHex[k]} ≠ 文档 ${docHex[k]}`);
      const extra = Object.keys(docHex).filter((k) => !rootHex[k])
        .map((k) => `文档有但 CSS 无: ${k}`);
      checkPal("配色规范文档的色值表与 :root 的 --ch-* 逐个一致（改色必须同步文档）",
        doc !== "" && missing.length === 0 && drift.length === 0 && extra.length === 0,
        [doc === "" ? "缺 docs/渠道配色规范.md" : "",
         missing.length ? `文档缺 ${missing.join(", ")}` : "",
         ...drift, ...extra].filter(Boolean).join("; "));
    }
  }

  for (const [label, pass, extra] of needPal) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}${!pass && extra ? `  ← ${extra}` : ""}`);
    if (!pass) failed++;
  }
}

console.log(failed === 0 ? "\n全部通过" : `\n${failed} 项失败`);
process.exit(failed === 0 ? 0 : 1);

