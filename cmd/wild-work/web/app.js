// wild-work 管理面板前端（原生 JS，无构建步骤，直接 fetch 管理 API）
"use strict";

const $ = (id) => document.getElementById(id);

// ---------- API 封装 ----------
async function api(path, body) {
  const opts = { method: "GET", headers: { "Content-Type": "application/json" } };
  if (body !== undefined) {
    opts.method = "POST";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    throw new Error(data.error || ("请求失败 " + resp.status));
  }
  return data;
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
    toast("费率已刷新");
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshFees").disabled = false;
  }
}

// ---------- 积分明细 tooltip ----------
let detailTimer = null;
let detailCache = {};

async function showCreditDetail(e, uid) {
  const el = e.currentTarget;
  if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; }

  let d = detailCache[uid];
  if (!d) {
    try {
      d = await api("/api/account/resource_detail", { uid });
      detailCache[uid] = d;
    } catch (err) { return; }
  }
  if (!d || !d.items || !d.items.length) return;

  let html = `<table class="detail-table"><thead><tr><th>套餐</th><th>总额</th><th>已用</th><th>剩余</th></tr></thead><tbody>`;
  for (const it of d.items) {
    html += `<tr><td>${esc(it.name)}</td><td>${it.total}</td><td>${it.used}</td><td>${it.remain}</td></tr>`;
  }
  html += `</tbody></table>`;

  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }
  tip.innerHTML = html;
  tip.style.display = "block";

  const rect = el.getBoundingClientRect();
  let left = rect.left, top = rect.bottom + 4;
  if (top + 200 > window.innerHeight) top = rect.top - 200;
  if (left + 280 > window.innerWidth) left = window.innerWidth - 290;
  tip.style.left = left + "px";
  tip.style.top = top + "px";
}

function hideCreditDetail() {
  detailTimer = setTimeout(() => {
    const tip = $("creditTip");
    if (tip) tip.style.display = "none";
  }, 200);
  const tip = $("creditTip");
  if (tip) {
    tip.onmouseenter = () => { if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; } };
    tip.onmouseleave = () => { tip.style.display = "none"; };
  }
}

// ---------- 渲染 ----------
function render() {
  renderTopbar();
  renderAccounts();
  renderTimes();
}

