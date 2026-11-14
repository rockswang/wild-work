// E2E suite: fallback-primary-overlap (spec: .scratch/fallback-primary-overlap/spec.md)
//
// Every story below is written from the USER's point of view: open the panel, click/type
// what a person would click/type, then assert what they can observe (visible candidates,
// chips, toast texts) and what the real backend then did (config.json + hot-applied state).
// The panel is the shipping app.js (not a stub) and the backend is the shipping Go daemon,
// so these stories cover the wiring that unit tests cannot: panel ↔ backend agreement.
//
// Run: node scripts/e2e/fallback-primary-overlap.mjs
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { spawn } from "node:child_process";
import { createSuite, sameRules, normalizeRules } from "./lib/harness.mjs";
import { REPO_ROOT, TEST_CHANNELS } from "./lib/backend.mjs";

// Model fixtures: always present in `/api/fees` (custom channels are static, no upstream).
const A = "demoa/alpha";   // used as a rule's primary in most stories
const B = "demoa/beta";    // ditto
const C = "demob/gamma";
const D = "demob/delta";
const E = "demoa/epsilon";
const F = "demob/zeta";

const rules = (...rs) => rs.map(([from, to]) => ({ from, to: Array.isArray(to) ? to : [to] }));
const cfg = (seed) => ({ custom_channels: TEST_CHANNELS, model_fallback: seed });
const has = (list, id) => list.includes(id);

const suite = createSuite({ name: "fallback-primary-overlap — panel ⇄ backend E2E" });

