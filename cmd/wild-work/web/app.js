// wild-work 管理面板前端（原生 JS，无构建步骤，直接 fetch 管理 API）
"use strict";

const $ = (id) => document.getElementById(id);

// ---------- API 封装 ----------
// 管理 API 会话：cookie 为 HttpOnly 不读（fetch 需 same-origin 自动携带），
// 前端只持有「口令指纹」用于判断登录态（session=state.auth_session）。
let authSession = "";
async function api(path, body) {
  const opts = { method: "GET", headers: { "Content-Type": "application/json" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.method = "POST";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    if (resp.status === 401 && data.need_login) showLogin(); // 会话失效：即时弹登录层
    throw new Error(data.error || ("请求失败 " + resp.status));
  }
  return data;
}

// ---------- 管理面板登录 ----------
// 注意：id 一律带 admin 前缀，与「添加账号」弹层（loginOverlay/loginMsg）语义不同，切勿混用。
// 已渲染过面板数据时不再只盖一层遮罩，而是整页重载：彻底清掉 DOM/内存里的账号与密钥
// （登出或会话失效后残留展示不安全）。首次加载（state 为 null）不重载，避免死循环。
function showLogin(msg) {
  const hadData = state !== null;
  state = null;
  if (hadData) { location.reload(); return; }
  $("adminLoginOverlay").classList.remove("hidden");
  $("adminLoginErr").textContent = msg || "";
  $("adminLoginPass").focus();
}

async function submitLogin() {
  const pass = $("adminLoginPass").value;
  if (!pass) { $("adminLoginErr").textContent = "请输入管理员密码"; return; }
  $("btnAdminLogin").disabled = true;
  try {
    const r = await api("/api/auth/login", { password: pass });
    authSession = r.session || "";
    $("adminLoginPass").value = "";
    $("adminLoginOverlay").classList.add("hidden");
    await loadState();
    await loadFees();
  } catch (e) {
    $("adminLoginErr").textContent = e.message;
  } finally {
    $("btnAdminLogin").disabled = false;
  }
}

async function logout(msg) {
  try { await api("/api/auth/logout", {}); } catch (e) { /* 会话已失效也继续收敛到登录层 */ }
  authSession = "";
  showLogin(msg || "已退出登录");
}

// ensureSession 重新探测登录态：未登录弹登录层，否则加载面板数据。
// 用于密码变更/清空后重新同步（改密码会作废旧会话，清空密码则鉴权消失）。
async function ensureSession(msg) {
  try {
    const st = await api("/api/auth/state");
    if (st.auth_enabled && !st.auth_session) { showLogin(msg); return; }
    authSession = st.auth_session || "";
  } catch (e) { showLogin(msg); return; }
  await loadState();
}

// ---------- 工具 ----------
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

let toastTimer = null;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 3000);
}

function shortUid(uid) {
  if (!uid) return "";
  return uid.length <= 12 ? uid : uid.slice(0, 6) + "…" + uid.slice(-4);
}

// ---------- 全局状态 ----------
let state = null;

// ---------- 数据加载 ----------
async function loadState() {
  state = await api("/api/state");
  // 账号数据已变（刷新/签到/增删），明细缓存随之失效，避免 tooltip 展示旧余额。
  detailCache = {};
  render();
}

async function loadFees() {
  try {
    const fees = await api("/api/fees");
    renderFees(fees);
  } catch (e) { /* 费率接口失败不阻塞 */ }
}

async function refreshFees() {
  $("btnRefreshFees").disabled = true;
  try {
    await api("/api/fees/refresh", {});
    const fees = await api("/api/fees");
    renderFees(fees);
    toast("模型列表和费率已刷新");
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshFees").disabled = false;
  }
}

// ---------- 积分明细 tooltip ----------
// 明细条数可能很多（TraeWork 每个签到奖励都是独立条目，常见 30+），
// 故分页展示：每页 DETAIL_PAGE_SIZE 条，页码状态存在 detailState 里，
// 翻页不重新请求（detailCache 已缓存该账号的完整响应）。
const DETAIL_PAGE_SIZE = 8;
let detailTimer = null;
let detailCache = {};
// detailState 当前 tooltip 的展示状态：{uid, page}。
// detailState 当前 tooltip 的展示状态：{uid, page}。
let detailState = null;

async function showCreditDetail(e, uid) {
  const el = e.currentTarget;
  if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; }

  // 切换到另一个账号时重置页码；同一账号重复 hover 保留原页码。
  const page = (detailState && detailState.uid === uid) ? detailState.page : 0;

  let d = detailCache[uid];
  if (!d) {
    try {
      d = await api("/api/account/resource_detail", { uid });
      // 竞态守卫：等待接口期间鼠标已移开（hideCreditDetail 已排定关闭且未被
      // tip hover/翻页取消）→ 不再弹出，避免 tooltip 残留在页面上。
      if (detailTimer !== null) return;
      detailCache[uid] = d;
    } catch (err) { return; }
  }
  if (!d || !d.items || !d.items.length) return;

  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }

  const rect = el.getBoundingClientRect();
  detailState = { uid, page };
  renderCreditDetail();
  // 先渲染再量尺寸，才能决定向上还是向下弹出。
  const h = tip.offsetHeight;
  let top = rect.bottom + 4;
  if (top + h > window.innerHeight) top = Math.max(4, rect.top - h - 4);
  let left = rect.left;
  if (left + tip.offsetWidth > window.innerWidth) left = Math.max(4, window.innerWidth - tip.offsetWidth - 10);
  tip.style.left = left + "px";
  tip.style.top = top + "px";
}

// renderCreditDetail 按 detailState 重绘 tooltip（含分页控件与可用/不可用小计）。
function renderCreditDetail() {
  const tip = $("creditTip");
  if (!tip || !detailState) return;
  const d = detailCache[detailState.uid];
  if (!d || !d.items || !d.items.length) return;

  const items = d.items.slice();
  // 临期判定与后端 ExpiringWithin 同口径：北京时间墙钟 now + 阈值天数，到期日当天零点前即临期。
  const expDays = state.expiring_days || 1;
  const bjNow = Date.now() + new Date().getTimezoneOffset() * 60000 + 8 * 3600 * 1000;
  const expDeadline = bjNow + expDays * 24 * 3600 * 1000;
  // 排序：临期（可消耗、有剩余、窗口内到期）→ 可用 → 已用完/不可用/仅展示垫底；组内按到期日升序。
  const expUtcOf = (it) => {
    if (!it.expire_at) return Infinity;
    const [ey, em, ed] = it.expire_at.split("-").map(Number);
    return ey && em && ed ? Date.UTC(ey, em - 1, ed) : Infinity;
  };
  const groupOf = (it) => (it.usable && it.remain > 0) ? (expUtcOf(it) < expDeadline ? 0 : 1) : 2;
  items.sort((a, b) => groupOf(a) - groupOf(b) || expUtcOf(a) - expUtcOf(b));
  const pages = Math.max(1, Math.ceil(items.length / DETAIL_PAGE_SIZE));
  const page = Math.min(Math.max(0, detailState.page), pages - 1);
  detailState.page = page;
  const slice = items.slice(page * DETAIL_PAGE_SIZE, (page + 1) * DETAIL_PAGE_SIZE);

  // 有效期列仅当上游确实下发了到期时间时才出现——渠道未返回则不显示该列，
  // 避免一列全空或把「无到期信息」误读成「永不过期」。
  const hasExpiry = items.some((it) => it.expire_at);

  // 头部：翻页器在顶部居中，标题与条数分列两端（用户方案：翻页不跨出浮窗）。
  let html = `<div class="detail-head">`;
  html += `<span class="detail-count">共 ${items.length} 条</span>`;
  if (pages > 1) {
    html += `<span class="detail-pager">`;
    html += `<span class="detail-pg${page === 0 ? " off" : ""}" data-pg="${page - 1}">‹</span>`;
    html += `<span class="detail-pg-info">${page + 1} / ${pages}</span>`;
    html += `<span class="detail-pg${page >= pages - 1 ? " off" : ""}" data-pg="${page + 1}">›</span>`;
    html += `</span>`;
  }
  html += `<span class="detail-title">积分明细</span></div>`;
  html += `<table class="detail-table"><thead><tr><th>套餐</th><th>总额</th><th>已用</th><th>剩余</th>`;
  if (hasExpiry) html += `<th>有效期</th>`;
  html += `</tr></thead><tbody>`;
  for (const it of slice) {
    // 不可用额度整行淡显 + 角标，与可用额度区分开（如 TraeWork 的官方客户端专用池）。
    let cls = it.usable ? "" : ' class="detail-unusable"';
    let tag = "";
    if (!it.usable) {
      tag = '<span class="detail-tag" title="该额度仅供官方客户端使用，本工具无法消耗">不可用</span>';
    } else if (it.info_only) {
      // 单位与积分不同（如 MonkeyCode 的每日 Token 额度）：展示但不计入合计。
      tag = '<span class="detail-tag" title="单位与积分不同，仅作展示，不计入合计">仅展示</span>';
    }
    // 临期条目（可消耗且有剩余、窗口内到期，与后端 ExpiringWithin 余额口径一致）：
    // 整行淡红底 + 到期日红字 + 临期红章；已用完（remain=0）不标临期，避免与汇总口径打架。
    let expCell = "-", expCellCls = "";
    if (it.usable && it.remain > 0 && it.expire_at) {
      const [ey, em, ed] = it.expire_at.split("-").map(Number);
      if (ey && em && ed) {
        const expUtc = Date.UTC(ey, em - 1, ed) - 8 * 3600 * 1000;
        if (expUtc < expDeadline) {
          expCell = `${esc(it.expire_at)}<span class="detail-exp-tag" title="${expDays} 天内到期，优先消耗">临期</span>`;
          expCellCls = ' class="detail-exp"';
          cls = ' class="detail-exp-row"';
        } else {
          expCell = esc(it.expire_at);
        }
      } else {
        expCell = esc(it.expire_at);
      }
    }
    html += `<tr${cls}><td>${esc(it.name)}${tag}</td><td>${it.total}</td><td>${it.used}</td><td>${it.remain}</td>`;
    if (hasExpiry) html += `<td${expCellCls}>${expCell}</td>`;
    html += `</tr>`;
  }
  html += `</tbody></table>`;

  // 小计行：只在确实存在不可用额度时才拆开展示，否则保持单数字（不制造无意义的 0）。
  const usable = d.usable_remain || 0;
  const unusable = d.unusable_remain || 0;
  html += `<div class="detail-sum">`;
  html += `<span>可用 <b>${usable}</b></span>`;
  if (unusable > 0) html += `<span class="detail-sum-unusable">不可用 <b>${unusable}</b></span>`;
  html += `</div>`;
  tip.innerHTML = html;
  tip.style.display = "block";

  // 翻页：只改状态重绘，不重新请求接口。事件重挂由 hideCreditDetail 统一负责。
  tip.querySelectorAll(".detail-pg").forEach((btn) => {
    if (btn.classList.contains("off")) return;
    btn.onclick = (ev) => {
      ev.stopPropagation();
      if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; } // 翻页即取消关闭
      const target = Number(btn.dataset.pg);
      if (Number.isFinite(target)) { detailState.page = target; renderCreditDetail(); }
    };
  });
}

function hideCreditDetail() {
  detailTimer = setTimeout(() => {
    const tip = $("creditTip");
    if (tip) tip.style.display = "none";
  }, 300);
  const tip = $("creditTip");
  if (tip) {
    tip.onmouseenter = () => { if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; } };
    tip.onmouseleave = () => { tip.style.display = "none"; };
  }
}

// ---------- 渲染 ----------
function render() {
  renderTopbar();
  renderSummary();
  renderAccounts();
  applyAcctFilter();
  refreshTabBalances();
  renderTimes();
}

// renderTopbar 顶栏 API 地址渲染：
// - 监听 127.0.0.1/localhost → 原样显示；
// - 监听 0.0.0.0/::/空（所有网卡）→ 显示局域网 IP（lan_ip），客户端可跨机接入；
//   取不到局域网 IP 时兑底 127.0.0.1；
// - 其他自定义地址 → 原样显示。
// 此前 0.0.0.0 被硬换成 127.0.0.1，局域网客户端复制到的是本机环回地址，无法跨机接入。
function renderTopbar() {
  $("ver").textContent = "v" + state.version;
  $("serverLine").textContent = state.running ? "服务运行中" : "服务未启动";
  $("aboutVer").textContent = state.version;

  const wildcard = state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::";
  const host = wildcard ? (state.lan_ip || "127.0.0.1")
    : (state.listen_host === "localhost" ? "127.0.0.1" : state.listen_host);
  const apiURL = `http://${host}:${state.listen_port}/v1`;
  $("apiAddr").querySelector(".val").textContent = apiURL;
  // 监听所有网卡时提示真实含义（避免误导为仅本机可用）
  $("apiAddr").title = wildcard
    ? `监听 0.0.0.0（所有网卡），局域网可用；点击复制`
    : "点击复制地址";

  const key = state.api_key;
  $("apiKeyDisplay").querySelector(".val").textContent = key === "" ? "（无鉴权）" : key;
}

