// Headless browser pass for the File browser page's dialogs (/files).
//
// The panes are URL-addressable and scripts/screenshots.mjs captures them; the
// dialogs are not, so this pass drives them against a live demo seeded by
// tools/seeddemo (`SEED_LIVE=1`), on both device profiles:
//
//   1. both panes populated: the share root (the watched Autorun badge) beside
//      the watched folder holding the held full-length frame;
//   2. the copy plan with a conflict: Preview/M42 on the FRA300 into the same
//      folder on the Newtonian, where two of its files already exist;
//   3. the report: that copy run with the conflicts left on Skip;
//   4. the copy plan with the watched warning: a frame into the Newtonian's
//      watched Autorun;
//   5. the delete dialog for the held frame: an unarchived count of 1 and the
//      confirmation checkbox;
//   6. the toolbar with Upload and Rename enabled, and the Rename dialog on a
//      Preview file with its stem selected; a rename onto a taken name, then a
//      real rename and its toast;
//   7. Rename refused in the watched Autorun folder (the folder notice's "Items
//      here can't be renamed." and the button's reason), and refused for the
//      Autorun folder itself at the share root (the visible refusal line);
//   8. an upload picked through the hidden file input: the Sending state (the
//      staging request held back), the plan with a conflict on a Preview file,
//      the Overwrite choice and the result toast;
//   9. the upload plan with the watched warning, into the Newtonian's Autorun;
//  10. the reserved-path refusal: a file named Autorun at the share root;
//  11. an upload result whose row is skipped with Overwrite offered: the file
//      appears on the device between the plan and the run (`appeared`);
//  12. an upload result with a failed row: the run's request fails at the
//      network (the interrupted path), and the row's Retry writes the staged
//      file without a second staging request.
//
// The writes are the copy in step 3, the rename in step 6, the uploads in steps
// 8, 11 and 12 and the file that appears in step 11. Each is undone through the API
// afterwards (the copied, appeared and retried files deleted, the rename reversed, the
// overwritten Preview file written back with its seeded size and time), so the
// second profile (and a re-run) sees the same plans. The delete dialog is
// cancelled, never run.
//
// Playwright lives in the GLOBAL node_modules on this machine.
//   make demo-up SEED=seeddemo                 # live seed, -tags demo build, :8686
//   OUT=<dir> NODE_PATH=$(npm root -g) node tools/seeddemo/uipass.js
//   make demo-down
const { chromium } = require("playwright");
const fs = require("fs");
const path = require("path");

const OUT = process.env.OUT || ".";
const BASE = process.env.TARGET || "http://localhost:8686";
const PASSWORD = process.env.AC_PASSWORD || "12345678";

// What tools/seeddemo and internal/demofiles stage.
const SHARE = "EMMC Images";
const FULL_NAME = "Light_M42_300.0s_Bin1_2600MM_gain100_20261007-231502_-10.0C_0001.fit";
const NEW_ON_FRA = "Light_M42_300.0s_Bin1_2600MM_gain100_20261007-232012_-10.0C_0002.fit";
// The FRA300's Preview file the Rename steps rename, and its new name.
const STACK = { dir: "Preview", name: "Stack_M42_20261007.jpg", renamed: "Stack_M42_20261007-final.jpg" };
// The Newtonian's Preview file an upload overwrites, with its seeded size and
// time, which the pass writes back afterwards.
const IC = {
  dir: "Preview/IC 1805",
  name: "Light_IC 1805_180.0s_Bin1_533MC_gain100_20261006-222210_-5.0C_0012.fit",
  size: 96 << 10,
  mtimeMs: Date.UTC(2026, 9, 6, 22, 15, 2),
};
// A name nothing on either rig carries: the watched warning's and the failed
// row's upload.
const NEW_FRAME = "Light_M42_300.0s_Bin1_2600MM_gain100_20261007-233522_-10.0C_0003.fit";
const FRA = { id: "rig-fra", name: "FRA300 Rig" };
const NEWT = { id: "rig-newt", name: "Newtonian Rig" };

