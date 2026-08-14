// WorkBuddy2API 托盘面板逻辑（原生 JS，无构建步骤）
/* global window */
"use strict";

const Go = window.go.app.App;
const rt = window.runtime;

let state = null;
let loginTimer = null;
let loginURL = "";

const $ = (id) => document.getElementById(id);

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}

// 监听主机默认值（host 为空 = 全部接口，展示为本机可达地址）
function displayHost(host) {
  if (!host || host === "0.0.0.0" || host === "::") return "127.0.0.1";
  return host;
}

// ---------------------------------------------------------------------------
// 初始加载与渲染
// ---------------------------------------------------------------------------

async function load() {
  try {
    state = await Go.GetState();
  } catch (e) {
    console.error("GetState failed", e);
    return;
  }
  render();
}

function render() {
  $("ver").textContent = state.version;
  $("serverLine").textContent =
    `http://${displayHost(state.listen_host)}:${state.listen_port} · ${state.running ? "运行中" : "未监听"}`;
  $("acctCount").textContent = `(${state.accounts.length})`;
  $("nextCheckin").textContent = state.next_checkin || "-";
  $("acctEmpty").classList.toggle("hidden", state.accounts.length > 0);
  $("inPort").value = state.listen_port;
  $("chkAutostart").checked = state.autostart;
  renderKey();
  renderHostSelect();
  renderAccounts();
  renderHours();
}

function renderKey() {
  const v = state.api_key;
  $("keyVal").textContent = v ? v : "（未设置）";
  $("keyVal").classList.toggle("muted", !v);
  $("keyBox").classList.remove("hidden");
  $("keyEdit").classList.add("hidden");
}

function editKey() {
  $("keyInput").value = state.api_key || "";
  $("keyInput").dataset.orig = state.api_key || "";
  $("keyBox").classList.add("hidden");
  $("keyEdit").classList.remove("hidden");
  $("keyInput").focus();
}

function commitKey() {
  const key = $("keyInput").value.trim();
  if (key === $("keyInput").dataset.orig) { renderKey(); return; }
  Go.SetAPIKey(key).then(() => {
    state.api_key = key;
    renderKey();
    toast("API-Key 已保存");
  }).catch((e) => { toast("保存失败：" + e); renderKey(); });
}

function cancelKey() { renderKey(); }

function renderHostSelect() {
  const sel = $("selHost");
  const host = state.listen_host || "";
  if (host === "127.0.0.1") sel.value = "127.0.0.1";
  else if (host === "0.0.0.0" || host === "") sel.value = "0.0.0.0";
  else {
    sel.value = "__custom__";
    $("inHost").value = host;
    $("customHostRow").classList.remove("hidden");
  }
}

function renderAccounts() {
  const box = $("acctList");
  if (!state || !state.accounts) return;
  box.innerHTML = state.accounts.map((a) => {
    const name = a.nickname || a.uid;
    const uid = a.uid.length > 14 ? a.uid.slice(0, 14) + "…" : a.uid;
    const credits = a.credits ? Number(a.credits).toLocaleString() : "0";
    const status = accountStatus(a);
    return `<div class="acct">
      <div class="acct-main">
        <span class="acct-name" title="${esc(name)}">${esc(name)}</span>
        <span class="acct-uid">${esc(uid)}</span>
        <span class="acct-credits">${credits}</span>
      </div>
      <div class="acct-status ${status.cls}">${esc(status.txt)}</div>
      <div class="acct-actions">
        <button class="btn" data-action="checkin" data-uid="${esc(a.uid)}">签到</button>
        <button class="btn" data-action="refresh" data-uid="${esc(a.uid)}">刷新</button>
        <button class="btn danger" data-action="remove" data-uid="${esc(a.uid)}">删除</button>
      </div>
    </div>`;
  }).join("");
}

function accountStatus(a) {
  if (a.disabled) return { cls: "err", txt: "已禁用（session 失效，需重新登录）" };
  if (a.cooling) {
    const until = a.until ? ` 至 ${a.until}` : "";
    return { cls: "warn", txt: `冷却中（${a.reason || "余额不足"}${until}）` };
  }
  if (a.last_checkin_at) {
    const icon = a.last_checkin_ok ? "✓" : "✗";
    const cls = a.last_checkin_ok ? "ok" : "err";
    return { cls, txt: `${icon} ${a.last_checkin_at} 签到：${a.last_checkin_msg || (a.last_checkin_ok ? "成功" : "失败")}` };
  }
  return { cls: "", txt: "尚未签到" };
}

