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

// fmtDateTime24 时间戳 → "MM-DD HH:MM"（强制 24 小时制，两位补零）。
// 不用 toLocaleString：它随浏览器 locale/ICU 走，某些环境会渲染成
// 12 小时制（7:07 PM）或「上午/下午」前缀，与全站 24h 制口径不一致。
function fmtDateTime24(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// fmtLeft 解冻时刻（unix 秒）→ 「剩 X 时 X 分」；已过期返回「即将解冻」。
// 冷却跨零点时 24h 制绝对时刻（如 10-18 07:07 = 次日清晨）极易被误读成
// 「19 点被 12 小时制截成 7 点」，并列相对时长后不再歧义。
function fmtLeft(untilTs) {
  const sec = untilTs - Date.now() / 1000;
  if (sec <= 0) return "即将解冻";
  const m = Math.floor(sec / 60);
  if (m < 60) return `剩 ${m} 分`;
  return `剩 ${Math.floor(m / 60)} 时 ${String(m % 60).padStart(2, "0")} 分`;
}

// shortKey 顶栏 API-Key 中间省略（同 shortUid 风格），完整值存 data-key 供复制。
function shortKey(k) {
  return k.length <= 24 ? k : k.slice(0, 6) + "…" + k.slice(-4);
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
    lastFees = fees; // 供模型查找面板取全渠道模型（纯前端，不改后端）
    renderFees(fees);
  } catch (e) { /* 费率接口失败不阻塞 */ }
}

async function refreshFees() {
  $("btnRefreshFees").disabled = true;
  try {
    await api("/api/fees/refresh", {});
    const fees = await api("/api/fees");
    lastFees = fees;
    renderFees(fees);
    toast("模型列表和费率已刷新");
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshFees").disabled = false;
  }
}

// ---------- 模型查找（纯前端：从最近一次 /api/fees 结果里按关键字过滤） ----------
// lastFees 最近一次费率接口响应（loadFees/refreshFees 填充）；查找面板读它取全渠道模型。
let lastFees = null;

// collectAllModels 展平全渠道模型，附带裸名/完整 ID（完整 ID = 渠道/模型，即客户端该填的名字）。
function collectAllModels() {
  const out = [];
  for (const ch of (lastFees && lastFees.channels) || []) {
    for (const m of ch.models || []) {
      const raw = m.model;
      const short = raw.includes("/") ? raw.slice(raw.indexOf("/") + 1) : raw;
      // 完整 ID 一律取服务端给的 m.id（与 /v1/models、provider.PublicModelID 同源）；
      // 缺字段时退回旧拼接规则（兼容旧后端缓存）：上游 ID 自带命名空间（cline-free/*）
      // 的渠道由服务端直接透传上游 ID，不再补 `<渠道>/` 前缀（R59）。
      const full = m.id || (ch.channel && !raw.startsWith(`${ch.channel}/`) ? `${ch.channel}/${raw}` : raw);
      out.push({ ch: ch.channel, m, short, full });
    }
  }
  return out;
}

function openModelSearch() {
  // 弹层互斥：打开查找模型时收起设置（#134）
  $("settingsOverlay").classList.add("hidden");
  $("modelSearchOverlay").classList.remove("hidden");
  const inp = $("modelSearchInput");
  inp.value = "";
  renderModelSearch("");
  inp.focus();
}
function closeModelSearch() { $("modelSearchOverlay").classList.add("hidden"); }

// renderModelSearch 按关键字过滤全渠道模型并渲染（对话框固定高度，结果在 .ms-list 内滚动）。
// 关键字匹配完整 ID 或裸名（大小写不敏感）——两处都包含渠道名，故搜渠道亦可。
function renderModelSearch(q) {
  const kw = String(q || "").trim().toLowerCase();
  const all = collectAllModels();
  const hit = kw ? all.filter((x) => x.full.toLowerCase().includes(kw) || x.short.toLowerCase().includes(kw)) : all;
  const box = $("modelSearchList");
  if (!hit.length) {
    box.innerHTML = `<div class="ms-empty">${all.length ? "无匹配模型" : "费率数据尚未加载，请点「刷新」后再试"}</div>`;
  } else {
    box.innerHTML = hit.map(({ ch, m, short, full }) => {
      const caps = [m.supports_images ? "👁" : "", m.supports_reasoning ? "🧠" : ""].join("");
      // 档位小字（issue #96）：上游声明才显示
      const eff = (m.reasoning_efforts && m.reasoning_efforts.length)
        ? `<span class="ms-eff" title="思考档位：${esc(m.reasoning_efforts.join("·"))}${m.default_reasoning_effort ? `（默认 ${m.default_reasoning_effort}）` : ""}">${esc(m.reasoning_efforts.join("·"))}</span>` : "";
      const ctx = (m.has_context && m.context_window)
        ? `<span title="上下文窗口：${esc(fmtTokensCompact(m.context_window))} tokens（${m.hand_measured ? "本工具实测，仅供参考" : "上游接口返回"}）">(${esc(fmtTokensCompact(m.context_window))}${m.hand_measured ? "*" : ""})</span>` : "";
      const rate = m.free ? "Free" : (m.priced ? `x${m.rate.toFixed(2)}` : "unknown");
      return `<div class="ms-row" data-mid="${esc(full)}" role="button" tabindex="0" title="点击复制完整模型 ID：${esc(full)}">`
        + `<span class="badge ${chClass(ch)}">${esc(chLabel(ch))}</span>`
        + `<span class="ms-name">${esc(short)}</span>`
        + `<span class="ms-tags">${ctx}${caps}${eff}</span>`
        + `<span class="ms-rate">${esc(rate)}</span>`
        + `</div>`;
    }).join("");
  }
  // 底部提示：命中数（结果全量渲染，列表本身可滚动，故不再提示「未显示」）。
  $("modelSearchFoot").textContent = !all.length ? ""
    : (hit.length === 0 ? "无匹配" : `共 ${hit.length} 个匹配`);
}

// ---------- 配置区模糊搜索下拉（combobox）：模型ID / 思考档位输入框复用「查找模型」数据 ----------
// attachModelPicker 把 <input> 升级为可搜索下拉：focus/input 渲染候选（数据源与「查找模型」
// 弹层同源——lastFees 缓存展平，字段与文案对齐 renderModelSearch），↑↓ 高亮、Enter 选中、
// Esc / 失焦 / 点击外部关闭；仍可自由手输（无匹配不强制选择，输入框即 combobox）。
// opts.onPick(id)：选中回调（缺省=仅回填并关菜单）；
// opts.enterAction(e)：菜单关闭时按回车的原行为（如「跳到下一个输入框 / 提交」）；
// opts.stayOpen：多选模式——点选后不关菜单、清空输入、刷新候选（配合 excludeSet 连续多选）；
// opts.multiAccum：累积多选——逗号分隔逐个追加，过滤关键字取末段、已累积段从候选隐藏；
// opts.excludeSet()：返回需在候选中完全隐藏的模型集合（完整 ID 精确等值 -> true）。
let pkOpenMenu = null;
function pkClose() {
  document.querySelectorAll(".pk-menu.open").forEach((m) => m.classList.remove("open"));
  pkOpenMenu = null;
}
// 点击任意输入框/菜单之外的区域关闭当前下拉（capture 保证先于其他 mousedown 处理器）
document.addEventListener("mousedown", (e) => {
  if (!pkOpenMenu) return;
  if (e.target.closest(".pk-wrap")) return;
  pkClose();
}, true);
// pkHl 命中段高亮（先分段转义再拼 <mark>，与 .ms-list 的 mark 样式共用）
function pkHl(text, kw) {
  const k = String(kw || "").trim().toLowerCase();
  if (!k) return esc(text);
  const i = String(text).toLowerCase().indexOf(k);
  if (i < 0) return esc(text);
  return esc(text.slice(0, i)) + "<mark>" + esc(text.slice(i, i + k.length)) + "</mark>" + esc(text.slice(i + k.length));
}
// pkItemHTML 候选项两行富信息：行1 渠道徽标 + 完整 ID（命中段高亮）+ 价格；
// 行2 上下文窗口（实测带 *）/ 能力图标 / 思考档位（含默认）——与 renderModelSearch 逐字对齐。
function pkItemHTML(x, kw) {
  const m = x.m;
  const caps = (m.supports_images ? "👁" : "") + (m.supports_reasoning ? "🧠" : "");
  const ctx = (m.has_context && m.context_window) ? `(${esc(fmtTokens(m.context_window))}${m.hand_measured ? "*" : ""})` : "";
  const eff = (m.reasoning_efforts && m.reasoning_efforts.length)
    ? esc(m.reasoning_efforts.join("·")) + (m.default_reasoning_effort ? `（默认 ${esc(m.default_reasoning_effort)}）` : "") : "";
  const rate = m.free ? "Free" : (m.priced ? `x${(+m.rate).toFixed(2)}` : "unknown");
  return `<div class="pk-item pk-rich" data-id="${esc(x.full)}" title="点击选用 ${esc(x.full)}">`
    + `<div class="pk-line1"><span class="badge ${chClass(x.ch)}">${esc(chLabel(x.ch))}</span><span class="id">${pkHl(x.full, kw)}</span><span class="rate">${esc(rate)}</span></div>`
    + `<div class="pk-line2">${ctx}${caps}${eff ? `<span class="pk-eff">${eff}</span>` : ""}</div></div>`;
}
function attachModelPicker(input, opts) {
  opts = opts || {};
  const wrap = input.closest(".pk-wrap");
  if (!wrap) return;
  let menu = wrap.querySelector(".pk-menu");
  if (!menu) { menu = document.createElement("div"); menu.className = "pk-menu"; wrap.appendChild(menu); }
  let hlIdx = -1;
  function render() {
    let kw, exclude;
    const baseExcl = opts.excludeSet ? opts.excludeSet() : null;
    if (opts.multiAccum) {
      // 累积多选：过滤关键字取末段（最后一个逗号后），已累积段从候选隐藏（精确等值）；
      // 同时合并 excludeSet（如主力模型ID全局排除），每次 render 实时取——主力列表变更即同步可选范围。
      const parts = String(input.value || "").split(/[,，]/);
      kw = (parts[parts.length - 1] || "").trim();
      exclude = baseExcl ? { ...baseExcl } : {};
      for (let k = 0; k < parts.length - 1; k++) { const seg = parts[k].trim(); if (seg) exclude[seg] = true; }
    } else {
      kw = input.value.trim();
      exclude = baseExcl;
    }
    const kwl = kw.toLowerCase();
    const all = collectAllModels();
    const items = all.filter((x) => {
      if (exclude && exclude[x.full]) return false;
      if (!kwl) return true;
      return x.full.toLowerCase().includes(kwl) || x.short.toLowerCase().includes(kwl);
    });
    hlIdx = -1;
    menu.innerHTML = items.length
      ? items.slice(0, 40).map((x) => pkItemHTML(x, kw)).join("")
      : `<div class="pk-empty">${all.length ? "无匹配模型（可继续手动输入）" : "费率数据尚未加载，请点「刷新」后再试"}</div>`;
    menu.classList.add("open");
    pkOpenMenu = menu;
  }
  function pick(id) {
    if (opts.stayOpen && !opts.multiAccum) {
      // 多选（点选即追加）：回调追加后清空输入继续搜；onPick 可能重建 DOM（重渲染），先判连接
      if (opts.onPick) opts.onPick(id);
      if (!input.isConnected) return;
      input.value = "";
      render();
      return;
    }
    if (opts.multiAccum) {
      // 累积多选：选中项替换末段并补逗号，光标留在新末段继续搜索/输入
      const parts = String(input.value || "").split(/[,，]/);
      parts[parts.length - 1] = id;
      input.value = parts.join(",") + ",";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      render();
      return;
    }
    input.value = id;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    pkClose();
    if (opts.onPick) opts.onPick(id); else input.focus();
  }
  function moveHl(d) {
    const nodes = menu.querySelectorAll(".pk-item");
    if (!nodes.length) return;
    hlIdx = (hlIdx + d + nodes.length) % nodes.length;
    nodes.forEach((n, k) => n.classList.toggle("pk-hl", k === hlIdx));
    nodes[hlIdx].scrollIntoView({ block: "nearest" });
  }
  input.addEventListener("focus", render);
  input.addEventListener("input", render);
  input.addEventListener("keydown", (e) => {
    if (!menu.classList.contains("open")) { if (e.key === "Enter" && opts.enterAction) opts.enterAction(e); return; }
    // 菜单开着时按键由下拉消化，stopPropagation 防触发 #fbList 上的 Enter 委托（如「回车追加备胎」）
    if (e.key === "ArrowDown") { e.preventDefault(); e.stopPropagation(); moveHl(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); e.stopPropagation(); moveHl(-1); }
    else if (e.key === "Escape") { pkClose(); }
    else if (e.key === "Enter") {
      e.preventDefault(); e.stopPropagation();
      const nodes = menu.querySelectorAll(".pk-item");
      if (hlIdx >= 0 && nodes[hlIdx]) pick(nodes[hlIdx].dataset.id);
      else if (nodes.length === 1) pick(nodes[0].dataset.id);
      else { pkClose(); if (opts.enterAction) opts.enterAction(e); }
    }
  });
  // 失焦即关（菜单项 mousedown 已 preventDefault，点菜单项不会先触发 blur）
  input.addEventListener("blur", () => setTimeout(pkClose, 120));
  menu.addEventListener("mousedown", (e) => e.preventDefault());
  menu.addEventListener("click", (e) => {
    const it = e.target.closest(".pk-item");
    if (it) pick(it.dataset.id);
  });
}

// 思考档位下拉（单选模糊搜索，交互与模型下拉一致；候选为常见档位，仍可自由输入）。
// 渠道支持什么档位由渠道模型自己决定（后端哑管道不校验），故候选只是输入建议。
const PK_EFFORTS = ["none", "low", "medium", "high", "xhigh", "max"];
function attachEffortPicker(input, opts) {
  opts = opts || {};
  const wrap = input.closest(".pk-wrap");
  if (!wrap) return;
  let menu = wrap.querySelector(".pk-menu");
  if (!menu) { menu = document.createElement("div"); menu.className = "pk-menu"; wrap.appendChild(menu); }
  let hlIdx = -1;
  function render() {
    const kw = input.value.trim().toLowerCase();
    const list = PK_EFFORTS.filter((t) => !kw || t.includes(kw));
    hlIdx = -1;
    menu.innerHTML = list.length
      ? list.map((t) => `<div class="pk-item" data-id="${t}" title="点击选用 ${t}"><span class="id">${pkHl(t, kw)}</span><span class="pk-ch-tag">档位</span></div>`).join("")
      : `<div class="pk-empty">无匹配档位（可自由输入）</div>`;
    menu.classList.add("open");
    pkOpenMenu = menu;
  }
  function pick(id) {
    input.value = id;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    pkClose();
    if (opts.onPick) opts.onPick(id); else input.focus();
  }
  function moveHl(d) {
    const nodes = menu.querySelectorAll(".pk-item");
    if (!nodes.length) return;
    hlIdx = (hlIdx + d + nodes.length) % nodes.length;
    nodes.forEach((n, k) => n.classList.toggle("pk-hl", k === hlIdx));
  }
  input.addEventListener("focus", render);
  input.addEventListener("input", render);
  input.addEventListener("keydown", (e) => {
    if (!menu.classList.contains("open")) { if (e.key === "Enter" && opts.enterAction) opts.enterAction(e); return; }
    if (e.key === "ArrowDown") { e.preventDefault(); e.stopPropagation(); moveHl(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); e.stopPropagation(); moveHl(-1); }
    else if (e.key === "Escape") { pkClose(); }
    else if (e.key === "Enter") {
      e.preventDefault(); e.stopPropagation();
      const nodes = menu.querySelectorAll(".pk-item");
      if (hlIdx >= 0 && nodes[hlIdx]) pick(nodes[hlIdx].dataset.id);
      else if (nodes.length === 1) pick(nodes[0].dataset.id);
      else { pkClose(); if (opts.enterAction) opts.enterAction(e); }
    }
  });
  input.addEventListener("blur", () => setTimeout(pkClose, 120));
  menu.addEventListener("mousedown", (e) => e.preventDefault());
  menu.addEventListener("click", (e) => {
    const it = e.target.closest(".pk-item");
    if (it) pick(it.dataset.id);
  });
}

// ---------- 积分明细浮窗 ----------
// 明细条数可能很多（TraeWork 每个签到奖励都是独立条目，常见 30+），
// 故分页展示：每页 DETAIL_PAGE_SIZE 条，页码状态存在 detailState 里，
// 翻页不重新请求（detailCache 已缓存该账号的完整响应）。
//
// 交互：**点击**积分标签打开，✕ / 点击浮窗之外 / Esc 关闭（三个关闭路径都归一到
// closeCreditDetail）。为什么不用 hover：
//   ① 浮窗开着时后台 loadState() 会重建卡片 DOM，被替换的元素不再派发 mouseleave
//      ⇒ 浮窗变成孤儿、关不掉（用户实际遇到的「经常不能自动关闭」就是这个）；
//   ② 触屏设备不派发 mouseenter/mouseleave，手机上既打不开也关不掉；
//   ③ hover 模型把「翻页」与「移出即关」耦合成矛盾诉求（要翻页就得移进浮窗）。
const DETAIL_PAGE_SIZE = 8;
let detailCache = {};
// detailState 当前浮窗的展示状态：{uid, page}。
let detailState = null;
// detailOpenSeq 打开序号：快速连点两个账号时，只让「最后一次点击」的响应上屏。
// 取代了 hover 版靠 detailTimer 的竞态守卫（点击式没有「等待期间鼠标移开」这回事）。
let detailOpenSeq = 0;

// closeCreditDetail 关闭浮窗（唯一的关闭入口，X / 点外部 / Esc / 切 tab 都走它）。
function closeCreditDetail() {
  const tip = $("creditTip");
  if (tip) tip.style.display = "none";
  detailOpenSeq++; // 让在途的明细请求失效，避免关掉后又被异步回调弹出来
}

