// Headless browser pass for the remote bridge's card states.
//
// Drives a server seeded by tools/seedremote across its timeline, asserting on
// the live snapshot at each step and capturing Devices and Dashboard on both
// device profiles:
//
//   1. Every remote that runs connects, and its rigs list through it. One rig
//      behind Ridge Pier is paused.
//   2. Commander's own link drops (relay reachability down): captured, then
//      restored.
//   3. Ridge Pier stops and ages past the unreachable threshold; then Valley
//      Field and Creek Shed stop (Reconnecting, Creek's rig mid-session
//      through the drop) and Desert Camp is re-paired without its new .env
//      (Waiting). Captured with the relay back up.
//
// The timeline runs through marker files in <data-dir>/seedremote-ctl, which
// the `-tags demo` build polls, so the pass decides when each step happens.
// Allow about four minutes: the unreachable threshold is seeded at two.
//
// Playwright lives in the GLOBAL node_modules on this machine.
//   make demo-up SEED=seedremote                                 # :8790
//   OUT=<dir> AC_DATA_DIR=tmp/demo NODE_PATH=$(npm root -g) node tools/seedremote/uipass.js
//   make demo-down
const { chromium } = require("playwright");
const fs = require("fs");
const path = require("path");

const OUT = process.env.OUT || ".";
const BASE = process.env.TARGET || "http://localhost:8790";
const PASSWORD = process.env.AC_PASSWORD || "12345678";
const DATA_DIR = process.env.AC_DATA_DIR;

// The ids tools/seedremote pairs, in its order.
const R = {
  spring: "r1", // never connected
  hilltop: "r2", // direct, warnings, an unadopted device
  lodge: "r3", // relayed
  pier: "r4", // needs update (remote), stale build
  farm: "r5", // needs update (Commander)
  valley: "r6", // stopped late: reconnecting
  ridge: "r7", // stopped early: unreachable
  creek: "r8", // stopped late, its rig mid-session: reconnecting
  desert: "r9", // re-paired: waiting
};
const UNADOPTED_GUID = "hl0unadopt1";

