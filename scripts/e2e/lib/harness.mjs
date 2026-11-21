// Tiny E2E runner: one story = one isolated daemon + one freshly loaded panel.
//
// Isolation is per story on purpose. A story that saves rules would otherwise leak into
// the next story's "what does the config contain" assertions, and debugging a red line
// would become guesswork. A fresh daemon costs ~50ms (binary is built once per run).
import { ensureBinary, startBackend, sleep } from "./backend.mjs";
import { openPanel } from "./panel.mjs";

const RESET = "\u001b[0m";
const GREEN = "\u001b[32m";
const RED = "\u001b[31m";
const DIM = "\u001b[2m";

export function createSuite({ name }) {
  const stories = [];

  /**
   * Registers a user-story scenario.
   * opts.config    — config.json seed merged over the base fixture
   * opts.backend   — set false for pure static/doc assertions (no daemon needed)
   */
  const story = (id, title, body, opts = {}) => stories.push({ id, title, body, opts });

  async function run() {
    const started = Date.now();
    const bin = await ensureBinary();
    console.log(`${DIM}[build] ${bin.path} (${bin.buildMs}ms)${RESET}`);
    console.log(`\n=== ${name} ===\n`);

    const failures = [];
    let fired = 0;

    for (const s of stories) {
      const checks = [];
      const check = (label, pass, detail = "") => {
        checks.push({ label, pass: Boolean(pass), detail });
      };
      let error = null;
      let panelLogs = [];
      const ctx = { check, story: s };

      if (s.opts.backend === false) {
        try { await s.body(ctx); } catch (e) { error = e; }
      } else {
        const be = await startBackend({ name: s.id.toLowerCase().replace(/[^a-z0-9]+/g, "-"), config: s.opts.config || {} });
        ctx.be = be;
        try {
          ctx.p = await openPanel({ baseURL: be.baseURL });
          await s.body(ctx);
        } catch (e) {
          error = e;
        } finally {
          // Let in-flight panel requests finish before the daemon goes away, otherwise a
          // cancelled poll would look like a scenario failure.
          await sleep(120);
          panelLogs = ctx.p ? ctx.p.logs() : [];
          await be.cleanup();
        }
      }

      const bad = checks.filter((c) => !c.pass);
      fired += checks.length;
      const ok = !error && bad.length === 0;
      const tail = `${checks.length - bad.length}/${checks.length}`;
      console.log(`${ok ? GREEN + "✅" : RED + "❌"} ${s.id}  ${s.title}  ${DIM}${tail}${RESET}`);
      if (error) console.log(`   ${RED}✗ scenario threw: ${error && error.stack ? error.stack.split("\n")[0] : error}${RESET}`);
      for (const c of bad) {
        console.log(`   ${RED}✗ ${c.label}${c.detail ? "  ← " + c.detail : ""}${RESET}`);
      }
      if (!ok && panelLogs.length) {
        console.log(`   ${DIM}panel console: ${panelLogs.slice(-3).join(" | ")}${RESET}`);
      }
      if (!ok) failures.push(s.id);
    }

    const secs = ((Date.now() - started) / 1000).toFixed(1);
    console.log(
      `\n${failures.length === 0 ? GREEN : RED}${stories.length} stories, ${fired} assertions, ` +
      `${failures.length} failed${RESET}  ${DIM}(${secs}s)${RESET}`,
    );
    return failures.length === 0 ? 0 : 1;
  }

  return { story, run, stories };
}

/** Structural comparison helper for rule lists (backend omits empty alias names). */
export function normalizeRules(rules) {
  const list = Array.isArray(rules) ? rules : rules ? [rules] : [];
  return list.map((r) => ({ from: r.from, to: [...(r.to || [])], name: r.name || "" }));
}

export function sameRules(a, b) {
  return JSON.stringify(normalizeRules(a)) === JSON.stringify(normalizeRules(b));
}