// 渠道显示名与 CSS 短类名（后端 group / 费率 channel 均为 provider.Kind）。
const CH_LABEL = { workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork", traecode: "TraeCode", qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", monkeycode: "MonkeyCode", raccoon: "小浣熊", loomy: "Loomy", oczen: "OpenCodeZen" };
const CH_CLASS = { workbuddy: "wb", workbuddyai: "wbai", traework: "trae", traecode: "traecode", qoder: "qoder", qodercn: "qodercn", qodercom: "qodercom", qwenwork: "qwenwork", glm: "glm", monkeycode: "monkeycode", raccoon: "raccoon", loomy: "loomy", oczen: "oczen" };
const chLabel = (k) => CH_LABEL[k] || "WorkBuddy";
const chClass = (k) => CH_CLASS[k] || "wb";
// 不支持显式签到（手动按钮）的渠道：
// WorkBuddy 国际版不提供手动签到，而是自动对话保活领日活奖励；
// 千问办公无签到活动；MonkeyCode 无签到端点；小浣熊无签到端点；Loomy 无签到端点；OpenCodeZen 匿名通道无账号概念（也无积分）。
const NO_EXPLICIT_CHECKIN = new Set(["workbuddyai", "qwenwork", "monkeycode", "raccoon", "loomy", "oczen"]);
const noExplicitCheckin = (g) => NO_EXPLICIT_CHECKIN.has(g);
// 导入型渠道：凭据由本机已登录的官方客户端提供，没有浏览器登录流程（见 internal/app/import_local.go）。
const IMPORT_LOCAL_CHANNELS = new Set(["monkeycode", "raccoon", "loomy"]);
const isImportLocal = (ch) => IMPORT_LOCAL_CHANNELS.has(ch);
// 支持「浏览器授权登录」的渠道：登录期间临时把该渠道的自定义协议回调指向本工具，
// 以便接住授权码并完成 token 兑换。
// 小浣熊两个集合都命中 —— 弹窗里同时给「浏览器授权登录」与「从客户端导入」两个动作。
const PROTOCOL_LOGIN_CHANNELS = new Set(["raccoon"]);
const hasProtocolLogin = (ch) => PROTOCOL_LOGIN_CHANNELS.has(ch);
// 无手动签到渠道的状态文案：国际版是「自动领日活奖励」，千问办公为「无签到」。
const NO_CHECKIN_TAG = { workbuddyai: "自动领日活奖励", oczen: "不支持" };
const noCheckinText = (g) => NO_CHECKIN_TAG[g] || "无签到";
// 无积分概念的渠道（匿名通道）：积分区域显示「不适用」，并隐藏刷新积分/明细入口。
const NO_CREDITS = new Set(["oczen"]);

// creditsText 账号卡片的积分文案。
// 拆成「可用 / 不可用 / 临期」三个数字：渠道（如 TraeWork）会下发官方客户端专用的
// 额度池，对本工具是看得见用不了的，混进一个数字会让人误判可用余额；
// 临期是可消耗余额中 24h 内（到期日≤明天）到期的部分，提示优先消耗。
// 不可用仅在 >0 时显示；临期同理，凭空多个灰/红 0 很吵。
// 旧版本 state 文件（v2.2.0 及之前，无 unusable 字段）读入后 credits_stale=true，
// 此时不把旧值当真值，改显示「待刷新」；自动刷新首刷成功后即变回真实拆分。
function creditsText(a) {
  if (a.credits_na) {
    return `<span class="credit-na" title="匿名通道无积分概念">不适用</span>`;
  }
  if (a.credits_stale) {
    return `<span class="credit-stale" title="余额口径已过期（旧版本状态文件），正在自动刷新…">待刷新</span>`;
  }
  let html = `<span class="credit-num">${a.credits}</span><span>可用积分</span>`;
  if ((a.expiring_credits || 0) > 0) {
    html += `<span class="credit-expiring"> (临期${a.expiring_credits})</span>`;
  }
  if ((a.unusable_credits || 0) > 0) {
    html += `<span class="credit-unusable"> (不可用${a.unusable_credits})</span>`;
  }
  return html;
}

// 渠道汇总固定顺序（与添加按钮布局一致；traecode 是别名池不单独出现，qoder 下线仅存量兜底）
const CH_SUMMARY_ORDER = ["workbuddy", "workbuddyai", "traework", "qodercn", "qodercom", "qwenwork", "glm", "oczen", "qoder"];

// renderSummary 渠道积分汇总条：每渠道一行「可用总积分 / 临期」，末尾合计。
// 数据全部来自 state.accounts（pool 实时值），与账号卡片同源；
// 口径与 R17 一致：不可用额度只作角标、不进合计；credits_na（匿名）不参与合计。
function renderSummary() {
  const el = $("creditSummary");
  if (!el) return;
  if (!state.accounts.length) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden");

  const byCh = {};
  for (const a of state.accounts) {
    const g = a.group || "workbuddy";
    let c = byCh[g];
    if (!c) { c = byCh[g] = { n: 0, credits: 0, expiring: 0, unusable: 0, stale: 0, na: false }; }
    c.n++;
    if (a.credits_na) { c.na = true; continue; }
    c.credits += a.credits || 0;
    c.expiring += a.expiring_credits || 0;
    c.unusable += a.unusable_credits || 0;
    if (a.credits_stale) c.stale++;
  }

  const fmt = (n) => n.toLocaleString("zh-CN");
  // 已知渠道按固定顺序，未知渠道（新增时）按字母序垫后，保证不丢行
  const chs = CH_SUMMARY_ORDER.filter((k) => byCh[k])
    .concat(Object.keys(byCh).filter((k) => !CH_SUMMARY_ORDER.includes(k)).sort());

  let total = 0, totalExp = 0, staleChs = 0;
  let html = `<span class="cs-title">积分汇总</span>`;
  for (const k of chs) {
    const c = byCh[k];
    const allStale = c.stale === c.n;
    // R17 口径：口径过期的数字不当真值——全渠道待刷新时不进合计
    // （否则"待刷新"的旧值会悄悄抬高合计，用户看到的数字无从解释）。
    if (!c.na && !allStale) { total += c.credits; totalExp += c.expiring; }
    if (!c.na && allStale) staleChs++;
    let body;
    if (c.na) {
      body = `<span class="credit-na">不适用</span>`;
    } else if (allStale) {
      body = `<span class="credit-stale">待刷新</span>`;
    } else {
      body = `<span class="cs-num">${fmt(c.credits)}</span>`
        + (c.expiring > 0 ? `<span class="cs-exp" title="24 小时内到期">临期${fmt(c.expiring)}</span>` : "")
        + (c.unusable > 0 ? `<span class="cs-unu" title="账号名下、本工具不可消耗的额度（不计入合计）">不可用${fmt(c.unusable)}</span>` : "")
        + (c.stale > 0 ? `<span class="cs-stale" title="部分账号余额待刷新">${c.stale} 待刷新</span>` : "");
    }
    const on = acctFilter === k ? " on" : "";
    html += `<span class="cs-chip clickable${on}" data-wg="${esc(k)}" role="button" tabindex="0"`
      + ` title="点击筛选该渠道账号，再次点击取消"><span class="badge ${chClass(k)}">${esc(chLabel(k))}</span>`
      + `<span class="cs-n">${c.n}号</span>${body}</span>`;
  }
  // 「全部」重置项：仅在有筛选时出现，点击清除筛选。整条不参与合计，故放在合计 chip 之前。
  if (acctFilter) {
    html += `<span class="cs-chip clickable cs-all" data-wg="" role="button" tabindex="0" title="显示全部账号">`
      + `<span class="cs-n">全部</span></span>`;
  }
  html += `<span class="cs-chip cs-total" title="各渠道可用积分合计（不含「不可用」与匿名通道）">`
    + `<span class="cs-title">合计</span><span class="cs-num">${fmt(total)}</span>`
    + (totalExp > 0 ? `<span class="cs-exp">临期${fmt(totalExp)}</span>` : "")
    + (staleChs > 0 ? `<span class="cs-stale" title="有渠道整体待刷新，未计入合计">${staleChs} 渠道待刷新</span>` : "")
    + `</span>`;
  el.innerHTML = html;
}

// acctFilter 账号筛选状态：null = 全部；否则为渠道 key（provider.Kind）。
// 汇总条 chip 与账号卡片同源于 state.accounts，点某个渠道 chip 即筛选其账号卡片。
let acctFilter = null;

// applyAcctFilter 按 acctFilter 显示/隐藏账号卡片（与汇总条 chip 联动）。
// 逐张卡片读徽章的渠道类名反查（卡片本身不带 data-group，避免改动 renderAccounts 的既有结构）。
function applyAcctFilter() {
  const grid = $("acctList");
  if (!grid) return;
  grid.querySelectorAll(".acct-card").forEach((card) => {
    const badge = card.querySelector(".acct-top .badge");
    const key = badge ? (CLS_KIND[badge.classList[1]] || "") : "";
    const show = !acctFilter || key === acctFilter;
    card.classList.toggle("hidden", !show);
  });
}

// CLS_KIND 徽章 CSS 短类名 → 渠道 key 的反查表（与 CH_CLASS 互逆）。
const CLS_KIND = {};
for (const k of Object.keys(CH_CLASS)) CLS_KIND[CH_CLASS[k]] = k;

// setAcctFilter 设置筛选并同步汇总条高亮与账号卡片可见性。
function setAcctFilter(kind) {
  acctFilter = (acctFilter === kind) ? null : kind; // 再次点击同一渠道 = 取消
  renderSummary();
  applyAcctFilter();
}

// bindSummaryFilter 汇总条 chip 点击/键盘筛选（事件委托，绑定一次）。
function bindSummaryFilter() {
  const el = $("creditSummary");
  if (!el) return;
  const pick = (target) => {
    const chip = target.closest(".cs-chip.clickable");
    if (!chip) return;
    setAcctFilter(chip.dataset.wg || null);
  };
  el.addEventListener("click", (e) => pick(e.target));
  el.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); pick(e.target); }
  });
}
function renderAccounts() {
  const grid = $("acctList");
  const empty = $("acctEmpty");
  if (!state.accounts.length) {
    grid.innerHTML = "";
    empty.classList.remove("hidden");
    return;
  }
  empty.classList.add("hidden");

  grid.innerHTML = state.accounts.map((a) => {
    const group = chClass(a.group);
    const groupName = chLabel(a.group);
    const noCheckin = noExplicitCheckin(a.group); // 无显式签到，签到按钮灰掉
    const noCheckinTitle = a.group === "workbuddyai"
      ? "无需手动签到：定时自动对话保活并领取日活奖励"
      : `${groupName} 不支持手动签到`;

    const checkinTag = a.last_checkin_at
      ? `<span class="tag ${a.last_checkin_ok ? "ok" : "bad"}">${a.last_checkin_ok ? "签到成功" : "签到失败"}</span>`
      : (noCheckin ? `<span class="tag neutral" title="${esc(noCheckinTitle)}">${noCheckinText(a.group)}</span>` : '<span class="tag neutral">未签到</span>');

    const disabledClass = a.disabled ? " disabled" : "";
    const disableIcon = a.disabled ? "▶" : "⏸";
    const disableTitle = a.disabled ? "启用" : "停用";

    // 冷却标签（后端 pool.Status 已透出 cooling/until/reason，此前前端未渲染）。
    // 为什么需要：账号被冷却时挑号会跳过它，表现为「积分明明很多却报 503/上游限流」，
    // 而卡片上毫无提示——用户只能靠猜。这里把「冷却到几点 + 原因」直接摆出来。
    // until 由后端 fmtTime 预格式化为 "01-02 15:04"，为空则退化为「冷却中」。
    const coolingTag = a.cooling
      ? `<span class="tag warn" title="${esc(a.reason || "冷却中")}">冷却至 ${esc(a.until || "-")}</span>`
      : "";
    const coolingClass = a.cooling ? " cooling" : "";
    const checkinBtn = noCheckin
      ? `<span class="icon-op off" title="${esc(noCheckinTitle)}" onclick="return false">✓</span>`
      : `<span class="icon-op" title="签到" onclick="checkin('${a.uid}')">✓</span>`;

    // 匿名渠道（无积分/无签到）：只保留「不可操作」的静态指示，
    // 不给刷新积分/停用/删除入口——后端也会硬拒，避免用户白点一次。
    const noCredits = NO_CREDITS.has(a.group);
    // 问号图标：hover 展示 oczen 通道说明（免费反代范围/私有 Key/代理三项）。
    // 放在锁头左侧；用独立的 help 样式（正常亮度 + help 光标），不可点击但 tooltip 可用。
    // title 内换行用 &#10;（HTML 属性实体），字面 \n 会被部分浏览器吞掉导致无 tooltip。
    const oczenHelp = `<span class="icon-op help" title="关于 OpenCodeZen 通道：&#10;1. 本工具仅反代其免费模型（绕过官方客户端限制）；付费账号可直接使用官方端点，无需经此通道&#10;2. 在 OpenCodeZen 获取的 API-Key 可在设置中配置，避免匿名账号共享限额超限&#10;3. 配置代理后可使用有地域限制的模型，可用性取决于上游通道">?</span>`;
    const ops = noCredits
      ? `${oczenHelp}<span class="icon-op off" title="固定账号，不可停用/删除" onclick="return false">🔒</span>`
      : `${checkinBtn}
          <span class="icon-op" title="刷新积分" onclick="refreshOne('${a.uid}')">↻</span>
          <span class="icon-op warn" title="${disableTitle}" onclick="toggleDisable('${a.uid}',${a.disabled})">${disableIcon}</span>
          <span class="icon-op danger" title="删除账号" onclick="removeAcct('${a.uid}')">✕</span>`;
    // 显示名：点击直接弹出改名框（匿名渠道不可改，降级为普通文本）。
    // oczen 特例：渠道名已由 badge 承担，名字按凭证形态显示「匿名/私有Key」。
    const isOczen = a.group === "oczen";
    const oczenName = (state.oczen_api_key || "") ? "私有Key" : "匿名";
    const nameHtml = noCredits
      ? `<span class="acct-name">${esc(isOczen ? oczenName : (a.nickname || shortUid(a.uid)))}</span>`
      : `<span class="acct-name editable" title="点击修改显示名" onclick="openRename('${a.uid}','${esc(a.nickname || shortUid(a.uid)).replace(/'/g, "&#39;")}')">${esc(a.nickname || shortUid(a.uid))}</span>`;

    return `
    <div class="acct-card${disabledClass}${coolingClass}">
      <div class="acct-top">
        <div>
          <span class="badge ${group}">${groupName}</span>
          ${nameHtml}
        </div>
        <div class="acct-ops">
          ${ops}
        </div>
      </div>
      <div class="acct-uid">UID: ${esc(shortUid(a.uid))}</div>
      <div class="acct-mid">
        <div class="acct-credits"${noCredits ? "" : ` onmouseenter="showCreditDetail(event,'${a.uid}')" onmouseleave="hideCreditDetail()"`}>${creditsText(a)}</div>
        <div class="acct-checkin">${coolingTag}${checkinTag}</div>
      </div>
    </div>`;
  }).join("");
}