const PROFILES = [
  { name: "mobile", viewport: { width: 393, height: 852 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true },
  { name: "desktop", viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, isMobile: false, hasTouch: false },
];

const results = [];
function check(name, cond, detail = "") {
  results.push({ name, ok: !!cond, detail });
  console.log(`${cond ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}
const log = (msg) => console.log(`${new Date().toISOString().slice(11, 19)}  ${msg}`);

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

/** A JSON write through the API, with the CSRF double-submit the SPA uses. */
async function api(page, url, body) {
  return page.evaluate(
    async ({ base, url, body }) => {
      const token = decodeURIComponent((document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/) || [])[1] || "");
      const res = await fetch(base + url, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
        body: JSON.stringify(body),
      });
      return { status: res.status, text: await res.text() };
    },
    { base: BASE, url, body },
  );
}

/** Overflow of the page against the device width, with the panel's and the
 * dialog's elements past it; the other Tools cards are screenshots.mjs's. */
function overflowProbe(deviceWidth) {
  const offenders = [];
  for (const el of document.querySelectorAll(".dfb-panes *, [role='dialog'] *")) {
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

function filesUrl(left, right) {
  const p = new URLSearchParams();
  for (const [prefix, place] of [
    ["l", left],
    ["r", right],
  ]) {
    p.set(`${prefix}rig`, place.rig);
    p.set(`${prefix}share`, SHARE);
    if (place.dir) p.set(`${prefix}dir`, place.dir);
  }
  return `${BASE}/files?${p}`;
}

/** Opens the panel at two places and waits for both listings. */
async function openPanes(page, left, right) {
  await page.goto(filesUrl(left, right), { waitUntil: "domcontentloaded" });
  const panes = page.locator(".dfb-pane");
  await panes.nth(0).locator(".dfb-table").waitFor({ timeout: 20_000 });
  await panes.nth(1).locator(".dfb-table").waitFor({ timeout: 20_000 });
  await page.waitForTimeout(400);
  return panes;
}

/** Captures the page (viewport) and, on mobile, records its overflow. */
async function shot(page, profile, label, overflow) {
  await page.waitForTimeout(400);
  const file = path.join(OUT, `devfiles-${profile.name}-${label}.png`);
  await page.screenshot({ path: file, fullPage: false });
  log(`captured ${path.basename(file)}`);
  if (profile.isMobile) {
    const probe = await page.evaluate(overflowProbe, profile.viewport.width);
    overflow[label] = probe;
    check(
      `${profile.name} ${label}: no horizontal overflow`,
      probe.docWidth <= probe.deviceWidth && probe.offenders.length === 0,
      probe.offenders.length ? JSON.stringify(probe.offenders.slice(0, 3)) : "",
    );
  }
}

/** Scrolls an element's top just below the sticky mobile header; scrollIntoViewIfNeeded
 * would centre it, hiding the left pane's head on a phone. */
async function scrollToTop(locator) {
  await locator.evaluate((el) => {
    el.scrollIntoView({ block: "start" });
    window.scrollBy(0, -64);
  });
}

async function dialog(page) {
  const d = page.locator('[role="dialog"]');
  await d.waitFor({ timeout: 20_000 });
  return d;
}

async function closeDialog(page, buttonName) {
  // The footer's button, not the title bar's icon button of the same name.
  await page.locator('[role="dialog"] button.aac-btn', { hasText: buttonName }).click();
  await page.locator('[role="dialog"]').waitFor({ state: "detached", timeout: 10_000 });
}

/** Deletes one file through the API. Before a pass it may already be gone. */
async function removeFile(page, rig, dir, name, mustExist, label) {
  const plan = await api(page, `/api/rigs/${rig}/files/delete/plan`, { share: SHARE, dir, names: [name] });
  if (plan.status !== 200) {
    if (mustExist || !/not_found/.test(plan.text)) check(`${label} delete plan`, false, `${plan.status} ${plan.text}`);
    return;
  }
  const p = JSON.parse(plan.text);
  const run = await api(page, `/api/rigs/${rig}/files/delete`, {
    share: SHARE,
    dir,
    items: p.items,
    dirs: [],
    acknowledged_unarchived: p.unarchived,
  });
  check(`${label}: deleted through the API`, run.status === 200 && /"summary"/.test(run.text), `${run.status}`);
}

/**
 * Removes the frame the report's copy puts on the Newtonian, so every profile
 * and every re-run plans the same copy.
 */
async function removeCopied(page, mustExist) {
  await removeFile(page, NEWT.id, "Preview/M42", NEW_ON_FRA, mustExist, "cleanup: the copied frame on the Newtonian");
}

/**
 * Uploads `size` bytes as `name` into `dir` on `rig` through the API, the SPA's
 * three calls in a row: stage, plan, run, then the discard. With `restore` set,
 * an existing file of that name is overwritten unless it already has this size
 * and time; without it, a conflict is left alone. Answers the run's item line
 * (its summary when the file was written whole), or null when nothing ran.
 */
async function apiUpload(page, rig, dir, name, size, mtimeMs, restore) {
  return page.evaluate(
    async ({ base, rig, share, dir, name, size, mtimeMs, restore }) => {
      const token = decodeURIComponent((document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/) || [])[1] || "");
      const post = (url, body, json = true) =>
        fetch(base + url, {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": json ? "application/json" : "application/octet-stream", "X-CSRF-Token": token },
          body: json ? JSON.stringify(body) : body,
        });
      const q = new URLSearchParams({ name, mtime_ms: String(mtimeMs) });
      const staged = await post(`/api/files/upload/stage?${q}`, new Uint8Array(size), false);
      if (!staged.ok) throw new Error(`stage ${name}: ${staged.status} ${await staged.text()}`);
      const { id } = await staged.json();
      try {
        const to = { rig, share, dir };
        const planRes = await post("/api/files/upload/plan", { id, to });
        if (!planRes.ok) throw new Error(`plan ${name}: ${planRes.status} ${await planRes.text()}`);
        const plan = await planRes.json();
        const c = plan.conflicts.find((x) => x.kind === "file");
        const body = { id, to };
        if (c) {
          if (!restore || (c.src_size === c.dst_size && c.src_mtime === c.dst_mtime)) return null;
          body.dst_size = c.dst_size;
          body.dst_mtime = c.dst_mtime;
        }
        const run = await post("/api/files/upload", body);
        const lines = (await run.text()).split("\n").filter(Boolean).map((l) => JSON.parse(l));
        // A file written whole has no item line; the summary counts it done.
        return lines.find((l) => l.type === "item") || lines.find((l) => l.type === "summary") || { status: run.status, lines };
      } finally {
        await post("/api/files/upload/discard", { id });
      }
    },
    { base: BASE, rig, share: SHARE, dir, name, size, mtimeMs, restore },
  );
}

/** True when apiUpload's answer says the file was written. */
const written = (line) => line.outcome === "copied" || (line.type === "summary" && line.done === 1);

/** Writes the overwritten Preview file back with its seeded size and time. */
async function restoreOverwritten(page) {
  const item = await apiUpload(page, NEWT.id, IC.dir, IC.name, IC.size, IC.mtimeMs, true);
  check("cleanup: the overwritten Preview file is restored", item === null || written(item), JSON.stringify(item));
}

/** Reverses the Rename steps' rename; before a pass there may be none to reverse. */
async function renameBack(page, mustExist) {
  const res = await api(page, `/api/rigs/${FRA.id}/files/rename`, {
    share: SHARE,
    dir: STACK.dir,
    name: STACK.renamed,
    new_name: STACK.name,
  });
  const ok = res.status >= 200 && res.status < 300;
  if (!ok && (mustExist || !/not_found/.test(res.text))) {
    check("cleanup: the rename is reversed", false, `${res.status} ${res.text}`);
  } else if (mustExist) {
    check("cleanup: the rename is reversed", true);
  }
}

/** Puts an in-memory file on a pane's hidden file input, as the picker would. */
async function pick(pane, name, size) {
  await pane.locator('input[type="file"]').setInputFiles({
    name,
    mimeType: "application/octet-stream",
    buffer: Buffer.alloc(size, 7),
  });
}

/** The pane's selection, its Rename button and that button's reason. */
async function renameButton(pane) {
  const btn = pane.getByRole("button", { name: "Rename", exact: true });
  return { btn, disabled: await btn.isDisabled(), title: (await btn.getAttribute("title")) || "" };
}

async function pass(page, profile, overflow) {
  await removeCopied(page, false);
  await removeFile(page, NEWT.id, IC.dir, NEW_FRAME, false, "cleanup: the appeared upload");
  await renameBack(page, false);
  await restoreOverwritten(page);

  // --- 1. Both panes populated. ---
  let panes = await openPanes(page, { rig: FRA.id, dir: "" }, { rig: NEWT.id, dir: "Autorun/Light/M42" });
  await scrollToTop(page.locator(".dfb-panes"));
  check(`${profile.name}: the share root badges Autorun as watched`, await panes.nth(0).getByText("Watched").count());
  check(`${profile.name}: the right pane lists the full-length frame name`, await panes.nth(1).getByText(FULL_NAME).count());
  check(
    `${profile.name}: the watched folder notice shows`,
    await panes.nth(1).getByText("Commander watches this folder").count(),
  );
  await shot(page, profile, "panes", overflow);
  // The panes run longer than a phone screen: the right one is captured on its own too.
  if (profile.isMobile) {
    await scrollToTop(panes.nth(1));
    await shot(page, profile, "panes-right", overflow);
  }

  // --- 2. The copy plan with a conflict. ---
  panes = await openPanes(page, { rig: FRA.id, dir: "Preview/M42" }, { rig: NEWT.id, dir: "Preview/M42" });
  await panes.nth(0).getByRole("checkbox", { name: "Select all" }).check();
  await panes.nth(0).getByRole("button", { name: `Copy to ${NEWT.name}` }).click();
  let d = await dialog(page);
  await d.getByText(/already exists? on/).waitFor({ timeout: 20_000 });
  check(`${profile.name}: the copy plan lists two conflicts`, await d.getByText(`2 files already exist on ${NEWT.name}`).count());
  await shot(page, profile, "copy-conflict", overflow);

  // --- 3. The report: run it with the conflicts on Skip. ---
  await d.getByRole("button", { name: /^Copy 1 file/ }).click();
  await d.getByRole("heading", { name: /^Copied 1 of / }).waitFor({ timeout: 30_000 });
  check(`${profile.name}: the report lists both skipped conflicts`, (await d.getByText("Skipped", { exact: true }).count()) === 2);
  await shot(page, profile, "copy-report", overflow);
  await closeDialog(page, "Close");
  await removeCopied(page, true);

  // --- 4. The copy plan with the watched warning. ---
  panes = await openPanes(page, { rig: FRA.id, dir: "Preview/M42" }, { rig: NEWT.id, dir: "Autorun/Light/M42" });
  await panes.nth(0).getByRole("checkbox", { name: `Select ${NEW_ON_FRA}` }).check();
  await panes.nth(0).getByRole("button", { name: `Copy to ${NEWT.name}` }).click();
  d = await dialog(page);
  await d.getByText(/will be archived as/).waitFor({ timeout: 20_000 });
  check(`${profile.name}: the copy plan warns of the watched folder`, await d.getByText(`1 frame will be archived as one of ${NEWT.name}'s frames`).count());
  await shot(page, profile, "copy-watched", overflow);
  await closeDialog(page, "Cancel");

  // --- 5. The delete dialog for the held frame. ---
  panes = await openPanes(page, { rig: FRA.id, dir: "Autorun/Light/M42" }, { rig: NEWT.id, dir: "Preview/M42" });
  await panes.nth(0).getByRole("checkbox", { name: `Select ${FULL_NAME}` }).check();
  await panes.nth(0).getByRole("button", { name: "Delete", exact: true }).click();
  d = await dialog(page);
  const box = d.getByRole("checkbox", { name: "I understand this file will be lost" });
  await box.waitFor({ timeout: 20_000 });
  check(
    `${profile.name}: the delete dialog counts the held frame as unarchived`,
    await d.getByText("1 file has no archived copy in your library").count(),
  );
  check(
    `${profile.name}: delete stays disabled until the box is ticked`,
    await d.getByRole("button", { name: /^Delete/ }).isDisabled(),
  );
  await shot(page, profile, "delete-unarchived", overflow);
  await box.check();
  await shot(page, profile, "delete-unarchived-ticked", overflow);
  await closeDialog(page, "Cancel");

  await renamePass(page, profile, overflow);
  await uploadPass(page, profile, overflow);
}

async function renamePass(page, profile, overflow) {
  // --- 6. The toolbar with Upload and Rename, the Rename dialog, a taken name and a rename. ---
  let panes = await openPanes(page, { rig: FRA.id, dir: STACK.dir }, { rig: NEWT.id, dir: IC.dir });
  let left = panes.nth(0);
  await left.getByRole("checkbox", { name: `Select ${STACK.name}` }).check();
  let r = await renameButton(left);
  check(`${profile.name}: Rename is enabled on a Preview file`, !r.disabled, r.title);
  check(
    `${profile.name}: Upload is enabled in a Preview folder`,
    !(await left.getByRole("button", { name: "Upload", exact: true }).isDisabled()),
  );
  await scrollToTop(left.locator(".dfb-toolbar"));
  await shot(page, profile, "toolbar", overflow);

  await r.btn.click();
  let d = await dialog(page);
  const field = d.getByRole("textbox", { name: "New name" });
  await field.waitFor();
  const sel = await field.evaluate((el) => ({
    focused: document.activeElement === el,
    value: el.value,
    start: el.selectionStart,
    end: el.selectionEnd,
  }));
  const stem = STACK.name.lastIndexOf(".");
  check(
    `${profile.name}: the Rename field is focused with the stem selected`,
    sel.focused && sel.value === STACK.name && sel.start === 0 && sel.end === stem,
    JSON.stringify(sel),
  );
  await shot(page, profile, "rename", overflow);

  // A taken name: the FRA300's Preview holds the M42 folder.
  await field.fill("M42");
  await d.getByRole("button", { name: "Rename", exact: true }).click();
  await d.getByText(/already has an item named M42 here/).waitFor({ timeout: 20_000 });
  check(`${profile.name}: a taken name is refused inline`, await d.getByText("Nothing was replaced.", { exact: false }).count());
  await shot(page, profile, "rename-exists", overflow);

  await field.fill(STACK.renamed);
  await d.getByRole("button", { name: "Rename", exact: true }).click();
  await page.locator('[role="dialog"]').waitFor({ state: "detached", timeout: 20_000 });
  const toast = page.locator(".aac-toast", { hasText: `Renamed ${STACK.name} to ${STACK.renamed}` });
  await toast.waitFor({ timeout: 10_000 });
  await left.getByRole("checkbox", { name: `Select ${STACK.renamed}` }).waitFor({ timeout: 20_000 });
  check(
    `${profile.name}: the renamed entry is listed and selected`,
    await left.getByRole("checkbox", { name: `Select ${STACK.renamed}` }).isChecked(),
  );
  await shot(page, profile, "rename-done", overflow);
  await renameBack(page, true);

  // --- 7a. Rename refused in the watched folder: said once, in the folder notice. ---
  panes = await openPanes(page, { rig: FRA.id, dir: "Autorun/Light/M42" }, { rig: NEWT.id, dir: IC.dir });
  left = panes.nth(0);
  await left.getByRole("checkbox", { name: `Select ${FULL_NAME}` }).check();
  r = await renameButton(left);
  check(`${profile.name}: the watched folder notice says items can't be renamed`, await left.getByText("Items here can't be renamed.").count());
  check(
    `${profile.name}: Rename is disabled in the watched folder, with its reason`,
    r.disabled && /archives this folder by name/.test(r.title),
    r.title,
  );
  check(
    `${profile.name}: no refusal line under the toolbar in the watched folder`,
    !(await left.locator(".dfb-reason", { hasText: "archives this folder by name" }).count()),
  );
  await scrollToTop(left);
  await shot(page, profile, "rename-watched", overflow);

  // --- 7b. Rename refused for the Autorun folder itself: the visible refusal line. ---
  panes = await openPanes(page, { rig: FRA.id, dir: "" }, { rig: NEWT.id, dir: IC.dir });
  left = panes.nth(0);
  await left.getByRole("checkbox", { name: "Select Autorun" }).check();
  r = await renameButton(left);
  const line = left.locator(".dfb-reason", { hasText: "a folder Commander depends on" });
  check(`${profile.name}: Rename is disabled on the Autorun folder`, r.disabled, r.title);
  check(`${profile.name}: the reserved folder's refusal shows as a visible line`, await line.count(), await line.first().textContent().catch(() => ""));
  await scrollToTop(left.locator(".dfb-toolbar"));
  await shot(page, profile, "rename-reserved", overflow);
}

async function uploadPass(page, profile, overflow) {
  // --- 8. Sending, the conflict plan, Overwrite and the toast. ---
  let panes = await openPanes(page, { rig: FRA.id, dir: STACK.dir }, { rig: NEWT.id, dir: IC.dir });
  let right = panes.nth(1);
  // The Upload button opens the picker from its click; the pass then fills the
  // hidden input directly, as the picker does.
  const [chooser] = await Promise.all([
    page.waitForEvent("filechooser", { timeout: 10_000 }),
    right.getByRole("button", { name: "Upload", exact: true }).click(),
  ]);
  check(`${profile.name}: Upload opens a single-file picker`, chooser && !chooser.isMultiple());
  // Holds the staging request back long enough to capture the Sending state.
  let release, sent;
  const held = new Promise((res) => (release = res));
  const passed = new Promise((res) => (sent = res));
  await page.route("**/api/files/upload/stage**", async (route) => {
    await held;
    await route.continue();
    sent();
  });
  await pick(right, IC.name, 200 << 10);
  let d = await dialog(page);
  await d.getByText(`Sending ${IC.name} to Commander`).waitFor({ timeout: 10_000 });
  check(`${profile.name}: Sending says nothing is written yet`, await d.getByText(`Nothing is written to ${NEWT.name} until you confirm the upload.`).count());
  await shot(page, profile, "upload-sending", overflow);
  release();
  await passed;
  await page.unroute("**/api/files/upload/stage**");

  await d.getByText(`${IC.name} already exists on ${NEWT.name}`).waitFor({ timeout: 20_000 });
  const radios = d.getByRole("radiogroup", { name: `${IC.name}: overwrite or skip` });
  check(`${profile.name}: the upload conflict defaults to Skip`, await radios.getByRole("radio", { name: "Skip" }).isChecked());
  check(`${profile.name}: no archive wording for a Preview upload`, !(await d.getByText(/archived/).count()));
  await shot(page, profile, "upload-conflict", overflow);

  await radios.getByRole("radio", { name: "Overwrite" }).check();
  const go = d.getByRole("button", { name: "Upload and overwrite" });
  check(`${profile.name}: Overwrite relabels the button`, await go.count());
  await shot(page, profile, "upload-overwrite", overflow);
  await go.click();
  // An unarchived or unknown destination asks first, inline.
  const confirm = d.locator(".dfb-confirm");
  if (await confirm.waitFor({ timeout: 3_000 }).then(() => true, () => false)) {
    await shot(page, profile, "upload-overwrite-confirm", overflow);
    await confirm.getByRole("button", { name: "Overwrite" }).click();
  }
  await page.locator('[role="dialog"]').waitFor({ state: "detached", timeout: 30_000 });
  const toast = page.locator(".aac-toast", { hasText: `Uploaded ${IC.name} (200.0 KB) to ${NEWT.name}` });
  await toast.waitFor({ timeout: 10_000 });
  check(`${profile.name}: the upload ends with its toast`, await toast.count());
  await shot(page, profile, "upload-done", overflow);
  await restoreOverwritten(page);

  // --- 9. The watched warning: an upload into the Newtonian's Autorun. ---
  panes = await openPanes(page, { rig: FRA.id, dir: STACK.dir }, { rig: NEWT.id, dir: "Autorun/Light/M42" });
  right = panes.nth(1);
  await pick(right, NEW_FRAME, 160 << 10);
  d = await dialog(page);
  await d.getByText(/will be archived as/).waitFor({ timeout: 20_000 });
  check(`${profile.name}: the upload plan warns of the watched folder`, await d.getByText(`1 frame will be archived as one of ${NEWT.name}'s frames`).count());
  await shot(page, profile, "upload-watched", overflow);
  await closeDialog(page, "Cancel");

  // --- 10. A reserved path refuses the plan. ---
  panes = await openPanes(page, { rig: FRA.id, dir: STACK.dir }, { rig: NEWT.id, dir: "" });
  right = panes.nth(1);
  await pick(right, "Autorun", 4 << 10);
  d = await dialog(page);
  const refusal = d.getByText(`Autorun would stand where ${SHARE}/Autorun must go.`, { exact: false });
  await refusal.waitFor({ timeout: 20_000 });
  check(`${profile.name}: the reserved-path refusal leads with its fix`, await d.getByText(/Rename Autorun on your computer and choose it again/).count());
  check(`${profile.name}: the refused plan offers no Upload`, !(await d.getByRole("button", { name: "Upload", exact: true }).count()));
  await shot(page, profile, "upload-reserved", overflow);
  await closeDialog(page, "Close");

  // --- 11. A failed row: the file appears between the plan and the run. ---
  panes = await openPanes(page, { rig: FRA.id, dir: STACK.dir }, { rig: NEWT.id, dir: IC.dir });
  right = panes.nth(1);
  await pick(right, NEW_FRAME, 160 << 10);
  d = await dialog(page);
  const upload = d.getByRole("button", { name: "Upload", exact: true });
  await upload.waitFor({ timeout: 20_000 });
  const raced = await apiUpload(page, NEWT.id, IC.dir, NEW_FRAME, 8 << 10, IC.mtimeMs, false);
  check(`${profile.name}: the racing upload lands first`, raced && written(raced), JSON.stringify(raced));
  await upload.click();
  await d.getByText(`Appeared on ${NEWT.name} after the check, so it was not overwritten.`).waitFor({ timeout: 30_000 });
  check(`${profile.name}: the appeared row titles the result`, await d.getByText(`${NEW_FRAME} was not uploaded to ${NEWT.name}`).count());
  check(`${profile.name}: the appeared row offers Overwrite`, await d.getByRole("button", { name: /Overwrite/ }).count());
  await shot(page, profile, "upload-appeared", overflow);
  await closeDialog(page, "Close");
  await removeFile(page, NEWT.id, IC.dir, NEW_FRAME, true, "cleanup: the appeared upload");

  // --- 12. A failed row: the connection drops on the run, then Retry writes the
  // staged file without sending it again. ---
  panes = await openPanes(page, { rig: FRA.id, dir: STACK.dir }, { rig: NEWT.id, dir: IC.dir });
  right = panes.nth(1);
  let stages = 0;
  const countStage = (req) => {
    if (req.method() === "POST" && new URL(req.url()).pathname === "/api/files/upload/stage") stages++;
  };
  page.on("request", countStage);
  // The run request alone fails at the network, once: the interrupted path.
  await page.route(
    (u) => u.pathname === "/api/files/upload",
    (route) => route.abort("connectionreset"),
    { times: 1 },
  );
  await pick(right, NEW_FRAME, 160 << 10);
  d = await dialog(page);
  await d.getByRole("button", { name: "Upload", exact: true }).click();
  await d.getByText("Upload interrupted").first().waitFor({ timeout: 30_000 });
  const retry = d.getByRole("button", { name: `Retry ${NEW_FRAME}` });
  check(`${profile.name}: the interrupted row offers Retry`, await retry.count());
  await shot(page, profile, "upload-failed-row", overflow);
  await retry.click();
  await retry.waitFor({ state: "detached", timeout: 30_000 });
  page.off("request", countStage);
  check(`${profile.name}: Retry writes without sending the file again`, stages === 1, `${stages} staging requests`);
  await shot(page, profile, "upload-retried", overflow);
  await closeDialog(page, "Close");
  await removeFile(page, NEWT.id, IC.dir, NEW_FRAME, true, "cleanup: the retried upload");
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  const overflow = {};
  try {
    for (const profile of PROFILES) {
      const { ctx, page } = await login(browser, profile);
      try {
        await pass(page, profile, overflow);
      } catch (err) {
        check(`${profile.name}: pass ran to the end`, false, String(err));
        await page.screenshot({ path: path.join(OUT, `devfiles-${profile.name}-error.png`) });
      }
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
  fs.writeFileSync(path.join(OUT, "devfiles-overflow-mobile.json"), JSON.stringify(overflow, null, 2));
  const failed = results.filter((r) => !r.ok);
  console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
  process.exit(failed.length ? 1 : 0);
})();