function renderTopbar() {
  $("ver").textContent = "v" + state.version;
  $("serverLine").textContent = state.running ? "服务运行中" : "服务未启动";
  $("aboutVer").textContent = state.version;

  const host = (state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::")
    ? "127.0.0.1" : state.listen_host;
  const apiURL = `http://${host}:${state.listen_port}/v1`;
  $("apiAddr").textContent = apiURL;

  const key = state.api_key;
  $("apiKeyDisplay").textContent = key === "" ? "（无鉴权）" : key;
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
    const group = a.group === "traework" ? "trae" : (a.group === "qoder" ? "qoder" : "wb");
    const groupName = a.group === "traework" ? "TraeWork" : (a.group === "qoder" ? "Qoder" : "WorkBuddy");
    const noCheckin = a.group === "qoder"; // Qoder 无签到活动，签到按钮灰掉

    const checkinTag = a.last_checkin_at
      ? `<span class="tag ${a.last_checkin_ok ? "ok" : "bad"}">${a.last_checkin_ok ? "签到成功" : "签到失败"}</span>`
      : (noCheckin ? '<span class="tag neutral">无签到</span>' : '<span class="tag neutral">未签到</span>');

    const disabledClass = a.disabled ? " disabled" : "";
    const disableIcon = a.disabled ? "▶" : "⏸";
    const disableTitle = a.disabled ? "启用" : "停用";
    const checkinBtn = noCheckin
      ? `<span class="icon-op off" title="Qoder 无签到活动" onclick="return false">✓</span>`
      : `<span class="icon-op" title="签到" onclick="checkin('${a.uid}')">✓</span>`;

    return `
    <div class="acct-card${disabledClass}">
      <div class="acct-top">
        <div>
          <span class="badge ${group}">${groupName}</span>
          <span class="acct-name">${esc(a.nickname || shortUid(a.uid))}</span>
        </div>
        <div class="acct-ops">
          ${checkinBtn}
          <span class="icon-op" title="刷新积分" onclick="refreshOne('${a.uid}')">↻</span>
          <span class="icon-op warn" title="${disableTitle}" onclick="toggleDisable('${a.uid}',${a.disabled})">${disableIcon}</span>
          <span class="icon-op danger" title="删除账号" onclick="removeAcct('${a.uid}')">✕</span>
        </div>
      </div>
      <div class="acct-uid">UID: ${esc(shortUid(a.uid))}</div>
      <div class="acct-mid">
        <div class="acct-credits" onmouseenter="showCreditDetail(event,'${a.uid}')" onmouseleave="hideCreditDetail()">${a.credits}<span>积分</span></div>
        <div class="acct-checkin">${checkinTag}</div>
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

function renderFees(fees) {
  const box = $("feesBox");
  const channels = fees.channels || [];

  if (channels.length === 0) {
    box.innerHTML = `<div class="note">${esc(fees.note || "")}</div>
      <div class="note">${esc(fees.disclaimer || "")}</div>`;
    return;
  }

  let html = `<div class="note">${esc(fees.note || "")}</div>`;
  if (fees.cached_at) html += `<div class="note">上次更新：${esc(fees.cached_at)}</div>`;
  if (fees.error) html += `<div class="note" style="color:var(--danger)">${esc(fees.error)}</div>`;

  html += `<table><thead><tr><th>模型</th><th>倍率</th><th>模型</th><th>倍率</th></tr></thead><tbody>`;

  for (const ch of channels) {
    const chName = ch.channel === "traework" ? "TraeWork" : (ch.channel === "qoder" ? "Qoder" : "WorkBuddy");
    const chClass = ch.channel === "traework" ? "trae" : (ch.channel === "qoder" ? "qoder" : "wb");
    html += `<tr class="ch-header ${chClass}"><td colspan="4">${esc(chName)}</td></tr>`;

    const models = ch.models || [];
    // 每行两个模型
    for (let i = 0; i < models.length; i += 2) {
      const m1 = models[i];
      const m2 = models[i + 1];
      const c1 = m1 ? `<code>${esc(m1.model)}</code>` : "";
      const r1 = m1 ? (m1.rate ? `x${m1.rate.toFixed(2)}` : "auto") : "";
      const c2 = m2 ? `<code>${esc(m2.model)}</code>` : "";
      const r2 = m2 ? (m2.rate ? `x${m2.rate.toFixed(2)}` : "-") : "";
      html += `<tr><td>${c1}</td><td>${r1}</td><td>${c2}</td><td>${r2}</td></tr>`;
    }
  }

  html += `</tbody></table>`;
  html += `<div class="note" style="margin-top:8px">${esc(fees.disclaimer || "")}</div>`;
  box.innerHTML = html;
}

// ---------- 账号操作 ----------
async function checkin(uid) {
  try {
    const r = await api("/api/account/checkin", { uid });
    toast(r.ok ? `签到成功：${r.msg}（剩余 ${r.remain}）` : `签到：${r.msg}`);
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
    const skip = (state.accounts || []).filter((a) => a.group === "qoder").length;
    const total = (r.results || []).length + skip;
    toast(skip ? `批量签到完成：成功 ${ok} / ${total}（Qoder ${skip} 个无签到跳过）` : `批量签到完成：成功 ${ok} / 共 ${total}`);
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
function promptLogin(channel) {
  pendingChannel = channel;
  const name = channel === "traework" ? "TraeWork" : (channel === "qoder" ? "Qoder" : "WorkBuddy");
  $("lcTitle").textContent = "添加 " + name + " 账号";
  $("lcMsg").textContent = channel === "qoder"
    ? `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。（${name} 渠道无签到活动，仅 API 转发）`
    : `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。`;
  $("btnLoginConfirm").textContent = "登录" + name;
  $("loginConfirmOverlay").classList.remove("hidden");
}
function confirmLogin() {
  $("loginConfirmOverlay").classList.add("hidden");
  if (pendingChannel) startLogin(pendingChannel);
}

async function startLogin(channel) {
  try {
    const r = await api("/api/login/start", { channel });
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = channel === "traework" ? "添加 TraeWork 账号" : (channel === "qoder" ? "添加 Qoder 账号" : "添加 WorkBuddy 账号");
    $("loginMsg").textContent = "请在浏览器新窗口中完成登录…";
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

// ---------- 签到时间 ----------
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

// ---------- 开机自启 ----------
async function toggleAutostart() {
  try {
    await api("/api/config/autostart", { on: $("chkAutostart").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}

// ---------- API 配置弹层 ----------
function openApiConfig() {
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
  $("apiConfigOverlay").classList.remove("hidden");
}

function closeApiConfig() {
  $("apiConfigOverlay").classList.add("hidden");
}

async function saveApiConfig() {
  let host = $("selHost").value;
  if (host === "__custom__") host = $("inHost").value.trim() || "127.0.0.1";
  const port = parseInt($("inPort").value, 10);
  try {
    await api("/api/config/listen", { host, port });
    toast("监听已切换");
    closeApiConfig();
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- API Key 弹层 ----------
function openApiKey() {
  $("keyInput").value = state.api_key;
  $("apiKeyOverlay").classList.remove("hidden");
  $("keyInput").focus();
}

function closeApiKey() {
  $("apiKeyOverlay").classList.add("hidden");
}

async function saveApiKey() {
  const key = $("keyInput").value.trim();
  try {
    await api("/api/config/api_key", { key });
    closeApiKey();
    toast("API-Key 已更新");
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 帮助/关于弹层 ----------
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
  $("btnAddTrae").onclick = () => promptLogin("traework");
  $("btnAddQoder").onclick = () => promptLogin("qoder");
  $("btnCheckinAll").onclick = checkinAll;
  $("btnRefreshAll").onclick = refreshAll;
  $("btnAddTime").onclick = addTime;
  $("btnCopyUrl").onclick = copyUrl;
  $("btnCancelLogin").onclick = cancelLogin;
  $("btnRefreshFees").onclick = refreshFees;
  $("chkAutostart").onchange = toggleAutostart;

  $("apiAddr").onclick = openApiConfig;
  $("apiKeyDisplay").onclick = openApiKey;

  $("btnHelp").onclick = openHelp;
  $("btnAbout").onclick = openAbout;
  $("btnHelpClose").onclick = closeHelp;
  $("btnAboutClose").onclick = closeAbout;

  // 登录确认弹层
  $("btnLoginConfirm").onclick = confirmLogin;
  $("btnLoginConfirmCancel").onclick = () => $("loginConfirmOverlay").classList.add("hidden");
  $("loginConfirmOverlay").onclick = (e) => { if (e.target === $("loginConfirmOverlay")) $("loginConfirmOverlay").classList.add("hidden"); };

  $("btnApiSave").onclick = saveApiConfig;
  $("btnApiCancel").onclick = closeApiConfig;
  $("selHost").onchange = () => {
    $("customHostRow").classList.toggle("hidden", $("selHost").value !== "__custom__");
  };

  $("btnKeySave").onclick = saveApiKey;
  $("btnKeyCancel").onclick = closeApiKey;
  $("keyInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveApiKey(); });

  // 点击弹层空白处关闭
  $("apiConfigOverlay").onclick = (e) => { if (e.target === $("apiConfigOverlay")) closeApiConfig(); };
  $("apiKeyOverlay").onclick = (e) => { if (e.target === $("apiKeyOverlay")) closeApiKey(); };
  $("helpOverlay").onclick = (e) => { if (e.target === $("helpOverlay")) closeHelp(); };
  $("aboutOverlay").onclick = (e) => { if (e.target === $("aboutOverlay")) closeAbout(); };
}

// ---------- 初始化 ----------
(async function init() {
  bind();
  try {
    await loadState();
    await loadFees();
  } catch (e) {
    toast("无法连接后台服务：" + e.message);
  }
})();