function renderTimes() {
  const box = $("timesBox");
  box.innerHTML = (state.checkin_times || []).map((t) =>
    `<span class="time-chip" title="点击删除" onclick="delTime('${t}')">${t} ✕</span>`).join("");
  $("nextCheckin").textContent = state.next_checkin || "-";
}

// channelCredits 汇总某渠道全部账号的可用积分（与积分汇总条同源 state.accounts）。
// 无该渠道账号时返回 null（前端不渲染角标，避免显示误导性的 0）。
function channelCredits(kind) {
  if (!state || !state.accounts) return null;
  let sum = 0, has = false;
  for (const a of state.accounts) {
    if ((a.group || "workbuddy") !== kind) continue;
    if (a.credits_na) return null; // 匿名渠道无积分概念
    has = true;
    sum += a.credits || 0;
  }
  return has ? sum : null;
}

// fmtNum 千分位整数（与汇总条的 fmt 同口径，独立命名避免作用域冲突）。
function fmtNum(n) {
  return n.toLocaleString("zh-CN");
}

// refreshTabBalances 就地更新费率渠道 tab 的积分余额角标。
// 账号余额刷新远频于费率表（state 每次 loadState 都更新，fees 只在加载/手动刷新时变），
// 若只在 renderFees 里写余额，账号刷新后角标会停在旧值。这里在每次 render() 时
// 只改角标文本（不重建 DOM），既保持最新又不打断用户浏览/滚动。
function refreshTabBalances() {
  const box = $("feesBox");
  if (!box) return;
  box.querySelectorAll(".fees-tab[data-feech]").forEach((tab) => {
    const bal = channelCredits(tab.dataset.feech);
    const el = tab.querySelector(".fees-tab-bal");
    if (bal === null) {
      if (el) el.remove();
      return;
    }
    const txt = `余额 ${fmtNum(bal)} 分`;
    if (el) { if (el.textContent !== txt) el.textContent = txt; }
    else { const s = document.createElement("span"); s.className = "fees-tab-bal"; s.textContent = txt; s.title = "该渠道全部账号当前可用积分合计"; tab.appendChild(s); }
  });
}

function renderFees(fees) {
  const box = $("feesBox");
  const channels = fees.channels || [];

  if (channels.length === 0) {
    box.innerHTML = `<div class="note">${esc(fees.note || "")}</div>
      <div class="note">${esc(fees.disclaimer || "")}</div>`;
    return;
  }
  let html = `<div class="note">${esc(fees.note || "")}</div>`;
  if (fees.cached_at) html += `<div class="note">费率上次更新：${esc(fees.cached_at)}</div>`;
  if (fees.error) html += `<div class="note" style="color:var(--danger)">${esc(fees.error)}</div>`;

  const UNKNOWN_TIP = "上游未返回，请在客户端自行确认";

  // 能力图标：模型 ID 后的小标记，title 属性提供文字描述。
  // 只展示上游明确声明的能力；未声明的（字段缺失或上游返回 false）不显示图标。
  // tool_calls 不展示：几乎所有模型都支持，图标信息量低。
  const capIcons = (m) => {
    if (!m) return "";
    const caps = [];
    if (m.supports_images) {
      caps.push(`<span class="cap-icon cap-img" title="支持图像输入（多模态视觉）：可直接发送图片给该模型">👁</span>`);
    }
    if (m.supports_reasoning) {
      caps.push(`<span class="cap-icon cap-reason" title="支持思考/推理模式：回复前会进行推理（可能含 reasoning_content）">🧠</span>`);
    }
    return caps.length > 0 ? ` <span class="cap-icons">${caps.join("")}</span>` : "";
  };

  // 上下文标记：模型 ID 后的 (1M)/(180K) 小字标。
  // 只在上游接口真实返回时展示（has_context），不拿估算值充数。
  const ctxTag = (m) => {
    if (!m || !m.has_context || !m.context_window) return "";
    return ` <span class="ctx-tag" title="上下文窗口：${fmtTokens(m.context_window)} tokens">(${fmtTokens(m.context_window)})</span>`;
  };

  // 能力文字摘要，拼进模型 tooltip。
  // 措辞说明：上游模型列表接口未声明某能力时，本工具不自行断言其「不支持」，
  // 只说「未声明」——避免把缺失信息当成否定结论。
  const capText = (m) => {
    if (!m) return null;
    const yes = [], unknown = [];
    (m.supports_images ? yes : unknown).push("图像输入");
    (m.supports_reasoning ? yes : unknown).push("思考模式");
    const parts = [];
    if (yes.length) parts.push(`支持：${yes.join("、")}`);
    if (unknown.length) parts.push(`上游未声明：${unknown.join("、")}`);
    return parts.join("；");
  };

  // 模型 id 的 tooltip：能拿到上下文则展示，否则明确说未知
  const modelTip = (m) => {
    const parts = [`模型：${m.model}`];
    if (m.has_context && m.context_window) {
      parts.push(`上下文窗口：${fmtTokens(m.context_window)}`);
      if (m.max_tokens) parts.push(`最大输出：${fmtTokens(m.max_tokens)}`);
    } else {
      parts.push("上下文窗口：未知");
      parts.push("最大输出：未知");
      parts.push("（上游未提供该信息）");
    }
    const ct = capText(m);
    if (ct) parts.push(ct);
    return parts.join("\n");
  };

  // 单行倍率单元格
  const rateCell = (m) => {
    if (!m) return "";
    if (!m.priced) {
      return `<span class="rate-unknown" title="${esc(UNKNOWN_TIP)}">unknown</span>`;
    }
    if (m.free) {
      return `<span class="rate-free" title="上游标注为免费（x0.00）">✦ Free</span>`;
    }
    return `<span class="rate-paid">x${m.rate.toFixed(2)}</span>`;
  };

  // 促销标签：使用上游给的颜色（原本被拼在文案里没解析）
  const noteCell = (m) => {
    if (!m || !m.note) return "";
    const style = m.color ? ` style="color:${esc(m.color)}"` : "";
    return ` <span class="rate-note"${style}>${esc(m.note)}</span>`;
  };

  // 模型单元格（二开，升级勿丢）：上方为模型名 + 能力/上下文标记，下方为完整模型 ID
  // （含渠道前缀，即客户端添加模型时实际要填的模型名），点击一键复制到剪贴板。
  // 模型名本身也可点击复制完整 ID（官方 v2.6.1 交互），两处 data-mid 语义一致。
  // m.model 来自 /api/fees 是裸名（不带渠道前缀），完整 ID 需拼上 chPrefix（ch.channel）。
  const modelCellHtml = (m, chPrefix) => {
    if (!m) return "";
    const raw = m.model;
    // 短名：去掉渠道前缀（`xxx/name` -> `name`），展示在上行的模型名位置
    const short = raw.includes("/") ? raw.slice(raw.indexOf("/") + 1) : raw;
    // 完整 ID：裸名补上渠道前缀；模型名本身已带前缀则原样用
    const full = raw.includes("/") ? raw : (chPrefix ? `${chPrefix}/${raw}` : raw);
    const copyTip = `点击复制：${full}`;
    const line = `<div class="mline"><code class="fee-model-copy" data-mid="${esc(full)}" role="button" tabindex="0" title="${esc(modelTip(m))}\n${copyTip}">${esc(short)}</code>${ctxTag(m)}${capIcons(m)}${noteCell(m)}</div>`;
    // 仅当完整ID != 短名时才显示可复制的完整 ID 行，避免裸名模型重复
    const idRow = full !== short
      ? `<span class="full-id" data-mid="${esc(full)}" title="点击复制完整模型 ID：${esc(full)}">${esc(full)}</span>`
      : "";
    return `<div class="mcell">${line}${idRow}</div>`;
  };

  // 渠道多标签：每个渠道一个 tab，panel 内双列模型布局不变（issue：费率表太长）。
  // 记住上次选中的渠道，重渲染后自动恢复（后台刷新不打断用户浏览）。
  if (!renderFees.lastCh) renderFees.lastCh = channels[0].channel;
  // 选中的渠道若已不在列表里（渠道被移除），回退到第一个
  if (!channels.some((c) => c.channel === renderFees.lastCh)) {
    renderFees.lastCh = channels[0].channel;
  }
  const active = renderFees.lastCh;

  html += `<div class="fees-tabs">`;
  for (const ch of channels) {
    const n = (ch.models || []).length;
    const on = ch.channel === active ? " active" : "";
    // 渠道积分余额角标：取自 state.accounts（与积分汇总条同源），
    // 让用户在选择渠道时即可看到「这个渠道还有多少积分可用」。
    const bal = channelCredits(ch.channel);
    const balHtml = bal === null ? ""
      : `<span class="fees-tab-bal" title="该渠道全部账号当前可用积分合计">余额 ${fmtNum(bal)} 分</span>`;
    html += `<button class="fees-tab${on}" data-feech="${esc(ch.channel)}"
      title="${esc(chLabel(ch.channel))}">${esc(chLabel(ch.channel))}<span class="fees-tab-n">${n}</span>${balHtml}</button>`;
  }
  html += `</div>`;

  for (const ch of channels) {
    const chCls = chClass(ch.channel);
    const models = ch.models || [];
    const hidden = ch.channel !== active ? ' class="hidden"' : '';
    html += `<div class="fees-panel${hidden ? ' hidden' : ''}" data-feepanel="${esc(ch.channel)}">`;
    html += `<table><thead><tr><th>模型</th><th>倍率</th><th>模型</th><th>倍率</th></tr></thead><tbody>`;
    for (let i = 0; i < models.length; i += 2) {
      const m1 = models[i];
      const m2 = models[i + 1];
      const id1 = modelCellHtml(m1, ch.channel);
      const id2 = modelCellHtml(m2, ch.channel);
      html += `<tr><td>${id1}</td><td>${rateCell(m1)}</td><td>${id2}</td><td>${rateCell(m2)}</td></tr>`;
    }
    html += `</tbody></table></div>`;
  }

  html += `<div class="note" style="margin-top:8px">${esc(fees.disclaimer || "")}</div>`;
  box.innerHTML = html;
}

// bindFeesTabs 费率渠道标签点击切换（事件委托，绑定一次）。
// 渲染只改 active 类与面板可见性，不重建 DOM，避免滚动位置跳动。
function bindFeesTabs() {
  const box = $("feesBox");
  box.addEventListener("click", (e) => {
    // 模型名点击复制（官方 v2.6.1）：code.fee-model-copy 优先判定，
    // 与下方的 .full-id（二开完整 ID 行）互不冲突、语义一致。
    const code = e.target.closest("code.fee-model-copy");
    if (code && box.contains(code)) {
      copyText(code.dataset.mid, "模型名");
      return;
    }
    // 完整模型 ID 一键复制（二开，升级勿丢）：点费率表每行模型下方的完整ID即复制
    const cpid = e.target.closest && e.target.closest(".full-id");
    if (cpid) {
      copyText(cpid.dataset.mid || cpid.textContent, "模型 ID");
      return;
    }
    const btn = e.target.closest(".fees-tab");
    if (!btn) return;
    const ch = btn.dataset.feech;
    renderFees.lastCh = ch;
    box.querySelectorAll(".fees-tab").forEach((x) => x.classList.toggle("active", x === btn));
          box.querySelectorAll(".fees-panel").forEach(function (p) {
        if (p.dataset.feepanel === ch) { p.classList.remove("hidden"); }
        else { p.classList.add("hidden"); }
      });
  });
  // 键盘可达：模型名支持 Enter/Space 复制（与 role="button" 对应）。
  box.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const code = e.target.closest("code.fee-model-copy");
    if (code && box.contains(code)) { e.preventDefault(); copyText(code.dataset.mid, "模型名"); }
  });
}