// bindCreditDetailDismiss 绑定浮窗的外部关闭路径（绑定一次）。
// 为什么用 document 级委托而不是给每个入口挂 handler：浮窗与卡片都是动态重建的，
// 委托不受重建影响——这正是旧 hover 实现的失效根源（元素换掉后监听器一起消失）。
function bindCreditDetailDismiss() {
  document.addEventListener("click", (e) => {
    const tip = $("creditTip");
    if (!tip || tip.style.display === "none") return;
    // 归属判定必须用事件传播路径（composedPath），不能用 e.target.closest：
    // showCreditDetail → syncCreditsFromDetail 会在本次派发途中重写积分格 innerHTML，
    // 把被点的 SPAN 从 DOM 摘掉；detached 节点的 closest() 恒为 null，
    // 会让「点另一个账号的标签」被误判成「点外部」而把刚打开的浮窗关掉。
    // composedPath 在派发时就已固化，不受中途 DOM 替换影响。
    const path = e.composedPath ? e.composedPath() : [];
    const inPath = (pick) => path.some((n) => n instanceof Element && pick(n));
    if (inPath((n) => n.getAttribute("data-close-credit-tip") === "1")) { closeCreditDetail(); return; }
    if (path.includes(tip) || tip.contains(e.target)) return;          // 浮窗内部（含翻页）不关
    if (inPath((n) => n.classList.contains("acct-credits"))) return;   // 点另一个账号的标签 = 切换，不关
    closeCreditDetail();                                               // 其余任何位置都关
  });
  // Esc 关闭：与其它弹层（设置/帮助/查找模型）的键盘行为一致
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeCreditDetail(); });
}

async function showCreditDetail(e, uid) {
  // e（内联 onclick 传入的 event）刻意只作占位：浮窗位置由 CSS 固定（水平居中、距顶 12vh），
  // 不需要锚定元素。**不要**在这里取 `e.currentTarget` —— 它在下面第一个 `await` 之后恒为
  // null（事件派发早已结束），留着只会误导后来者（2026-10-17 清理掉一处这样的死代码）。
  const seq = ++detailOpenSeq; // 本次打开的身份；被更晚的打开或关闭动作作废时不再上屏

  // 切换到另一个账号时重置页码；同一账号重复点击保留原页码。
  const page = (detailState && detailState.uid === uid) ? detailState.page : 0;

  let d = detailCache[uid];
  if (!d) {
    try {
      d = await api("/api/account/resource_detail", { uid });
      // 竞态守卫：等待接口期间用户又点了别的账号 / 关闭了浮窗 → 本次结果作废。
      if (seq !== detailOpenSeq) return;
      detailCache[uid] = d;
      syncCreditsFromDetail(uid, d);
    } catch (err) {
      // 不再静默 return：接口失败以前什么都不显示，用户完全不知道发生了什么事。
      // 后端对 oczen / kilo / 自定义渠道直接 400（「无积分，不适用」），其余是网络/
      // 上游错误——两类都必须把原因摆到用户面前（浮窗 + toast 双通道：
      // 浮窗贴在被点的位置、常驻；toast 兜住浮窗可能被下一次点击关掉的情形）。
      if (seq !== detailOpenSeq) return;
      const msg = (err && err.message) ? err.message : "积分明细获取失败";
      renderCreditTipNotice(msg);
      toast(`积分明细获取失败：${msg}`);
      return;
    }
  }
  // 同理：请求成功但一件明细都没有（渠道没下发条目）也必须给出可见解释，不再静默返回。
  if (!d || !d.items || !d.items.length) {
    renderCreditTipNotice("该账号暂无可展示的积分明细（上游未下发条目）。");
    return;
  }

  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }

  // 位置由 CSS 固定（水平居中、距顶 12vh）：不再跟着被点卡片算 left/top，
  // 这样后台重渲染、滚动都不会让浮窗跑偏，也避免窄屏越界。
  detailState = { uid, page };
  renderCreditDetail();
  tip.style.display = "block";
}

// syncCreditsFromDetail 把卡片与汇总条的积分数字回写成明细响应的同源代码。
// 顶层原本是 pool 快照（仅刷新事件/30min 自动循环更新），明细是 hover 实时值，
// 区间内消耗会让「同屏两个数字对不上」；后端 ResourceDetail 现已把同一响应回写
// pool 快照，前端这里再把卡片与汇总条改为同源——弹层展开那一刻全屏数字一致。
// 只改展示状态与对应 DOM，不重请求、不全量重绘（全量重绘会替换正在 hover 的
// 卡片节点，tooltip 锚点与鼠标事件会丢）。
function syncCreditsFromDetail(uid, d) {
  const a = (state.accounts || []).find((x) => x.uid === uid);
  if (!a || a.credits_na) return;
  a.credits = d.usable_remain || 0;
  a.unusable_credits = d.unusable_remain || 0;
  a.expiring_credits = d.expiring_remain || 0;
  a.credits_stale = false;
  const cell = document.querySelector(`.acct-credits[data-uid="${uid}"]`);
  if (cell) cell.innerHTML = creditsText(a);
  renderSummary();
}

// renderCreditTipNotice 在浮窗里显示一条可见提示（替代「静默什么都不发生」）。
// 为什么必须用浮窗而不用 toast：toast 3 秒后消失且位置在页面底部，用户点的是卡片上的
// 某个数字，反馈必须出现在「他刚点的那个地方」，否则容易被当成没反应。
// 提示条沿用浮窗壳子（关闭按钮/点外部/Esc 全部照常），只是正文换成一行说明。
function renderCreditTipNotice(msg) {
  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }
  tip.innerHTML = `<div class="detail-head">`
    + `<span class="detail-count"></span>`
    + `<span class="detail-pager"></span>`
    + `<span class="detail-title">积分明细</span>`
    + `<button type="button" class="detail-close" data-close-credit-tip="1" title="关闭" aria-label="关闭积分明细">✕</button>`
    + `</div><div class="detail-empty">${esc(msg)}</div>`;
  tip.style.display = "block";
}