const PROFILES = [
  { name: "mobile", viewport: { width: 393, height: 852 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true },
  { name: "desktop", viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, isMobile: false, hasTouch: false },
];
const SURFACES = [
  { name: "devices", path: "/devices", ready: "text=Your remotes" },
  { name: "dashboard", path: "/", ready: "main" },
];

const results = [];
function check(name, cond, detail = "") {
  results.push({ name, ok: !!cond, detail });
  console.log(`${cond ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}
const log = (msg) => console.log(`${new Date().toISOString().slice(11, 19)}  ${msg}`);

const ctlDir = () => path.join(DATA_DIR, "seedremote-ctl");
function touch(marker) {
  fs.mkdirSync(ctlDir(), { recursive: true });
  fs.writeFileSync(path.join(ctlDir(), marker), "");
}
function untouch(marker) {
  fs.rmSync(path.join(ctlDir(), marker), { force: true });
}

async function login(browser, profile) {
  const { name, ...opts } = profile;
  const ctx = await browser.newContext(opts);
  const page = await ctx.newPage();
  page.on("console", (m) => {
    if (m.type() === "error") console.log(`  [${name} console.error]`, m.text());
  });
  // networkidle never settles (the SSE stream stays open), so every wait is
  // domcontentloaded plus an explicit element wait.
  await page.goto(`${BASE}/login`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector('input[type="password"]');
  await page.fill('input[type="password"]', PASSWORD);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith("/login")), page.click('button[type="submit"]')]);
  return { ctx, page };
}

/** A write through the API, with the CSRF double-submit the SPA uses. */
async function api(page, method, url, body) {
  return page.evaluate(
    async ({ base, method, url, body }) => {
      const token = decodeURIComponent((document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/) || [])[1] || "");
      const res = await fetch(base + url, {
        method,
        credentials: "include",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      return { status: res.status, text: await res.text() };
    },
    { base: BASE, method, url, body },
  );
}

/** The live snapshot: the first `snapshot` event of /events. */
async function snapshot(page) {
  return page.evaluate(async (base) => {
    const ctl = new AbortController();
    const res = await fetch(base + "/events", { credentials: "include", signal: ctl.signal });
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) throw new Error("event stream ended before a snapshot");
      buf += dec.decode(value, { stream: true });
      for (const block of buf.split("\n\n").slice(0, -1)) {
        if (!block.includes("event: snapshot")) continue;
        const data = block
          .split("\n")
          .filter((l) => l.startsWith("data:"))
          .map((l) => l.slice(5).trim())
          .join("\n");
        ctl.abort();
        return JSON.parse(data);
      }
    }
  }, BASE);
}

async function discovered(page) {
  return page.evaluate(async (base) => (await fetch(base + "/api/discovered", { credentials: "include" })).json(), BASE);
}

/** Poll the snapshot until pred holds; returns it, or the last one on timeout. */
async function waitSnap(page, label, pred, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let snap;
  for (;;) {
    snap = await snapshot(page);
    if (pred(snap)) {
      log(`${label}: reached`);
      return snap;
    }
    if (Date.now() > deadline) {
      log(`${label}: NOT reached within ${timeoutMs / 1000}s`);
      return snap;
    }
    await page.waitForTimeout(1000);
  }
}

const remote = (s, id) => (s.remotes || []).find((r) => r.id === id);
const rig = (s, id) => (s.rigs || []).find((r) => r.id === id);

/** Overflow of the page against the device width, with the elements past it. */
function overflowProbe(deviceWidth) {
  const offenders = [];
  for (const el of document.querySelectorAll("body *")) {
    const r = el.getBoundingClientRect();
    if (r.width > 0 && r.right > deviceWidth + 1) {
      offenders.push({
        tag: el.tagName.toLowerCase(),
        cls: String(el.className || "").slice(0, 80),
        right: Math.round(r.right),
        text: (el.textContent || "").trim().slice(0, 60),
      });
    }
  }
  return {
    docWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth,
    deviceWidth,
    offenders: offenders.slice(0, 20),
  };
}

/** Captures every surface on every profile under a label. */
async function capture(sessions, label, overflow) {
  for (const { profile, page } of sessions) {
    for (const s of SURFACES) {
      await page.goto(`${BASE}${s.path}`, { waitUntil: "domcontentloaded" });
      await page.waitForSelector(s.ready, { timeout: 15_000 });
      await page.waitForTimeout(1500);
      const base = path.join(OUT, `${label}-${profile.name}-${s.name}`);
      await page.screenshot({ path: `${base}-full.png`, fullPage: true });
      await page.screenshot({ path: `${base}-viewport.png`, fullPage: false });
      if (profile.isMobile) {
        overflow[`${label}-${s.name}`] = await page.evaluate(overflowProbe, profile.viewport.width);
      }
      log(`captured ${label}-${profile.name}-${s.name}`);
    }
  }
}

(async () => {
  if (!DATA_DIR) {
    console.error("AC_DATA_DIR is required — the pass drives the timeline through markers in it.");
    process.exit(2);
  }
  fs.mkdirSync(OUT, { recursive: true });
  untouch("link-down");

  const browser = await chromium.launch({ headless: true });
  const sessions = [];
  for (const profile of PROFILES) sessions.push({ profile, ...(await login(browser, profile)) });
  const page = sessions[1].page;
  const overflow = {};

  // --- 1. Every running remote connects and its rigs list through it. ---
  const remoteRigs = ["rig-hill", "rig-lodge", "rig-valley", "rig-seestar", "rig-ridge", "rig-newt", "rig-desert"];
  let snap = await waitSnap(
    page,
    "remotes up and rigs online through them",
    (s) =>
      ["hilltop", "lodge", "valley", "ridge", "creek", "desert"].every((k) => remote(s, R[k])?.state === "connected") &&
      remoteRigs.every((id) => rig(s, id)?.online && rig(s, id)?.site),
    60_000,
  );
  check("Spring Camp never connected reads waiting", remote(snap, R.spring)?.state === "waiting");
  check("Hilltop is connected direct", remote(snap, R.hilltop)?.state === "connected" && remote(snap, R.hilltop)?.direct);
  check("Dark Sky Lodge is connected relayed", remote(snap, R.lodge)?.state === "connected" && !remote(snap, R.lodge)?.direct);
  check(
    "Pier Shed needs an update on the remote side, with a stale build",
    remote(snap, R.pier)?.state === "needs_update" &&
      remote(snap, R.pier)?.needs_update === "remote" &&
      remote(snap, R.pier)?.build_stale,
    JSON.stringify(remote(snap, R.pier)),
  );
  check(
    "Farm Barn needs an update on Commander's side",
    remote(snap, R.farm)?.state === "needs_update" && remote(snap, R.farm)?.needs_update === "commander",
  );
  check(
    "Hilltop carries a refused forward and a rejected key",
    ["refused_forward", "rejected_client"].every((k) => (remote(snap, R.hilltop)?.warnings || []).some((w) => w.kind === k)),
  );
  check(
    "the refused forward also shows on the rig at that address",
    (rig(snap, "rig-hill")?.refused_forwards || []).length === 1,
    JSON.stringify(rig(snap, "rig-hill")?.refused_forwards),
  );
  check("the local rig has no site", rig(snap, "rig-home") && !rig(snap, "rig-home").site);
  check("relay reads up", snap.relay?.state === "up");

  // A scan round through the remotes fills the found list.
  await api(page, "POST", "/api/scan");
  const found = (await discovered(page)).find((d) => d.guid === UNADOPTED_GUID);
  check("the unadopted device is found through Hilltop", found && found.site === R.hilltop && !found.managed, JSON.stringify(found));

  // Pause a rig behind Ridge Pier while its remote is still up, so its card
  // keeps its remote.
  const paused = await api(page, "POST", "/api/rigs/rig-newt:pause");
  check("rig-newt paused", paused.status < 300, `${paused.status} ${paused.text}`);
  await waitSnap(page, "rig-newt paused", (s) => rig(s, "rig-newt")?.paused, 20_000);

  // --- 2. Commander's own link drops. ---
  touch("link-down");
  snap = await waitSnap(page, "relay down", (s) => s.relay?.state === "down", 90_000);
  check("relay reads down while Commander's link is down", snap.relay?.state === "down");
  await capture(sessions, "relay-down", overflow);
  untouch("link-down");
  snap = await waitSnap(
    page,
    "relay and remotes back",
    (s) => s.relay?.state === "up" && ["hilltop", "lodge", "valley", "ridge", "creek", "desert"].every((k) => remote(s, R[k])?.state === "connected"),
    90_000,
  );
  check("relay reads up again", snap.relay?.state === "up");

  // --- 3. Ridge Pier stops and goes past the threshold. ---
  touch(`stop-${R.ridge}`);
  snap = await waitSnap(page, "Ridge Pier unreachable", (s) => remote(s, R.ridge)?.state === "unreachable", 200_000);
  check("Ridge Pier reads unreachable", remote(snap, R.ridge)?.state === "unreachable");

  // Valley Field and Creek Shed stop; Desert Camp is re-paired and never gets
  // its new .env.
  check("Creek Seestar is mid-session before its remote drops", rig(snap, "rig-seestar")?.in_session);
  touch(`stop-${R.valley}`);
  touch(`stop-${R.creek}`);
  const repair = await api(page, "POST", `/api/remotes/${R.desert}:repair`);
  check("Desert Camp re-paired", repair.status < 300, `${repair.status}`);
  snap = await waitSnap(
    page,
    "every remote-down rig card written",
    (s) =>
      remote(s, R.valley)?.state === "reconnecting" &&
      remote(s, R.creek)?.state === "reconnecting" &&
      remote(s, R.desert)?.state === "waiting" &&
      ["rig-valley", "rig-ridge", "rig-desert"].every((id) => rig(s, id)?.remote?.down),
    180_000,
  );
  check("Valley Field reads reconnecting", remote(snap, R.valley)?.state === "reconnecting");
  check("Creek Shed reads reconnecting", remote(snap, R.creek)?.state === "reconnecting");
  check("Desert Camp reads waiting after its re-pair", remote(snap, R.desert)?.state === "waiting");
  for (const [id, state] of [
    ["rig-valley", "reconnecting"],
    ["rig-ridge", "unreachable"],
    ["rig-desert", "waiting"],
  ]) {
    const r = rig(snap, id);
    check(
      `${id}: remote ${state}, Safe to power off Unknown for its remote, with a last-known line`,
      r?.remote?.state === state &&
        r.remote.down &&
        r.safe_to_power_off === "unknown" &&
        r.power_off_reason === "remote" &&
        r.last_known,
      JSON.stringify({ remote: r?.remote, safe: r?.safe_to_power_off, reason: r?.power_off_reason, last: r?.last_known }),
    );
  }
  const seestar = rig(snap, "rig-seestar");
  check(
    "rig-seestar keeps its live session while its remote reconnects",
    seestar?.in_session && seestar.remote?.state === "reconnecting" && !seestar.remote.down,
    JSON.stringify({ in_session: seestar?.in_session, remote: seestar?.remote, state: seestar?.state }),
  );
  const newt = rig(snap, "rig-newt");
  check(
    "rig-newt stays paused behind its unreachable remote",
    newt?.paused && newt.remote?.state === "unreachable" && !newt.remote.down,
    JSON.stringify({ state: newt?.state, paused: newt?.paused, remote: newt?.remote }),
  );
  check("connected rigs carry no remote-down view", ["rig-hill", "rig-lodge"].every((id) => !rig(snap, id)?.remote));
  check("relay reads up at the final capture", snap.relay?.state === "up");

  await capture(sessions, "final", overflow);
  // capture ends each profile on Dashboard; the desktop page goes back to
  // Devices to read the remote cards' rig lists.
  await page.goto(`${BASE}/devices`, { waitUntil: "domcontentloaded" });
  const newtLine = page.locator(".remote-rigs--named li", { hasText: "Ridge Newtonian" });
  await newtLine.waitFor({ timeout: 15_000 });
  const newtText = (await newtLine.innerText()).replace(/\s+/g, " ");
  check("Ridge Pier lists its paused rig as last reached through it", /last reached through it/.test(newtText), newtText);
  fs.writeFileSync(path.join(OUT, "overflow-mobile.json"), JSON.stringify(overflow, null, 2));
  for (const [k, v] of Object.entries(overflow)) {
    log(`overflow ${k}: doc ${v.docWidth}px / device ${v.deviceWidth}px, ${v.offenders.length} offenders`);
  }
  fs.writeFileSync(path.join(OUT, "final-snapshot.json"), JSON.stringify(snap, null, 2));

  await browser.close();
  const failed = results.filter((r) => !r.ok);
  console.log(`\n=== ${results.length - failed.length}/${results.length} checks passed ===`);
  process.exit(failed.length ? 1 : 0);
})().catch((e) => {
  console.error("SCRIPT ERROR:", e);
  process.exit(2);
});