// fmtTokens 把 token 数格式化为 1M / 192k 形式。
function fmtTokens(n) {
  if (!n) return "-";
  if (n >= 1000000 && n % 1000000 === 0) return `${n / 1000000}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

// ---------- 账号操作 ----------
async function checkin(uid) {
  try {
    const r = await api("/api/account/checkin", { uid });
    // 未达成且可重试（活动尚未创建/上游瞬时故障）：说明会在签到窗口内自动重试，
    // 避免用户看到「无可用签到活动」误以为失败。
    if (!r.ok && r.retryable) toast(`暂未领到：${r.msg}（10:00–12:00 窗口内会自动重试）`);
    else toast(r.ok ? `签到成功：${r.msg}（剩余 ${r.remain}）` : `签到：${r.msg}`);
    loadState();
  } catch (e) { toast(e.message); }
}

async function refreshOne(uid) {
  try {
    const r = await api("/api/account/refresh", { uid });
    toast(`刷新成功：剩余积分 ${r.remain}`);
    loadState();
  } catch (e) { toast(e.message); }
}

async function toggleDisable(uid, currentlyDisabled) {
  const action = currentlyDisabled ? "启用" : "停用";
  confirmDialog(`确定${action}该账号？${currentlyDisabled ? "" : "停用后路由不会分配给该账号。"}`, async () => {
    try {
      await api("/api/account/disable", { uid, disabled: !currentlyDisabled });
      toast(`账号已${action}`);
      loadState();
    } catch (e) { toast(e.message); }
  });
}

function removeAcct(uid) {
  confirmDialog("确定删除该账号？删除后需重新登录。", async () => {
    try {
      await api("/api/account/remove", { uid });
      toast("账号已删除");
      loadState();
    } catch (e) { toast(e.message); }
  });
}

// ---------- 批量操作 ----------
async function checkinAll() {
  $("btnCheckinAll").disabled = true;
  try {
    const r = await api("/api/account/checkin_all", {});
    const ok = (r.results || []).filter((x) => x.ok).length;
    const pending = (r.results || []).filter((x) => !x.ok && x.retryable).length;
    const skip = (state.accounts || []).filter((a) => noExplicitCheckin(a.group)).length;
    const total = (r.results || []).length + skip;
    const parts = [skip ? `成功 ${ok} / ${total}（${skip} 个无签到活动跳过）` : `成功 ${ok} / 共 ${total}`];
    if (pending) parts.push(`${pending} 个未领到将在窗口内自动重试`);
    toast(`批量签到完成：${parts.join("；")}`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnCheckinAll").disabled = false;
  }
}

async function refreshAll() {
  $("btnRefreshAll").disabled = true;
  try {
    const r = await api("/api/account/refresh_all", {});
    toast(r.busy ? "已有刷新任务进行中" : `积分刷新完成：成功 ${r.ok} / 失败 ${r.failed}`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshAll").disabled = false;
  }
}

// ---------- 登录 ----------
let pendingChannel = null;
// 待执行动作："login" = 打开浏览器登录，"import" = 从本机客户端导入。
// 小浣熊两种都支持，由弹窗里的两个按钮分别设定。
let pendingAction = "login";
// 无手动签到渠道的登录提示差异文案（国际版会自动领日活奖励）。
const NO_CHECKIN_LOGIN_HINT = {
  workbuddyai: "（无需手动签到，定时自动对话保活并领取日活奖励）",
  qwenwork: "（每日积分服务端 00:00 自动发放；若浏览器已登录千问办公则全自动完成，否则需扫码一次）",
  monkeycode: "（凭据来自本机已登录的 MonkeyCode 客户端；上游无续期接口，客户端重新登录后需再次导入）",
  raccoon: "（凭据来自本机已登录的小浣熊客户端；access_token 约 2 小时，本工具会自动续期）",
  loomy: "（凭据来自本机已登录的 Loomy 客户端；上游无续期接口，约 14 天后需重新登录并再次导入）",
  glm: "（登录后请按下方指引复制 refresh_token 粘贴回来）",
};
function promptLogin(channel) {
  pendingChannel = channel;
  const name = chLabel(channel);
  pendingAction = isImportLocal(channel) ? "import" : "login";
  // 次按钮默认隐藏，只在「两条路都通」的渠道（小浣熊）里显示。
  const altBtn = $("btnLoginAlt");
  altBtn.classList.add("hidden");
  altBtn.onclick = null;

  // 浏览器授权登录渠道：主按钮走浏览器授权 + 协议回调接管，次按钮回退到本机客户端导入。
  if (hasProtocolLogin(channel)) {
    pendingAction = "login";
    $("lcTitle").textContent = "添加 " + name + " 账号";
    $("lcMsg").textContent = `点击「登录${name}」将打开浏览器授权页，登录完成后本工具会自动接管回调并保存账号。`
      + `登录期间会把 ${name} 的协议注册临时指向本工具（结束即恢复），请勿在此期间启动${name}客户端，否则协议注册会被它覆盖。`
      + `也可以改用「从客户端导入」：直接读取本机已登录客户端的凭据。`;
    $("btnLoginConfirm").textContent = "登录" + name;
    altBtn.textContent = "从客户端导入";
    altBtn.classList.remove("hidden");
    altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }

  // 纯导入型渠道：文案与动作都不同（读本机客户端凭据，而不是打开浏览器登录）。
  if (isImportLocal(channel)) {
    $("lcTitle").textContent = "导入 " + name + " 账号";
    $("lcMsg").textContent = `将读取本机已登录的${name}客户端凭据并保存到 wild-work（不会修改客户端本身）。`
      + `若提示未找到凭据，请先打开并登录${name}客户端后重试。${NO_CHECKIN_LOGIN_HINT[channel] || ""}`;
    $("btnLoginConfirm").textContent = "导入";
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }
  $("lcTitle").textContent = "添加 " + name + " 账号";
  $("lcMsg").textContent = noExplicitCheckin(channel)
    ? `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。${NO_CHECKIN_LOGIN_HINT[channel] || ""}`
    : `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。`;
  $("btnLoginConfirm").textContent = "登录" + name;
  $("loginConfirmOverlay").classList.remove("hidden");
}
function confirmLogin() {
  $("loginConfirmOverlay").classList.add("hidden");
  if (!pendingChannel) return;
  // 按 pendingAction 分发：小浣熊两种动作都支持，不能只看渠道是否属于导入型。
  if (pendingAction === "import") importLocal(pendingChannel);
  else startLogin(pendingChannel);
}

// importLocal 从本机已登录的官方客户端导入凭据（MonkeyCode / 小浣熊 / Loomy）。
// note 只在有降级时非空（如某个凭据文件没读到）—— 必须显示出来，
// 否则用户只看到"已导入成功"，等积分不显示时才发现。
async function importLocal(channel) {
  try {
    const r = await api("/api/account/import_local", { channel });
    const head = `已导入 ${chLabel(channel)} 账号 ${r.uid || ""}`;
    if (r.note) toast(`${head}。${r.note}`, 8000);
    else toast(head);
    await loadState();
  } catch (e) {
    toast(e.message);
  }
}

async function startLogin(channel) {
  try {
    const r = await api("/api/login/start", { channel });
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = `添加 ${chLabel(channel)} 账号`;
    $("loginMsg").textContent = hasProtocolLogin(channel)
      ? `请在浏览器新窗口中完成${chLabel(channel)}登录；完成后本工具会自动接管回调并保存账号（期间请勿启动${chLabel(channel)}客户端）。`
      : "请在浏览器新窗口中完成登录…";
    $("loginOverlay").classList.remove("hidden");
    $("btnCopyUrl").dataset.url = url;
    window.open(url, "_blank", "noopener,noreferrer");
    startLoginPoll();
  } catch (e) {
    toast(e.message);
  }
}

async function cancelLogin() {
  try {
    await api("/api/login/cancel", {});
    stopLoginPoll();
    $("loginOverlay").classList.add("hidden");
    toast("登录已取消");
  } catch (e) { toast(e.message); }
}

function copyUrl() {
  const url = $("btnCopyUrl").dataset.url;
  if (!url) { toast("暂无链接"); return; }
  navigator.clipboard.writeText(url).then(() => toast("链接已复制")).catch(() => toast("复制失败，请手动复制"));
}

// 登录轮询
let loginPoll = null;
function startLoginPoll() {
  stopLoginPoll();
  loginPoll = setInterval(async () => {
    try {
      const st = await api("/api/state");
      if (!st.login_busy) {
        stopLoginPoll();
        $("loginOverlay").classList.add("hidden");
        // 后端在 login_error 里回传终态失败（超时/回调不可用/兑换失败）；
        // 拿不到就是真的成功了。
        if (st.login_error) {
          toast(st.login_error, 8000);
          return;
        }
        toast("登录完成，正在同步账号…");
        await loadState();
        refreshFees();
      }
    } catch (e) { /* 忽略 */ }
  }, 3000);
}
function stopLoginPoll() {
  if (loginPoll) { clearInterval(loginPoll); loginPoll = null; }
}

// ---------- 智谱清言登录（自动捕获 Cookie，手工粘贴为兜底） ----------
// 自动路径：wild-work 拉起独立 profile 的浏览器 → 用户正常登录 → CDP 捕获 Cookie。
// 独立 profile 天然隔离，多账号逐个添加互不干扰（无需手动开无痕）。
let glmPoll = null;

function startGLMLogin() {
  $("glmErr").textContent = "";
  $("glmStatus").textContent = "正在启动浏览器…";
  $("glmManual").classList.add("hidden");
  $("glmAuto").classList.remove("hidden");
  $("glmOverlay").classList.remove("hidden");
  api("/api/login/glm_auto", {})
    .then(() => {
      $("glmStatus").textContent = "浏览器已打开，请在其中登录智谱清言…";
      startGLMPoll();
    })
    .catch((e) => {
      $("glmStatus").textContent = "";
      $("glmErr").textContent = (e.message || "启动失败") + "（可展开下方手工方式）";
      $("glmManual").classList.remove("hidden");
    });
}

function startGLMPoll() {
  stopGLMPoll();
  glmPoll = setInterval(async () => {
    try {
      const st = await api("/api/login/glm_auto_status");
      if (st.status === "success") {
        stopGLMPoll();
        $("glmStatus").textContent = `✅ 已添加账号：${st.nickname || st.uid}`;
        toast("智谱清言账号已添加");
        await loadState();
        refreshFees();
        setTimeout(() => $("glmOverlay").classList.add("hidden"), 1200);
      } else if (st.status === "failed") {
        stopGLMPoll();
        $("glmStatus").textContent = "";
        $("glmErr").textContent = (st.error || "登录失败") + "（可展开下方手工方式）";
        $("glmManual").classList.remove("hidden");
      } else if (st.status === "cancelled" || st.status === "idle") {
        stopGLMPoll();
      }
    } catch (e) { /* 忽略瞬时错误 */ }
  }, 2000);
}

function stopGLMPoll() {
  if (glmPoll) { clearInterval(glmPoll); glmPoll = null; }
}

function cancelGLMLogin() {
  stopGLMPoll();
  api("/api/login/glm_auto_cancel", {}).catch(() => {});
  $("glmOverlay").classList.add("hidden");
}

// 手工兜底：粘贴 refresh_token
async function submitGLMToken() {
  const token = ($("glmTokenInput").value || "").trim();
  if (!token) { $("glmErr").textContent = "请粘贴 refresh_token"; return; }
  const btn = $("btnGLMSubmit");
  btn.disabled = true;
  $("glmErr").textContent = "验证中…";
  try {
    await api("/api/login/glm_token", { refresh_token: token });
    stopGLMPoll();
    $("glmOverlay").classList.add("hidden");
    toast("智谱清言账号已添加");
    await loadState();
    refreshFees();
  } catch (e) {
    $("glmErr").textContent = e.message || "验证失败";
  } finally {
    btn.disabled = false;
  }
}

// ---------- 签到时间（设置弹层内编辑；单次变更立即保存） ----------
function delTime(t) {
  const times = (state.checkin_times || []).filter((x) => x !== t);
  saveTimes(times);
}

function addTime() {
  const times = (state.checkin_times || []).slice();
  const now = new Date();
  const next = `${String(now.getHours()).padStart(2, "0")}:${String(now.getMinutes()).padStart(2, "0")}`;
  if (!times.includes(next)) times.push(next);
  saveTimes(times.sort());
}

async function saveTimes(times) {
  try {
    await api("/api/config/checkin_times", { times });
    toast("签到时间已更新");
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 开机自启（设置弹层内，切换立即保存） ----------
async function toggleAutostart() {
  try {
    await api("/api/config/autostart", { on: $("chkAutostart").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}

// ---------- 设置弹层（统一配置：监听/API-Key/签到/自启/模型路由/渠道代理） ----------
// PROXY_CHANNELS 渠道上游代理列表（顺序与面板渠道序一致；旧 qoder 已下线不提供代理配置）。
const PROXY_CHANNELS = ["oczen", "workbuddy", "workbuddyai", "qodercn", "qodercom", "traework", "qwenwork", "glm", "monkeycode", "raccoon", "loomy"];
const PROXY_HINT = { workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", oczen: "OpenCodeZen" };

// renderProxyList 按当前 state.proxies 渲染每渠道一个输入行。
function renderProxyList() {
  const proxies = state.proxies || {};
  $("proxyList").innerHTML = PROXY_CHANNELS.map((ch) => {
    const val = proxies[ch] || "";
    return `<div class="row proxy-row">
      <label class="lbl wide" title="${esc(PROXY_HINT[ch] || ch)}">${esc(PROXY_HINT[ch] || ch)}</label>
      <input class="input grow proxy-input" data-ch="${ch}" value="${esc(val)}" spellcheck="false" placeholder="如 socks5://127.0.0.1:1080（留空直连）">
    </div>`;
  }).join("");
  $("proxyErr").textContent = "";
}

// selectedHost 当前下拉框（+自定义输入）选定的监听主机名。
// 与后端 config.Listen 归一化口径保持一致：空/`::` 等通配写法按「全部网卡」看待（即对外暴露）。
function selectedHost() {
  const v = $("selHost").value;
  if (v !== "__custom__") return v;
  return $("inHost").value.trim();
}

// isLoopbackHost 判定是否「仅本机可访问」：环回 IP / localhost。
// 对应后端 config.Listen.IsLoopback：**显式写环回才算安全**，空主机名视为对外暴露。
function isLoopbackHost(h) {
  const s = (h || "").trim().toLowerCase().replace(/^\[|\]$/g, "");
  if (s === "localhost") return true;
  return s === "127.0.0.1" || s === "::1" || /^127\./.test(s);
}

// syncListenRisk 按当前选定的监听地址，动态显示/隐藏黄色警告条与管理密码的必填星号。
// 默认 127.0.0.1（本机）时两者均不显示——本机监听无需密码，提示只会干扰。
function syncListenRisk() {
  const needPass = !isLoopbackHost(selectedHost());
  $("listenRiskTip").classList.toggle("hidden", !needPass);
  $("adminPassReq").classList.toggle("hidden", !needPass);
}

function openSettings() {
  // 监听
  $("inPort").value = state.listen_port;
  $("selHost").value = state.listen_host === "127.0.0.1" ? "127.0.0.1"
    : (state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::") ? "0.0.0.0"
    : "__custom__";
  if ($("selHost").value === "__custom__") {
    $("inHost").value = state.listen_host;
    $("customHostRow").classList.remove("hidden");
  } else {
    $("customHostRow").classList.add("hidden");
  }
  // API-Key
  $("keyInput").value = state.api_key;
  // 管理密码：后端不回显，只提示是否已设置（留空 = 不改动，与 oczen key 同一「未改动」约定）
  $("adminPassInput").value = "";
  $("adminPassInput").placeholder = state.admin_pass_set
    ? "已设置（留空 = 不改动；填入新值 = 修改）"
    : (state.auth_required ? "必填：监听非 127.0.0.1 时面板必须鉴权" : "留空 = 面板不鉴权（仅本机监听时允许）");
  // 模型路由
  const cc = state.compat || {};
  const channels = cc.channels || [];
  $("selCh").innerHTML = channels.map(c => `<option value="${c}">${c}</option>`).join("");
  $("selCh").value = cc.default_channel || (channels[0] || "");
  $("inMaxTok").value = cc.max_tokens_cap || 0;
  const map = cc.model_map || {};
  const entries = Object.entries(map);
  $("mapSummary").textContent = entries.length === 0 ? "（空）" : entries.map(([k,v]) => `${k} → ${v}`).join("\u00A0 \u00A0");
  // 映射编辑器：随对话框打开而重置为当前值，收起
  $("mapText").value = entries.map(([k,v]) => `${k} = ${v}`).join("\n");
  $("mapEditor").classList.add("hidden");
  $("mapErr").textContent = "";
  renderMapPresets(channels);
  // 渠道代理 + oczen 自定义 key
  renderProxyList();
  $("oczenKeyInput").value = state.oczen_api_key || ""; // 回显脱敏值；未改动则原样回传，后端按脱敏值识别为「未变」
  $("oczenKeyInput").dataset.touched = "";
  // 自动签到 + 开机自启 + 临期阈值
  $("chkAutostart").checked = !!state.autostart;
  $("selExpiring").value = String(state.expiring_days || 1);
  syncListenRisk(); // 按当前监听地址初始化警告条与必填星号
  $("settingsOverlay").classList.remove("hidden");
}

function closeSettings() {
  $("settingsOverlay").classList.add("hidden");
}

// validateProxies 收集代理输入并做本地形态校验，返回 {proxies, err}。
function validateProxies() {
  const proxies = {};
  for (const inp of document.querySelectorAll(".proxy-input")) {
    const ch = inp.dataset.ch;
    const v = inp.value.trim();
    if (!v) continue;
    let u;
    try { u = new URL(v); } catch { return [null, `渠道 ${ch} 代理地址无效：${v}`]; }
    if (!["http:", "https:", "socks5:"].includes(u.protocol)) {
      return [null, `渠道 ${ch} 代理协议不支持（仅 http/https/socks5）：${v}`];
    }
    if (!u.hostname) return [null, `渠道 ${ch} 代理缺少主机名：${v}`];
    proxies[ch] = v;
  }
  return [proxies, ""];
}

// testOczenKey 「测试」按钮：用输入框当前的候选 key（含脱敏值/空）调后端实测。
// 后端逻辑：候选 key 临时写入渠道 client → big-pickle 发最小对话 → 200/429 均算通过
// （200 = 配额正常；429 = 连通但匿名共享配额限流，属预期现象）；测试后恢复配置原值。
async function testOczenKey() {
  const btn = $("btnOczenTest");
  if (btn.disabled) return;
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = "测试中…";
  $("proxyErr").textContent = "";
  try {
    let k = $("oczenKeyInput").value.trim();
    if (k.includes("…")) k = ""; // 脱敏形态 = 未改动，用当前配置值测
    const r = await api("/api/config/oczen_test", { api_key: k });
    if (r.ok) {
      toast(r.status === 200 ? "✓ OpenCodeZen 凭证可用（200）" : "✓ 连通正常（429 匿名限流，属预期）");
    } else {
      const brief = (r.body || r.error || "").slice(0, 90).replace(/\s+/g, " ");
      toast(`✗ 测试失败 HTTP ${r.status}：${brief}`);
    }
  } catch (e) {
    toast("✗ 测试失败：" + e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
}

async function saveSettings() {
  // 1) 监听地址 + 管理密码（同请求提交：后端先存密码再切监听）
  let host = $("selHost").value;
  if (host === "__custom__") host = $("inHost").value.trim() || "127.0.0.1";
  const port = parseInt($("inPort").value, 10);
  const adminPass = $("adminPassInput").value.trim();
  const listenBody = { host, port };
  if (adminPass) listenBody.admin_password = adminPass; // 空 = 不改动，避免覆盖已设密码
  try {
    await api("/api/config/listen", listenBody);
  } catch (e) { toast(e.message); return; }
  // 刚设置/修改密码：旧会话已被作废，立即用新密码重新登录，保证本次保存的后续步骤可用。
  // （从「本机监听 + 无密码」切到 0.0.0.0 时鉴权刚启用，此前根本没有 cookie。）
  if (adminPass) {
    try {
      const r = await api("/api/auth/login", { password: adminPass });
      authSession = r.session || "";
    } catch (e) { toast(e.message); closeSettings(); showLogin("管理密码已变更，请重新登录"); return; }
  }

  // 2) 代理（先本地校验，失败阻断保存）+ oczen key（仅在用户改动过时提交，避免把脱敏回显值存回）
  const [proxies, perr] = validateProxies();
  if (perr) { $("proxyErr").textContent = perr; toast(perr); return; }
  const body = { proxies };
  const kIn = $("oczenKeyInput");
  if (kIn.dataset.touched === "1") {
    let k = kIn.value.trim();
    // 值仍是脱敏形态（含 …）则视为未修改，保持服务端现值
    if (k && k.includes("…")) k = undefined;
    if (k !== undefined) body.oczen_api_key = k; // undefined 时不携带字段 = 不改动；空串 = 清除回匿名
  }
  try {
    await api("/api/config/proxies", body);
  } catch (e) { toast(e.message); return; }

  // 3) 模型映射：优先读编辑器；编辑器从未展开过则用原值（保证「只改监听/渠道不碰映射」）
  let modelMap = state.compat?.model_map || {};
  if (!$("mapEditor").classList.contains("hidden")) {
    const [parsed, err] = parseMapText($("mapText").value);
    if (err) { $("mapErr").textContent = err; toast(err); return; }
    modelMap = parsed;
  }
  const defaultChannel = $("selCh").value;
  const maxTokensCap = parseInt($("inMaxTok").value, 10) || 0;
  try {
    await api("/api/config/compat", { default_channel: defaultChannel, max_tokens_cap: maxTokensCap, model_map: modelMap });
  } catch (e) { toast(e.message); return; }

  // 4) 临期阈值（1/2/3 天，独立端点即时生效）
  const expDays = parseInt($("selExpiring").value, 10) || 1;
  if (expDays !== (state.expiring_days || 1)) {
    try {
      await api("/api/config/expiring_days", { days: expDays });
    } catch (e) { toast(e.message); return; }
  }

  // 5) API-Key（最后保存：改 Key 可能影响当前会话的后续请求）
  try {
    await api("/api/config/api_key", { key: $("keyInput").value.trim() });
  } catch (e) { toast(e.message); return; }

  // 6) 密码已在第 1 步生效（会话已换新），无需再强制重登
  toast(adminPass ? "设置已保存，管理密码已生效" : "设置已保存");
  closeSettings();
  loadState();
}

// clearAdminPassword 「清除」按钮：关闭面板鉴权（仅环回监听允许，后端会校验）。
async function clearAdminPassword() {
  if (!confirm("确定清除管理密码？清除后面板将不再鉴权（仅允许监听 127.0.0.1）。")) return;
  try {
    await api("/api/config/admin_password", { password: "" });
  } catch (e) { toast(e.message); return; }
  authSession = "";
  toast("管理密码已清除，面板鉴权已关闭");
  closeSettings();
  await ensureSession();
}

// ---------- 显示名修改弹层 ----------
let renameUid = null;

function openRename(uid, currentName) {
  if (uid === "oczen-anon") { toast("OpenCodeZen 匿名通道账号不可改名"); return; }
  renameUid = uid;
  $("rnUid").textContent = shortUid(uid);
  $("rnInput").value = currentName === shortUid(uid) ? "" : (currentName || "");
  $("renameOverlay").classList.remove("hidden");
  $("rnInput").focus();
}

function closeRename() {
  $("renameOverlay").classList.add("hidden");
  renameUid = null;
}

async function saveRename() {
  if (!renameUid) return;
  const nickname = $("rnInput").value.trim();
  if (!nickname) { toast("显示名不能为空"); return; }
  try {
    await api("/api/account/nickname", { uid: renameUid, nickname });
    toast("显示名已更新");
    closeRename();
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 模型映射编辑器 ----------

// CHANNEL_PRESETS 各渠道的缺省建议映射（模型为该渠道常用/免费模型）。
// 用途：未绑定某渠道账号、或不知道该渠道有哪些模型时，给出可点选的起点。
const CHANNEL_PRESETS = {
  workbuddy:   { label: "Claude Code → workbuddy",  items: ["claude-* = workbuddy/glm-5.2", "claude-sonnet-* = workbuddy/kimi-k2.7"] },
  traework:    { label: "Codex → traework",          items: ["gpt-5* = traework/glm-5.2", "codex-* = traework/DeepSeek-V4-Pro"] },
  // TraeCode 与 TraeWork 共用账号，预设沿用 Codex 语义（面向代码场景的新版模型）。
  traecode:    { label: "Codex → traecode",          items: ["gpt-5* = traecode/deepseek-v4.1-flash", "codex-* = traecode/glm-5.3-flash"] },
  workbuddyai: { label: "Claude Code → workbuddyai", items: ["claude-* = workbuddyai/deepseek-v4.1-flash"] },
  qodercn:     { label: "→ qodercn",                 items: ["gpt-* = qodercn/glm-5.3"] },
  qodercom:    { label: "→ qodercom",                items: ["gpt-* = qodercom/glm-5.3"] },
  qwenwork:    { label: "→ qwenwork",                items: ["gpt-* = qwenwork/flash", "claude-* = qwenwork/pro"] },
  glm:         { label: "→ glm (智谱清言)",           items: ["gpt-* = glm/chatglm", "claude-* = glm/chatglm-think"] },
  oczen:       { label: "→ oczen (匿名免费)",        items: ["claude-* = oczen/mimo-v2.6-flash-free", "gpt-* = oczen/big-pickle"] },
};

// renderMapPresets 按当前已接入渠道渲染缺省建议按钮。
// 仅列出「已接入（有账号）」的渠道——未绑定的渠道点了也会因无账号而失败，不给误导性入口。
function renderMapPresets(channels) {
  const bound = new Set((state.accounts || []).map(a => a.group));
  // TraeCode 与 TraeWork 共用账号，账号列表里只会出现 traework；
  // 但 TraeCode 是可独立路由的渠道，其预设也应可见。
  if (bound.has("traework")) bound.add("traecode");
  const box = $("mapPresets");
  box.innerHTML = channels.filter(c => CHANNEL_PRESETS[c] && bound.has(c)).map(c => {
    const p = CHANNEL_PRESETS[c];
    return `<span class="btn tiny preset" data-ch="${c}" title="${esc(p.items.join("\n"))}">${esc(p.label)}</span>`;
  }).join(" ") || `<span class="hint">（尚未接入任何渠道，先在账号管理添加账号）</span>`;
  box.querySelectorAll(".preset").forEach(b => {
    b.onclick = () => {
      const p = CHANNEL_PRESETS[b.dataset.ch];
      // 追加尚未存在的行，避免重复插入
      const cur = $("mapText").value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
      const have = new Set(cur.map(l => l.split("=")[0].trim()));
      const add = p.items.filter(it => !have.has(it.split("=")[0].trim()));
      if (!add.length) { toast(`${p.label} 的建议映射已存在`); return; }
      $("mapText").value = cur.concat(add).join("\n");
    };
  });
}

// toggleMapEditor 展开/收起映射编辑器。
function toggleMapEditor() {
  $("mapEditor").classList.toggle("hidden");
}

// parseMapText 校验编辑器内容，返回 (modelMap, 错误信息)。
// 语法错误就地提示并阻断保存，不再弹 prompt 循环。
function parseMapText(raw) {
  const map = {};
  const channels = new Set(state.compat?.channels || []);
  for (const line0 of raw.split(/\r?\n/)) {
    const line = line0.trim();
    if (!line || line.startsWith("#")) continue;
    const idx = line.indexOf("=");
    if (idx < 0) return [null, `格式错误（缺少 =）：${line}`];
    const k = line.substring(0, idx).trim(), v = line.substring(idx + 1).trim();
    if (!k || !v) return [null, `格式错误（键或值为空）：${line}`];
    const vi = v.indexOf("/");
    if (vi <= 0 || !v.substring(vi + 1).trim()) return [null, `映射目标必须是「渠道/模型」形式：${v}`];
    const ch = v.substring(0, vi);
    if (channels.size && !channels.has(ch)) {
      return [null, `未知渠道「${ch}」；已知渠道：${[...channels].join(", ")}`];
    }
    if (map[k] !== undefined) return [null, `重复的键：${k}`];
    map[k] = v;
  }
  return [map, ""];
}

// ---------- 复制到剪贴板 ----------
async function copyText(text, label) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`${label}已复制到剪贴板`);
  } catch (e) {
    // 降级方案：execCommand
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
      toast(`${label}已复制到剪贴板`);
    } catch (err) {
      toast("复制失败，请手动复制");
    }
    document.body.removeChild(ta);
  }
}
function openHelp() { $("helpOverlay").classList.remove("hidden"); }
function closeHelp() { $("helpOverlay").classList.add("hidden"); }
function openAbout() { $("aboutOverlay").classList.remove("hidden"); }
function closeAbout() { $("aboutOverlay").classList.add("hidden"); }

// ---------- 通用确认框 ----------
function confirmDialog(msg, onOk) {
  $("confirmMsg").textContent = msg;
  $("confirmOverlay").classList.remove("hidden");
  $("btnConfirmOk").onclick = () => { $("confirmOverlay").classList.add("hidden"); onOk(); };
  $("btnConfirmCancel").onclick = () => $("confirmOverlay").classList.add("hidden");
}

// ---------- 事件绑定 ----------
function bind() {
  $("btnAddWB").onclick = () => promptLogin("workbuddy");
  $("btnAddWBAI").onclick = () => promptLogin("workbuddyai");
  $("btnAddTrae").onclick = () => promptLogin("traework");
  $("btnAddQoderCN").onclick = () => promptLogin("qodercn");
  $("btnAddQoderCOM").onclick = () => promptLogin("qodercom");
  $("btnAddQwen").onclick = () => promptLogin("qwenwork");
  $("btnAddMonkeyCode").onclick = () => promptLogin("monkeycode");
  $("btnAddRaccoon").onclick = () => promptLogin("raccoon");
  $("btnAddLoomy").onclick = () => promptLogin("loomy");
  $("btnAddGLM").onclick = () => startGLMLogin();
  $("btnGLMSubmit").onclick = submitGLMToken;
  $("btnGLMCancel").onclick = cancelGLMLogin;
  $("btnGLMToggleManual").onclick = () => $("glmManual").classList.toggle("hidden");
  $("btnCheckinAll").onclick = checkinAll;
  $("btnRefreshAll").onclick = refreshAll;
  $("btnAddTime").onclick = addTime;
  $("btnCopyUrl").onclick = copyUrl;
  $("btnCancelLogin").onclick = cancelLogin;
  $("btnRefreshFees").onclick = refreshFees;
  $("chkAutostart").onchange = toggleAutostart;
  $("btnAdminLogout").onclick = () => logout();
  $("btnClearAdminPass").onclick = clearAdminPassword;
  $("btnAdminLogin").onclick = submitLogin;
  $("adminLoginPass").onkeydown = (e) => { if (e.key === "Enter") submitLogin(); };

  $("apiAddr").onclick = () => {
    const v = $("apiAddr").querySelector(".val").textContent;
    copyText(v, "OpenAI 接口地址");
  };
  $("apiKeyDisplay").onclick = () => {
    const v = $("apiKeyDisplay").querySelector(".val").textContent;
    if (v === "（无鉴权）") { toast("当前未设置 API-Key"); return; }
    copyText(v, "API-Key");
  };
  // 顶栏齿轮进入统一设置（点击地址/Key 文本仍为复制）
  $("btnSettings").onclick = openSettings;

  // 统一设置弹层
  $("btnSettingsSave").onclick = saveSettings;
  $("btnSettingsCancel").onclick = closeSettings;
  $("btnCompatMap").onclick = toggleMapEditor;
  $("selHost").onchange = () => {
    $("customHostRow").classList.toggle("hidden", $("selHost").value !== "__custom__");
    syncListenRisk(); // 监听地址变化 → 实时间同步警告条与必填星号
  };
  // 自定义主机名边输边判（可能一开始就填着非本机地址）
  $("inHost").addEventListener("input", syncListenRisk);
  $("keyInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveSettings(); });

  // 显示名修改弹层
  $("btnRenameSave").onclick = saveRename;
  $("btnRenameCancel").onclick = closeRename;
  $("rnInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveRename(); });
  // oczen key 用户改动标记：避免把脱敏回显值误存回
  $("oczenKeyInput").addEventListener("input", (e) => { e.target.dataset.touched = "1"; });
  $("btnOczenTest").onclick = testOczenKey;

  // 点击弹层空白处关闭
  $("settingsOverlay").onclick = (e) => { if (e.target === $("settingsOverlay")) closeSettings(); };
  $("renameOverlay").onclick = (e) => { if (e.target === $("renameOverlay")) closeRename(); };
  $("helpOverlay").onclick = (e) => { if (e.target === $("helpOverlay")) closeHelp(); };

  $("btnHelp").onclick = openHelp;
  $("btnAbout").onclick = openAbout;
  $("btnHelpClose").onclick = closeHelp;
  $("btnAboutClose").onclick = closeAbout;

  // 登录确认弹层
  $("btnLoginConfirm").onclick = confirmLogin;
  $("btnLoginConfirmCancel").onclick = () => $("loginConfirmOverlay").classList.add("hidden");
  $("loginConfirmOverlay").onclick = (e) => { if (e.target === $("loginConfirmOverlay")) $("loginConfirmOverlay").classList.add("hidden"); };

  $("aboutOverlay").onclick = (e) => { if (e.target === $("aboutOverlay")) closeAbout(); };
}

// ---------- 初始化 ----------
(async function init() {
  bind();
  bindMainTabs();
  bindUsage();
  // 会话探针（不返回 401）：未登录状态下只拉这接口，避免控制台报错
  try {
    const st = await api("/api/auth/state");
    if (st.auth_enabled && !st.auth_session) { showLogin(); return; }
    authSession = st.auth_session || "";
  } catch (e) { /* 探针失败走下面的常规加载（如代理拦截） */ }  loadUsage(); // 页面加载即拉取（首次渲染自动刷新，不依赖手动点击）
  await loadState(); // 状态瞬间返回
  await loadFees();  // 费率表用缓存/静态兜底，秒开
  // 运行统计：统计/请求日志 30s、运行日志 15s 常驻轮询
  // （不依赖当前 tab——里程碑 toast 要在任何 tab 下及时弹出；tab 切入时另有即时拉取）
  loadStats();
  setInterval(loadStats, STATS_POLL_MS);
  loadAppLog();
  setInterval(loadAppLog, APPLOG_POLL_MS);
})();

// ---------- 用量与流水面板 ----------
let usageDays = 7;
let usageChart = null;    // token 折线图（echarts 实例）
let creditChart = null;   // 积分消耗折线图（echarts 实例）
let recentAll = [];       // 全量最近流水（分页源，时间升序）
let recentPage = 0;
let lastNames = {};       // 最近一次渲染的 uid→昵称表（翻页时复用）
const RECENT_PAGE_SIZE = 20;
const MODEL_TOP_N = 20;   // 模型榜只保留前 N 名

function fmtCredits(n) {
  if (n == null) return "-";
  return Number(n).toLocaleString("zh-CN");
}

function fmtTokensFull(n) {
  if (!n) return "0";
  if (n >= 1e8) return (n / 1e8).toFixed(2) + "亿";
  if (n >= 1e4) return (n / 1e4).toFixed(1) + "万";
  return Number(n).toLocaleString("zh-CN");
}

const CH_NAMES = {
  workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork",
  qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", oczen: "OpenCodeZen",
};

async function loadUsage() {
  try {
    const st = await api(`/api/usage?days=${usageDays}`);
    if (st.disabled) {
      return;
    }
    renderUsage(st);
  } catch (e) { /* 统计加载失败不阻塞 */ }
}

function renderUsage(st) {
  $("ucTokens").textContent = fmtTokensFull(st.token.total);
  $("ucReqs").textContent = fmtCredits(st.token.requests);
  $("ucSpend").textContent = fmtCredits(st.credit.spend);
  $("ucEarn").textContent = fmtCredits(st.credit.earn);

  renderModelTable(st.token.by_model || [], usageDays);
  renderCreditTab(st.credit || {});
  renderUsageChart(st);
  renderCreditChart(st.credit || {});
}

function renderModelTable(rows, days) {
  const tb = $("tblModels").querySelector("tbody");
  if (!rows.length) { tb.innerHTML = `<tr><td colspan="5" class="empty-cell">暂无数据（流水自本功能上线后开始记录）</td></tr>`; return; }
  const top = rows.slice(0, MODEL_TOP_N); // 前 20 名，已按 token 降序
  tb.innerHTML = top.map((r) => {
    const avg = days > 1 ? Math.round((r.pt + r.ct) / days) : (r.pt + r.ct);
    return `<tr>
      <td>${esc(r.model)}</td><td><span class="badge ${chClass(r.channel)}">${esc(chLabel(r.channel))}</span></td>
      <td class="num">${fmtCredits(r.requests)}</td><td class="num">${fmtTokensFull(r.pt + r.ct)}</td>
      <td class="num">${fmtTokensFull(avg)}</td></tr>`;
  }).join("");
}

function renderCreditTab(credit) {
  const entries = credit.entries || [];
  const names = credit.name_map || {};
  // 分页渲染流水（折线图由 renderCreditChart 基于同一份条目自算）
  recentAll = entries;
  recentPage = 0;
  lastNames = names;
  renderRecentPage(names);
  renderCreditChart(entries, names);
}

function renderRecentPage(names) {
  const kinds = { earn: "↑签到/发放", spend: "↓消耗", expire: "✖过期" };
  const pages = Math.max(1, Math.ceil(recentAll.length / RECENT_PAGE_SIZE));
  if (recentPage >= pages) recentPage = pages - 1;
  // 倒序展示（最新在前）：从尾部往前取当前页
  const end = recentAll.length - recentPage * RECENT_PAGE_SIZE;
  const start = Math.max(0, end - RECENT_PAGE_SIZE);
  const slice = recentAll.slice(start, end).reverse();
  $("recentList").innerHTML = slice.length
    ? slice.map((r) => `<div class="rline ${r.kind}">
        <span class="rtime">${new Date(r.ts * 1000).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })}</span>
        <span class="rkind">${kinds[r.kind] || r.kind}</span>
        <span class="ramount">${fmtCredits(r.amount)}</span>
        <span class="racct"><span class="badge ${chClass(r.channel)}">${esc(chLabel(r.channel))}</span> ${esc((names || {})[r.uid] || shortUid(r.uid))}</span>
        <span class="rname">${esc(r.note || "")}</span>
        <span class="rbal">余额 ${fmtCredits(r.balance)}</span></div>`).join("")
    : `<div class="muted" style="padding:8px">暂无流水</div>`;
  $("pgInfo").textContent = `${recentPage + 1} / ${pages}`;
  $("pgPrev").disabled = recentPage === 0;
  $("pgNext").disabled = recentPage >= pages - 1;
}

// makeChart 懒建 echarts 实例；CDN 不可达时在容器里显示提示并返回 null。
function makeChart(box, existing) {
  if (typeof echarts === "undefined") {
    box.textContent = "图表库加载失败（CDN 不可达），表格不受影响";
    return null;
  }
  return existing || echarts.init(box);
}

function renderUsageChart(st) {
  const box = $("usageChart");
  usageChart = makeChart(box, usageChart);
  if (!usageChart) return;
  const byDay = (st.token.by_day || []);
  const dates = byDay.map((d) => d.date);
  // 渠道系列：取所有出现过的渠道并集
  const chans = [...new Set(byDay.flatMap((d) => Object.keys(d.by_channel || {})))];
  const series = chans.map((ch) => ({
    name: CH_NAMES[ch] || ch, type: "line", smooth: true,
    data: byDay.map((d) => d.by_channel[ch] || 0),
  }));
  usageChart.setOption({
    tooltip: { trigger: "axis" },
    legend: { data: series.map((s) => s.name) },
    grid: { left: 50, right: 20, top: 36, bottom: 28 },
    xAxis: { type: "category", data: dates },
    yAxis: { type: "value", axisLabel: { formatter: (v) => fmtTokensFull(v) } },
    series,
  }, true);
  usageChart.resize();
}

// renderCreditChart 积分消耗折线图：按日聚合各账号 spend（原始条目自算），
// 只画消耗总量前 10 的账号；不含入项/过期。
function renderCreditChart(entries, names) {
  const box = $("creditChart");
  const spends = entries.filter((e) => e.kind === "spend");
  if (!spends.length) { box.style.display = "none"; if (creditChart) { creditChart.dispose(); creditChart = null; } return; }
  box.style.display = "";
  creditChart = makeChart(box, creditChart);
  if (!creditChart) return;
  // 按日 × 账号聚合
  const dates = [...new Set(spends.map((e) => fmtDay(e.ts)))].sort();
  const byAcct = {}; // uid -> {date: spend}
  const totals = {}; // uid -> 总消耗
  for (const e of spends) {
    const d = fmtDay(e.ts);
    (byAcct[e.uid] ||= {})[d] = (byAcct[e.uid][d] || 0) + -e.amount;
    totals[e.uid] = (totals[e.uid] || 0) + -e.amount;
  }
  const top10 = Object.keys(totals).sort((a, b) => totals[b] - totals[a]).slice(0, 10);
  // 系列名带渠道前缀（跨渠道同名昵称可区分）：取该账号消耗条目中最多的渠道
  const mainChOf = (uid) => {
    const cnt = {};
    for (const e of spends) if (e.uid === uid) cnt[e.channel] = (cnt[e.channel] || 0) + 1;
    return Object.keys(cnt).sort((a, b) => cnt[b] - cnt[a])[0] || "";
  };
  const series = top10.map((uid) => ({
    name: `${chLabel(mainChOf(uid))} ${names[uid] || shortUid(uid)}`, type: "line", smooth: true,
    data: dates.map((d) => byAcct[uid][d] || 0),
  }));
  creditChart.setOption({
    tooltip: { trigger: "axis", valueFormatter: (v) => fmtCredits(v) },
    legend: { data: series.map((s) => s.name) },
    grid: { left: 50, right: 20, top: 36, bottom: 28 },
    xAxis: { type: "category", data: dates },
    yAxis: { type: "value", axisLabel: { formatter: (v) => fmtCredits(v) } },
    series,
  }, true);
  creditChart.resize();
}

// fmtDay 时间戳 → "09-21"（与 token 图 X 轴口径一致）
function fmtDay(ts) {
  const d = new Date(ts * 1000);
  return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

// 主面板 tab 切换（账号管理 / 用量与流水 / 运行统计）
function bindMainTabs() {
  document.querySelectorAll(".main-tabs .mtab").forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll(".main-tabs .mtab").forEach((x) => x.classList.toggle("active", x === b));
      $("mtabAccounts").classList.toggle("hidden", b.dataset.mtab !== "accounts");
      $("mtabUsage").classList.toggle("hidden", b.dataset.mtab !== "usage");
      $("mtabStats").classList.toggle("hidden", b.dataset.mtab !== "stats");
      // 切换主 tab 强制收起积分明细 tooltip，避免残留浮在其他页签上
      const tip = $("creditTip");
      if (tip) { tip.style.display = "none"; if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; } }
      if (b.dataset.mtab === "usage") {
        loadUsage(); // 切到用量 tab 时拉最新（首次渲染自动刷新）
        if (usageChart) usageChart.resize();
        if (creditChart) creditChart.resize();
      }
      if (b.dataset.mtab === "stats") {
        loadStats(); // 切到运行统计 tab 时拉最新（后台 30s 轮询兜底）
      }
    };
  });

  bindFeesTabs(); // 费率面板渠道标签切换（事件委托一次绑定）
  bindSummaryFilter(); // 积分汇总条 chip 点击筛选账号（事件委托一次绑定）
}

// 面板 tab / 范围切换事件（bind 末尾调用）
function bindUsage() {
  $("btnRefreshUsage").onclick = loadUsage;
  $("usageRange").querySelectorAll(".seg-btn").forEach((b) => {
    b.onclick = () => {
      usageDays = parseInt(b.dataset.days, 10);
      $("usageRange").querySelectorAll(".seg-btn").forEach((x) => x.classList.toggle("active", x === b));
      loadUsage();
    };
  });
  document.querySelectorAll(".tabs .tab").forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll(".tabs .tab").forEach((x) => x.classList.toggle("active", x === b));
      $("tabToken").classList.toggle("hidden", b.dataset.tab !== "token");
      $("tabCredit").classList.toggle("hidden", b.dataset.tab !== "credit");
      if (b.dataset.tab === "token" && usageChart) usageChart.resize();
      if (b.dataset.tab === "credit" && creditChart) creditChart.resize();
    };
  });
  $("pgPrev").onclick = () => { if (recentPage > 0) { recentPage--; renderRecentPage(lastNames); } };
  $("pgNext").onclick = () => { recentPage++; renderRecentPage(lastNames); };
  window.addEventListener("resize", () => { if (usageChart) usageChart.resize(); if (creditChart) creditChart.resize(); });
}

// ---------- 运行统计面板（第三个主 tab；数据来自 internal/stats 引擎的 /api/stats） ----------
const STATS_POLL_MS = 30000;   // 统计/请求日志轮询周期
const APPLOG_POLL_MS = 15000;  // 运行日志（app.log）轮询周期
const APPLOG_MAX_LINES = 200;  // 渲染上限（上游 /api/logs 最多回 300 行，前端再多不渲染）
const STATS_LOGS_URL = "/api/stats/logs?limit=15";
const TOAST_SEEN_KEY = "stats-toast-seen"; // 里程碑 toast 已读去重（localStorage，防每轮轮询重复弹）

// 小数积分（模型行）：整数走千分位，非整数保留两位
function fmtCreditS(x) {
  x = Number(x) || 0;
  return Number.isInteger(x) ? fmtCredits(x) : x.toFixed(2);
}
// tok/s 一位小数（≥100 取整）；0 值由调用方给「—」不解为 0 速度
function fmtPct(x) {
  if (!x) return "";
  return x >= 100 ? String(Math.round(x)) : x.toFixed(1);
}
// Trae 系渠道判定（TraeWork/TraeCode 共用账号积分池，上游 usage 帧不含单次积分 → 积分为分摊估算）
function isTraeChannel(model) {
  const s = String(model || "");
  const i = s.indexOf("/");
  return i > 0 && (s.slice(0, i) === "traework" || s.slice(0, i) === "traecode");
}

let statsRefreshing = false; // 防重入：上轮未完成时跳过本轮（防慢响应晚到覆盖新数据）
let statsWarned = false;     // 拉取失败只 warn 一次，避免控制台刷屏
let statsLastOk = "";        // 最近一次成功拉取时刻（失败角标要注明数据停止于何时）

// 更新角标：成功 → 记录时刻；失败 → 标红注明数据停止时间（陈旧数据必须可感知）
function stMarkUpdated() {
  const el = $("stUpdated");
  el.classList.remove("st-stale");
  el.textContent = "上次更新 " + (statsLastOk = new Date().toTimeString().slice(0, 8));
}
function stMarkStale() {
  const el = $("stUpdated");
  el.classList.add("st-stale");
  el.textContent = statsLastOk ? `拉取失败 · 数据停止于 ${statsLastOk}` : "拉取失败";
}

async function loadStats() {
  if (statsRefreshing) return;
  statsRefreshing = true;
  try {
    const [s, lg] = await Promise.all([
      api("/api/stats"),
      api(STATS_LOGS_URL).catch(() => null), // 失败保旧：跳过本轮渲染，不清空已渲染的请求日志
    ]);
    renderStats(s);
    renderAbnormal(s.abnormal || []);
    if (lg) renderLogs(lg.rows || []);
    stMarkUpdated();
  } catch (e) {
    stMarkStale();
    if (!statsWarned) { statsWarned = true; console.warn("运行统计加载失败：", e); }
  } finally {
    statsRefreshing = false;
  }
}

async function loadAppLog() {
  try {
    const d = await api("/api/logs");
    renderAppLog(d.lines || []);
  } catch (e) { /* 上游瞬时不可达：保留旧内容，下轮重试 */ }
}

// 汇总横条条目（标签 + 等宽数值 + 可选小字注解）
function stSumItem(label, val, cls, sub) {
  return `<span class="st-sum-item"><span class="st-sum-label">${label}</span>` +
    `<span class="st-sum-val ${cls || ""}">${val}</span>` +
    (sub ? `<span class="st-sum-sub">${sub}</span>` : "") + `</span>`;
}
// Token 多天行条目
function stTkItem(label, val) {
  return `<span class="st-tk-item">${label} <span class="st-tk-val">${val}</span></span>`;
}

function renderStats(s) {
  const tu = s.token_usage || {};
  // 汇总横条：作废/今日到期仅 >0 时出现（避免每天多条 0 值噪音）
  $("stSum").innerHTML =
    stSumItem("总积分", fmtCredits(s.total_credits) + "分", "pur", fmtCredits(s.total_accounts) + " 个账号") +
    stSumItem("收入积分", fmtCredits(s.credit_in), "ok", "签到事件口径") +
    stSumItem("今日消耗", fmtCredits(s.credit_used), "warn", "精确 " + fmtCreditS(s.credit_out_exact)) +
    (Number(s.credit_expired) > 0
      ? stSumItem("今日作废", fmtCredits(s.credit_expired), "risk", "积分包到期 · 非消耗")
      : "") +
    stSumItem("消耗 Token", fmtTokensFull(s.tokens), "warn") +
    (Number(s.expire_today) > 0
      ? stSumItem("今日到期", fmtCredits(s.expire_today) + "分", "risk", "今晚作废尽快消耗")
      : "");
  $("stMilestone").textContent = s.milestone_count > 0 ? `里程碑 ${s.milestone_count}×100分` : "";
  renderPlatformTable(s);
  // Token 多天行（今日情况标题右侧）
  $("stToken").innerHTML =
    stTkItem("Token 今日", fmtTokensFull(tu.today)) +
    stTkItem("7日", fmtTokensFull(tu.days7)) +
    stTkItem("30日", fmtTokensFull(tu.days30)) +
    stTkItem("请求 今日", fmtCredits(tu.req_today) + " / " + fmtCredits(tu.req_all)) +
    stTkItem("已记录", fmtCredits(tu.days) + " 天");
  renderRotation(s.rotation || []);
  renderExpiry(s.expiry || []);
  renderModels(s.models || []);
  handleToasts(s.toasts || []);
}

// 平台表（今日情况主视图）：列 = #/平台/账号/当前积分/今日收入/今日消耗(净值)/今日Token/
// 今日到期/明日到期/7日内到期，末行合计。「今日消耗」= 差值花费 − 积分包到期作废（净值才是真消耗）。
// 合计行取全池口径，与顶部汇总横条严格同源（合计 == 横条可互相印证）。
function renderPlatformTable(s) {
  const rows = s.platforms || [];
  if (!rows.length) { $("stPlat").innerHTML = ""; return; }
  let html = `<div class="st-plat-row st-plat-head">` +
    `<span class="st-plat-no">#</span>` +
    `<span class="st-plat-slot">平台</span>` +
    `<span class="st-plat-accts">账号</span>` +
    `<span class="st-plat-credits">当前积分</span>` +
    `<span class="st-plat-in">今日收入</span>` +
    `<span class="st-plat-out" title="花费差值扣除积分包到期作废后的净消耗">今日消耗</span>` +
    `<span class="st-plat-tokens">今日消耗Token</span>` +
    `<span class="st-plat-exp">今日到期</span>` +
    `<span class="st-plat-exp">明日到期</span>` +
    `<span class="st-plat-exp">7日内到期</span>` +
    `</div>`;
  rows.forEach((r, i) => {
    html += `<div class="st-plat-row">` +
      `<span class="st-plat-no">${i + 1}</span>` +
      `<span class="st-plat-slot"><span class="badge ${chClass(r.group)}">${esc(chLabel(r.group))}</span></span>` +
      `<span class="st-plat-accts">${fmtCredits(r.accounts)}</span>` +
      stPlatNum("st-plat-credits", r.credits, "分") +
      stPlatNum("st-plat-in", r.credit_in) +
      stPlatNum("st-plat-out", r.credit_used) +
      stPlatTok(r.tokens) +
      stPlatNum("st-plat-exp", r.expire_today, "分") +
      stPlatNum("st-plat-exp", r.expire_tomorrow, "分") +
      stPlatNum("st-plat-exp", r.expire_7d, "分") +
      `</div>`;
  });
  // 合计行：与顶部汇总横条同源
  html += `<div class="st-plat-row st-plat-sum-row">` +
    `<span class="st-plat-no"></span>` +
    `<span class="st-plat-slot">合计</span>` +
    `<span class="st-plat-accts">${fmtCredits(s.total_accounts)}</span>` +
    stPlatNum("st-plat-credits", s.total_credits, "分") +
    stPlatNum("st-plat-in", s.credit_in) +
    stPlatNum("st-plat-out", s.credit_used) +
    stPlatTok(s.tokens) +
    stPlatNum("st-plat-exp", s.expire_today, "分") +
    stPlatNum("st-plat-exp", s.expire_tomorrow, "分") +
    stPlatNum("st-plat-exp", s.expiring_7d, "分") +
    `</div>`;
  $("stPlat").innerHTML = html;
}

