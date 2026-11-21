// Real daemon lifecycle for E2E suites.
//
// The E2E layer must exercise the real Go binary, not a fake API: the point of these
// scripts is the wiring unit tests cannot reach (panel gesture → HTTP → config file →
// hot-applied runtime state). Each instance runs with WILDWORK_HOME pointed at a throwaway
// directory, so it writes its own config.json / data/ and never touches the developer's
// real state. No accounts are seeded, therefore no channel scheduler can hit an upstream
// (AGENTS §0.1 discipline: E2E must not send real upstream requests).
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = resolve(HERE, "../../..");
const TEMP_ROOT = join(REPO_ROOT, "temp", "e2e");

/** Returns a currently free TCP port on the loopback interface. */
export function freePort() {
  return new Promise((ok, fail) => {
    const srv = createServer();
    srv.on("error", fail);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => ok(port));
    });
  });
}

let cachedBinary = null;

// Live daemon children, so an interrupted run (Ctrl-C / CI timeout / SIGKILL of the
// runner) cannot leave daemons behind. A leak used to survive until reboot and keep
// holding a temp home + port.
const liveDaemons = new Set();
function killAllDaemons() {
  for (const c of liveDaemons) {
    try { c.kill("SIGKILL"); } catch { /* already gone */ }
  }
  liveDaemons.clear();
}
process.on("exit", killAllDaemons);
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => { killAllDaemons(); process.exit(130); });
}

/** Builds `cmd/wild-work` once per process run and returns the binary path. */
export async function ensureBinary() {
  if (cachedBinary) return cachedBinary;
  const outDir = join(TEMP_ROOT, "_bin");
  mkdirSync(outDir, { recursive: true });
  const bin = join(outDir, `wild-work-${process.pid}`);
  // Drop binaries left by previous runs (the pid suffix keeps concurrent runs apart, but
  // nothing would ever collect the old ones — a few runs of that is 70MB of dead weight).
  for (const name of readdirSync(outDir)) {
    if (name.startsWith("wild-work-") && name !== `wild-work-${process.pid}`) {
      try { rmSync(join(outDir, name), { force: true }); } catch { /* in use by another run */ }
    }
  }
  const started = Date.now();
  await runCmd("go", ["build", "-o", bin, "./cmd/wild-work"], REPO_ROOT);
  cachedBinary = { path: bin, buildMs: Date.now() - started };
  return cachedBinary;
}

function runCmd(cmd, args, cwd) {
  return new Promise((ok, fail) => {
    const child = spawn(cmd, args, { cwd, stdio: ["ignore", "pipe", "pipe"] });
    let stderr = "";
    child.stderr.on("data", (b) => { stderr += b; });
    child.stdout.on("data", () => {});
    child.on("error", fail);
    child.on("exit", (code) => {
      if (code === 0) ok();
      else fail(new Error(`${cmd} ${args.join(" ")} exited ${code}\n${stderr}`));
    });
  });
}

/**
 * Starts the daemon in an isolated home with the given config (merged over a base).
 * Returns handles the suite uses like a user would: real HTTP calls.
 */
export async function startBackend({ name, config }) {
  const binary = await ensureBinary();
  // pid suffix: two concurrent runs in the same worktree must not share a home dir.
  const dir = join(TEMP_ROOT, `${name}-${process.pid}`);
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const port = await freePort();
  const full = {
    listen: { host: "127.0.0.1", port },
    ...config,
  };
  const cfgPath = join(dir, "config.json");
  const seededText = JSON.stringify(full, null, 2) + "\n";
  writeFileSync(cfgPath, seededText);
  const seededBytes = Buffer.from(seededText, "utf8");

  const logPath = join(dir, "daemon.log");
  const child = spawn(binary.path, ["--no-tray"], {
    cwd: dir,
    env: { ...process.env, WILDWORK_HOME: dir },
    stdio: ["ignore", "pipe", "pipe"],
  });
  liveDaemons.add(child);
  child.on("exit", () => liveDaemons.delete(child));
  let log = "";
  child.stdout.on("data", (b) => { log += b; });
  child.stderr.on("data", (b) => { log += b; });

  const baseURL = `http://127.0.0.1:${port}`;
  const started = Date.now();
  let up = false;
  for (let i = 0; i < 200 && child.exitCode === null; i++) {
    try {
      const r = await fetch(baseURL + "/api/state");
      if (r.ok) { up = true; break; }
    } catch { /* not listening yet */ }
    await sleep(100);
  }
  if (!up) {
    child.kill("SIGKILL");
    throw new Error(`daemon failed to become ready (${log || "no output"})`);
  }
  writeFileSync(logPath, log);

  const call = async (path, body) => {
    const opts = body === undefined
      ? { method: "GET", headers: { "Content-Type": "application/json" } }
      : { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) };
    const r = await fetch(baseURL + path, opts);
    const text = await r.text();
    let data = {};
    try { data = JSON.parse(text); } catch { data = { raw: text }; }
    return { status: r.status, ok: r.ok, data, text };
  };

  return {
    baseURL,
    dir,
    port,
    startupMs: Date.now() - started,
    log: () => log,
    seededBytes,
    call,
    state: async () => (await call("/api/state")).data,
    configOnDisk: () => JSON.parse(readFileSync(cfgPath, "utf8")),
    configBytes: () => readFileSync(cfgPath),
    configPath: cfgPath,
    async stop() {
      if (child.exitCode !== null) return;
      child.kill("SIGTERM");
      for (let i = 0; i < 50; i++) {
        if (child.exitCode !== null) return;
        await sleep(100);
      }
      child.kill("SIGKILL");
      liveDaemons.delete(child);
    },
    async cleanup() {
      await this.stop();
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

export function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

/** Deterministic custom channels used as model fixtures: ids are `<channel>/<model>`. */
export const TEST_CHANNELS = [
  { name: "demoa", base_url: "http://127.0.0.1:9/v1", models: [{ id: "alpha" }, { id: "beta" }, { id: "epsilon" }] },
  { name: "demob", base_url: "http://127.0.0.1:9/v1", models: [{ id: "gamma" }, { id: "delta" }, { id: "zeta" }] },
];

/** True when the repo tree has a Go toolchain available (build is required by the suite). */
export async function goAvailable() {
  try {
    await runCmd("go", ["version"], REPO_ROOT);
    return true;
  } catch { return false; }
}

export { readdirSync };