function renderHours() {
  const box = $("hoursBox");
  box.innerHTML = (state.checkin_hours || []).map((h, i) => {
    const v = String(h).padStart(2, "0") + ":00";
    return `<span class="hour-chip">
      <input type="time" value="${v}" step="3600">
      <button class="del" data-i="${i}" title="删除">×</button>
    </span>`;
  }).join("");
  // 变更即生效（无保存按钮）
  box.querySelectorAll("input[type=time]").forEach((inp) => {
    inp.onchange = commitHours;
  });
  box.querySelectorAll(".del").forEach((btn) => {
    btn.onclick = () => {
      state.checkin_hours.splice(Number(btn.dataset.i), 1);
      renderHours();
      commitHours();
    };
  });
}

function collectHours() {
  const out = [];
  $("hoursBox").querySelectorAll("input[type=time]").forEach((inp) => {
    const m = /^(\d{1,2}):/.exec(inp.value || "");
    if (m) out.push(parseInt(m[1], 10));
  });
  return out;
}

function commitHours() {
  const hours = collectHours();
  if (hours.length === 0) { toast("至少保留一个时间"); renderHours(); return; }
  Go.SetCheckinHours(hours).then(() => {
    state.checkin_hours = hours.sort((a, b) => a - b);
    toast("签到时间已保存");
  }).catch((e) => { toast("保存失败：" + e); renderHours(); });
}

// 监听地址（主机 + 端口）变更即生效
function commitListen() {
  let host = $("selHost").value;
  if (host === "__custom__") {
    host = $("inHost").value.trim();
    if (!host) { toast("请输入自定义主机"); render(); return; }
  }
  const port = parseInt($("inPort").value, 10);
  if (!port || port < 1 || port > 65535) { toast("端口无效"); render(); return; }
  if (host === state.listen_host && port === state.listen_port) return; // 未变化
  Go.SetListen(host, port).then(() => {
    state.listen_host = host;
    state.listen_port = port;
    state.running = true;
    render();
    toast("监听已生效");
  }).catch((e) => {
    toast("切换失败：" + e);
    render(); // 回退到当前状态
  });
}

// ---------------------------------------------------------------------------
// Toast
// ---------------------------------------------------------------------------

let toastTimer = null;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 2600);
}

// ---------------------------------------------------------------------------
// 登录流程
// ---------------------------------------------------------------------------

async function startLogin() {
  let url = "";
  try {
    url = await Go.StartLogin();
  } catch (e) {
    toast("发起登录失败：" + e);
    return;
  }
  loginURL = url;
  $("loginOverlay").classList.remove("hidden");
  $("loginMsg").textContent = "已打开无痕浏览器，请在浏览器中完成登录…";
  let remain = 300;
  $("loginCountdown").textContent = `剩余 ${Math.floor(remain / 60)}:${String(remain % 60).padStart(2, "0")}`;
  clearInterval(loginTimer);
  loginTimer = setInterval(() => {
    remain -= 1;
    if (remain < 0) { clearInterval(loginTimer); return; }
    $("loginCountdown").textContent = `剩余 ${Math.floor(remain / 60)}:${String(remain % 60).padStart(2, "0")}`;
  }, 1000);
}

function closeLoginOverlay() {
  clearInterval(loginTimer);
  loginTimer = null;
  $("loginOverlay").classList.add("hidden");
}

function handleLoginEvent(e) {
  $("loginMsg").textContent = e.msg || "";
  if (e.phase === "success" || e.phase === "failed" || e.phase === "cancelled") {
    closeLoginOverlay();
    if (e.phase === "success") toast(e.msg);
    load();
  }
}

async function copyLoginURL() {
  if (!loginURL) return;
  try {
    await rt.ClipboardSetText(loginURL);
    toast("授权链接已复制");
  } catch (e) {
    try {
      await navigator.clipboard.writeText(loginURL);
      toast("授权链接已复制");
    } catch (e2) {
      toast("复制失败：" + e2);
    }
  }
}

// ---------------------------------------------------------------------------
// 事件绑定
// ---------------------------------------------------------------------------