// 平台表数值单元格：0 值置灰「—」（避免满屏 0 淹没真正有数的列），颜色由列类自带
function stPlatNum(cls, amount, suffix) {
  amount = Number(amount) || 0;
  if (amount <= 0) return `<span class="${cls} st-plat-zero">—</span>`;
  return `<span class="${cls}">${fmtCredits(amount)}${suffix || ""}</span>`;
}
// Token 单元格：单位走 万/亿，0 同样置灰
function stPlatTok(amount) {
  amount = Number(amount) || 0;
  if (amount <= 0) return `<span class="st-plat-tokens st-plat-zero">—</span>`;
  return `<span class="st-plat-tokens">${fmtTokensFull(amount)}</span>`;
}

const ST_ROT_MARKS = ["①", "②", "③"];

// 使用中 / 接棒顺序：当前=粘性账号；接棒=临期优先+余额排序推算的前 3
function renderRotation(rot) {
  if (!rot.length) { $("stRotation").innerHTML = ""; $("stRotation").className = "st-rotation"; return; }
  let html = `<div class="st-block-title">使用中 / 接棒顺序<span class="st-hint">当前=粘性账号 · 接棒=临期优先+余额排序推算</span></div>`;
  for (const rv of rot) {
    const cur = rv.current
      ? `使用中 <b>${esc(rv.current.nickname)}</b>（粘性 ${esc(rv.current.sticky || "0")}，余额 ${fmtCredits(rv.current.credits)}分）`
      : `<span class="st-rot-none">暂无使用记录</span>`;
    let nextHtml = "";
    (rv.next || []).forEach((n, j) => {
      nextHtml += `<span class="st-rot-next">${ST_ROT_MARKS[j]} ${esc(n.nickname)}` +
        `（${fmtCredits(n.credits)}分${n.expiring > 0 ? "·临期" + fmtCredits(n.expiring) : ""}）</span>`;
    });
    html += `<div class="st-rot-row"><span class="badge ${chClass(rv.group)}">${esc(chLabel(rv.group))}</span>` +
      `<span class="st-rot-cur">${cur}</span>` +
      (nextHtml ? `<span class="st-rot-nexts">${nextHtml}</span>` : "") + `</div>`;
  }
  $("stRotation").innerHTML = html;
  $("stRotation").className = "st-rotation st-block";
}