// renderCreditDetail 按 detailState 重绘 tooltip（含分页控件与可用/不可用小计）。
function renderCreditDetail() {
  const tip = $("creditTip");
  if (!tip || !detailState) return;
  const d = detailCache[detailState.uid];
  // 空明细不能静默 return：此前这里直接返回，用户点了积分数字后浮窗不出现也没有任何解释
  // （典型：渠道有积分但没有下发条目、或该账号额度桶为空）。改为在浮窗里说明。
  if (!d || !d.items || !d.items.length) {
    renderCreditTipNotice("该账号暂无可展示的积分明细（上游未下发条目）。");
    return;
  }

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
  // 标题随计量单位切换：token 渠道不叫「积分明细」（避免把 token 数当积分读）。
  // ⚠️ 账号必须由 detailState.uid 取（本函数的单一数据源）：`uid` 是 showCreditDetail 的
  // **形参**，不在本函数作用域内。此处曾写成 `x.uid === uid` ⇒ ReferenceError: uid is not
  // defined，抛在下面的 `tip.innerHTML = html` 之前 ⇒ 浮窗 display:block 但内容为空，
  // 用户看到的就是「点积分数字什么都不出现」（发布级回归，由 scripts/ui-static-check.mjs 锁死）。
  const detailAcct = (state.accounts || []).find((x) => x.uid === detailState.uid);
  const detailIsToken = !!(detailAcct && detailAcct.credits_unit === "token");
  html += `<span class="detail-title">${detailIsToken ? "Token 额度明细" : "积分明细"}</span>`;
  // 关闭按钮：桌面与触屏共用同一入口（触屏没有 hover，只能靠显式按钮）
  html += `<button type="button" class="detail-close" data-close-credit-tip="1" title="关闭" aria-label="关闭积分明细">✕</button>`;
  html += `</div>`;
  html += `<table class="detail-table"><thead><tr><th>套餐</th><th>总额</th><th>已用</th><th>剩余</th>`;
  if (hasExpiry) html += `<th>有效期</th>`;
  html += `</tr></thead><tbody>`;
  for (const it of slice) {
    // 不可用额度整行淡显 + 角标，与可用额度区分开（如 TraeWork 的官方客户端专用池）。
    let cls = it.usable ? "" : ' class="detail-unusable"';
    let tag = "";
    if (!it.usable) {
      tag = '<span class="detail-tag" title="该额度仅供官方客户端使用，本工具无法消耗">不可用</span>';
    } else if (it.unit === "token") {
      // Token 计量（ZCode 的额度桶、MonkeyCode 的每日 Token 额度）：明示单位，不计入积分合计。
      tag = '<span class="detail-tag" title="以 token 计量，不计入积分合计">tokens</span>';
    } else if (it.info_only) {
      // 其它「单位与积分不同」的展示型条目：展示但不计入合计。
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
    // Token 条目用紧凑单位显示（上亿数字直接铺开会撑破表格）。
    const cellNum = (v) => (it.unit === "token" ? fmtTokens(v) : v);
    html += `<tr${cls}><td>${esc(it.name)}${tag}</td><td>${cellNum(it.total)}</td><td>${cellNum(it.used)}</td><td>${cellNum(it.remain)}</td>`;
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

  // 翻页：只改状态重绘，不重新请求接口。关闭路径由 bindCreditDetailDismiss 统一接管。
  tip.querySelectorAll(".detail-pg").forEach((btn) => {
    if (btn.classList.contains("off")) return;
    btn.onclick = (ev) => {
      ev.stopPropagation();
      const target = Number(btn.dataset.pg);
      if (Number.isFinite(target)) { detailState.page = target; renderCreditDetail(); }
    };
  });
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
  const keyEl = $("apiKeyDisplay");
  const keyVal = keyEl.querySelector(".val");
  if (key === "") {
    keyVal.textContent = "（无鉴权）";
    delete keyEl.dataset.key;
    keyEl.title = "当前未设置 API-Key";
  } else {
    // 脱敏显示：中间省略，完整值存 data-key 供一键复制（防旁人瞟屏）
    keyEl.dataset.key = key;
    keyVal.textContent = shortKey(key);
    keyEl.title = "完整 API-Key：" + key + "；点击复制；进入设置修改";
  }
}

// 渠道显示名与 CSS 短类名（后端 group / 费率 channel 均为 provider.Kind）。
const CH_LABEL = { workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork", traecode: "TraeCode", qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", monkeycode: "MonkeyCode", raccoon: "小浣熊", loomy: "Loomy", oczen: "OpenCodeZen", kilo: "Kilo", minimax: "MiniMax", lobsterai: "LobsterAI", zcode: "ZCode", gemini: "Gemini", codearts: "CodeArts", cline: "Cline", officeace: "OfficeAce" };
const CH_CLASS = { workbuddy: "wb", workbuddyai: "wbai", traework: "trae", traecode: "traecode", qoder: "qoder", qodercn: "qodercn", qodercom: "qodercom", qwenwork: "qwenwork", glm: "glm", monkeycode: "monkeycode", raccoon: "raccoon", loomy: "loomy", oczen: "oczen", kilo: "kilo", minimax: "minimax", lobsterai: "lobsterai", zcode: "zcode", gemini: "gemini", codearts: "codearts", cline: "cline", officeace: "officeace" };
const chLabel = (k) => CH_LABEL[k] || k;
const chClass = (k) => CH_CLASS[k] || "custom";
// ---------- 图表配色：与徽章同源，但保证同图任意两条线颜色可辨 ----------
// 自定义渠道无品牌色，多个自定义渠道若共用 --ch-custom 会完全撞色；
// 按渠道名稳定哈希从高区分度色板取色（刷新/重启不变，图表间一致）。
const CUSTOM_PALETTE = ["#e07a1f", "#0ea5e9", "#d946ef", "#84cc16", "#f43f5e", "#14b8a6", "#a855f7", "#eab308", "#64748b", "#ec4899"];
function stableHash(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}
const _chColorCache = {};
function chColor(k) {
  if (isCustomGroup(k)) return CUSTOM_PALETTE[stableHash(k) % CUSTOM_PALETTE.length];
  const cls = CH_CLASS[k] || "custom";
  if (_chColorCache[cls]) return _chColorCache[cls];
  let v = "";
  try { v = getComputedStyle(document.documentElement).getPropertyValue(`--ch-${cls}`).trim(); } catch (e) { /* ignore */ }
  return (_chColorCache[cls] = v || "#6366f1");
}
// varyColor 同渠道第 i（0 起）个系列做颜色变体，避免同渠道多条线同色：
// 明度阶梯 +14/-14/+28/-28... 封顶在 [22,78]；封顶后转色相偏移（每档 ±18°）继续拉开。
function hexToHsl(hex) {
  const n = parseInt(hex.slice(1), 16);
  const r = ((n >> 16) & 255) / 255, g = ((n >> 8) & 255) / 255, b = (n & 255) / 255;
  const mx = Math.max(r, g, b), mn = Math.min(r, g, b), l = (mx + mn) / 2;
  if (mx === mn) return { h: 0, s: 0, l: l * 100 };
  const d = mx - mn, s = l > 0.5 ? d / (2 - mx - mn) : d / (mx + mn);
  let h = mx === r ? (g - b) / d + (g < b ? 6 : 0) : mx === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return { h: h * 60, s: s * 100, l: l * 100 };
}
function hslToHex(h, s, l) {
  const f = (n) => {
    const k2 = (n + h / 30) % 12;
    const a = (s / 100) * Math.min(l / 100, 1 - l / 100);
    const c = l / 100 - a * Math.max(-1, Math.min(k2 - 3, Math.min(9 - k2, 1)));
    return Math.round(c * 255).toString(16).padStart(2, "0");
  };
  return `#${f(0)}${f(8)}${f(4)}`;
}
function varyColor(hex, i) {
  if (!i || !/^#[0-9a-fA-F]{6}$/.test(hex)) return hex;
  const { h, s, l } = hexToHsl(hex);
  const k = Math.ceil(i / 2), dir = i % 2 ? 1 : -1;
  const lRaw = l + k * 14 * dir;
  const l2 = Math.min(78, Math.max(22, lRaw));
  const hShift = (lRaw < 22 || lRaw > 78) && k >= 2 ? k * 18 * dir : 0;
  return hslToHex(((h + hShift) % 360 + 360) % 360, s, l2);
}
// 不支持显式签到（手动按钮）的渠道：
// WorkBuddy 国际版不提供手动签到，而是自动对话保活领日活奖励；
// 千问办公无签到活动；OpenCodeZen 与 Kilo 是免密渠道，
// 无账号概念（也无积分）。
// 小浣熊已实现签到（setting_info 触发每日积分落账，见 internal/raccoon/checkin.go），支持手动按钮。
// MonkeyCode 已实现签到（POST {console}/api/v1/users/wallet/checkin，见 internal/monkeycode/checkin.go），
// 故**不在**本名单内：它要有手动按钮，也要参与批量签到。
// Loomy 同样已实现签到（宠物每日打工奖励，见 internal/loomy/checkin.go），故也在名单外。
// 注意：Loomy 的奖励判据是「当日有效对话轮次」（上游算），未在官方客户端用过该账号时
// 领取结果会是「未达档位」——那是如实状态，不是按钮坏了。
const NO_EXPLICIT_CHECKIN = new Set(["workbuddyai", "qwenwork", "oczen", "kilo", "zcode", "gemini", "cline"]);
const noExplicitCheckin = (g) => NO_EXPLICIT_CHECKIN.has(g) || isCustomGroup(g); // 自定义渠道无签到活动（R66）
// isCustomGroup 该分组是否为用户自定义 OpenAI 兼容渠道（R66）。
// 以 state.custom_channels（后端 /api/state 下发，保存即热更新）为准，勿在前端另维护一份。
const isCustomGroup = (g) => !!(state && (state.custom_channels || []).some((c) => c && c.name === g));
// hasNoCredits 该分组是否无积分概念：内建 NA 名单 ∪ 动态自定义渠道。与后端 creditsNAKind 同口径。
const hasNoCredits = (g) => NO_CREDITS_BUILTIN.has(g) || isCustomGroup(g);
// 导入型渠道：凭据由本机已登录的官方客户端提供，没有浏览器登录流程（见 internal/app/import_local.go）。
const IMPORT_LOCAL_CHANNELS = new Set(["monkeycode", "raccoon", "loomy", "officeace"]);
const isImportLocal = (ch) => IMPORT_LOCAL_CHANNELS.has(ch);
// 支持「网页版自动登录」的渠道：后端拉起独立 profile 的浏览器，用户在网页里登录，
// 后端经 CDP 捕获凭据（小浣熊：Cookie 里的 xhx_token）。
// 小浣熊两个集合都命中 —— 弹窗里同时给「网页版登录」与「从客户端导入」两个动作。
// Loomy 也两个集合都命中，但机制不同：**不**拉浏览器、**不**用 CDP 捕获，而是起一个
// 127.0.0.1 的本机登录引导页（手机号 + 短信验证码），由前端 window.open 打开
// （见 internal/login_loomy）。故文案需按渠道区分，不能套用小浣熊那段。
const WEB_LOGIN_CHANNELS = new Set(["raccoon", "loomy"]);
const hasWebLogin = (ch) => WEB_LOGIN_CHANNELS.has(ch);
// 粘贴凭据型渠道：没有浏览器登录编排，凭据由用户从官方站点自行创建后粘贴进来。
// Cline：app.cline.bot → 注册账号 → Account / API Keys 建 sk_ Key（见 internal/cline）。
const PASTE_KEY_CHANNELS = new Set(["cline"]);
const isPasteKey = (ch) => PASTE_KEY_CHANNELS.has(ch);
// 无手动签到渠道的状态文案：国际版是「自动领日活奖励」，千问办公为「无签到」。
const NO_CHECKIN_TAG = { workbuddyai: "自动领日活奖励", oczen: "不支持", kilo: "不支持", zcode: "不支持", gemini: "不支持", cline: "无签到" };
const noCheckinText = (g) => NO_CHECKIN_TAG[g] || "无签到";
// 无积分概念的渠道（匿名通道）：积分区域显示「不适用」，并隐藏刷新积分/明细入口。
// 内建名单只有 oczen/kilo/cline；自定义渠道（R66）同样无积分，但渠道名是用户配置的、
// 运行期可增删，故运行时以后端下发的 custom_channels 动态判定（isCustomGroup）。
const NO_CREDITS_BUILTIN = new Set(["oczen", "kilo", "cline"]);
// 支持跳转登录编排的渠道：登录态失效时卡片显示 ⟳ 重新登录入口（#134）。
// officeace 一并纳入（2026-10-17）：它除了「登录态失效」，还有「缺 refresh_token」
// 这种**不能自愈**的形态（客户端导入只拿得到不过期的网关 Basic），此时同样只能重新登录
// 才能恢复积分/签到 —— 徽标与 ⟳ 入口由同一处逻辑驱动（见 renderAccounts 的 needRelogin）。
const RELOGIN_CHANNELS = new Set(["workbuddy", "workbuddyai", "traework", "qoder", "qodercn", "qodercom", "qwenwork", "raccoon", "officeace"]);

// creditsText 账号卡片的积分文案。
// 拆成「可用 / 不可用 / 临期」三个数字：渠道（如 TraeWork）会下发官方客户端专用的
// 额度池，对本工具是看得见用不了的，混进一个数字会让人误判可用余额；
// 临期是可消耗余额中 24h 内（到期日≤明天）到期的部分，提示优先消耗。
// 不可用仅在 >0 时显示；临期同理，凭空多个灰/红 0 很吵。
// 旧版本 state 文件（v2.2.0 及之前，无 unusable 字段）读入后 credits_stale=true，
// 此时不把旧值当真值，改显示「待刷新」；自动刷新首刷成功后即变回真实拆分。
// fmtTokens Token 数格式化：这类数字动辄上亿，直接铺开会撑破卡片与汇总条，
// 故用中文紧凑单位（万/亿）显示；完整语义靠 title 说明。
function fmtTokens(n) {
  n = Number(n) || 0;
  const trim = (s) => s.replace(/\.?0+$/, "");
  if (Math.abs(n) >= 1e8) return trim((n / 1e8).toFixed(2)) + "亿";
  if (Math.abs(n) >= 1e4) return trim((n / 1e4).toFixed(2)) + "万";
  return n.toLocaleString("zh-CN");
}

function creditsText(a) {
  if (a.credits_na) {
    return `<span class="credit-na" title="该渠道无积分概念">不适用</span>`;
  }
  if (a.credits_stale) {
    return `<span class="credit-stale" title="余额口径已过期（旧版本状态文件），正在自动刷新…">待刷新</span>`;
  }
  // Token 计量渠道（credits_unit="token"，如 ZCode）：同一位置改显示 **Token 计数**。
  // 与积分分开统计——它**不进**积分汇总合计（见 renderSummary），
  // 但保留临期角标（token 桶同样有到期时间，来自内部 ResourceItem.ExpireAt）。
  if (a.credits_unit === "token") {
    let html = `<span class="credit-num">${fmtTokens(a.credits)}</span>`
      + `<span title="本渠道额度以 token 计量，与积分分开统计">tokens</span>`;
    if ((a.expiring_credits || 0) > 0) {
      html += `<span class="credit-expiring"> (临期${fmtTokens(a.expiring_credits)})</span>`;
    }
    return html;
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
const CH_SUMMARY_ORDER = ["workbuddy", "workbuddyai", "traework", "qodercn", "qodercom", "qwenwork", "glm", "minimax", "lobsterai", "zcode", "gemini", "codearts", "cline", "officeace", "oczen", "kilo", "qoder"];

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
    if (!c) { c = byCh[g] = { n: 0, credits: 0, expiring: 0, unusable: 0, stale: 0, na: false, isToken: false, tokens: 0 }; }
    c.n++;
    if (a.credits_na) { c.na = true; continue; }
    // Token 计量渠道单列：只累加 token 数，**不并入**积分合计（与积分分开统计）。
    if (a.credits_unit === "token") { c.isToken = true; c.tokens += a.credits || 0; c.expiring += a.expiring_credits || 0; continue; }
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
    // Token 计量渠道不进积分合计（这就是「和积分分开统计」的落点）。
    if (!c.na && !c.isToken && !allStale) { total += c.credits; totalExp += c.expiring; }
    if (!c.na && !c.isToken && allStale) staleChs++;
    let body;
    if (c.na) {
      body = `<span class="credit-na">不适用</span>`;
    } else if (c.isToken) {
      // Token 计数：以 tokens 为单位单列，明示它不是积分。
      body = `<span class="cs-num">${fmtTokens(c.tokens)}</span>`
        + `<span class="cs-tok" title="本渠道额度以 token 计量，不计入「合计」">tokens</span>`
        + (c.expiring > 0 ? `<span class="cs-exp" title="24 小时内到期">临期${fmtTokens(c.expiring)}</span>` : "");
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
  html += `<span class="cs-chip cs-total" title="各渠道可用积分合计（不含「不可用」、匿名通道与 Token 计量渠道）">`
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
    // 为什么需要：账号被冷却时挑号会跳过它，表现为「积分明明很多却报 503/上游限流」,
    // 而卡片上毫无提示——用户只能靠猜。这里把「冷却到几点 + 原因」直接摆出来。
    // until 由后端 fmtTime 预格式化为 "01-02 15:04"（24h 制），为空则退化为「冷却中」。
    // 24h 制歧义修复（如 10-18 07:07 是次日清晨，却极易被读成「19 点显示成 7 点」）：
    // 后端同时透出 until_ts，这里实时算出「剩 X 时 X 分」并列展示，绝对时刻 +
    // 相对时长双保险，跨零点不再误读；无 until_ts（旧后端）时退回纯文案，不破兼容。
    const coolingTag = a.cooling
      ? `<span class="tag warn" title="${esc(a.reason || "冷却中")}">冷却至 ${esc(a.until || "-")}${a.until_ts ? `<span data-until-ts="${a.until_ts}">（${fmtLeft(a.until_ts)}）</span>` : ""}</span>`
      : "";
    const coolingClass = a.cooling ? " cooling" : "";
    const checkinBtn = noCheckin
      ? `<span class="icon-op off" title="${esc(noCheckinTitle)}" onclick="return false">✓</span>`
      : `<span class="icon-op" title="签到" onclick="checkin('${a.uid}')">✓</span>`;

    // 无积分账号分两类，处理不同：
    // - oczen/cline（固定账号）：只保留「不可操作」的静态指示，
    //   不给刷新积分/停用/删除入口——后端也会硬拒，避免用户白点一次。
    // - 自定义渠道（R66）：无积分/无签到，但账号可在设置里停用/删除，
    //   故保留停用/删除/改名入口，只去掉签到/刷新积分两个无意义按钮。
    const noCredits = hasNoCredits(a.group);
    const fixedAcct = noCredits && !isCustomGroup(a.group);
    // 问号图标：hover 展示 oczen 通道说明（免费反代范围/私有 Key/代理三项）。
    // 放在锁头左侧；用独立的 help 样式（正常亮度 + help 光标），不可点击但 tooltip 可用。
    // title 内换行用 &#10;（HTML 属性实体），字面 \n 会被部分浏览器吞掉导致无 tooltip。
    const oczenHelp = `<span class="icon-op help" title="关于 OpenCodeZen 通道：&#10;1. 本工具仅反代其免费模型（绕过官方客户端限制）；付费账号可直接使用官方端点，无需经此通道&#10;2. 在 OpenCodeZen 获取的 API-Key 可在设置中配置，避免匿名账号共享限额超限&#10;3. 地域受限的免费模型（如 muse-spark 系）会经「备用上游通道」自动重发（设置里配置代理地址；未配置则原文透传 403）">?</span>`;
    // kilo 的说明与 oczen 不同：没有私有 Key 可配，也没有备用通道重放。
    const kiloHelp = `<span class="icon-op help" title="关于 Kilo 免费池：&#10;1. 上游是 api.kilo.ai 网关的免费模型池，**不需要任何凭据**（无账号、无 Key）&#10;2. 只反代免费池（模型清单里 isFree=true 的那些）；请求付费模型会返回 PAID_MODEL_AUTH_REQUIRED&#10;3. 无签到、无积分，故没有「刷新积分」入口">?</span>`;
    const channelHelp = a.group === "kilo" ? kiloHelp : oczenHelp;
    // 登录态失效（401/登录已失效）：徽标 + 一键重新登录（#134）。
    const sessionDeadTag = a.session_dead
      ? `<span class="tag bad" title="${esc(a.session_dead_reason || "登录态已失效，请重新登录")}">需重新登录</span>`
      : "";
    // 凭据缺 refresh_token（目前只有 OfficeAce，见后端 refreshTokenMissing）：
    // 性质与「登录态失效」不同 —— 不是被上游拒了，而是本地根本没这条凭据，
    // **不会自愈**，所以徽标要**持久**显示（原先只在导入那一刻弹一次提示，事后无从复查）。
    // 用 warn 色与 bad 区分：前者「还能对话，但积分/签到不可用」，后者「登录已失效」。
    const missingRT = Boolean(a.refresh_token_missing);
    const missingRTTag = missingRT
      ? `<span class="tag warn" title="${esc(a.refresh_token_missing_reason || "凭据缺 refresh_token：积分/签到不可用，需重新登录或重新导入")}">缺续期凭据</span>`
      : "";
    const needRelogin = Boolean(a.session_dead) || missingRT;
    const reloginTitle = missingRT
      ? "重新登录（本账号缺 refresh_token，积分/签到不可用；登录成功后自动补签）"
      : "重新登录（跳转登录页换新凭据，成功后自动补签）";
    // 一键重新登录：沿用既有内联 onclick 形态（本仓无 CSP 头，全文件已有 10 处同款写法）。
    // 渠道差异由 `startLogin` 内部**分派**：OfficeAce 是「华为云授权码 + 云端轮询捕获」的
    // 两步流程，必须走 startOfficeAceWebLogin（→ POST /api/login/officeace_auto），不能走
    // 一次性的 POST /api/login/start。2026-10-18 修正：先前 OfficeAce 的 ⟳ 直接打
    // /api/login/start，而后端 StartLoginFor 未登记 officeace ⇒ 点了只弹
    // `unknown login provider officeace`（死按钮，独立复核抓出）。
    const reloginBtn = needRelogin && RELOGIN_CHANNELS.has(a.group)
      ? `<span class="icon-op" title="${reloginTitle}" onclick="startLogin('${a.group}')">⟳</span>`
      : "";
    const ops = fixedAcct
      ? `${channelHelp}<span class="icon-op off" title="固定账号，不可停用/删除" onclick="return false">🔒</span>`
      : `${checkinBtn}
          ${reloginBtn}
          ${noCredits ? "" : `<span class="icon-op" title="刷新积分" onclick="refreshOne('${a.uid}')">↻</span>`}
          <span class="icon-op warn" title="${disableTitle}" onclick="toggleDisable('${a.uid}',${a.disabled})">${disableIcon}</span>
          <span class="icon-op danger" title="${isCustomGroup(a.group) ? esc("删除账号（自定义渠道将移除整个渠道配置）") : "删除账号"}" onclick="removeAcct('${a.uid}')">✕</span>`;
    // 显示名：点击直接弹出改名框（匿名渠道不可改，降级为普通文本）。
    // oczen 特例：渠道名已由 badge 承担，名字按凭证形态显示「匿名/私有Key」。
    const isOczen = a.group === "oczen";
    const oczenName = (state.oczen_api_key || "") ? "私有Key" : "匿名";
    const nameHtml = fixedAcct
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
        <div class="acct-credits${noCredits ? "" : " clickable"}" data-uid="${a.uid}"${noCredits ? "" : ` onclick="showCreditDetail(event,'${a.uid}')" title="点击查看积分明细（含分页）" role="button" tabindex="0" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();showCreditDetail(event,'${a.uid}')}"`}>${creditsText(a)}</div>
        <div class="acct-checkin">${coolingTag}${sessionDeadTag}${missingRTTag}${checkinTag}</div>
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

// channelCredits 汇总某渠道全部账号的可用额度（与积分汇总条同源 state.accounts）。
//
// 返回 { bal, unit }：unit 为 ""（积分）或 "token"（Token 计数，如 ZCode）。
// ⚠️ 调用方**必须**按 unit 选单位与格式化（token 用 fmtTokens、积分用 fmtNum）——
// 否则会把上亿 token 显示成「N 分」（2026-10-17 用户截图报告：费率页 ZCode 角标
// 显示「余额 8,000,000 分」，正是漏了这一步）。
// 无该渠道账号时返回 null（不渲染角标，避免显示误导性的 0）。
function channelCredits(kind) {
  if (!state || !state.accounts) return null;
  let sum = 0, has = false, unit = "";
  for (const a of state.accounts) {
    if ((a.group || "workbuddy") !== kind) continue;
    if (a.credits_na) return null; // 匿名渠道无额度概念
    has = true;
    if (a.credits_unit === "token") unit = "token";
    sum += a.credits || 0;
  }
  return has ? { bal: sum, unit } : null;
}

// tabBalText 渠道 tab 余额角标文案（按计量单位切换：token 与积分分开显示）。
function tabBalText(v) {
  if (!v) return "";
  return v.unit === "token"
    ? `余额 ${fmtTokens(v.bal)} tokens`
    : `余额 ${fmtNum(v.bal)} 分`;
}

// tabBalTitle 角标 tooltip：明示单位与统计口径（token 不计入积分合计）。
function tabBalTitle(v) {
  return v && v.unit === "token"
    ? "该渠道全部账号当前可用 Token 数（与积分分开统计，不计入积分合计）"
    : "该渠道全部账号当前可用积分合计";
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
    const txt = tabBalText(bal);
    const tip = tabBalTitle(bal);
    if (el) {
      if (el.textContent !== txt) el.textContent = txt;
      if (el.title !== tip) el.title = tip;
    }
    else { const s = document.createElement("span"); s.className = "fees-tab-bal"; s.textContent = txt; s.title = tip; tab.appendChild(s); }
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

  // 思考档位标记（issue #96）：模型 ID 后的档位小字标，如 [low·high·max]。
  // 只在上游目录声明时展示（reasoning_efforts 非空），不自行编造。
  const effortTag = (m) => {
    if (!m || !m.reasoning_efforts || !m.reasoning_efforts.length) return "";
    const list = m.reasoning_efforts.join("·") + (m.default_reasoning_effort ? `（默认 ${m.default_reasoning_effort}）` : "");
    return ` <span class="effort-tag" title="思考档位（上游目录声明）：${esc(list)}；客户端传 reasoning_effort 即可调档">[${esc(m.reasoning_efforts.join("·"))}]</span>`;
  };

  // 上下文标记：模型 ID 后的 (1M)/(180K) 小字标。
  // has_context = 上游接口真实返回（context_from_api）或本工具手工实测（hand_measured）。
  // 实测值用带 * 的写法「(1M*)」+ tooltip 标注来源——不把实测冒充成上游官方值，
  // 同时也不埋没它（cline 这类渠道上游根本不返回上下文，只能靠实测）。
  const ctxTag = (m) => {
    if (!m || !m.has_context || !m.context_window) return "";
    const hand = m.hand_measured ? "*" : "";
    const src = m.hand_measured ? "本工具实测值（仅供参考，非上游官方声明）" : "上游接口返回";
    return ` <span class="ctx-tag${m.hand_measured ? " hand" : ""}" title="上下文窗口：${fmtTokensCompact(m.context_window)} tokens（${src}）">(${fmtTokensCompact(m.context_window)}${hand})</span>`;
  };

  // 能力文字摘要，拼进模型 tooltip。
  // 措辞说明：上游模型列表接口未声明某能力时，本工具不自行断言其「不支持」，
  // 只说「未声明」；hand_measured（本工具实测）时改说「实测不支持」，如实标出来源。
  const capText = (m) => {
    if (!m) return null;
    const yes = [], unknown = [];
    (m.supports_images ? yes : unknown).push("图像输入");
    (m.supports_reasoning ? yes : unknown).push("思考模式");
    const parts = [];
    if (yes.length) parts.push(`支持：${yes.join("、")}`);
    if (unknown.length) parts.push(`${m.hand_measured ? "实测不支持" : "上游未声明"}：${unknown.join("、")}`);
    if (m.reasoning_efforts && m.reasoning_efforts.length) {
      parts.push(`思考档位：${m.reasoning_efforts.join("/")}${m.default_reasoning_effort ? `（默认 ${m.default_reasoning_effort}）` : ""}`);
    }
    if (m.hand_measured) parts.push("（能力与上下文为本工具实测值，仅供参考）");
    return parts.join("；");
  };

  // 模型 id 的 tooltip：能拿到上下文则展示，否则明确说未知
  const modelTip = (m) => {
    const parts = [`模型：${m.model}`];
    if (m.has_context && m.context_window) {
      const src = m.hand_measured ? "（本工具实测，非上游官方值）" : "";
      parts.push(`上下文窗口：${fmtTokensCompact(m.context_window)}${src}`);
      if (m.max_tokens) parts.push(`最大输出：${fmtTokensCompact(m.max_tokens)}`);
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
    // 完整 ID 取服务端给的 m.id（同 /v1/models 口径）：普通渠道是 `<渠道>/<模型>`；
    // 上游 ID 自带命名空间的渠道（cline-free/*）直接透传上游 ID（R59）。
    // 缺字段时退回旧拼接（兼容旧后端）：仅当裸名已以渠道前缀开头时原样保留（防重复）。
    const full = m.id || (chPrefix && !raw.startsWith(`${chPrefix}/`) ? `${chPrefix}/${raw}` : raw);
    const copyTip = `点击复制：${full}`;
    const line = `<div class="mline"><code class="fee-model-copy" data-mid="${esc(full)}" role="button" tabindex="0" title="${esc(modelTip(m))}\n${copyTip}">${esc(short)}</code>${ctxTag(m)}${effortTag(m)}${capIcons(m)}${noteCell(m)}</div>`;
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
    // 渠道余额角标：取自 state.accounts（与积分汇总条同源），让用户在选择渠道时即可看到
    // 「这个渠道还有多少可用」；单位随渠道计量单位切换（token / 积分，见 tabBalText）。
    const bal = channelCredits(ch.channel);
    const balHtml = bal === null ? ""
      : `<span class="fees-tab-bal" title="${esc(tabBalTitle(bal))}">${esc(tabBalText(bal))}</span>`;
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

// fmtTokensCompact 把 token 数格式化为 1M / 192k 紧凑形式。
//
// ⚠️ 用途边界（2026-10-17 修「同名函数互相覆盖」后确立）：
//   - 本函数（k/M）**只**用于「模型元数据」——上下文窗口、最大输出这类上游以
//     k/M 口径描述的能力值，保持与官方文档一致（131072 -> 131k）。
//   - **额度 / 用量数字**（账号卡片的 Token 计数、明细单元格、平台表）必须用
//     `fmtTokens`（万/亿中文单位）—— 那些数字动辄上亿，`94539k` 用户读不出来。
// 历史：本函数原名 `fmtTokens`，与后面新增的万/亿版**同名**，后者在前、本函数在后，
// 于是本函数静默覆盖了它 ⇒ Token 卡片显示成 `94539k`。改名以消除覆盖。
function fmtTokensCompact(n) {
  if (!n) return "-";
  if (n >= 1000000 && n % 1000000 === 0) return `${n / 1000000}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

// ---------- 账号操作 ----------
// 签到失败信息友好化：登录态失效形态（401 等）追加可操作指引（#134）。
function friendlyCheckinMsg(m) {
  return /401|unauthorized|登录已失效|登录已过期/i.test(m || "")
    ? m + "（该账号登录已过期，点账号卡片右上角 ⟳ 重新登录）" : m;
}
// checkinToast 签到后的提示文案。
// 为什么不能一律写「签到成功」：Qoder 双区的 campaigns 路径有三个**都是 ok=true** 的结果——
// 真领到（msg「签到成功 +100」）/ 今日已领（msg「今日已领取 +100」）/ 上游还没发可领活动
// （no_campaign，msg 是诊断摘要）。旧文案把第三种也说成「签到成功：上游未下发可领取活动…」，
// 读起来像领到了，是 10-17「签到没实现」误判的来源之一。这里按 msg 区分，绝不越权声称。
//
// 第三档再分「失败」与「暂未领到」：后端在 ok=true 下也可能给出失败形态的 msg
// （如「领取失败 http 400: …」「claim parse: …」），那是**真失败**，说成「暂未领到」
// 等于换了说法的假成功。判据放在兜底分支之前，不给「先被 +N 命中」的机会。
const CHECKIN_FAIL_RE = /失败|http \d{3}|error|无法解析|parse|未知状态|401|403|500/i;
// 第五档「无领取资格」（2026-10-18 用户实测定性）：该活动由**上游按账号授予资格**，
// 不是每个号都有；没资格的号在官方客户端里同样看不到入口。后端会直说「该账号无积分
// 领取活动资格……不是本工具的问题，重试无意义」。
// 为什么必须单独成档：这条 msg 既不含「失败」也不含错误码，会落到兜底分支被说成
// 「暂未领到」，再叠加下面那句「窗口内会自动重试」——对一个永远不会有结果的账号，
// 等于让用户一直等一个不会发生的事。这正是「以为工具没实现」的最后一处来源。
const CHECKIN_NO_ELIGIBILITY_RE = /无积分领取活动资格|无领取资格/;
function checkinToast(r) {
  const msg = r.msg || "";
  if (!r.ok) return `签到：${friendlyCheckinMsg(msg)}`;
  if (/^今日已领取/.test(msg)) return `今日已领过：${msg}（剩余 ${r.remain}）`;
  // 注意「领取失败 http 400」里也可能带数字，故失败判据必须先于「含 +N」判据。
  if (CHECKIN_NO_ELIGIBILITY_RE.test(msg)) return `无领取资格：${msg}`;
  if (CHECKIN_FAIL_RE.test(msg)) return `签到失败：${friendlyCheckinMsg(msg)}`;
  if (/^签到成功/.test(msg) || /\+-?\d+/.test(msg)) return `签到成功：${msg}（剩余 ${r.remain}）`;
  return `暂未领到：${msg}（剩余 ${r.remain}）`;
}
async function checkin(uid) {
  try {
    const r = await api("/api/account/checkin", { uid });
    // 无资格优先于「可重试」：该状态 retryable 仍为 true（上游可能在当天补发资格，
    // 窗口内重试是免费的），但对**当前**这个账号说「会自动重试」会让人白等，
    // 故这里明说「无需重试」并点出这不是工具故障。
    if (CHECKIN_NO_ELIGIBILITY_RE.test(r.msg || "")) {
      toast(`无领取资格：${r.msg}`);
    } else if (!r.ok && r.retryable) {
      // 未达成且可重试（活动尚未创建/上游瞬时故障）：说明会在签到窗口内自动重试，
      // 避免用户看到「无可用签到活动」误以为失败。
      toast(`暂未领到：${r.msg}（10:00–12:00 窗口内会自动重试）`);
    } else {
      toast(checkinToast(r));
    }
    delete detailCache[uid]; // 签到改变余额，旧明细缓存会与新顶层打架
    loadState();
  } catch (e) { toast(e.message); }
}

async function refreshOne(uid) {
  try {
    const r = await api("/api/account/refresh", { uid });
    // 单位随账号切换：token 计量渠道（如 ZCode）不叫「积分」（否则上亿 token 被读成积分）。
    const acc = (state.accounts || []).find((x) => x.uid === uid);
    toast(acc && acc.credits_unit === "token"
      ? `刷新成功：剩余 ${fmtTokens(r.remain)} tokens`
      : `刷新成功：剩余积分 ${r.remain}`);
    delete detailCache[uid]; // 顶层已更新，旧明细缓存须作废，否则「新顶层+旧弹层」
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
    detailCache = {}; // 批量签到改变余额，全部明细缓存作废
    const ok = (r.results || []).filter((x) => x.ok).length;
    const pending = (r.results || []).filter((x) => !x.ok && x.retryable).length;
    const skip = (state.accounts || []).filter((a) => noExplicitCheckin(a.group)).length;
    const total = (r.results || []).length + skip;
    const parts = [skip ? `成功 ${ok} / ${total}（${skip} 个无签到活动跳过）` : `成功 ${ok} / 共 ${total}`];
    if (pending) parts.push(`${pending} 个未领到将在窗口内自动重试`);
    toast(`批量签到完成：${parts.map(friendlyCheckinMsg).join("；")}`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnCheckinAll").disabled = false;
  }
}

async function refreshAll() {
  $("btnRefreshAll").disabled = true;
  try {
    const r = await api("/api/account/refresh_all", {});
    detailCache = {}; // 顶层已全部更新，全部明细缓存作废
    // 无积分概念的渠道（oczen/cline/自定义）后端不参与统计，无需前端再补偿数
    toast(r.busy ? "已有刷新任务进行中"
      : r.failed > 0 ? `积分刷新完成：成功 ${r.ok} / 失败 ${r.failed}${failedDetail(r)}`
      : `积分刷新完成：全部成功（${r.ok} 个）`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshAll").disabled = false;
  }
}

// failedDetail 把失败账号拼成 toast 后缀：只报「失败 N」时用户无从知道
// 是哪几个号、为什么失败（典型：两个号的 refresh token 被上游吊销，
// 重试永远无效，必须重新登录）。取前 2 个 + 超出计数，避免 toast 过长。
function failedDetail(r) {
  const names = [];
  for (const [ch, ps] of Object.entries(r.platforms || {})) {
    for (const a of ps.accounts || []) {
      if (a.ok) continue;
      const who = (state.accounts || []).find((x) => x.uid === a.uid);
      const label = `${chLabel(ch)}·${who ? (who.nickname || shortUid(a.uid)) : shortUid(a.uid)}`;
      names.push({ label, msg: a.msg || "未知错误" });
    }
  }
  if (!names.length) return "";
  const head = names.slice(0, 2).map((n) => `${n.label}（${n.msg}）`).join("；");
  return `：${head}${names.length > 2 ? ` 等 ${names.length} 个` : ""}`;
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
  raccoon: "（支持「网页版登录」与「从客户端导入」两种方式；access_token 约 1 小时，本工具会自动续期，上游刷新时会轮换 refresh_token）",
  loomy: "（凭据来自本机已登录的 Loomy 客户端；上游无续期接口，约 14 天后需重新登录并再次导入）",
  glm: "（登录后请按下方指引复制 refresh_token 粘贴回来）",
  cline: "（无需登录：在 app.cline.bot 注册账号，Account / API Keys → Create API Key 后粘贴即可；Key 不过期也不轮换）",
  officeace: "（凭据来自本机已登录的 OfficeAce 客户端；每日签到的临时凭据约 2 小时有效，本工具会自动续期）",
  minimax: "（设备码授权：点「浏览器登录」后在新页面确认授权，本工具会自动轮询直到完成；可重复添加多个账号）",
  lobsterai: "（本机回调授权：点「浏览器登录」后在有道页面完成登录，浏览器会跳回本机完成授权；支持多个账号）",
  zcode: "（设备授权：点「浏览器登录」后在智谱页面完成授权，本工具会轮询直到完成；⚠️ 本渠道的积分领取依赖网页验证码，本工具不自动领取）",
  gemini: "（本机回调授权：点「浏览器登录」后完成 Google 账号授权，浏览器会跳回本机；支持多个账号。⚠️ 额度以「配额窗口剩余百分比」呈现，不是积分数）",
  codearts: "（PKCE 授权：点「浏览器登录」后在华为云授权页完成授权，浏览器会跳回本机；支持多个账号。⚠️ 免费额度按 UTC+8 自然日重置）",
};
function promptLogin(channel) {
  pendingChannel = channel;
  const name = chLabel(channel);
  pendingAction = isImportLocal(channel) ? "import" : "login";
  // 「账号标识」输入行：只对**上游不提供稳定身份字段**的渠道显示（当前是 CodeArts）。
  // 为什么要把区分账号的责任交给用户：上游不回传任何身份信息，账号 ID 只能落到每次新签发的
  // 临时凭据上 ⇒ 既无法区分多个账号、重复登录又会被写成新条目（见 docs/渠道接入备忘.md）。
  const labelRow = $("lcLabelRow");
  if (labelRow) {
    const need = channel === "codearts";
    labelRow.classList.toggle("hidden", !need);
    if (need) {
      const inp = $("lcAccountLabel");
      if (inp) inp.value = "";
    }
  }
  // 次按钮默认隐藏，只在「两条路都通」的渠道（小浣熊）里显示。
  const altBtn = $("btnLoginAlt");
  altBtn.classList.add("hidden");
  altBtn.onclick = null;

  // OfficeAce（华为云果办）：主按钮走自助 OAuth（打开华为云授权页），
  // 次按钮回退到「从本机 OfficeAce 客户端导入」。
  if (channel === "officeace") {
    pendingAction = "login";
    $("lcTitle").textContent = "添加 OfficeAce 账号";
    $("lcMsg").innerHTML = `点击「浏览器登录」将打开<b>华为云授权页</b>，用你的华为账号登录后，`
      + `本工具会自动捕获凭据并添加账号（无需安装 OfficeAce 桌面端）。`
      + `也可以点「从客户端导入」直接读取本机已登录的 OfficeAce 桌面端凭据。`
      + `<div class="glm-warn" style="margin-top:10px">`
      + `<div class="glm-warn-title">提示</div>`
      + `对话走不过期的网关凭据；每日签到的临时凭据约 2 小时有效，本工具会自动续期。`
      + `若同一账号的 refresh_token 被本工具与桌面端共用可能互踢，需在本工具单独登录一次。`
      + `</div>`;
    $("btnLoginConfirm").textContent = "浏览器登录";
    altBtn.textContent = "从客户端导入";
    altBtn.classList.remove("hidden");
    altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }

  // 网页版登录渠道：主按钮走「拉起浏览器 + 自动捕获凭据」，次按钮回退到本机客户端导入。
  if (hasWebLogin(channel)) {
    pendingAction = "login";
    $("lcTitle").textContent = "添加 " + name + " 账号";
    if (channel === "loomy") {
      // Loomy 的机制与小浣熊不同：不拉浏览器、不捕获凭据，而是由本工具起一个本机登录引导页，
      // 用户在页面里填手机号 + 短信验证码（见 internal/login_loomy）。
      // 另外它**没有 refresh 端点**，所以不存在「互踢 / 自动续期」那一段说明。
      $("lcMsg").innerHTML = `点击「登录${name}」会打开一个<b>本机登录页</b>（地址在你的电脑上，不经过本工具的服务器），`
        + `请在其中输入手机号、接收并填写短信验证码，登录完成后本工具会自动保存账号。`
        + `<div class="glm-warn" style="margin-top:10px">`
        + `<div class="glm-warn-title">⚠️ 关于登录有效期</div>`
        + `Loomy 的登录态<b>约 14 天</b>到期，且上游<b>没有刷新接口</b> —— `
        + `到期后需要重新点「登录${name}」再登一次，或改用「从客户端导入」重新读取已登录客户端的凭据。`
        + `</div>`
        + `也可以改用「从客户端导入」：直接读取本机已登录客户端的凭据。`;
      $("btnLoginConfirm").textContent = "登录" + name;
      altBtn.textContent = "从客户端导入";
      altBtn.classList.remove("hidden");
      altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
      $("loginConfirmOverlay").classList.remove("hidden");
      return;
    }
    $("lcMsg").innerHTML = `点击「登录${name}」会打开一个<b>独立的浏览器窗口</b>（不影响你日常浏览器的登录态），`
      + `请在其中正常登录（扫码 / 手机号 / 微信均可），登录完成后本工具会自动捕获凭据并保存账号。`
      + `<div class="glm-warn" style="margin-top:10px">`
      + `<div class="glm-warn-title">⚠️ 关于登录态互踢</div>`
      + `本工具会用捕获到的凭据<b>自动续期</b>（上游刷新时会轮换 refresh_token）。`
      + `若你把这个账号的凭据<b>同时</b>用在本工具和浏览器 / 官方客户端上，两边会争抢同一个 refresh_token，`
      + `可能出现<b>其中一侧被挤下线</b>的情况（上游可能对同一账号限制单会话）。`
      + `想两边同时用，请在本工具里点「登录${name}」<b>单独登录一次</b>（本流程新建独立会话），`
      + `而不是从浏览器复制凭据粘进来。`
      + `</div>`
      + `也可以改用「从客户端导入」：直接读取本机已登录客户端的凭据。`;
    $("btnLoginConfirm").textContent = "登录" + name;
    altBtn.textContent = "从客户端导入";
    altBtn.classList.remove("hidden");
    altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }

  // 粘贴凭据型渠道（Cline）：弹专用输入框，而不是「打开浏览器登录」。
  // Cline 的凭据是官方 API Key，创建动作发生在 app.cline.bot 网页里，本工具无法代劳，
  // 所以这里没有轮询/回调，只有一次「验证 + 落盘」（后端只调 /api-keys，零 token 消耗）。
  if (isPasteKey(channel)) {
    $("clineKeyInput").value = "";
    $("clineErr").textContent = "";
    $("clineWarn").classList.add("hidden");
    $("clineOverlay").classList.remove("hidden");
    setTimeout(() => $("clineKeyInput").focus(), 30);
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
  // CodeArts：授权页必须避开日常浏览器里的华为云登录态（否则永远被授权成已登录的那个账号），
  // 后端会用**无痕窗口**打开；同时需要用户填「账号标识」来区分多个华为账号。
  if (channel === "codearts") {
    $("lcTitle").textContent = "添加 CodeArts 账号";
    $("lcMsg").innerHTML = `点击「登录 CodeArts」会打开一个<b>无痕浏览器窗口</b> —— `
      + `无痕会话不带日常浏览器的华为云登录态（否则你只会被授权成<b>已登录的那个</b>账号）。`
      + `请在其中登录<b>你想要添加的那个</b>华为账号。`
      + `<div class="glm-warn" style="margin-top:10px">`
      + `<div class="glm-warn-title">为什么要填「账号标识」</div>`
      + `华为云不回传账号身份信息，本工具无法自动分辨「同一账号重新登录」与「另一个账号」。`
      + `所以：<b>留空</b> = 覆盖已有的 CodeArts 账号；<b>填写</b>（例如"工作号"）= 作为独立账号并存。`
      + `以后重登同一个账号时填<b>同一个标识</b>，就会覆盖它而不是再新增一个。`
      + `</div>`;
    $("btnLoginConfirm").textContent = "登录 CodeArts";
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }
  $("lcTitle").textContent = "添加 " + name + " 账号";
  // 此处只会收到内建渠道名（自定义渠道走设置弹层，不经 promptLogin），
  // 故直接查内建名单，不走动态判定的 noExplicitCheckin()。
  $("lcMsg").textContent = NO_EXPLICIT_CHECKIN.has(channel)
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
  else if (pendingChannel === "officeace") startOfficeAceWebLogin();
  else startLogin(pendingChannel, loginLabelValue());
}

// loginLabelValue 读取登录弹窗里的「账号标识」：只对需要它的渠道（CodeArts）有意义，
// 其余渠道那一行是隐藏的，恒返回空串。
function loginLabelValue() {
  const row = $("lcLabelRow");
  if (!row || row.classList.contains("hidden")) return "";
  const inp = $("lcAccountLabel");
  return inp ? inp.value.trim() : "";
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

async function startLogin(channel, accountLabel) {
  // 渠道分派：OfficeAce 是**两步**流程（华为云授权码 + 云端轮询捕获），专属入口是
  // startOfficeAceWebLogin（→ POST /api/login/officeace_auto）。它与下面
  // `POST /api/login/start` 的形态不同——后者一次调用就返回 auth_url，且后端
  // StartLoginFor 不会为 officeace 拉起轮询。这里收口成一个入口，卡片上的 ⟳
  // 与「添加账号」都走同一条路，避免再出现「前端能点、后端不认」的死按钮。
  // （2026-10-18：卡片 ⟳ 曾直接 startLogin('officeace') 而只弹 unknown login provider。）
  if (channel === "officeace") return startOfficeAceWebLogin();
  try {
    const r = await api("/api/login/start", { channel, account_label: accountLabel || "" });
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = `添加 ${chLabel(channel)} 账号`;
    // 三种情形（顺序即优先级）：
    //  ① 后端已用**无痕窗口**拉起（CodeArts）—— 绝不能 window.open：那会在日常浏览器里再开一个
    //     带登录态的标签页，用户又会被授权成老账号，无痕那一步就白做了；
    //  ② 网页版登录渠道（glm / 小浣熊）—— 后端用独立 profile + CDP 拉起，同理不能 open
    //     （那个标签页没有调试端口，捕获不到）；
    //  ③ 其余渠道 —— 由前端 window.open 打开（它们不需要隔离登录态）。
    if (r.backend_opened) {
      $("loginMsg").textContent = `已为你打开一个**无痕窗口**，请在其中完成 ${chLabel(channel)} 登录；`
        + `登录完成后无需任何操作，本工具会自动添加账号。`;
    } else if (channel === "loomy") {
      // Loomy 与 ② 相反：后端**不**拉浏览器，返回的是本机登录引导页地址，
      // 必须由前端 window.open 打开（见 internal/login_loomy）。它也不「捕获凭据」——
      // 用户在引导页里填手机号 + 短信验证码，后端据此换 session。
      $("loginMsg").textContent = `已为你打开 Loomy 登录页，请在其中输入手机号并填写短信验证码；`
        + `登录完成后本工具会自动添加账号。登录态约 14 天后到期，届时需重新登录。`;
      window.open(url, "_blank", "noopener,noreferrer");
    } else if (hasWebLogin(channel)) {
      $("loginMsg").textContent = `已为你打开一个独立的浏览器窗口，请在其中完成${chLabel(channel)}登录；`
        + `登录完成后无需任何操作，本工具会自动捕获凭据并添加账号。`;
    } else {
      $("loginMsg").textContent = "请在浏览器新窗口中完成登录…";
      window.open(url, "_blank", "noopener,noreferrer");
    }
    $("loginOverlay").classList.remove("hidden");
    $("btnCopyUrl").dataset.url = url;
    startLoginPoll();
  } catch (e) {
    toast(e.message);
  }
}

async function cancelLogin() {
  try {
    // 仅在 OfficeAce 流程进行中才调它的取消端点（否则对其它渠道是无谓的 400 噪音）。
    if (oaPoll) { stopOfficeAcePoll(); cancelOfficeAceLogin(); }
    await api("/api/login/cancel", {});
    stopLoginPoll();
    $("loginOverlay").classList.add("hidden");
    toast("登录已取消");
  } catch (e) { toast(e.message); }
}

// ---- OfficeAce 自助登录（华为云 OAuth 授权码 + PKCE）----
let oaPoll = null;
async function startOfficeAceWebLogin() {
  try {
    const r = await api("/api/login/officeace_auto", {});
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = "添加 OfficeAce 账号";
    $("loginMsg").textContent = "已在本设备新标签页打开华为云授权页，请在其中登录并授权；"
      + "完成后本工具会自动捕获凭据（捕获走云端轮询，与在哪台设备打开无关）。"
      + "若浏览器拦截了弹窗未打开，可点「复制链接」手动打开。";
    $("loginOverlay").classList.remove("hidden");
    $("btnCopyUrl").dataset.url = url;
    if (r.warning) toast(r.warning, 6000);
    // 在用户当前设备打开授权页（与其它渠道一致）。
    // 注意：带 noopener 时 window.open 的返回值**恒为 null**（MDN windowFeatures），
    // 不能拿它判断是否被弹窗拦截（否则每次登录都误报「被拦截」）。故：先开、后断链——
    // 不带 feature 打开才能拿到窗口句柄，随后手动置 opener=null 兼顾安全。
    const w = window.open(url, "_blank");
    if (w) {
      try { w.opener = null; } catch (e) { /* 跨域时可能被拒，忽略 */ }
    } else {
      toast("浏览器拦截了弹窗，请点「复制链接」手动打开", 6000);
    }
    startOfficeAcePoll();
  } catch (e) {
    toast(e.message);
  }
}

function startOfficeAcePoll() {
  stopOfficeAcePoll();
  oaPoll = setInterval(async () => {
    try {
      const st = await api("/api/login/officeace_auto_status");
      if (st.status === "done") {
        stopOfficeAcePoll();
        $("loginOverlay").classList.add("hidden");
        toast(`OfficeAce 账号已添加${st.nickname ? "：" + st.nickname : ""}`);
        await loadState();
        refreshFees();
      } else if (st.status === "failed" || st.status === "expired") {
        stopOfficeAcePoll();
        toast(st.error || "OfficeAce 登录失败", 8000);
      } else if (st.status === "canceled" || st.status === "idle") {
        stopOfficeAcePoll();
      }
    } catch (e) { /* 忽略瞬时错误 */ }
  }, 2000);
}

function stopOfficeAcePoll() {
  if (oaPoll) { clearInterval(oaPoll); oaPoll = null; }
}

function cancelOfficeAceLogin() {
  stopOfficeAcePoll();
  api("/api/login/officeace_auto_cancel", {}).catch(() => {});
}

function copyUrl() {
  const url = $("btnCopyUrl").dataset.url;
  if (!url) { toast("暂无链接"); return; }
  // 必须走 copyText：局域网 http:// 是非安全上下文，navigator.clipboard 被浏览器
  // 直接禁用，需降级 execCommand（与顶栏 API-Key 复制同一 helper）。
  copyText(url, "链接");
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

// submitClineKey 提交用户粘贴的 Cline 官方 API Key。
// 后端只做「GET /api/v1/api-keys 验证 + 落盘」，零 token 消耗；
// 返回的 warning 非空表示「这把 Key 与已有账号同属一个 Cline 账号」——
// 官方配额按账号计（…UsageCostUSDPerUser），同账号多 Key 不会扩额度，必须显式告知。
async function submitClineKey() {
  const key = ($("clineKeyInput").value || "").trim();
  if (!key) { $("clineErr").textContent = "请粘贴 API Key（sk_ 开头）"; return; }
  const btn = $("btnClineSubmit");
  btn.disabled = true;
  $("clineErr").textContent = "验证中…";
  try {
    const r = await api("/api/login/cline_key", { api_key: key });
    $("clineOverlay").classList.add("hidden");
    $("clineKeyInput").value = "";
    if (r && r.warning) toast(r.warning);
    toast("Cline 账号已添加");
    await loadState();
    refreshFees();
  } catch (e) {
    $("clineErr").textContent = e.message || "验证失败";
  } finally {
    btn.disabled = false;
  }
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
// ---------- 请求体摘要（诊断开关，设置弹层内，切换立即保存） ----------
async function toggleBodyLog() {
  try {
    await api("/api/config/body_log", { on: $("chkBodyLog").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}

async function toggleAutostart() {
  try {
    await api("/api/config/autostart", { on: $("chkAutostart").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}

// ---------- 模型降级链（设置弹层：原模型无可用账号时按链顶备胎，不粘滞自动切回） ----------
// fbRules 编辑态副本（openSettings 时从 state.model_fallback 深拷贝；保存成功后由 loadState 刷新）
let fbRules = [];

// fbFromSet 汇总当前全部主力模型ID（跨规则）：备胎候选/追加/清洗统一从这里排除——
// 同一 ID 不允许既是某规则主力又出现在任何备胎链。每次下拉 render 实时取，主力列表变更即同步可选范围。
function fbFromSet() {
  const s = {};
  for (const r of fbRules) if (r.from) s[r.from] = true;
  return s;
}
// fbSanitize 就地清洗 fbRules：从每条备胎链剔除任意规则的主力ID（含历史脏数据），返回剔除条数。
// renderFbList 每轮渲染前先过一遍（所有编辑路径都经 renderFbList 重渲染，chip 索引始终与清洗后
// 数组一致），保存前再兜底一次——渲染/比对/落盘共用同一份互斥干净数据。
function fbSanitize() {
  const fs = fbFromSet();
  let dropped = 0;
  for (const r of fbRules) {
    const keep = r.to.filter((t) => !fs[t]);
    dropped += r.to.length - keep.length;
    r.to = keep;
  }
  return dropped;
}

// updateFbDirty / updateEffDirty：编辑态与服务端态比对，有差异时亮「未保存」提示。
// 比对基于 JSON 序列化（fbRules/to 顺序敏感——改序也算未保存，语义正确）；保存成功
// 后服务端态刷新（loadState → openSettings 未重开时由 save 回调手动清），零差异即隐藏。
function fbServerRules() { return JSON.stringify(state.model_fallback || []); }
function updateFbDirty() {
  const el = $("fbDirty");
  if (!el) return;
  el.style.display = JSON.stringify(fbRules) === fbServerRules() ? "none" : "";
}
function updateEffDirty() {
  const el = $("effDirty");
  if (!el) return;
  const effs = state.reasoning_efforts || {};
  const g = $("effGlobalInput").value.trim();
  const cur = {}; if (g) cur["*"] = g;
  for (const r of effRules) cur[r.model] = r.effort;
  const norm = (o) => JSON.stringify(Object.keys(o).sort().map((k) => [k, o[k]]));
  el.style.display = norm(cur) === norm(effs) ? "none" : "";
}

// renderFbList 渲染降级链编辑区：每条规则一个卡片区块（自定义渠道卡片同款设计语言）——
// head 行 = 「规则 N」徽标 + 主力模型ID 输入框 + 🗑；body = 备胎链 chips（含序号与拖拽）+ 添加行。
// chip 序号即降级优先级（内核 pickModelFallback 按 rule.To 顺序取第一个可承载备胎），
// 拖动改序只改编辑态 fbRules，点「保存降级链」才落盘（语义与上游一致）。
// 交互全部事件委托在 #fbList 上（data-del-rule / data-del-to / data-add-to / data-edit-idx /
// dragstart / dragover / drop / dragend），每轮 innerHTML 重建后委托仍然有效。
function renderFbList() {
  const dropped = fbSanitize(); // 互斥校验：主力ID不出现在任何备胎链（chip 索引与数据一致）
  if (dropped > 0) toast(`已自动剔除 ${dropped} 个与主力模型互斥的备胎`); // 唯一剔除时机：打开面板首轮（历史脏数据）/ 改主力波及他链
  const rows = fbRules.map((r, i) => {
    const tos = r.to.map((t, j) =>
      `<span class="fb-to-chip" draggable="true" data-chip="${i}:${j}" data-model="${esc(t)}" title="${esc(t)}（拖动调整顺序，序号 = 降级优先级）">`
      + `<span class="chip-idx">${j + 1}</span><span class="chip-grip">⠿</span>${esc(t)}`
      + `<a data-del-to="${i}:${j}" title="移除该备胎">×</a></span>`
    ).join("");
    return `<div class="fb-rule-card">
      <div class="fb-rule-head">
        <span class="fb-rule-badge">规则 ${i + 1}</span>
        <span class="pk-wrap fb-from-wrap"><input class="input fb-from-edit" data-edit-idx="${i}" value="${esc(r.from)}" spellcheck="false" title="主力模型ID（channel/model，可编辑；失焦或回车确认，点「保存降级链」生效）" placeholder="主力模型ID（channel/model）"></span>
        <button class="icon-btn" data-del-rule="${i}" title="删除整条规则">🗑</button>
      </div>
      <div class="fb-rule-body">
        <div class="fb-chips">${tos || `<span class="hint">（无备胎）</span>`}</div>
        <div class="row" style="margin-top:2px">
          <span class="pk-wrap grow"><input class="input fb-to-add" data-add-idx="${i}" spellcheck="false" placeholder="＋ 备胎模型ID（模糊搜索 / 回车添加 / 逗号分隔多个）"></span>
          <button class="icon-btn" data-add-to="${i}" title="把添加框中的备胎追加到该规则末尾">＋</button>
        </div>
      </div>
    </div>`;
  }).join("");
  $("fbList").innerHTML = rows || `<div class="hint">（暂无规则——原模型无可用账号时将直接返回 503 提示）</div>`;
  $("fbErr").textContent = "";
  attachFbPickers();
  updateFbDirty();
}

// parseModelList 解析备胎输入：逗号/空格/换行分隔多个 channel/model，本地形态校验。
function parseModelList(raw) {
  const out = [];
  for (const p of String(raw || "").split(/[,，\s]+/)) {
    const t = p.trim();
    if (!t) continue;
    if (!/^[^/]+\/.+/.test(t)) return [null, `备胎模型 ${t} 需为 channel/model 形式（如 workbuddyai/glm-5.2）`];
    out.push(t);
  }
  return [out, ""];
}

// attachFbPickers 给 renderFbList 产出的行内输入框挂模糊搜索下拉（每轮 innerHTML 重建后重挂）：
// - 主力模型ID（.fb-from-edit）：单选下拉，选中回填；Enter 无候选时 blur 确认（原行为）。
// - 行内备胎添加框（.fb-to-add）：多选下拉——点选即追加到备胎链末尾（精确等值去重，
//   与上游 to.includes 全等语义一致）、已上链模型从候选隐藏、可连续选择多个。
function attachFbPickers() {
  for (const inp of $("fbList").querySelectorAll(".fb-from-edit")) {
    const self = fbRules[+inp.dataset.editIdx].from; // 自身当前值保留可见，仅排除其他规则主力（重复由 change 唯一性校验兜底）
    attachModelPicker(inp, { excludeSet: () => { const s = { ...fbFromSet() }; delete s[self]; return s; } });
  }
  for (const inp of $("fbList").querySelectorAll(".fb-to-add")) {
    const i = +inp.dataset.addIdx;
    attachModelPicker(inp, {
      stayOpen: true,
      excludeSet: () => { const s = { ...fbFromSet() }; for (const t of (fbRules[i] && fbRules[i].to) || []) s[t] = true; return s; },
      onPick: (id) => {
        const r = fbRules[i];
        if (!r) return;
        if (fbFromSet()[id] || r.to.includes(id)) return; // 主力ID或已上链 → 不追加（候选已隐藏，双保险）
        r.to.push(id);
        renderFbList();
        toast(`已加入备胎链第 ${r.to.length} 位（点保存生效）`);
      },
    });
  }
}

async function saveModelFallback() {
  try {
    fbSanitize(); // 落盘前互斥兜底（正常路径 renderFbList 已清洗过，防御未来路径）
    await api("/api/config/model_fallback", { rules: fbRules });
    toast("降级链已保存（热生效）");
    $("fbErr").textContent = "";
    state.model_fallback = JSON.parse(JSON.stringify(fbRules)); // 即时同步服务端态基准，提示随之熄灭
    updateFbDirty();
    loadState();
  } catch (e) { $("fbErr").textContent = e.message; }
}

// ---------- 自定义渠道（OpenAI 兼容，R66） ----------
// RESERVED_CHANNEL_NAMES 与 provider.BuiltinKinds() 同集：与内建渠道（或上游命名空间）
// 同名时 server/app 两层会指向不同 Runtime，故前端先给即时反馈（后端同样会拒）。
const RESERVED_CHANNEL_NAMES = new Set([
  "workbuddy", "workbuddyai", "traework", "traecode", "qoder", "qodercn", "qodercom",
  "qwenwork", "monkeycode", "raccoon", "loomy", "oczen", "kilo", "minimax", "lobsterai", "zcode", "gemini", "codearts", "glm", "cline", "cline-free",
]);
let editCustomChannels = [];

// parseTokens 解析带 k/M 单位的 token 数（如 1M→1000000、64k→64000）；纯数字原样返回。
function parseTokens(str, fallback) {
  if (str === undefined || str === null || str === "") return fallback || 0;
  if (typeof str === "number") return str;
  const s = String(str).trim();
  if (!s) return fallback || 0;
  const upper = s.toUpperCase();
  let mult = 1;
  let numStr = upper;
  if (upper.endsWith("M")) {
    mult = 1000000;
    numStr = upper.slice(0, -1).trim();
  } else if (upper.endsWith("K")) {
    mult = 1000;
    numStr = upper.slice(0, -1).trim();
  }
  const n = parseFloat(numStr);
  if (isNaN(n) || n < 0) return fallback || 0;
  return Math.round(n * mult);
}

// formatTokenCount 反向格式化：优先整数 k/M（1M、64k），非整除时保留一位小数。
function formatTokenCount(num) {
  if (!num) return "—";
  if (num >= 1000000 && num % 100000 === 0) return `${num / 1000000}M`;
  if (num >= 1000 && num % 1000 === 0) return `${num / 1000}k`;
  if (num >= 1000000) return `${(num / 1000000).toFixed(1).replace(/\.0$/, "")}M`;
  if (num >= 1000) return `${(num / 1000).toFixed(1).replace(/\.0$/, "")}k`;
  return String(num);
}

function renderCustomChList() {
  const cards = editCustomChannels.map((c, i) => {
    const isOff = c.enabled === false;
    const offTag = isOff ? '<span class="tag neutral" style="margin-left:auto">已禁用</span>' : '<span class="tag ok" style="margin-left:auto">已启用</span>';
    const keyMask = c.api_key ? (c.api_key.length > 8 ? c.api_key.slice(0, 3) + "…" + c.api_key.slice(-4) : "••••••") : "无密钥";
    const models = c.models || [];

    const modelRows = models.map((m, j) => {
      const mid = typeof m === "string" ? m : m.id;
      const ctxNum = typeof m === "object" ? parseTokens(m.context_window, 0) : 0;
      const outNum = typeof m === "object" ? parseTokens(m.max_tokens, 0) : 0;
      const ctx = ctxNum ? formatTokenCount(ctxNum) : "—";
      const maxTok = outNum ? formatTokenCount(outNum) : "—";
      const efforts = typeof m === "object" && m.reasoning_efforts && m.reasoning_efforts.length ? m.reasoning_efforts : [];
      const defEff = typeof m === "object" && m.default_reasoning_effort ? m.default_reasoning_effort : (efforts[0] || "");
      const rsnHtml = (typeof m === "object" && (m.supports_reasoning || efforts.length > 0))
        ? `<span class="tag ok" title="默认: ${esc(defEff)}">${esc(defEff || "开启")}</span> <span class="hint">[${esc(efforts.join(", "))}]</span>`
        : `<span class="hint">—</span>`;
      const hasImg = typeof m === "object" && m.supports_images;
      const hasRsn = typeof m === "object" && (m.supports_reasoning || efforts.length > 0);
      const hasTools = typeof m === "object" ? m.supports_tools !== false : true;
      const capTags = [];
      if (hasImg) capTags.push('<span class="tag">识图</span>');
      if (hasRsn) capTags.push('<span class="tag">思考</span>');
      if (hasTools) capTags.push('<span class="tag">工具</span>');
      const caps = capTags.length > 0 ? capTags.join(" ") : '<span class="hint">—</span>';

      return `<tr>
        <td><code>${esc(mid)}</code></td>
        <td>${esc(ctx)}</td>
        <td>${esc(maxTok)}</td>
        <td>${rsnHtml}</td>
        <td>${caps}</td>
        <td style="text-align:center"><a data-del-model="${i}:${j}" style="cursor:pointer;color:var(--danger);font-weight:bold;font-size:14px" title="移除此模型">×</a></td>
      </tr>`;
    }).join("");

    const tableHtml = models.length > 0 ? `
      <table class="custom-models-table">
        <colgroup>
          <col style="width:auto">
          <col style="width:75px">
          <col style="width:75px">
          <col style="width:130px">
          <col style="width:130px">
          <col style="width:36px">
        </colgroup>
        <thead>
          <tr>
            <th>模型 ID</th>
            <th>上下文</th>
            <th>最大输出</th>
            <th>思考等级</th>
            <th>能力</th>
            <th style="text-align:center">操作</th>
          </tr>
        </thead>
        <tbody>${modelRows}</tbody>
      </table>
    ` : `<div class="hint" style="margin:4px 0 8px">（该渠道尚未配置模型，请在下方添加模型及参数）</div>`;

    return `
    <div class="custom-ch-card">
      <div class="custom-ch-header">
        <span class="badge custom">${esc(c.name)}</span>
        <span style="font-family:ui-monospace,monospace;font-size:12px;font-weight:600;color:var(--text)">${esc(c.base_url)}</span>
        <span class="hint" title="${esc(c.api_key || '')}">[${esc(keyMask)}]</span>
        ${offTag}
        <button class="btn small" data-test-custom="${i}">测试连通</button>
        <button class="btn small" data-toggle-custom="${i}">${isOff ? "启用" : "停用"}</button>
        <button class="icon-btn" data-del-custom="${i}" title="删除该渠道">🗑</button>
      </div>

      <div class="custom-models-box">
        ${tableHtml}
        <div class="custom-model-add-row">
          <input class="input" data-m-id="${i}" placeholder="模型 ID" style="flex:1;min-width:110px">
          <input class="input" data-m-ctx="${i}" placeholder="上下文(如 1M)" style="width:90px;flex-shrink:0" value="1M">
          <input class="input" data-m-out="${i}" placeholder="输出(如 64k)" style="width:80px;flex-shrink:0" value="64k">
          <input class="input" data-m-eff="${i}" placeholder="思考档位" style="width:110px;flex-shrink:0">
          <label class="hint" style="display:inline-flex;align-items:center;gap:2px;cursor:pointer;white-space:nowrap;margin:0 1px"><input type="checkbox" data-m-img="${i}" checked>识图</label>
          <label class="hint" style="display:inline-flex;align-items:center;gap:2px;cursor:pointer;white-space:nowrap;margin:0 1px"><input type="checkbox" data-m-rsn="${i}" checked>思考</label>
          <label class="hint" style="display:inline-flex;align-items:center;gap:2px;cursor:pointer;white-space:nowrap;margin:0 1px"><input type="checkbox" data-m-tools="${i}" checked>工具</label>
          <button class="btn small icon-btn" data-add-model="${i}" style="font-size:15px;width:32px;height:30px;padding:0;display:inline-flex;align-items:center;justify-content:center;flex-shrink:0" title="添加模型">➕</button>
        </div>
      </div>
    </div>`;
  }).join("");

  $("customChList").innerHTML = cards || `<div class="hint">（暂无自定义渠道，请先在下方输入渠道标识、地址与 API 密钥添加渠道）</div>`;
  $("customChErr").textContent = "";
  $("customChOk").textContent = "";
}

// addPendingCustomInputs 自定义渠道输入行有内容但未点「＋新增渠道」时，在保存前自动收录
// 进编辑列表。返回 "" 表示无需收录或收录成功；否则返回错误提示。
// 背景：此前直接点「保存全部配置」只提交内存列表——用户填完输入行没点「＋新增渠道」就点
// 保存，会把空列表存进后端（toast 显示已保存但渠道实际未创建），重开弹层输入也会丢。
function addPendingCustomInputs() {
  const name = $("inCustomName").value.trim().toLowerCase();
  const url = $("inCustomURL").value.trim();
  const key = $("inCustomKey").value.trim();
  if (!name && !url && !key) return "";
  if (!name) return "输入行有未添加的渠道：请填入渠道标识";
  if (!/^[a-zA-Z0-9_\-]+$/.test(name)) return "渠道标识仅允许字母、数字、下划线、短横线";
  if (RESERVED_CHANNEL_NAMES.has(name)) return `渠道标识 ${name} 与内建渠道同名，请另取一个`;
  if (!url) return "输入行有未添加的渠道：请填入 Base URL";
  if (editCustomChannels.some(c => c.name.toLowerCase() === name)) return `已存在同名渠道 ${name}`;
  editCustomChannels.push({ name, base_url: url, api_key: key, models: [], enabled: true });
  renderCustomChList();
  $("inCustomName").value = ""; $("inCustomURL").value = ""; $("inCustomKey").value = "";
  return "";
}

async function saveCustomChannels() {
  try {
    const pendingErr = addPendingCustomInputs(); // 输入行有内容但没点「＋新增渠道」时自动收录
    if (pendingErr) { $("customChErr").textContent = pendingErr; $("customChOk").textContent = ""; return; }
    await api("/api/config/custom_channels", { channels: editCustomChannels });
    toast("自定义渠道已保存（热生效）");
    $("customChErr").textContent = "";
    $("customChOk").textContent = "渠道及模型配置已成功保存并立即生效";
    loadState();
  } catch (e) {
    $("customChErr").textContent = e.message;
    $("customChOk").textContent = "";
  }
}

// ---------- 思考强度注入（设置弹层：客户端未传 reasoning_effort 时按模型注入档位） ----------
// effRules 编辑态副本（openSettings 时从 state.reasoning_efforts 拷贝，`*` 全局默认
// 单独走「全局默认」输入行；保存成功后由 loadState 刷新）。
let effRules = [];

// renderEffList 渲染模型档位编辑区：每条映射一个卡片区块（降级链规则卡片同款设计语言）——
// 「映射 N」徽标 + 模型 ID（超长省略，title 悬浮看全）→ 档位 + 🗑；左对齐不撑满整行。
function renderEffList() {
  const rows = effRules.map((r, i) =>
    `<div class="fb-rule-card fb-map-card">
      <div class="fb-rule-head">
        <span class="fb-rule-badge">映射 ${i + 1}</span>
        <span class="fb-map-model" title="${esc(r.model)}">${esc(r.model)}</span>
        <span class="fb-map-arrow">→</span>
        <span class="fb-map-eff">${esc(r.effort)}</span>
        <button class="icon-btn" data-del-eff="${i}" title="删除该映射">🗑</button>
      </div>
    </div>`
  ).join("");
  $("effList").innerHTML = rows || `<div class="hint">（暂无模型档位——未配置的请求不注入）</div>`;
  $("effErr").textContent = "";
  updateEffDirty();
}

async function saveReasoningEfforts() {
  try {
    const efforts = {};
    const g = $("effGlobalInput").value.trim();
    if (g) efforts["*"] = g;
    for (const r of effRules) efforts[r.model] = r.effort;
    await api("/api/config/reasoning_efforts", { efforts });
    toast("思考强度已保存（热生效）");
    $("effErr").textContent = "";
    state.reasoning_efforts = efforts; // 即时同步服务端态基准，提示随之熄灭
    updateEffDirty();
    loadState();
  } catch (e) { $("effErr").textContent = e.message; }
}

// ---------- 设置弹层（统一配置：监听/API-Key/签到/自启/模型路由/备用上游通道） ----------
// renderBackupProxy 回显全局备用上游通道（单一地址，空 = 关闭）。
// 定位是**备份链路**而不是全局代理：不参与常规请求，仅渠道命中「上游地区拒绝」
// 时在同一次请求内自动取用（决议 R57），故文案里不说「走代理」而说「重发」。
function renderBackupProxy() {
  const inp = $("backupProxyInput");
  inp.value = state.backup_proxy || "";
  inp.placeholder = "如 socks5://127.0.0.1:1080 或 http://127.0.0.1:7890（留空 = 关闭）";
  // 生效位：后端同时回 backup_proxy（配置）与 backup_proxy_active（运行时已装载）。
  // 两者不等 = 配置写了但进程内未生效（地址不合法/启动失败），必须让用户看见。
  const s = $("backupProxyState");
  s.textContent = !state.backup_proxy ? ""
    : (state.backup_proxy_active ? "已生效" : "未生效（地址无效或启动失败）");
  s.style.color = state.backup_proxy && !state.backup_proxy_active ? "var(--danger)" : "var(--muted)";
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
  // 弹层互斥：打开设置时收起「查找模型」（#134）
  $("modelSearchOverlay").classList.add("hidden");
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
  // 备用上游通道 + oczen 自定义 key
  renderBackupProxy();
  $("oczenKeyInput").value = state.oczen_api_key || ""; // 回显脱敏值；未改动则原样回传，后端按脱敏值识别为「未变」
  $("oczenKeyInput").dataset.touched = "";
  // 模型降级链
  fbRules = JSON.parse(JSON.stringify(state.model_fallback || []));
  renderFbList();
  $("fbFromInput").value = ""; $("fbToInput").value = "";
  // 思考强度注入（`*` 全局默认单独回显到「全局默认」行）
  const effs = state.reasoning_efforts || {};
  effRules = Object.entries(effs).filter(([k]) => k !== "*").map(([model, effort]) => ({ model, effort }));
  $("effGlobalInput").value = effs["*"] || "";
  renderEffList();
  $("effModelInput").value = ""; $("effEffortInput").value = "";
  // 自定义渠道
  editCustomChannels = JSON.parse(JSON.stringify(state.custom_channels || []));
  renderCustomChList();
  $("inCustomName").value = ""; $("inCustomURL").value = ""; $("inCustomKey").value = "";
  // 自动签到 + 开机自启 + 临期阈值
  $("chkAutostart").checked = !!state.autostart;
  $("selExpiring").value = String(state.expiring_days || 1);
  $("chkLoopGuard").checked = state.loop_guard !== false; // 缺省（旧后端）按开启显示
  $("chkBodyLog").checked = !!state.body_log;
  syncListenRisk(); // 按当前监听地址初始化警告条与必填星号
  $("settingsOverlay").classList.remove("hidden");
}

function closeSettings() {
  $("settingsOverlay").classList.add("hidden");
}

// validateBackupProxy 校验备用上游通道地址，返回 [url, err]（空串合法 = 关闭）。
// 判据与后端 backuplink.Parse 同源：仅 http/https/socks5，且必须有主机名。
function validateBackupProxy() {
  const v = $("backupProxyInput").value.trim();
  if (!v) return ["", ""];
  let u;
  try { u = new URL(v); } catch { return [null, `备用代理地址无效：${v}`]; }
  if (!["http:", "https:", "socks5:"].includes(u.protocol)) {
    return [null, `备用代理协议不支持（仅 http/https/socks5）：${v}`];
  }
  if (!u.hostname) return [null, `备用代理缺少主机名：${v}`];
  return [v, ""];
}

// testOczenKey 「测试」按钮：用输入框当前的候选 key（含脱敏值/空）调后端实测。
// 后端逻辑：候选 key 临时写入渠道 client → big-pickle 发最小对话 → 200/429 均算通过
// （200 = 配额正常；429 = 连通但匿名共享配额限流，属预期现象）；测试后恢复配置原值。
// 与真实对话同源（issue #117）：直连被地区拒时后端会自动改走备用通道，
// via_backup 标出答复来自哪个出口——否则用户配了代理点测试仍看到 403 会误以为功能没生效。
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
    const via = r.via_backup ? "经备用通道" : "直连";
    if (r.ok) {
      toast(r.status === 200 ? `✓ OpenCodeZen 凭证可用（200，${via}）` : `✓ 连通正常（429 匿名限流，属预期，${via}）`);
    } else {
      const brief = (r.body || r.error || "").slice(0, 90).replace(/\s+/g, " ");
      // 403 且经直连 = 最可能是地区限制，而备用通道未配置/未生效：给出可行动提示
      const hint = (!r.via_backup && r.status === 403) ? "（未走备用通道；若已配代理仍为直连，说明本次不是地区拒绝或备用链路拨不动，详见日志）" : "";
      toast(`✗ 测试失败 HTTP ${r.status}${hint}：${brief}`);
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

  // 2) 备用上游通道（先本地校验，失败阻断保存）+ oczen key（仅在用户改动过时提交，避免把脱敏回显值存回）
  const [proxy, perr] = validateBackupProxy();
  if (perr) { $("proxyErr").textContent = perr; toast(perr); return; }
  const body = { proxy };
  const kIn = $("oczenKeyInput");
  if (kIn.dataset.touched === "1") {
    let k = kIn.value.trim();
    // 值仍是脱敏形态（含 …）则视为未修改，保持服务端现值
    if (k && k.includes("…")) k = undefined;
    if (k !== undefined) body.oczen_api_key = k; // undefined 时不携带字段 = 不改动；空串 = 清除回匿名
  }
  try {
    await api("/api/config/proxy", body);
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

  // 4.5) 思考链防癫开关（独立端点即时生效）
  const loopGuardOn = $("chkLoopGuard").checked;
  if (loopGuardOn !== (state.loop_guard !== false)) {
    try {
      await api("/api/config/loop_guard", { on: loopGuardOn });
    } catch (e) { toast(e.message); return; }
  }

  // 4.6) 自定义渠道（R66）——输入行有未点「＋新增渠道」的内容时先收录再存
  const customPendingErr = addPendingCustomInputs();
  if (customPendingErr) { toast(customPendingErr); return; }
  try {
    await api("/api/config/custom_channels", { channels: editCustomChannels });
  } catch (e) { toast(e.message); return; }

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
  if (uid === "kilo-anonymous") { toast("Kilo 免费池账号不可改名"); return; }
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
  $("btnAddCline").onclick = () => promptLogin("cline");
  $("btnClineSubmit").onclick = submitClineKey;
  $("btnClineCancel").onclick = () => $("clineOverlay").classList.add("hidden");
  $("btnAddOfficeAce").onclick = () => promptLogin("officeace");
  $("btnAddMiniMax").onclick = () => promptLogin("minimax");
  $("btnAddLobsterAI").onclick = () => promptLogin("lobsterai");
  $("btnAddZCode").onclick = () => promptLogin("zcode");
  $("btnAddGemini").onclick = () => promptLogin("gemini");
  $("btnAddCodeArts").onclick = () => promptLogin("codearts");
  $("btnCheckinAll").onclick = checkinAll;
  $("btnRefreshAll").onclick = refreshAll;
  $("btnAddTime").onclick = addTime;
  $("btnCopyUrl").onclick = copyUrl;
  $("btnCancelLogin").onclick = cancelLogin;
  $("btnRefreshFees").onclick = refreshFees;
  // 模型查找面板（纯前端）：入口按钮 + 关闭 + 输入即过滤 + 结果点击复制完整 ID。
  $("btnModelSearch").onclick = openModelSearch;
  // 设置弹层内「模型降级链 / 思考强度」标题右侧的查找入口：与首页共用同一套弹层与渲染，
  // 故只做入口接线（这些按钮在动态渲染的配置区之外，属静态 DOM，用属性选择器统一绑定）。
  for (const el of document.querySelectorAll("[data-model-search]")) {
    el.onclick = openModelSearch;
  }
  $("modelSearchOverlay").onclick = (e) => { if (e.target === $("modelSearchOverlay")) closeModelSearch(); };
  $("modelSearchInput").oninput = (e) => renderModelSearch(e.target.value);
  $("modelSearchInput").onkeydown = (e) => { if (e.key === "Escape") closeModelSearch(); };
  $("modelSearchList").onclick = (e) => {
    const row = e.target.closest(".ms-row");
    if (row) copyText(row.dataset.mid, "模型 ID");
  };
  $("modelSearchList").onkeydown = (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const row = e.target.closest(".ms-row");
    if (row) { e.preventDefault(); copyText(row.dataset.mid, "模型 ID"); }
  };
  $("chkAutostart").onchange = toggleAutostart;
  $("chkBodyLog").onchange = toggleBodyLog;
  $("btnAdminLogout").onclick = () => logout();
  $("btnClearAdminPass").onclick = clearAdminPassword;
  $("btnAdminLogin").onclick = submitLogin;
  $("adminLoginPass").onkeydown = (e) => { if (e.key === "Enter") submitLogin(); };

  $("apiAddr").onclick = () => {
    const v = $("apiAddr").querySelector(".val").textContent;
    copyText(v, "OpenAI 接口地址");
  };
  $("apiKeyDisplay").onclick = () => {
    const v = $("apiKeyDisplay").dataset.key;
    if (v === undefined || v === "") { toast("当前未设置 API-Key"); return; }
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

  // 模型降级链：添加规则 / 保存 / 列表内事件委托（删规则、删备胎、追加备胎、回车添加、拖拽排序）
  $("btnFbAdd").onclick = () => {
    const from = $("fbFromInput").value.trim();
    if (!/^[^/]+\/.+/.test(from)) { $("fbErr").textContent = "主力模型ID 需为 channel/model 形式（如 workbuddyai/deepseek-v4.1-flash）"; return; }
    if (fbRules.some((r) => r.from === from)) { $("fbErr").textContent = `主模型 ${from} 已有规则（每个主模型只允许一条）`; return; }
    const [tos, err] = parseModelList($("fbToInput").value);
    if (err) { $("fbErr").textContent = err; return; }
    if (tos.length === 0) { $("fbErr").textContent = "至少填一个备胎模型ID"; return; }
    // 互斥预过滤：剔除已有规则主力 + 新 from 自身（renderFbList 兜底同规则，此处前置给即时反馈）
    const fsx = fbFromSet(); fsx[from] = true;
    const kept = tos.filter((t) => !fsx[t]);
    if (kept.length === 0) { $("fbErr").textContent = "备胎均与主力模型互斥（含新主力自身），请更换备胎"; return; }
    if (kept.length < tos.length) toast(`已剔除 ${tos.length - kept.length} 个与主力模型互斥的备胎`);
    fbRules.push({ from, to: kept });
    $("fbFromInput").value = ""; $("fbToInput").value = "";
    renderFbList(); // 内部先 fbSanitize：互斥兜底（预过滤后此处不再剔除）
  };
  $("btnFbSave").onclick = saveModelFallback;
  $("fbList").addEventListener("click", async (e) => {
    const addBtn = e.target.closest("[data-add-to]");
    if (addBtn) {
      const i = +addBtn.dataset.addTo;
      const inp = document.querySelector(`.fb-to-add[data-add-idx="${i}"]`);
      const [tos, err] = parseModelList(inp && inp.value);
      if (err) { $("fbErr").textContent = err; return; }
      if (tos.length === 0) { $("fbErr").textContent = "先在输入框填备胎模型ID 再点 ＋"; return; }
      fbRules[i].to.push(...tos.filter((t) => !fbFromSet()[t] && !fbRules[i].to.includes(t))); // 主力ID全局排除
      renderFbList();
      return;
    }
    const delTo = e.target.closest("[data-del-to]");
    if (delTo) {
      const [i, j] = delTo.dataset.delTo.split(":").map(Number);
      fbRules[i].to.splice(j, 1);
      renderFbList();
      return;
    }
    const delRule = e.target.closest("[data-del-rule]");
    if (delRule) {
      fbRules.splice(+delRule.dataset.delRule, 1);
      renderFbList();
    }
  });
  // 行内备胎输入框回车 = 追加到该规则（事件委托 keydown）；主模型输入框回车 = blur 确认。
  // combobox 的 Enter 在下拉开着时已被其自身 stopPropagation 消化，走到这里的必是「下拉关闭态」。
  $("fbList").addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    if (e.target.classList.contains("fb-from-edit")) { e.target.blur(); return; }
    if (!e.target.classList.contains("fb-to-add")) return;
    const i = +e.target.dataset.addIdx;
    const [tos, err] = parseModelList(e.target.value);
    if (err) { $("fbErr").textContent = err; return; }
    if (tos.length === 0) return;
    fbRules[i].to.push(...tos.filter((t) => !fbFromSet()[t] && !fbRules[i].to.includes(t))); // 主力ID全局排除
    renderFbList();
  });
  // 行内主模型 ID 编辑：change（失焦/回车后 blur）提交——形态校验 + 主模型唯一性校验，
  // 失败回滚输入框；成功写回 fbRules 并滤掉与新 from 自环的备胎，重渲染刷新 title。
  $("fbList").addEventListener("change", (e) => {
    if (!e.target.classList.contains("fb-from-edit")) return;
    const i = +e.target.dataset.editIdx;
    const from = String(e.target.value || "").trim();
    const cur = fbRules[i].from;
    if (!from || from === cur) { e.target.value = cur; return; }
    if (!/^[^/]+\/.+/.test(from)) { $("fbErr").textContent = `主力模型ID ${from} 需为 channel/model 形式（如 workbuddyai/deepseek-v4.1-flash）`; e.target.value = cur; return; }
    if (fbRules.some((r, k) => k !== i && r.from === from)) { $("fbErr").textContent = `主模型 ${from} 已有规则（每个主模型只允许一条）`; e.target.value = cur; return; }
    fbRules[i].from = from;
    fbRules[i].to = fbRules[i].to.filter((t) => t !== from);
    renderFbList();
  });
  // 备胎 chip 拖拽排序（委托在 #fbList；仅同规则内拖动）。drop 后就地 splice fbRules[i].to
  // 并重渲染（chip 序号随之刷新）；只改编辑态，点「保存降级链」才落盘。
  // 从 × 删除链接起手不拖（chip 内 a[data-del-to] 命中时跳过 dragstart）。
  let fbDragSrc = null; // { i, j, el }
  $("fbList").addEventListener("dragstart", (e) => {
    const chip = e.target.closest(".fb-to-chip");
    if (!chip || e.target.closest("[data-del-to]")) { e.preventDefault(); return; }
    const [i, j] = chip.dataset.chip.split(":").map(Number);
    fbDragSrc = { i, j, el: chip };
    chip.classList.add("dragging");
    try { e.dataTransfer.setData("text/plain", "x"); e.dataTransfer.effectAllowed = "move"; } catch (err) {}
  });
  $("fbList").addEventListener("dragover", (e) => {
    if (!fbDragSrc) return;
    e.preventDefault();
    const chip = e.target.closest(".fb-to-chip");
    // 清掉上一轮指示条，再按指针相对 chip 左右半标记插入位
    document.querySelectorAll(".fb-to-chip.drop-before,.fb-to-chip.drop-after").forEach((c) => c.classList.remove("drop-before", "drop-after"));
    if (!chip || chip === fbDragSrc.el) return;
    const r = chip.getBoundingClientRect();
    chip.classList.add(e.clientX - r.left < r.width / 2 ? "drop-before" : "drop-after");
  });
  $("fbList").addEventListener("drop", (e) => {
    e.preventDefault();
    const chip = e.target.closest(".fb-to-chip");
    const mark = document.querySelector(".fb-to-chip.drop-before, .fb-to-chip.drop-after");
    if (chip && mark && fbDragSrc && chip.dataset.chip && fbDragSrc.el.dataset.chip) {
      const [si, sj] = fbDragSrc.el.dataset.chip.split(":").map(Number);
      const [ti] = chip.dataset.chip.split(":").map(Number);
      if (si === ti) { // 仅同规则内拖动
        const before = mark.classList.contains("drop-before");
        let dj = +chip.dataset.chip.split(":")[1] + (before ? 0 : 1);
        const arr = fbRules[si].to;
        const [moved] = arr.splice(sj, 1);
        if (dj > sj) dj--; // 移除原位后修正目标下标
        arr.splice(dj, 0, moved);
        renderFbList();
        toast("顺序已调整（序号已同步），点「保存降级链」生效");
      }
    }
    document.querySelectorAll(".fb-to-chip.drop-before,.fb-to-chip.drop-after").forEach((c) => c.classList.remove("drop-before", "drop-after"));
    fbDragSrc = null;
  });
  $("fbList").addEventListener("dragend", () => {
    if (fbDragSrc) fbDragSrc.el.classList.remove("dragging");
    fbDragSrc = null;
    document.querySelectorAll(".fb-to-chip.drop-before,.fb-to-chip.drop-after").forEach((c) => c.classList.remove("drop-before", "drop-after"));
  });

  // 思考强度：添加映射 / 保存 / 列表内删除 / 回车添加
  $("btnEffAdd").onclick = () => {
    const model = $("effModelInput").value.trim();
    const effort = $("effEffortInput").value.trim();
    if (!model) { $("effErr").textContent = "模型ID 不能为空（全局默认请在上方「全局默认」行填）"; return; }
    if (model === "*") { $("effErr").textContent = "* 是全局默认专用键，请在上方「全局默认」行填"; return; }
    if (!effort) { $("effErr").textContent = "思考档位不能为空"; return; }
    if (effRules.some((r) => r.model === model)) { $("effErr").textContent = `模型 ${model} 已有档位（每个模型只允许一条）`; return; }
    effRules.push({ model, effort });
    $("effModelInput").value = ""; $("effEffortInput").value = "";
    renderEffList();
  };
  $("btnEffSave").onclick = saveReasoningEfforts;
  $("effList").addEventListener("click", (e) => {
    const del = e.target.closest("[data-del-eff]");
    if (del) {
      effRules.splice(+del.dataset.delEff, 1);
      renderEffList();
    }
  });

  // 配置区模糊搜索下拉接线（静态输入框一次性挂；行内输入框由 attachFbPickers 每轮重挂）。
  // 回车链：底部主力框 → 跳备胎框；备胎框 → 添加规则；映射模型框 → 跳档位框；档位框 → 添加映射。
  attachModelPicker($("fbFromInput"), {
    excludeSet: () => fbFromSet(), // 已配置的主力从候选隐藏（重复由下方添加校验兜底），与备胎框行为对齐
    enterAction: () => $("fbToInput").focus(),
  });
  attachModelPicker($("fbToInput"), {
    stayOpen: true, multiAccum: true, // 累积多选：点选逐个逗号追加，末段继续搜
    excludeSet: () => fbFromSet(), // 已配置的主力模型ID从备胎候选隐藏（实时取，主力变更即同步）
    enterAction: () => $("btnFbAdd").onclick(),
  });
  attachModelPicker($("effModelInput"), { enterAction: () => $("effEffortInput").focus() });
  attachEffortPicker($("effGlobalInput"));
  attachEffortPicker($("effEffortInput"), { enterAction: () => $("btnEffAdd").onclick() });
  // 全局默认框手输/选择后即时刷新未保存提示（effRules 变化经 renderEffList 已覆盖）
  $("effGlobalInput").addEventListener("input", updateEffDirty);

  // 自定义渠道（R66）：测试、停用/启用、删除、添加、保存
  $("customChList").addEventListener("click", async (e) => {
    const testBtn = e.target.closest("[data-test-custom]");
    if (testBtn) {
      const idx = +testBtn.dataset.testCustom;
      const ch = editCustomChannels[idx];
      if (!ch) return;
      testBtn.disabled = true;
      const oldText = testBtn.textContent;
      testBtn.textContent = "测试中…";
      $("customChErr").textContent = "";
      $("customChOk").textContent = "";
      try {
        // name + base_url + api_key（回显态可能是脱敏值）一起发：后端按 name 找回真实密钥，
        // 否则未改动过的渠道点测试会恒报 401（同 #117 的「探针没走同一条链路」）。
        const res = await api("/api/config/custom_channel_test", { name: ch.name, base_url: ch.base_url, api_key: ch.api_key });
        if (res.ok) {
          toast(`[${ch.name}] 连接成功！拉取到 ${res.models?.length || 0} 个模型`);
          $("customChOk").textContent = `[${ch.name}] 连接成功，模型: ${(res.models || []).slice(0, 6).join(", ")}${res.models?.length > 6 ? '…' : ''}`;
        } else {
          $("customChErr").textContent = `[${ch.name}] 测试失败：${res.error}`;
        }
      } catch (err) {
        $("customChErr").textContent = `[${ch.name}] 测试失败：${err.message}`;
      } finally {
        testBtn.disabled = false;
        testBtn.textContent = oldText;
      }
      return;
    }
    const toggleBtn = e.target.closest("[data-toggle-custom]");
    if (toggleBtn) {
      const idx = +toggleBtn.dataset.toggleCustom;
      if (editCustomChannels[idx]) {
        const cur = editCustomChannels[idx].enabled !== false;
        editCustomChannels[idx].enabled = !cur;
        renderCustomChList();
      }
      return;
    }
    const delBtn = e.target.closest("[data-del-custom]");
    if (delBtn) {
      const idx = +delBtn.dataset.delCustom;
      editCustomChannels.splice(idx, 1);
      renderCustomChList();
      return;
    }
    const delModelLink = e.target.closest("[data-del-model]");
    if (delModelLink) {
      const [i, j] = delModelLink.dataset.delModel.split(":").map(Number);
      if (editCustomChannels[i] && editCustomChannels[i].models) {
        editCustomChannels[i].models.splice(j, 1);
        renderCustomChList();
      }
      return;
    }
    const addModelBtn = e.target.closest("[data-add-model]");
    if (addModelBtn) {
      const idx = +addModelBtn.dataset.addModel;
      const ch = editCustomChannels[idx];
      if (!ch) return;
      const idInp = document.querySelector(`[data-m-id="${idx}"]`);
      const ctxInp = document.querySelector(`[data-m-ctx="${idx}"]`);
      const outInp = document.querySelector(`[data-m-out="${idx}"]`);
      const effInp = document.querySelector(`[data-m-eff="${idx}"]`);
      const imgInp = document.querySelector(`[data-m-img="${idx}"]`);
      const rsnInp = document.querySelector(`[data-m-rsn="${idx}"]`);
      const toolsInp = document.querySelector(`[data-m-tools="${idx}"]`);

      const mid = idInp ? idInp.value.trim() : "";
      if (!mid) {
        $("customChErr").textContent = "请填入模型 ID";
        return;
      }
      ch.models = ch.models || [];
      if (ch.models.some((m) => (typeof m === "string" ? m : m.id) === mid)) {
        $("customChErr").textContent = `该渠道下已存在模型 ${mid}`;
        return;
      }
      // 上下文/输出支持 k、M 单位（如 1M / 64k），未填时分别以 1M / 64k 兜底
      const ctx = parseTokens(ctxInp ? ctxInp.value : "", 1000000);
      const maxTok = parseTokens(outInp ? outInp.value : "", 64000);
      const effRaw = effInp ? effInp.value.trim() : "";
      const efforts = effRaw ? effRaw.split(/[,，\s]+/).filter(Boolean) : [];
      const hasRsn = rsnInp ? rsnInp.checked : true;
      const hasImg = imgInp ? imgInp.checked : true;
      const hasTools = toolsInp ? toolsInp.checked : true;

      ch.models.push({
        id: mid,
        context_window: ctx,
        max_tokens: maxTok,
        supports_images: hasImg,
        supports_reasoning: hasRsn,
        supports_tools: hasTools,
        reasoning_efforts: efforts,
        default_reasoning_effort: efforts[0] || (hasRsn ? "high" : ""),
      });
      renderCustomChList();
      $("customChErr").textContent = "";
      $("customChOk").textContent = `已添加模型 [${mid}]，点击「保存全部配置」生效`;
      return;
    }
  });

  // 模型添加输入框回车快捷添加（整行任一输入框回车 = 点 ➕）
  $("customChList").addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    const addBtn = e.target.closest(".custom-model-add-row")?.querySelector("[data-add-model]");
    if (addBtn) { e.preventDefault(); addBtn.click(); }
  });

  $("btnCustomTest").onclick = async () => {
    const u = $("inCustomURL").value.trim();
    const k = $("inCustomKey").value.trim();
    const name = $("inCustomName").value.trim().toLowerCase();
    if (!u) {
      $("customChErr").textContent = "请先填入 Base URL";
      $("customChOk").textContent = "";
      return;
    }
    const btn = $("btnCustomTest");
    btn.disabled = true;
    const old = btn.textContent;
    btn.textContent = "测试中…";
    $("customChErr").textContent = "";
    $("customChOk").textContent = "";
    try {
      const res = await api("/api/config/custom_channel_test", { name, base_url: u, api_key: k });
      if (res.ok) {
        toast(`连接成功！拉取到 ${res.models?.length || 0} 个模型`);
        $("customChOk").textContent = `连接成功，发现模型: ${(res.models || []).slice(0, 8).join(", ")}${res.models?.length > 8 ? '…' : ''}`;
      } else {
        $("customChErr").textContent = `测试失败：${res.error}`;
      }
    } catch (err) {
      $("customChErr").textContent = `测试失败：${err.message}`;
    } finally {
      btn.disabled = false;
      btn.textContent = old;
    }
  };

  $("btnCustomAdd").onclick = () => {
    if (!$("inCustomName").value.trim() && !$("inCustomURL").value.trim() && !$("inCustomKey").value.trim()) {
      $("customChErr").textContent = "请先填入渠道标识、Base URL 与 API 密钥";
      $("customChOk").textContent = "";
      return;
    }
    const err = addPendingCustomInputs();
    if (err) { $("customChErr").textContent = err; $("customChOk").textContent = ""; return; }
    $("customChErr").textContent = "";
    const added = editCustomChannels[editCustomChannels.length - 1];
    $("customChOk").textContent = `已创建渠道 [${added.name}]，现在可在其下方配置模型 ID、上下文大小、思考等级与能力`;
  };

  $("btnCustomSave").onclick = saveCustomChannels;

  // 点击弹层空白处关闭
  $("settingsOverlay").onclick = (e) => { if (e.target === $("settingsOverlay")) closeSettings(); };
  $("renameOverlay").onclick = (e) => { if (e.target === $("renameOverlay")) closeRename(); };
  $("helpOverlay").onclick = (e) => { if (e.target === $("helpOverlay")) closeHelp(); };

  // 积分明细浮窗的外部关闭路径（✕ / 点浮窗之外 / Esc）：document 级委托，绑定一次。
  // 浮窗与账号卡片都会随时重建，委托不受重建影响。
  bindCreditDetailDismiss();

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

// ---------- 用量与流水面板（变量声明必须在 init 执行前——bindUsage() 在 init 顶部调用） ----------
// usageDays 从 localStorage 恢复上次选择（今日/7日），刷新页面不重置回 7 日。
// 存储键与 TOAST_SEEN_KEY 同族（usage-range-seen）；非法值回退默认 7。
// ⚠️ 本段必须在 init() 之前执行：bindUsage() 在 init 顶部被调用并引用 usageDays，
//    若声明滞后于 init（IIFE 立即执行），let 处于暂时性死区（TDZ）会抛
//    ReferenceError 中断整个 init —— 实测表现：账号卡片/费率表全空白、
//    控制台报 "Cannot access 'usageDays' before initialization"。
const USAGE_DAYS_KEY = "usage-range-seen";
const USAGE_DAYS_VALID = [1, 7];
let usageDays = 7;
try {
  const v = parseInt(localStorage.getItem(USAGE_DAYS_KEY) || "", 10);
  if (USAGE_DAYS_VALID.includes(v)) usageDays = v;
} catch (e) { /* localStorage 不可用时保持默认 */ }
// persistUsageDays 落盘当前区间（不可用存储时静默失败，不阻塞功能）。
function persistUsageDays() {
  try { localStorage.setItem(USAGE_DAYS_KEY, String(usageDays)); } catch (e) { /* 忽略 */ }
}
let usageChart = null;    // token 折线图（echarts 实例）
let creditChart = null;   // 积分消耗折线图（echarts 实例）
let recentAll = [];       // 全量最近流水（分页源，时间升序）
let recentPage = 0;
let lastNames = {};       // 最近一次渲染的 uid→昵称表（翻页时复用）
const RECENT_PAGE_SIZE = 20;
const MODEL_TOP_N = 20;   // 模型榜只保留前 N 名

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

// fmtAxisTokens 坐标轴专用紧凑刻度：万/亿取整不带小数（刻度无需精度，悬浮明细仍用 fmtTokensFull）。
// 「8000.0万」这类长标签会撑爆 50px 网格边距、被画布裁成 0000.0万。
function fmtAxisTokens(n) {
  if (!n) return "0";
  if (n >= 1e8) return Math.round(n / 1e8) + "亿";
  if (n >= 1e4) return Math.round(n / 1e4) + "万";
  return String(Math.round(n));
}

// cacheCell 渲染「缓存利用率」单元格。**优雅降级的三态**（后端约定）：
//   cr_cov > 0 → 有上报样本，显示百分比（可准确到 0%，那是真没命中）
//   cr_cov = 0 → 上游不提供该维度（glm / raccoon 等）→ 显示「—」，**绝不显示 0%**
// 把「命中率为零」与「不知道」区分开是本列的核心要求：后者显示 0% 会误导用户
// 以为换渠道/改提示词就能改善，而实际上是上游根本不回报这个指标。
function cacheCell(r) {
  const cov = Number(r.cr_cov) || 0;
  if (cov <= 0) return `<span class="muted" title="该模型的上游不提供缓存命中字段（本工具不做估算）">—</span>`;
  const hit = Number(r.cr) || 0;
  const pct = (hit / cov) * 100;
  // 样本量少时刻意标低可信度：单次请求的命中率很可能不代表稳态
  const n = Number(r.cr_reqs) || 0;
  const tip = `命中 ${fmtTokensFull(hit)} / 上报请求 prompt ${fmtTokensFull(cov)}（${n} 次上报请求）`;
  const shown = pct >= 10 ? pct.toFixed(1) : pct.toFixed(2);
  return `<span class="cache-pct" title="${esc(tip)}">${shown}%</span>`;
}

// renderUsageCacheCard 汇总卡：无任何上报样本时显示「—」（同样不伪造 0%）
function renderUsageCacheCard(t) {
  const cov = Number(t.cr_cov) || 0;
  const el = $("ucCache");
  if (cov <= 0) { el.textContent = "—"; el.title = "当前窗口内没有渠道上报缓存字段"; return; }
  const pct = ((Number(t.cr) || 0) / cov) * 100;
  el.textContent = (pct >= 10 ? pct.toFixed(1) : pct.toFixed(2)) + "%";
  el.title = `命中 ${fmtTokensFull(t.cr)} / 上报请求 prompt ${fmtTokensFull(cov)}（${t.cr_reqs || 0} 次上报请求）；未上报的渠道不计入分母`;
}

const CH_NAMES = {
  workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork",
  qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", oczen: "OpenCodeZen", kilo: "Kilo", minimax: "MiniMax", lobsterai: "LobsterAI", zcode: "ZCode", gemini: "Gemini", codearts: "CodeArts",
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
  renderUsageCacheCard(st.token || {});
  syncUsageRangeActive(); // 高亮始终跟随 usageDays（含异步返回路径）

  renderModelTable(st.token.by_model || [], usageDays);
  renderCreditTab(st.credit || {});
  renderUsageChart(st);
  renderCreditChart(st.credit || {});
}

// syncUsageRangeActive 把「今日/7日」按钮高亮同步为当前 usageDays。
// 单一入口：初始化、点击切换、异步返回都走它，避免三处 toggle 竞态
// （此前点击后 loadUsage 异步返回时高亮被回写错误的 bug）。
function syncUsageRangeActive() {
  $("usageRange").querySelectorAll(".seg-btn").forEach((x) =>
    x.classList.toggle("active", parseInt(x.dataset.days, 10) === usageDays));
}

function renderModelTable(rows, days) {
  const tb = $("tblModels").querySelector("tbody");
  if (!rows.length) { tb.innerHTML = `<tr><td colspan="7" class="empty-cell">暂无数据（流水自本功能上线后开始记录）</td></tr>`; return; }
  const top = rows.slice(0, MODEL_TOP_N); // 前 20 名，已按 token 降序
  tb.innerHTML = top.map((r) => {
    const avg = days > 1 ? Math.round((r.pt + r.ct) / days) : (r.pt + r.ct);
    return `<tr>
      <td>${esc(r.model)}</td><td><span class="badge ${chClass(r.channel)}">${esc(chLabel(r.channel))}</span></td>
      <td class="num">${fmtCredits(r.requests)}</td><td class="num">${fmtTokensFull(r.pt + r.ct)}</td>
      <td class="num">${fmtTokensFull(avg)}</td>
      <td class="num">${cacheCell(r)}</td><td class="num">${perfCellFor(r.channel, r.model)}</td></tr>`;
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
        <span class="rtime">${fmtDateTime24(r.ts)}</span>
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
  // 渠道系列：取所有出现过的渠道并集，按区间总量降序（主力渠道排在图例最前）
  const totalOf = {};
  const chans = [...new Set(byDay.flatMap((d) => Object.keys(d.by_channel || {})))];
  for (const d of byDay) for (const [ch, v] of Object.entries(d.by_channel || {})) totalOf[ch] = (totalOf[ch] || 0) + v;
  chans.sort((a, b) => (totalOf[b] || 0) - (totalOf[a] || 0));
  const series = chans.map((ch) => ({
    name: CH_NAMES[ch] || ch, type: "line", smooth: true,
    itemStyle: { color: chColor(ch) }, lineStyle: { color: chColor(ch) },
    data: byDay.map((d) => d.by_channel[ch] || 0),
  }));
  usageChart.setOption({
    color: chans.map((ch) => chColor(ch)),
    tooltip: {
      trigger: "axis",
      // 全 0 的时刻点不弹悬浮（避免清晨无用量时一片 0 刷屏）；
      // 有数据时也过滤掉 0 值系列，只看当下真正在跑的渠道；
      // 并按该时刻用量从多到少排序（与图例「主力在前」同一口径，不再按系列声明序）。
      formatter: (params) => {
        const list = params.filter((p) => p.value > 0).sort((a, b) => b.value - a.value);
        if (!list.length) return "";
        const head = `<div style="font-weight:600;margin-bottom:4px">${esc(params[0].axisValue)}</div>`;
        return head + list.map((p) =>
          `<div>${p.marker}${esc(p.seriesName)}：${fmtTokensFull(p.value)}</div>`).join("");
      },
    },
    legend: { type: "scroll", data: series.map((s) => s.name),
      tooltip: { show: true } },
    grid: { left: 8, right: 20, top: 36, bottom: 28, containLabel: true },
    xAxis: { type: "category", data: dates },
    yAxis: { type: "value", axisLabel: { formatter: (v) => fmtAxisTokens(v) } },
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
  // 同渠道多个账号时做颜色变体，保证 10 条线互不相同
  const colorByUid = {};
  const vIdx = {};
  for (const uid of top10) {
    const ch = mainChOf(uid);
    colorByUid[uid] = varyColor(chColor(ch), vIdx[ch] = (vIdx[ch] ?? -1) + 1);
  }
  const series = top10.map((uid) => ({
    name: `${chLabel(mainChOf(uid))} ${names[uid] || shortUid(uid)}`, type: "line", smooth: true,
    itemStyle: { color: colorByUid[uid] }, lineStyle: { color: colorByUid[uid] },
    data: dates.map((d) => byAcct[uid][d] || 0),
  }));
  creditChart.setOption({
    color: top10.map((uid) => colorByUid[uid]),
    tooltip: {
      trigger: "axis", valueFormatter: (v) => fmtCredits(v),
      formatter: (params) => {
        // 同 token 图：悬浮按该时刻消耗从多到少排序，最大的一条在最上面。
        const list = params.filter((p) => p.value > 0).sort((a, b) => b.value - a.value);
        if (!list.length) return "";
        const head = `<div style="font-weight:600;margin-bottom:4px">${esc(params[0].axisValue)}</div>`;
        return head + list.map((p) =>
          `<div>${p.marker}${esc(p.seriesName)}：${fmtCredits(p.value)}</div>`).join("");
      },
    },
    legend: { type: "scroll", data: series.map((s) => s.name),
      tooltip: { show: true } },
    grid: { left: 8, right: 20, top: 36, bottom: 28, containLabel: true },
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

// 主面板 tab 切换（账号管理 / 用量与流水 / 运行统计 / 模型列表和费率）
function bindMainTabs() {
  document.querySelectorAll(".main-tabs .mtab").forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll(".main-tabs .mtab").forEach((x) => x.classList.toggle("active", x === b));
      $("mtabAccounts").classList.toggle("hidden", b.dataset.mtab !== "accounts");
      $("mtabUsage").classList.toggle("hidden", b.dataset.mtab !== "usage");
      $("mtabStats").classList.toggle("hidden", b.dataset.mtab !== "stats");
      $("mtabFees").classList.toggle("hidden", b.dataset.mtab !== "fees");
      // 切换主 tab 强制收起积分明细浮窗，避免残留浮在其他页签上
      closeCreditDetail();
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
      persistUsageDays(); // 记住选择，刷新不重置
      syncUsageRangeActive(); // 单一入口同步高亮
      loadUsage();
    };
  });
  syncUsageRangeActive(); // 初始高亮按恢复的 usageDays（localStorage 命中「今日」不显示 7日 高亮）
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
    renderFallbacks(s.fallbacks || []);
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

// perfLookup 运行统计引擎的「模型 → 速度」索引（key = "channel/model"）。
// 速度数据只存在于 stats 引擎内存（ledger 只扫文件、无速度字段），
// 用量 tab 展示平均速度时按此表匹配；键形与 stats 的 models[].model 一致。
let perfLookup = {};

// perfCellFor 按 "channel/model" 取速度单元格；无样本显示「—」（与运行统计同款三态）。
function perfCellFor(channel, model) {
  const m = perfLookup[channel + "/" + model];
  if (!m) return `<span class="muted" title="该模型暂无成功请求的速度样本">—</span>`;
  const tps = Number(m.avg_tok_per_sec) || 0;
  const ttfb = Number(m.avg_ttfb_ms) || 0;
  const parts = [];
  if (tps > 0) parts.push(fmtPct(tps) + " tok/s");
  if (ttfb > 0) parts.push(fmtCredits(ttfb) + "ms");
  if (!parts.length) return `<span class="muted" title="无速度样本">—</span>`;
  return `<span title="样本 ${fmtCredits(m.perf_samples)} 次成功请求">${parts.join(" · ")}</span>`;
}

// buildPerfLookup 用 stats 快照的 models[] 建索引（30s 轮询时刷新）。
function buildPerfLookup(models) {
  const lk = {};
  for (const m of models || []) {
    if (m && m.model) lk[m.model] = m;
  }
  perfLookup = lk;
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
  buildPerfLookup(s.models || []); // 用量 tab 的速度列数据源（同源同频刷新）
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

// groupUnit 渠道的计量单位（""=积分 / "token"=Token 计数）：从 state.accounts 同渠道账号取，
// 保证与卡片、汇总条、费率页角标**同源**（单一来源仍是后端的 credits_unit）。
function groupUnit(group) {
  const a = (state.accounts || []).find((x) => (x.group || "workbuddy") === group);
  return a && a.credits_unit === "token" ? "token" : "";
}

// 使用中 / 接棒顺序：当前=粘性账号；接棒=临期优先+余额排序推算的前 3
function renderRotation(rot) {
  if (!rot.length) { $("stRotation").innerHTML = ""; $("stRotation").className = "st-rotation"; return; }
  let html = `<div class="st-block-title">使用中 / 接棒顺序<span class="st-hint">当前=粘性账号 · 接棒=临期优先+余额排序推算</span></div>`;
  for (const rv of rot) {
    // 单位随渠道切换：token 计量渠道（如 ZCode）显示 tokens，不再一律写「分」。
    const isTok = groupUnit(rv.group) === "token";
    const bal = (v) => (isTok ? `${fmtTokens(v)} tokens` : `${fmtCredits(v)}分`);
    const cur = rv.current
      ? `使用中 <b>${esc(rv.current.nickname)}</b>（粘性 ${esc(rv.current.sticky || "0")}，余额 ${bal(rv.current.credits)}）`
      : `<span class="st-rot-none">暂无使用记录</span>`;
    let nextHtml = "";
    (rv.next || []).forEach((n, j) => {
      nextHtml += `<span class="st-rot-next">${ST_ROT_MARKS[j]} ${esc(n.nickname)}` +
        `（${bal(n.credits)}${n.expiring > 0 ? "·临期" + bal(n.expiring) : ""}）</span>`;
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
  let mh = `<tr><th>#</th><th>模型</th><th>次数</th><th>Token</th><th>缓存</th><th>积分</th>` +
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
      `<td>${fmtCredits(m.reqs)}</td><td>${fmtTokensFull(m.tokens)}</td>` +
      `<td class="st-mono">${cacheCell(m)}</td>` +
      `<td>${creditCell}</td>` +
      `<td class="st-mono">${perfCell}</td></tr>`;
  });
  $("stModels").innerHTML = models.length ? mh : `<tr><td class="st-empty" colspan="7">今日暂无消耗</td></tr>`;
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

// ---------- 降级记录（原模型无可用账号切备胎的事件流；无事件整块隐藏） ----------
// fmtFbDur 秒数 → 紧凑文案（"42 秒" / "3 分 12 秒" / "1 时 02 分"）
function fmtFbDur(sec) {
  sec = Math.max(0, Math.floor(sec));
  if (sec < 60) return sec + " 秒";
  const m = Math.floor(sec / 60), s = sec % 60;
  if (m < 60) return m + " 分 " + s + " 秒";
  return Math.floor(m / 60) + " 时 " + String(m % 60).padStart(2, "0") + " 分";
}

function renderFallbacks(evs) {
  const block = $("stFbBlock");
  block.hidden = false; // 常显：让用户随时知道有这个区块；无事件显示占位行
  const $t = $("stFbs");
  if (!evs.length) {
    $t.innerHTML = `<tr><td class="st-empty">暂无降级记录——原模型无可用账号切备胎时这里会出现事件（降级/恢复时刻与持续时长）</td></tr>`;
    return;
  }
  let h = `<tr><th>#</th><th>降级时间</th><th>原模型</th><th></th><th>备胎</th><th>恢复</th><th>持续</th></tr>`;
  evs.forEach((e, i) => {
    h += `<tr class="${e.active ? "fb-active" : ""}">` +
      `<td>${i + 1}</td>` +
      `<td class="st-mono">${esc(e.at)}</td>` +
      `<td class="st-mono" title="${esc(e.from)}">${esc(e.from)}</td>` +
      `<td class="fb-arrow">→</td>` +
      `<td class="st-mono" title="${esc(e.to)}">${esc(e.to)}</td>` +
      (e.active
        ? `<td><span class="fb-live">● 降级中</span></td><td><span class="fb-live" data-fb-epoch="${e.at_unix}"></span></td>`
        : `<td class="st-mono">${esc(e.recovered || "—")}</td><td>${fmtFbDur(e.dur_sec)}</td>`) +
      `</tr>`;
  });
  $("stFbs").innerHTML = h;
}

// 进行中事件的「持续」每秒重算（只改文本不重渲染；元素不存在时空转，开销可忽略）
setInterval(() => {
  document.querySelectorAll("[data-fb-epoch]").forEach((el) => {
    el.textContent = "已 " + fmtFbDur((Date.now() - Number(el.dataset.fbEpoch) * 1000) / 1000);
  });
}, 1000);

// 冷却卡片的「剩 X 时 X 分」每 30 秒重算：卡片只在 loadState 事件时重渲染，
// 不定时刷新会让倒计时停在渲染时刻的值，静止几小时后反而误导。
setInterval(() => {
  document.querySelectorAll("[data-until-ts]").forEach((el) => {
    el.textContent = `（${fmtLeft(Number(el.dataset.untilTs))}）`;
  });
}, 30000);

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