function bind() {
  $("btnHide").onclick = () => Go.HidePanel();
  $("btnAdd").onclick = startLogin;
  $("btnCopyUrl").onclick = copyLoginURL;
  $("btnCancelLogin").onclick = async () => {
    await Go.CancelLogin();
    closeLoginOverlay();
  };
  $("btnQuit").onclick = () => Go.Quit();
  $("btnLog").onclick = async () => {
    try {
      await Go.OpenLogFile();
      toast("已用记事本打开日志");
    } catch (e) { toast("打开日志失败：" + e); }
  };

  // API-Key：点击明文值进入编辑，blur/Enter 即生效，Esc 取消
  $("keyVal").onclick = editKey;
  $("keyInput").addEventListener("blur", commitKey);
  $("keyInput").addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); commitKey(); }
    if (e.key === "Escape") cancelKey();
  });

  // 自动签到时间：变更即生效；增加时间
  $("btnAddHour").onclick = () => {
    if ((state.checkin_hours || []).length >= 4) { toast("最多 4 个时间"); return; }
    state.checkin_hours.push(9);
    renderHours();
    commitHours();
  };

  // API 监听：选择/失焦即生效
  $("selHost").onchange = () => {
    $("customHostRow").classList.toggle("hidden", $("selHost").value !== "__custom__");
    if ($("selHost").value === "__custom__") {
      $("inHost").focus();
    } else {
      commitListen();
    }
  };
  $("inPort").addEventListener("change", commitListen);
  $("inPort").addEventListener("blur", commitListen);
  $("inHost").addEventListener("change", commitListen);

  $("chkAutostart").onchange = async (e) => {
    try { await Go.SetAutostart(e.target.checked); }
    catch (err) { toast("设置失败：" + err); e.target.checked = !e.target.checked; }
  };

  $("btnCheckinAll").onclick = async () => {
    $("btnCheckinAll").disabled = true;
    try {
      const res = await Go.CheckinAll();
      const ok = res.filter((r) => r.ok).length;
      toast(`签到完成：成功 ${ok}/${res.length}`);
    } catch (e) { toast("签到失败：" + e); }
    finally { $("btnCheckinAll").disabled = false; }
  };

  $("btnRefreshAll").onclick = async () => {
    $("btnRefreshAll").disabled = true;
    try { await Go.RefreshAll(); toast("积分已刷新"); }
    finally { $("btnRefreshAll").disabled = false; }
  };

  $("acctList").onclick = async (e) => {
    const btn = e.target.closest("button[data-action]");
    if (!btn) return;
    const uid = btn.dataset.uid;
    const action = btn.dataset.action;
    btn.disabled = true;
    try {
      if (action === "checkin") {
        await Go.CheckinAccount(uid);
        toast("签到完成");
      } else if (action === "refresh") {
        const remain = await Go.RefreshCredits(uid);
        toast(`积分 ${Number(remain).toLocaleString()}`);
      } else if (action === "remove") {
        if (confirm("删除账号 " + uid + "？\n（auth 文件将一并删除）")) {
          await Go.RemoveAccount(uid);
          toast("账号已删除");
        }
      }
    } catch (err) { toast("操作失败：" + err); }
    finally { btn.disabled = false; }
  };

  // 失焦自动隐藏（点击面板外部 / 托盘图标时收起）
  // 防抖 + 宽限期：面板刚显示时 WebView 焦点竞态（hasFocus 短暂为 false）
  // 会触发误隐藏，导致“一闪而过”；显示后 400ms 内的失焦一律忽略。
  let lastShownAt = 0;
  let hideTimer = null;
  const onShown = () => {
    lastShownAt = Date.now();
    // 重触发内容上浮动效
    const el = $("app");
    el.classList.remove("pop");
    void el.offsetWidth;
    el.classList.add("pop");
  };
  rt.EventsOn("panel:shown", onShown);
  window.addEventListener("focus", () => { lastShownAt = Date.now(); });
  window.addEventListener("blur", () => {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => {
      if (Date.now() - lastShownAt < 400) return; // 显示宽限期
      if (!document.hasFocus()) Go.HidePanel();
    }, 180);
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") Go.HidePanel();
  });

  // 后端事件
  rt.EventsOn("accounts", (accts) => {
    if (!state) return;
    state.accounts = accts;
    renderAccounts();
  });
  rt.EventsOn("login", (e) => handleLoginEvent(e));
}

document.addEventListener("DOMContentLoaded", () => {
  bind();
  load();
});