// 最近临期：到期日升序；risk=按今日速度花不完 → 给预计作废量（额度−外推消耗）
function renderExpiry(ex) {
  if (!ex.length) { $("stExpiry").innerHTML = ""; $("stExpiry").className = "st-expiry"; return; }
  let html = `<div class="st-block-title">最近临期<span class="st-hint">按到期日升序 · ${ex.length} 个账号</span></div>` +
    `<div class="st-scroll st-ex-scroll">` +
    `<div class="st-ex-row st-ex-head">` +
    `<span class="st-ex-tag">#</span>` +
    `<span class="st-ex-plat-slot">平台</span>` +
    `<span class="st-ex-name">账号</span>` +
    `<span class="st-ex-date">到期日</span>` +
    `<span class="st-ex-days">剩余</span>` +
    `<span class="st-ex-amt">额度 · 今日已耗</span>` +
    `</div>`;
  ex.forEach((r, i) => {
    const dayTxt = r.days_left <= 0 ? "今日到期" : (r.days_left === 1 ? "明日到期" : `${r.days_left} 天后到期`);
    const burn = r.risk ? ` · 预计作废 ${fmtCredits(r.amount - r.burn_forecast)}分` : "";
    html += `<div class="st-ex-row${r.risk ? " st-risk" : ""}">` +
      `<span class="st-ex-tag">${i + 1}</span>` +
      `<span class="st-ex-plat-slot"><span class="badge ${chClass(r.group)}">${esc(chLabel(r.group))}</span></span>` +
      `<span class="st-ex-name" title="${esc(r.uid)}">${esc(r.nickname || r.uid)}</span>` +
      `<span class="st-ex-date">${esc(r.expire_at)}</span>` +
      `<span class="st-ex-days">${esc(dayTxt)}</span>` +
      `<span class="st-ex-amt">${fmtCredits(r.amount)}分 · 已耗 ${fmtCredits(r.today_out)}${burn}</span>` +
      `</div>`;
  });
  html += `</div>`;
  $("stExpiry").innerHTML = html;
  $("stExpiry").className = "st-expiry st-block";
}

