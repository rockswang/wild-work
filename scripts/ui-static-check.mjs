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
      { model: "glm-5.3", priced: true, rate: 0.5, free: false, has_context: true, context_window: 200000, supports_images: true, supports_reasoning: true },
    ] },
    { channel: "workbuddy", models: [
      { model: "deepseek-v4", priced: false, rate: 0, free: false, has_context: false },
      { model: "muse-free", priced: true, rate: 0, free: true, has_context: true, context_window: 128000 },
    ] },
    // 溢出场景：另 12 个 qwen* 模型，与 qodercn/qwen3.8-flash 合并共 13 条命中 > 上限 12
    { channel: "loomy", models: Array.from({ length: 12 }, (_, i) =>
      ({ model: `qwen-bulk-${i}`, priced: true, rate: 0.1, free: false, has_context: false })) },
  ] }) + ";"
  + "\n; globalThis.__T = { renderAccounts, renderSummary, loadStats, renderStats, renderPlatformTable, renderRotation, renderExpiry, renderModels, renderLogs, renderAbnormal, renderAppLog, handleToasts, renderModelSearch, collectAllModels };";

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
check(
  "只有冷却账号带冷却标签（健康号不带）",
  (cards.match(/冷却至/g) || []).length === 1,
  `实际 ${(cards.match(/冷却至/g) || []).length} 个`
);
check("健康号仍在（未被过滤）", cards.includes("健康号"));

// ---- 场景 2：渠道汇总条 ----
T.renderSummary();
const sum = document.getElementById("creditSummary").innerHTML;
check("汇总条渲染了 traework", sum.includes("TraeWork"));
check("汇总可用 = 2050+2686 = 4736", sum.includes("4,736"), sum.slice(0, 160));
check("汇总临期 = 100", sum.includes("临期100"));
check("汇总含合计", sum.includes("合计"));

// ---- 场景 2.5：模型查找面板（纯前端过滤，固定高度/限量/超出提示） ----
// 假缓存共 16 个模型：qodercn 2 + workbuddy 2 + loomy 12（含 13 个 qwen* 用于溢出断言）
check("collectAllModels 展平全渠道 16 个模型", T.collectAllModels().length === 16, `实际 ${T.collectAllModels().length}`);
check("collectAllModels 完整 ID = 渠道/裸名", T.collectAllModels()[0].full === "qodercn/qwen3.8-flash");
T.renderModelSearch("glm");
let msHtml = document.getElementById("modelSearchList").innerHTML;
check("搜索 glm 命中 1 条", (msHtml.match(/class="ms-row"/g) || []).length === 1, msHtml.slice(0, 120));
check("命中项 data-mid 为完整 ID", msHtml.includes('data-mid="qodercn/glm-5.3"'));
check("命中项显示渠道徽章与费率", msHtml.includes("QoderCN") && msHtml.includes("x0.50"));
check("底部提示共 N 个匹配", /共 1 个匹配/.test(document.getElementById("modelSearchFoot").textContent));
T.renderModelSearch("");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("空关键字受上限约束只列 12 条", (msHtml.match(/class="ms-row"/g) || []).length === 12, `实际 ${(msHtml.match(/class="ms-row"/g) || []).length}`);
check("空关键字提示可输关键字缩小范围", /输入关键字可缩小范围/.test(document.getElementById("modelSearchFoot").textContent), document.getElementById("modelSearchFoot").textContent);
check("免费模型显示 Free", msHtml.includes("Free"));
check("未定价模型显示 unknown", msHtml.includes("unknown"));
T.renderModelSearch("zzz-none");
check("无匹配时提示无匹配", /无匹配/.test(document.getElementById("modelSearchList").innerHTML));
// 限量：13 个 qwen* 命中 > 上限 12 → 只渲染 12 条，底部提示还有未显示
T.renderModelSearch("qwen");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("超上限时只渲染 12 条", (msHtml.match(/class="ms-row"/g) || []).length === 12, `实际 ${(msHtml.match(/class="ms-row"/g) || []).length}`);
check("超上限时底部提示未显示数量", /还有 1 个未显示/.test(document.getElementById("modelSearchFoot").textContent), document.getElementById("modelSearchFoot").textContent);

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
const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
check("style.css 含运行统计样式段（#mtabStats .st-sum 等）", /#mtabStats \.st-sum/.test(css) && /#mtabStats \.st-plat-row/.test(css));
check("更新角标失败警示样式存在（#stUpdated.st-stale）", /#stUpdated\.st-stale/.test(css));

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
  ["function render()", /^\s*function render\s*\(/gm],
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

console.log(failed === 0 ? "\n全部通过" : `\n${failed} 项失败`);
process.exit(failed === 0 ? 0 : 1);
