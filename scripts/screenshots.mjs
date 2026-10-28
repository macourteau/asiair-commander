/**
 * ASIAIR Commander — UI screenshot capture.
 *
 * Captures every authenticated surface plus the login screen, on two device
 * profiles (mobile iPhone 16 Pro and retina desktop), each as a full-page and a
 * viewport image. Also emits a machine-readable horizontal-overflow report for
 * the mobile profile so horizontal overflow on mobile can be quantified, not
 * just eyeballed.
 *
 * Playwright is resolved from the GLOBAL node_modules (the project has no local
 * dependency on it), via NODE_PATH. The script also falls back to scanning the
 * npx download cache if the global package is absent, so it keeps working
 * without a global install.
 *
 * Regenerate, from the repository root:
 *   OUT=scripts/screenshots AC_PASSWORD=12345678 \
 *     NODE_PATH=$(npm root -g) node scripts/screenshots.mjs
 *
 * For surfaces that need live content (the feed, dashboard, device cards) with no
 * real ASIAIR, run the backend in demo mode first — it stubs the device and drips
 * fake frames through the whole pipeline (see cmd/asiair-commander/demo.go):
 *   SEED_DIR=/tmp/acdemo SEED_LIVE=1 go run ./tools/seeddemo
 *   go run -tags demo ./cmd/asiair-commander --data-dir /tmp/acdemo run   # :8686
 *   ( cd web && npm run dev )                                             # :5173 proxy
 *
 * The first-run wizard needs an instance with NO password set, which the login
 * step above assumes away, so it is a separate optional pass driven by its own
 * target. Stand a fresh one up beside the configured instance:
 *   AC_SETUP_DIR=$(mktemp -d)
 *   go run ./cmd/asiair-commander --data-dir $AC_SETUP_DIR run   # picks a free port
 *   AC_SETUP_TARGET=http://localhost:8687 AC_SETUP_DATA_DIR=$AC_SETUP_DIR ... node scripts/screenshots.mjs
 * The wizard's first field demands the one-time setup token, so the harness
 * reads it from <data-dir>/setup-token — the same machine-readable channel the
 * Windows installer uses — and fills it before shooting. It never submits:
 * submitting would set a password and make the data dir useless for the next
 * run.
 *
 * Configuration (env vars):
 *   TARGET              Base URL of the running SPA.   Default http://localhost:5173
 *   AC_PASSWORD         Shared login password.         Required — no default, never hardcoded.
 *   OUT                 Output directory (repo-root relative or absolute).
 *                                                      Default scripts/screenshots
 *   AC_SETUP_TARGET     Base URL of an unconfigured instance. Optional; the
 *                       first-run pass is skipped when unset.
 *   AC_SETUP_DATA_DIR   That instance's data directory, holding setup-token.
 *                       Required when AC_SETUP_TARGET is set.
 *   COLOR_SCHEME        "light" or "dark": the prefers-color-scheme the browser
 *                       reports. The SPA follows it when no theme is stored, and
 *                       each run starts from a fresh context. Default light.
 */

import { execSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { existsSync, readdirSync } from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");

/**
 * Resolve the Playwright module across the ways it might be present:
 * NODE_PATH / global root first, then the npx download cache. Returns the
 * loaded module or throws with guidance.
 */
function loadPlaywright() {
  const candidates = [];

  // NODE_PATH (set by the documented run command) and the reported global root.
  if (process.env.NODE_PATH) candidates.push(join(process.env.NODE_PATH, "playwright"));
  try {
    const globalRoot = execSync("npm root -g", { encoding: "utf8" }).trim();
    if (globalRoot) candidates.push(join(globalRoot, "playwright"));
  } catch {
    /* npm not on PATH — fine, other candidates may work */
  }

  // Fallback: npx caches full playwright installs under hashed dirs.
  const npxCache = join(process.env.HOME ?? "", ".npm", "_npx");
  if (existsSync(npxCache)) {
    for (const hash of readdirSync(npxCache)) {
      candidates.push(join(npxCache, hash, "node_modules", "playwright"));
    }
  }

  for (const path of candidates) {
    try {
      return require(path);
    } catch {
      /* try the next candidate */
    }
  }

  // Last resort: a bare require, in case it is genuinely on the default path.
  try {
    return require("playwright");
  } catch {
    throw new Error(
      "Could not resolve the 'playwright' module. Install it globally " +
        "(npm install -g playwright) or run with NODE_PATH=$(npm root -g).",
    );
  }
}

const { chromium } = loadPlaywright();

const TARGET = process.env.TARGET ?? "http://localhost:5173";
const PASSWORD = process.env.AC_PASSWORD;
const OUT_RAW = process.env.OUT ?? "scripts/screenshots";
const OUT = isAbsolute(OUT_RAW) ? OUT_RAW : join(REPO_ROOT, OUT_RAW);
const SETUP_TARGET = process.env.AC_SETUP_TARGET;
const SETUP_DATA_DIR = process.env.AC_SETUP_DATA_DIR;
const COLOR_SCHEME = process.env.COLOR_SCHEME ?? "light";

if (!PASSWORD) {
  console.error("AC_PASSWORD is required (the shared login password). Set it and re-run.");
  process.exit(1);
}
if (COLOR_SCHEME !== "light" && COLOR_SCHEME !== "dark") {
  console.error(`COLOR_SCHEME must be "light" or "dark", not "${COLOR_SCHEME}".`);
  process.exit(1);
}
if (SETUP_TARGET && !SETUP_DATA_DIR) {
  console.error(
    "AC_SETUP_TARGET is set but AC_SETUP_DATA_DIR is not — the first-run pass needs " +
      "the instance's data directory to read <data-dir>/setup-token.",
  );
  process.exit(1);
}

const NAV_TIMEOUT = 15_000;
// The dashboard, feed, and devices surfaces hold a live SSE stream that never goes
// network-idle, so waiting the full NAV_TIMEOUT on every one of them serializes the
// whole run into minutes. Cap the idle wait short and lean on the ready-selector +
// a fixed settle instead.
const IDLE_TIMEOUT = 3_500;
const SETTLE_MS = 400; // late-layout settle after networkidle

// iPhone 16 Pro (non-Max) is not in Playwright's device registry, so the
// descriptor is defined by hand: 393x852 CSS px at 3x, touch-capable, mobile UA.
const IPHONE_16_PRO = {
  viewport: { width: 393, height: 852 },
  deviceScaleFactor: 3,
  isMobile: true,
  hasTouch: true,
  userAgent:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 " +
    "(KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
};

const DESKTOP = {
  viewport: { width: 1440, height: 900 },
  deviceScaleFactor: 2,
  isMobile: false,
  hasTouch: false,
};

const DEVICES = [
  { name: "mobile", profile: { ...IPHONE_16_PRO, colorScheme: COLOR_SCHEME }, mobile: true },
  { name: "desktop", profile: { ...DESKTOP, colorScheme: COLOR_SCHEME }, mobile: false },
];

// Authenticated surfaces. A stable selector per surface avoids shooting a spinner.
const SURFACES = [
  { name: "dashboard", path: "/", ready: "h1, .dashboard, main" },
  // The feed's rows carry the same "i" detail trigger as History.
  { name: "feed", path: "/feed", ready: "h1, main", detailTrigger: 'button[aria-label^="Details for"]' },
  // The rig editor is a modal, not a route, so a control added inside it is
  // invisible to a plain page capture. Open the first rig's editor and scroll
  // its per-rig toggles into view: the dialog scrolls internally, so its lower
  // half is off-screen on both device profiles and fullPage cannot reach it.
  {
    name: "devices",
    path: "/devices",
    ready: "h1, main",
    detailTrigger: 'button:has-text("Edit")',
    detailScrollTo: "#sync-clock-help",
  },
  // The Add-a-remote dialog is a modal too; this captures its first page (the
  // prerequisites and name), which needs no remote to exist.
  {
    name: "devices-add-remote",
    path: "/devices",
    ready: "h1, main",
    detailTrigger: 'button:has-text("Add a remote")',
  },
  // The "i" button per row opens the shared frame-detail dialog; capture it too.
  { name: "history", path: "/history", ready: "h1, main", detailTrigger: 'button[aria-label^="Details for"]' },
  // The backlog alert's target: the never-deletable view, with the first row's
  // "Why it stays" dialog.
  {
    name: "history-never-deletable",
    path: "/history?view=never_deletable",
    ready: "h1, main",
    detailTrigger: 'button:has-text("Why it stays")',
  },
  { name: "settings", path: "/settings", ready: "h1, main" },
  { name: "tools", path: "/tools", ready: "h1, main" },
  // The File browser, both panes addressed by query string: the left pane at a
  // share root (the watched Autorun badge), the right inside the watched folder
  // holding the full-length frame name tools/seeddemo stages (`SEED_LIVE=1` and a
  // `-tags demo` build). Its dialogs are not URL-addressable;
  // tools/seeddemo/uipass.js captures those.
  {
    name: "files",
    path:
      "/files?lrig=rig-fra&lshare=EMMC+Images&rrig=rig-newt&rshare=EMMC+Images" +
      "&rdir=Autorun%2FLight%2FM42",
    ready: ".dfb-table",
  },
  { name: "logs", path: "/logs", ready: "h1, main" },
  { name: "alerts", path: "/alerts", ready: "h1, main" },
  // The Dashboard card's "Review alerts" target, and a library_reconciled alert's.
  { name: "alerts-rig", path: "/alerts?rig=rig-fra", ready: "h1, main" },
  { name: "devices-missing", path: "/devices?rig=rig-fra&show=missing", ready: "h1, main" },
  // The missing panel's leftovers and folder filter are in-page state, not URLs:
  // `clicks` opens them before the capture. tools/seedack stages leftovers across
  // several folders; a seed without them captures the plain panel.
  {
    name: "devices-missing-leftovers",
    path: "/devices?rig=rig-fra&show=missing",
    ready: ".missing-section",
    clicks: ["details.missing-leftovers summary"],
    scrollTo: "details.missing-leftovers",
  },
  {
    name: "devices-missing-filtered",
    path: "/devices?rig=rig-fra&show=missing",
    ready: ".missing-section",
    clicks: [".missing-section .missing-folder__name"],
    scrollTo: ".missing-section",
  },
  // The backlog alert's link: History narrowed to one rig's never-deletable
  // frames (the held frame tools/seeddemo stages per rig).
  { name: "history-never-deletable", path: "/history?rig=rig-fra&view=never_deletable", ready: "h1, main" },
];

/**
 * In-page overflow probe. The classic "cut off on mobile" symptom shows up two
 * ways, and both are measured against the intended device width (not the live
 * innerWidth, which a wide page can itself inflate — masking the problem):
 *
 *   1. scrollWidth exceeds the device width — content wider than the screen.
 *   2. innerWidth itself exceeds the device width — an unbreakable element
 *      (e.g. a long path string) forced the layout viewport to expand/zoom out.
 *
 * Offenders are the elements poking past the device's right edge.
 */
function overflowProbe(deviceWidth) {
  const innerWidth = window.innerWidth;
  const scrollWidth = document.documentElement.scrollWidth;
  const offenders = [];
  for (const el of document.querySelectorAll("*")) {
    const right = el.getBoundingClientRect().right;
    if (right > deviceWidth + 1) {
      offenders.push({
        tag: el.tagName.toLowerCase(),
        className: typeof el.className === "string" ? el.className : "",
        right: Math.round(right),
      });
    }
  }
  offenders.sort((a, b) => b.right - a.right);
  const widest = Math.max(scrollWidth, innerWidth);
  return {
    deviceWidth,
    innerWidth,
    scrollWidth,
    overflow: widest > deviceWidth,
    overflowBy: widest - deviceWidth,
    topOffenders: offenders.slice(0, 5),
  };
}

/** Wait for a surface to settle: network idle, a stable selector, then a beat. */
async function settle(page, readySelector) {
  try {
    await page.waitForLoadState("networkidle", { timeout: IDLE_TIMEOUT });
  } catch {
    /* SSE-backed surfaces never go fully idle; fall through to the selector fast */
  }
  if (readySelector) {
    try {
      await page.waitForSelector(readySelector, { timeout: NAV_TIMEOUT });
    } catch {
      /* selector may not match on every surface; the screenshot still tells the story */
    }
  }
  await page.waitForTimeout(SETTLE_MS);
}

/** Authenticate the given context's page: fill the single password field, submit. */
async function authenticate(page) {
  await page.goto(`${TARGET}/login`, { waitUntil: "domcontentloaded", timeout: NAV_TIMEOUT });
  await page.waitForSelector('input[type="password"]', { timeout: NAV_TIMEOUT });
  await page.fill('input[type="password"]', PASSWORD);
  await Promise.all([
    page.waitForURL((url) => !url.pathname.startsWith("/login"), { timeout: NAV_TIMEOUT }),
    page.click('button[type="submit"]'),
  ]).catch(async (err) => {
    // Surface the server's rendered error text so failures are legible.
    const errText = await page
      .locator('[role="alert"]')
      .first()
      .textContent()
      .catch(() => null);
    throw new Error(`Login failed: ${errText?.trim() || err.message}`);
  });
  // Confirm the authenticated shell rendered, not a flash of the login route.
  // `main` and not a selector list: waitForSelector waits on the first match
  // becoming visible, and on mobile that is the closed (hidden) nav drawer.
  await page.waitForSelector("main", { timeout: NAV_TIMEOUT });
}

/**
 * Return the first locator in `candidates` that resolves to a visible element,
 * or null. The wizard is the one surface whose markup this script cannot pin
 * down by a single selector without coupling itself to class names, so it tries
 * the label first (stable, it is the visible copy) and falls back to names and
 * ids. The chosen candidate is logged so a miss is diagnosable from the run.
 */
async function firstVisible(page, candidates, what) {
  for (const { how, locator } of candidates) {
    try {
      const l = locator();
      if ((await l.count()) && (await l.first().isVisible())) {
        console.log(`  ${what}: matched by ${how}`);
        return l.first();
      }
    } catch {
      /* a malformed/unsupported selector is just a miss */
    }
  }
  console.log(`  ${what}: no match`);
  return null;
}

/**
 * First-run wizard pass against an instance with no password set.
 *
 * The token is read from <data-dir>/setup-token — the file channel the service
 * writes at startup, whose second line is the token (the first is the URL it
 * bound). The field is filled before anything is shot so the wizard is captured
 * in the state an operator actually works in: an empty required field renders
 * as a form that cannot be submitted.
 *
 * The form is deliberately never submitted. A submit would set a password and
 * close the first-run window, so the next run would find a login screen instead
 * of a wizard.
 */
async function captureFirstRun(browser, overflowReport, failures) {
  const tokenPath = join(SETUP_DATA_DIR, "setup-token");
  let token;
  try {
    const lines = (await readFile(tokenPath, "utf8")).split("\n");
    token = (lines[1] ?? "").trim();
    if (!token) throw new Error(`no token on line 2 of ${tokenPath}`);
  } catch (err) {
    console.error(
      `FAILED setup: could not read the setup token from ${tokenPath}: ${err.message}\n` +
        "  The instance must be unconfigured (the file is removed once setup completes).",
    );
    failures.push("setup-token-file");
    return;
  }

  for (const device of DEVICES) {
    const ctx = await browser.newContext(device.profile);
    const page = await ctx.newPage();
    page.setDefaultTimeout(NAV_TIMEOUT);
    page.setDefaultNavigationTimeout(NAV_TIMEOUT);
    try {
      await page.goto(`${SETUP_TARGET}/setup`, {
        waitUntil: "domcontentloaded",
        timeout: NAV_TIMEOUT,
      });
      await settle(page, "form, main, h1");

      const field = await firstVisible(
        page,
        [
          // The label is the field's visible copy; role=textbox keeps this off the
          // neighbouring "(?)" button, whose aria-label also says "setup token".
          {
            how: 'role=textbox name="setup token"',
            locator: () => page.getByRole("textbox", { name: /setup token/i }),
          },
          { how: 'label "setup token"', locator: () => page.getByLabel(/setup token/i) },
          { how: 'input[name="setup_token"]', locator: () => page.locator('input[name="setup_token"]') },
        ],
        "token field",
      );
      if (!field) throw new Error("the wizard's setup-token field was not found");
      await field.fill(token);
      // The rest of the form appears once the token is present; give it a beat.
      await page.waitForTimeout(SETTLE_MS);

      const base = join(OUT, `setup-${device.name}`);
      await page.screenshot({ path: `${base}-full.png`, fullPage: true });
      await page.screenshot({ path: `${base}-viewport.png`, fullPage: false });
      console.log(`captured setup-${device.name}`);
      if (device.mobile) {
        overflowReport.setup = await page.evaluate(overflowProbe, device.profile.viewport.width);
      }

      // The "(?)" disclosure beside the token field: platform-specific guidance
      // on where to find the token. Isolated so a markup miss costs one image.
      try {
        const help = await firstVisible(
          page,
          [
            {
              how: "role=button name=where-do-I-find",
              locator: () => page.getByRole("button", { name: /where do i find the setup token/i }),
            },
            { how: "aria-label ~ token", locator: () => page.locator('button[aria-label*="token" i]') },
            { how: "aria-expanded button", locator: () => page.locator("button[aria-expanded]") },
          ],
          "(?) disclosure",
        );
        if (!help) throw new Error("the (?) disclosure control was not found");
        await help.click();
        await page.waitForTimeout(SETTLE_MS);
        await page.screenshot({ path: `${base}-token-help.png`, fullPage: true });
        console.log(`captured setup-${device.name}-token-help`);
      } catch (err) {
        console.error(`FAILED setup-${device.name}-token-help: ${err.message}`);
        failures.push(`setup-${device.name}-token-help`);
      }
    } catch (err) {
      console.error(`FAILED setup-${device.name}: ${err.message}`);
      failures.push(`setup-${device.name}`);
    } finally {
      await ctx.close();
    }
  }
}

async function main() {
  await mkdir(OUT, { recursive: true });
  const browser = await chromium.launch();
  const failures = [];
  const overflowReport = {};

  try {
    // --- First-run wizard, against its own unconfigured instance. ---
    if (SETUP_TARGET) {
      await captureFirstRun(browser, overflowReport, failures);
    } else {
      console.log("skip setup wizard (AC_SETUP_TARGET unset)");
    }

    // --- Login screen (fresh, unauthenticated context) per device profile. ---
    for (const device of DEVICES) {
      const ctx = await browser.newContext(device.profile);
      const page = await ctx.newPage();
      page.setDefaultTimeout(NAV_TIMEOUT);
      try {
        await page.goto(`${TARGET}/login`, {
          waitUntil: "domcontentloaded",
          timeout: NAV_TIMEOUT,
        });
        await settle(page, ".auth-card, h1");
        const base = join(OUT, `login-${device.name}`);
        await page.screenshot({ path: `${base}-full.png`, fullPage: true });
        await page.screenshot({ path: `${base}-viewport.png`, fullPage: false });
        console.log(`captured login-${device.name}`);
        if (device.mobile) {
          overflowReport[`login`] = await page.evaluate(
            overflowProbe,
            device.profile.viewport.width,
          );
        }
      } catch (err) {
        console.error(`FAILED login-${device.name}: ${err.message}`);
        failures.push(`login-${device.name}`);
      } finally {
        await ctx.close();
      }
    }

    // --- Authenticated surfaces: one login per device context, reused across pages. ---
    for (const device of DEVICES) {
      const ctx = await browser.newContext(device.profile);
      const page = await ctx.newPage();
      page.setDefaultTimeout(NAV_TIMEOUT);
      page.setDefaultNavigationTimeout(NAV_TIMEOUT);
      try {
        await authenticate(page);
      } catch (err) {
        console.error(`FAILED auth (${device.name}): ${err.message}`);
        failures.push(`auth-${device.name}`);
        await ctx.close();
        continue;
      }

      for (const surface of SURFACES) {
        try {
          await page.goto(`${TARGET}${surface.path}`, {
            waitUntil: "domcontentloaded",
            timeout: NAV_TIMEOUT,
          });
          await settle(page, surface.ready);
          for (const sel of surface.clicks ?? []) {
            const target = page.locator(sel);
            if (await target.count()) {
              await target.first().click();
              await page.waitForTimeout(SETTLE_MS * 2);
            } else {
              console.log(`skip click ${sel} on ${device.name}-${surface.name} (absent)`);
            }
          }
          if (surface.scrollTo) {
            const target = page.locator(surface.scrollTo);
            if (await target.count()) await target.first().scrollIntoViewIfNeeded();
          }
          const base = join(OUT, `${device.name}-${surface.name}`);
          await page.screenshot({ path: `${base}-full.png`, fullPage: true });
          await page.screenshot({ path: `${base}-viewport.png`, fullPage: false });
          console.log(`captured ${device.name}-${surface.name}`);
          if (device.mobile) {
            overflowReport[surface.name] = await page.evaluate(
              overflowProbe,
              device.profile.viewport.width,
            );
          }
          // Detail modal: open the first row's "i" button and shoot the overlay.
          // Isolated so an empty table or a modal hiccup never fails the surface.
          if (surface.detailTrigger) {
            try {
              const triggers = page.locator(surface.detailTrigger);
              if (await triggers.count()) {
                await triggers.first().click();
                await page.waitForSelector('[role="dialog"]', { timeout: NAV_TIMEOUT });
                await page.waitForTimeout(400);
                if (surface.detailScrollTo) {
                  const target = page.locator(surface.detailScrollTo);
                  if (await target.count()) {
                    await target.first().scrollIntoViewIfNeeded();
                    await page.waitForTimeout(200);
                  }
                }
                await page.screenshot({ path: `${base}-modal.png` });
                console.log(`captured ${device.name}-${surface.name}-modal`);
                await page.keyboard.press("Escape");
              } else {
                console.log(`skip ${device.name}-${surface.name}-modal (no rows)`);
              }
            } catch (err) {
              console.error(`FAILED ${device.name}-${surface.name}-modal: ${err.message}`);
              failures.push(`${device.name}-${surface.name}-modal`);
            }
          }
        } catch (err) {
          console.error(`FAILED ${device.name}-${surface.name}: ${err.message}`);
          failures.push(`${device.name}-${surface.name}`);
        }
      }
      await ctx.close();
    }
  } finally {
    await browser.close();
  }

  const reportPath = join(OUT, "overflow-report.json");
  await writeFile(reportPath, JSON.stringify(overflowReport, null, 2));

  console.log("\n=== Mobile overflow summary ===");
  for (const [surface, r] of Object.entries(overflowReport)) {
    const verdict = r.overflow ? `OVERFLOW by ${r.overflowBy}px` : "ok";
    const worst = r.topOffenders[0];
    const worstStr = worst
      ? ` — widest: <${worst.tag}${worst.className ? ` class="${worst.className}"` : ""}> right=${worst.right}px`
      : "";
    console.log(`  ${surface}: innerWidth=${r.innerWidth} scrollWidth=${r.scrollWidth} — ${verdict}${worstStr}`);
  }
  console.log(`\nOverflow report: ${reportPath}`);
  console.log(`Screenshots:     ${OUT}`);

  if (failures.length) {
    console.error(`\n${failures.length} surface(s) failed: ${failures.join(", ")}`);
    process.exit(1);
  }
  console.log("\nAll surfaces captured.");
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