// 模型消耗（今日，按积分降序）：Trae 系积分加「≈」（分摊估算），平均速度=成功请求实测均值
function renderModels(models) {
  let mh = `<tr><th>#</th><th>模型</th><th>次数</th><th>Token</th><th>积分</th>` +
    `<th title="成功请求实测均值：tok/s = 生成 token ÷ 生成耗时（按时长加权）；TTFB = 首字节耗时算术平均">平均速度</th></tr>`;
  models.forEach((m, j) => {
    const isTrae = isTraeChannel(m.model);
    let creditCell;
    if (m.credit > 0) {
      creditCell = isTrae
        ? `<span title="TraeWork 上游不下发单次积分：按该渠道今日真实消耗（余额差值）与 token 占比分摊，为估算值">≈${fmtCreditS(m.credit)}</span>`
        : fmtCreditS(m.credit);
    } else {
      creditCell = isTrae
        ? `<span title="TraeWork 无单次精确积分，按渠道真实消耗分摊；暂无余额差值消耗">—</span>`
        : fmtCreditS(m.credit);
    }
    const tps = Number(m.avg_tok_per_sec) || 0;
    const ttfb = Number(m.avg_ttfb_ms) || 0;
    const parts = [];
    if (tps > 0) parts.push(fmtPct(tps) + " tok/s");
    if (ttfb > 0) parts.push(fmtCredits(ttfb) + "ms");
    const perfCell = parts.length
      ? `<span title="样本 ${fmtCredits(m.perf_samples)} 次成功请求">${parts.join(" · ")}</span>`
      : "—";
    mh += `<tr><td class="st-idx">${j + 1}</td>` +
      `<td class="st-mono" title="${esc(m.model)}">${esc(m.model)}</td>` +
      `<td>${fmtCredits(m.reqs)}</td><td>${fmtTokensFull(m.tokens)}</td><td>${creditCell}</td>` +
      `<td class="st-mono">${perfCell}</td></tr>`;
  });
  $("stModels").innerHTML = models.length ? mh : `<tr><td class="st-empty" colspan="6">今日暂无消耗</td></tr>`;
}