// ---------------------------------------------------------------------------
// Story 1: add a model that is another rule's primary as a fallback, and keep it
// ---------------------------------------------------------------------------
suite.story("US-01", "主力模型可作为另一条规则的备胎（面板 + 保存）", async ({ p, be, check }) => {
  p.openSettings();
  // Rule 1's candidates must offer rule 2's primary (B); add it the way a user would.
  check("备胎候选里能点到另一条规则主力", has(p.menuIdsOfInline(0, "demoa"), B), p.menuIdsOfInline(0, "demoa").join(","));
  p.typeInlineAndClickPlus(0, B);
  check("追加后立刻成为规则 1 的备胎", p.chipsOf(0).includes(B), p.chipsOf(0).join(","));

  const saved = await p.save();
  check("保存无报错", saved.fbErr === "", saved.fbErr);
  check("后端热生效并回读该重叠", (await be.state()).model_fallback[0].to.includes(B));
  check("落盘配置含该重叠", be.configOnDisk().model_fallback[0].to.includes(B));
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 2: other rules' primaries are visible in the fallback dropdown
// ---------------------------------------------------------------------------
suite.story("US-02", "备胎候选下拉直接列出其他规则的主力", async ({ p, check }) => {
  p.openSettings();
  const candidatesForRule2 = p.menuIdsOfInline(1, "demoa");
  check("规则 2 的候选含规则 1 的主力 A", has(candidatesForRule2, A), candidatesForRule2.join(","));
  check("规则 2 的候选不含自己的主力 B", !has(candidatesForRule2, B), candidatesForRule2.join(","));
  const candidatesForRule1 = p.menuIdsOfInline(0, "demoa");
  check("规则 1 的候选不含自己的主力 A", !has(candidatesForRule1, A), candidatesForRule1.join(","));
  check("规则 1 的候选含规则 2 的主力 B", has(candidatesForRule1, B), candidatesForRule1.join(","));
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 3: typing an "other rule's primary" id by hand is accepted, not dropped
// ---------------------------------------------------------------------------
suite.story("US-03", "手动输入恰好是其他规则主力的备胎 ID 被接受", async ({ p, check }) => {
  p.openSettings();
  const res = p.addRuleViaForm(D, B); // B is rule 2's primary
  check("没有报错", res.fbErr === "", res.fbErr);
  const added = res.rules.find((r) => r.from === D);
  check("新规则保留手输的重叠备胎 B", Boolean(added) && added.to.includes(B), JSON.stringify(res.rules));
});

// ---------------------------------------------------------------------------
// Story 4: opening the panel keeps an existing overlap entry (no silent rewrite)
// ---------------------------------------------------------------------------
suite.story("US-04", "打开面板时已有的重叠条目原样保留", async ({ p, check }) => {
  p.openSettings();
  check("重叠条目仍在规则 1 备胎链里，顺序不变", sameRules(p.chipsOf(0), [B, C]), p.chipsOf(0).join(","));
  check("没弹出「已自动剔除」提示", p.toast() === "", p.toast());
  check("清理没有把配置标成「未保存」", p.dirty() === "none", p.dirty());
}, { config: cfg(rules([A, [B, C]], [B, D])) });

// ---------------------------------------------------------------------------
// Story 5: saving an overlapping config succeeds (backend accepts, hot-applied)
// ---------------------------------------------------------------------------
suite.story("US-05", "保存「某模型既是主力、又是另一规则备胎」成功", async ({ p, be, check }) => {
  p.openSettings();
  p.pickInlineFromMenu(0, B, "demoa"); // fallback B == rule 2's primary
  const saved = await p.save();
  check("面板无错误提示", saved.fbErr === "", saved.fbErr);
  check("提示为保存成功", /已保存/.test(saved.toast), saved.toast);
  const state = await be.state();
  check("后端状态原样保存两条规则", sameRules(state.model_fallback, saved.rules), JSON.stringify(state.model_fallback));
  check("config.json 与面板一致", sameRules(be.configOnDisk().model_fallback, saved.rules));
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 6: a rule's own primary is still refused as its own fallback
// ---------------------------------------------------------------------------
suite.story("US-06", "规则自己的主力不能当它自己的备胎", async ({ p, check }) => {
  p.openSettings();
  const before = p.chipsOf(0);
  p.typeInlineAndEnter(0, A);
  check("回车型手动输入自身主力不追加", JSON.stringify(p.chipsOf(0)) === JSON.stringify(before), p.chipsOf(0).join(","));
  p.typeInlineAndClickPlus(0, A);
  check("＋ 型追加自身主力不追加", JSON.stringify(p.chipsOf(0)) === JSON.stringify(before), p.chipsOf(0).join(","));
  const res = p.addRuleViaForm(E, E); // E is not a primary yet, so the only error can be the self-loop
  check("新规则表单给出明确拒绝", res.fbErr !== "" && /自环|主力/.test(res.fbErr), res.fbErr);
  check("拒绝后没有新增规则", res.rules.length === 2, String(res.rules.length));
  check("候选里看不到自身主力", !has(p.menuIdsOfInline(0, "demoa"), A), p.menuIdsOfInline(0, "demoa").join(","));
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 7: the "auto removed" message only mentions self-loops
// ---------------------------------------------------------------------------
suite.story("US-07", "自动剔除提示只提自环、不提「与主力模型互斥」", async ({ p, check }) => {
  p.openSettings();
  // Legacy/hand-edited dirty state: a rule whose own primary sits in its own chain.
  p.T.__setFbRules(rules([A, [A, C]], [B, [B, D]]));
  p.T.renderFbList();
  const msg = p.toast();
  check("提示说明剔除的是自环", /自环/.test(msg), msg);
  check("提示出现具体条数（2 个）", /2 个/.test(msg), msg);
  check("提示不再出现「与主力模型互斥」", !/互斥/.test(msg), msg);
  // The new-rule form's pre-filter message must speak the same language.
  const res = p.addRuleViaForm(E, [E, D]);
  check("新规则预过滤提示同样只提自环", /自环/.test(p.toast()) && !/互斥/.test(p.toast()), p.toast());
  check("非自环备胎被保留", sameRules(res.rules.find((r) => r.from === E), { from: E, to: [D] }), JSON.stringify(res.rules));
});

// ---------------------------------------------------------------------------
// Story 8: the ? help bubble documents the capability
// ---------------------------------------------------------------------------
suite.story("US-08", "? 帮助说明「重叠合法、同规则自环除外」", async ({ check }) => {
  const html = readFileSync(join(REPO_ROOT, "cmd", "wild-work", "web", "index.html"), "utf8");
  check("帮助气泡有正面说明",
    html.includes("同一模型可既是某规则主力、又作为其他规则的备胎；同规则自环除外"));
  check("帮助气泡不再暗示「主力不能作备胎」",
    !/主力[^。<]{0,24}(不能|不得|不可|禁止)[^。<]{0,12}备胎/.test(html));
}, { backend: false });

// ---------------------------------------------------------------------------
// Story 9: the new-rule form excludes only the primary being typed
// ---------------------------------------------------------------------------
suite.story("US-09", "新规则表单只排除当前输入的主力自身", async ({ p, check }) => {
  p.openSettings();
  p.el("fbFromInput").value = C; // the rule being prepared would use C as its primary
  const candidates = p.menuIdsOfBottomTo("demo");
  check("排除刚输入的主力 C", !has(candidates, C), candidates.join(","));
  check("仍可选已有规则 1 的主力 A", has(candidates, A), candidates.join(","));
  check("仍可选已有规则 2 的主力 B", has(candidates, B), candidates.join(","));
  check("仍可选普通备胎 D", has(candidates, D), candidates.join(","));
  p.el("fbFromInput").value = A; // changing the primary re-evaluates the exclusion live
  const after = p.menuIdsOfBottomTo("demo");
  check("改主力后候选即时同步（只排 A）", !has(after, A) && has(after, B) && has(after, C), after.join(","));
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 10: dropdown pick / ＋ / Enter / new-rule form behave identically
// ---------------------------------------------------------------------------
suite.story("US-10", "四条追加路径行为一致（下拉点选 / ＋ / 回车 / 新规则表单）", async ({ p, check }) => {
  p.openSettings();
  const viaDrop = p.pickInlineFromMenu(0, B, "demoa");
  check("下拉点选追加了规则 1 的主力 B", viaDrop.includes(B), viaDrop.join(","));
  const viaPlus = p.typeInlineAndClickPlus(0, E);
  check("＋ 按钮追加了规则 3 的主力 E", viaPlus.includes(E), viaPlus.join(","));
  const viaEnter = p.typeInlineAndEnter(0, F);
  check("回车追加了规则 4 的主力 F", viaEnter.includes(F), viaEnter.join(","));  const viaForm = p.addRuleViaForm(C, A);
  check("新规则表单接受其他规则主力 A 作备胎",
    sameRules((viaForm.rules.find((r) => r.from === C) || {}).to, [A]), JSON.stringify(viaForm.rules));
  check("四条路径都没有报错", viaForm.fbErr === "" && p.fbErr() === "", `${viaForm.fbErr} / ${p.fbErr()}`);
}, { config: cfg(rules([A, D], [B, C], [E, F], [F, C])) });

// ---------------------------------------------------------------------------
// Story 11: loops (A→B and B→A) are allowed and persist
// ---------------------------------------------------------------------------
suite.story("US-11", "环路 A→B 与 B→A 都被允许并落盘", async ({ p, be, check }) => {
  p.openSettings();
  const res = p.addRuleViaForm(A, B); // B→A already exists in the seed
  check("面板接受构成环路的规则", res.fbErr === "", res.fbErr);
  const saved = await p.save();
  check("保存成功", saved.fbErr === "" && /已保存/.test(saved.toast), saved.toast);
  const persisted = (await be.state()).model_fallback;
  check("后端同时保存 A→B 与 B→A",
    sameRules(persisted, [{ from: B, to: [A] }, { from: A, to: [B] }]), JSON.stringify(persisted));
  check("磁盘配置同样含两条", sameRules(be.configOnDisk().model_fallback, persisted));
}, { config: cfg(rules([B, A])) });

// ---------------------------------------------------------------------------
// Story 12: a hand-edited self-loop is auto-removed with an accurate message
// ---------------------------------------------------------------------------
suite.story("US-12", "手改出的自环被自动剔除且提示准确", async ({ p, check }) => {
  p.openSettings();
  // Simulates what a hand edit / stale state produces: from == to inside one rule.
  p.T.__setFbRules(rules([A, [A, C]], [B, [D]]));
  p.T.renderFbList();
  const msg = p.toast();
  check("提示条数与剔除数一致（1 个）", /已自动剔除 1 个自环备胎/.test(msg), msg);
  check("提示给出判据（备胎不能等于本规则的主力）", /备胎不能等于本规则的主力/.test(msg), msg);
  check("自环已从备胎链消失", !p.chipsOf(0).includes(A), p.chipsOf(0).join(","));
  check("同规则的其他备胎保留", p.chipsOf(0).includes(C), p.chipsOf(0).join(","));
  check("别的规则完全不受影响", sameRules(p.chipsOf(1), [D]), p.chipsOf(1).join(","));
}, { config: cfg(rules([A, C])) });

// ---------------------------------------------------------------------------
// Story 13: one truth — the payload the panel sends is accepted verbatim
// ---------------------------------------------------------------------------
suite.story("US-13", "面板与后端只有一个真相（面板载荷后端原样接受）", async ({ p, be, check }) => {
  p.openSettings();
  p.typeInlineAndClickPlus(0, B);          // overlap: B is rule 2's primary
  p.addRuleViaForm(C, [B, A]);             // two more overlaps in a brand-new rule
  const payload = normalizeRules(p.T.__getFbRules());
  const resp = await be.call("/api/config/model_fallback", { rules: payload });
  check("后端接受面板载荷（200）", resp.status === 200, JSON.stringify(resp.data));
  check("没有任何规则被后端丢弃", sameRules(resp.data.rules || payload, payload), JSON.stringify(resp.data));
  const stored = await be.state();
  check("回读结果与面板载荷逐条相同", sameRules(stored.model_fallback, payload), JSON.stringify(stored.model_fallback));
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 14: the regression assertions exist and stay green
// ---------------------------------------------------------------------------
suite.story("US-14", "面板行为检查器的重叠回归断言存在且通过", async ({ check }) => {
  const out = await runNode([join(REPO_ROOT, "scripts", "ui-static-check.mjs")]);
  check("检查器整体通过（退出码 0）", out.code === 0, out.tail);
  check("锁住「其他规则主力被保留」", out.stdout.includes("其他规则的主力在清洗后被保留（重叠合法）"));
  check("锁住「同规则自环被剔除」", out.stdout.includes("同规则自环在清洗后被剔除（其他备胎不受影响）"));
  check("两条断言确实为通过态（✅）",
    (out.stdout.match(/✅ 其他规则的主力在清洗后被保留/g) || []).length === 1 &&
    (out.stdout.match(/✅ 同规则自环在清洗后被剔除/g) || []).length === 1);
  check("检查器整轮无失败", !/❌/.test(out.stdout), (out.stdout.match(/❌[^\n]*/g) || []).slice(0, 3).join(" | "));
}, { backend: false });

// ---------------------------------------------------------------------------
// Story 15: the authoritative doc records the behaviour as known (not a defect)
// ---------------------------------------------------------------------------
suite.story("US-15", "权威降级链文档记录「重叠合法」为已知行为", async ({ check }) => {
  const doc = readDoc("模型降级链定义.md");
  check("记录重叠合法（from ∈ 他链 to）", /重叠合法/.test(doc) && /其他规则的备胎/.test(doc));
  check("记录同规则自环禁止", /同规则自环禁止/.test(doc));
  check("记录旧版本已删除条目不可恢复", /不可恢复/.test(doc) && /不迁移/.test(doc));
}, { backend: false });

// ---------------------------------------------------------------------------
// Story 16: upgrading keeps an existing clean config untouched (zero migration)
// ---------------------------------------------------------------------------
suite.story("US-16", "升级后已有的干净配置零迁移、原样加载", async ({ p, be, check }) => {
  const seeded = rules([A, C], [B, D]);
  p.openSettings();
  check("后端读出的规则与文件逐条相同", sameRules((await be.state()).model_fallback, seeded));
  check("面板编辑态与文件逐条相同", sameRules(p.T.__getFbRules(), seeded), JSON.stringify(p.T.__getFbRules()));
  check("面板未标记「未保存」", p.dirty() === "none", p.dirty());
  check("加载过程没有改写 config.json（零迁移）", Buffer.compare(be.seededBytes, be.configBytes()) === 0);
}, { config: cfg(rules([A, C], [B, D])) });

// ---------------------------------------------------------------------------
// Story 17: entries the old version already deleted are never fabricated back
// ---------------------------------------------------------------------------
suite.story("US-17", "旧的被静默删除条目如实说明为不可恢复（不伪造回填）", async ({ p, be, check }) => {
  const seeded = rules([A, C]);
  p.openSettings();
  check("面板只显示文件里的内容（不凭空补条目）", sameRules(p.T.__getFbRules(), seeded), JSON.stringify(p.T.__getFbRules()));
  check("备胎链没有注入任何历史条目", sameRules(p.chipsOf(0), [C]), p.chipsOf(0).join(","));
  check("后端也没有回填", sameRules((await be.state()).model_fallback, seeded));
  const doc = readDoc("模型降级链定义.md");
  check("文档明确不可恢复且不迁移", /不可恢复、不迁移/.test(doc));
}, { config: cfg(rules([A, C])) });

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------
function readDoc(name) {
  return readFileSync(join(REPO_ROOT, "docs", name), "utf8");
}

function runNode(args) {
  return new Promise((ok) => {
    const child = spawn(process.execPath, args, { cwd: REPO_ROOT });
    let stdout = "";
    child.stdout.on("data", (b) => { stdout += b; });
    child.stderr.on("data", (b) => { stdout += b; });
    child.on("exit", (code) => {
      ok({ code, stdout, tail: stdout.trim().split("\n").slice(-3).join(" | ") });
    });
  });
}

process.exitCode = await suite.run();