// 请求日志：降序渲染（最新在最上）；tok/s 仅流式有值，0/空给「—」
function renderLogs(rows) {
  let h = `<tr><th>#</th><th>时间</th><th>模型</th><th>模式</th><th>状态</th><th>TTFB</th><th>tok</th><th>tok/s</th><th>total</th><th>积分</th></tr>`;
  for (let i = rows.length - 1; i >= 0; i--) {
    const r = rows[i];
    const status = r.status < 400 ? String(r.status) : `<span class="st-bad">${r.status}</span>`;
    const tps = r.tok_per_sec > 0 ? fmtPct(r.tok_per_sec) : "—";
    h += `<tr><td>${fmtCredits(r.seq)}</td><td class="st-mono">${esc(r.time)}</td><td class="st-mono">${esc(r.model)}</td>` +
      `<td>${esc(r.mode)}</td><td>${status}</td><td>${fmtCredits(r.ttfb_ms)}ms</td><td>${fmtCredits(r.tok)}</td><td>${tps}</td>` +
      `<td>${r.total_sec ? r.total_sec.toFixed(1) + "s" : "—"}</td><td>${r.credit > 0 ? fmtCreditS(r.credit) : "—"}</td></tr>`;
  }
  $("stLogs").innerHTML = rows.length ? h : `<tr><td class="st-empty">暂无请求</td></tr>`;
}

// 异常账号（三级：disabled 已停用 / cool 整号冷却 / err 错误累计；无异常整块隐藏）
const ABNORMAL_META = {
  disabled: { cls: "st-ab-disabled", tag: "已停用", tip: "账号被上游停用（如登录态失效），需人工处理" },
  cool: { cls: "st-ab-cool", tag: "整号冷却", tip: "账号整体进入冷却（限流/余额不足/账号故障），解冻前不可用" },
  err: { cls: "st-ab-err", tag: "错误累计", tip: "连续错误累计中，达阈值将触发冷却" },
};

function renderAbnormal(rows) {
  if (!rows.length) { $("stAbnormal").innerHTML = ""; $("stAbnormal").className = "st-abnormal"; return; }
  const counts = { disabled: 0, cool: 0, err: 0 };
  rows.forEach((r) => { if (counts[r.level] != null) counts[r.level]++; });
  // 摘要徽章只显示存在的级别（顺序固定：停用→整号冷却→错误累计）
  let sumHtml = "";
  ["disabled", "cool", "err"].forEach((lv) => {
    if (counts[lv] > 0) {
      sumHtml += `<span class="st-ab-sum ${ABNORMAL_META[lv].cls}" title="${esc(ABNORMAL_META[lv].tip)}">` +
        `${ABNORMAL_META[lv].tag} ${counts[lv]}</span>`;
    }
  });
  let html = `<div class="st-block-title">异常账号<span class="st-hint">限流 / 冻结 / 亚健康 · 共 ${rows.length} 个</span>` +
    `<span class="st-ab-sums">${sumHtml}</span></div>` +
    `<div class="st-scroll st-ab-scroll">` +
    `<div class="st-ab-row st-ab-head">` +
    `<span class="st-ab-idx">#</span>` +
    `<span class="st-ab-tag-slot">状态</span>` +
    `<span class="st-ab-plat-slot">平台</span>` +
    `<span class="st-ab-name">账号</span>` +
    `<span class="st-ab-reason">原因</span>` +
    `<span class="st-ab-until">解冻</span>` +
    `<span class="st-ab-credit">余额</span>` +
    `</div>`;
  rows.forEach((r, i) => {
    const meta = ABNORMAL_META[r.level] || ABNORMAL_META.err;
    let reason = r.reason || meta.tag;
    if (r.level === "err" && r.err_count) reason = `连续错误 ${fmtCredits(r.err_count)} 次`;
    html += `<div class="st-ab-row ${meta.cls}">` +
      `<span class="st-ab-idx">${i + 1}</span>` +
      `<span class="st-ab-tag-slot"><span class="st-ab-tag" title="${esc(meta.tip)}">${meta.tag}</span></span>` +
      `<span class="st-ab-plat-slot"><span class="badge ${chClass(r.group)}">${esc(chLabel(r.group))}</span></span>` +
      `<span class="st-ab-name" title="${esc(r.uid)}">${esc(r.nickname || r.uid)}</span>` +
      `<span class="st-ab-reason" title="${esc(reason)}">${esc(reason)}</span>` +
      `<span class="st-ab-until">${r.until ? esc(r.until) : "—"}</span>` +
      `<span class="st-ab-credit">${r.credits > 0 ? fmtCredits(r.credits) + "分" : "—"}</span>` +
      `</div>`;
  });
  html += `</div>`;
  $("stAbnormal").innerHTML = html;
  $("stAbnormal").className = "st-abnormal st-block";
}

// 运行日志（app.log 尾部）：级别判定顺序有讲究——
// ① code=9074/ok=false 先标红（「签到完成」话术里装的业务失败，先判会被误放行为 info）；
// ② 已签到 是良性重复签到，不标红；③ 汇总成功行（含 failed=N）仍按成功放行。
function appLogLevel(text) {
  if (/code=9074|ok=false/.test(text)) return "err";
  if (/完成|成功|ok=true|已签到/.test(text)) return "info";
  if (/PANIC|FATAL|失败|错误|无效|Error|failed/.test(text)) return "err";
  if (/warn|WARN|警告/.test(text)) return "warn";
  return "info";
}

let applogFirst = true;
function renderAppLog(lines) {
  // 用户上翻阅读时不打扰；贴底（或首帧）才自动吸底
  const box = $("stApplog");
  const pinned = applogFirst || box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  applogFirst = false;
  let html = "";
  const start = Math.max(0, lines.length - APPLOG_MAX_LINES);
  for (let i = start; i < lines.length; i++) {
    const line = lines[i];
    if (!line) continue;
    // Go log 前缀 "2026/09/22 15:04:05.123456 " → 展示 MM-DD HH:MM:SS
    const m = line.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?)\s(.*)$/);
    const cls = `st-logline st-log-${appLogLevel(line)}`;
    if (m) {
      html += `<div class="${cls}"><span class="st-log-time">${esc(m[1].slice(5, 19))}</span>${esc(m[2])}</div>`;
    } else {
      html += `<div class="${cls}">${esc(line)}</div>`;
    }
  }
  box.innerHTML = html || `<div class="st-empty">暂无日志</div>`;
  if (pinned) box.scrollTop = box.scrollHeight;
}

// 里程碑 toast：浏览器通知优先，降级面板 toast（复用上游 #toast）；localStorage 按 id 去重
function statsToastSeen() {
  try { return JSON.parse(localStorage.getItem(TOAST_SEEN_KEY) || "[]"); } catch (e) { return []; }
}
function statsToastMark(id) {
  const ids = statsToastSeen();
  ids.push(id);
  try { localStorage.setItem(TOAST_SEEN_KEY, JSON.stringify(ids.slice(-200))); } catch (e) { /* 忽略 */ }
}
function handleToasts(list) {
  let seen = {};
  try { statsToastSeen().forEach((x) => { seen[x] = 1; }); } catch (e) { seen = {}; }
  for (const t of list) {
    if (seen[t.id]) continue;
    showStatsNotification(t.title, t.body);
    statsToastMark(t.id);
  }
}
function showStatsNotification(title, body) {
  try {
    if ("Notification" in window) {
      if (Notification.permission === "granted") {
        new Notification(title, { body });
        return;
      }
      if (Notification.permission === "default") Notification.requestPermission();
    }
  } catch (e) { /* 降级面板提示 */ }
  toast(`${title}：${body}`);
}

