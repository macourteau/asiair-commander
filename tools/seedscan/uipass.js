// Headless browser pass for the network-scan device count.
//
// Two scans against a server seeded by tools/seedscan: one with both fake
// devices answering, one after the managed device has gone quiet and aged past
// the 135s staleness window. Between them the only thing that changed is that a
// device stopped answering while staying in the registry — so the toast's count
// must fall by exactly one while the registry still lists it.
//
// The assertion is on the change, not on an absolute number, because a real
// ASIAIR on the same LAN answers these broadcasts too and legitimately adds to
// every round.
//
// Playwright lives in the GLOBAL node_modules on this machine.
//   go run ./tools/seedscan tmp/demo           # foreground, answers scan_air
//   make demo-up TAGS=                         # another terminal; :8789
//   OUT=<dir> AC_DATA_DIR=tmp/demo NODE_PATH=$(npm root -g) node tools/seedscan/uipass.js
//   make demo-down
const { chromium } = require("playwright");
const fs = require("fs");
const path = require("path");

const OUT = process.env.OUT || ".";
const BASE = process.env.TARGET || "http://localhost:8789";
const PASSWORD = process.env.AC_PASSWORD || "12345678";
const DATA_DIR = process.env.AC_DATA_DIR;

// The guids tools/seedscan answers with.
const MANAGED_GUID = "f4a00001";
const SPARE_GUID = "5pa00002";
const NAMELESS_GUID = "0no00003";
// The ssid spareDev answers with, and the model every fake reports. Both fakes
// share the model, as same-generation hardware does, so only the ssid tells them
// apart — the reason naming leads with it.
const SPARE_SSID = "MyRig";
const SHARED_MODEL = "ZWO AirPlus-RK3568 (Linux)";
// stalenessWindow in internal/discovery, plus a margin for the scan round itself.
const STALE_WAIT_MS = 150_000;

const results = [];
function check(name, cond, detail = "") {
  results.push({ name, ok: !!cond, detail });
  console.log(`${cond ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}

/** Press "Scan network" and return the toast it raises, waiting for a fresh one. */
async function scan(page, label) {
  // Let any earlier toast expire so the one read below is unambiguously this
  // round's — they stack, and the newest is last in the DOM.
  await page.waitForFunction(() => document.querySelectorAll(".aac-toast").length === 0, {
    timeout: 30_000,
  });
  await page.getByRole("button", { name: "Scan network" }).click();
  const toast = page.locator(".aac-toast").last();
  await toast.waitFor({ timeout: 30_000 });
  const text = (await toast.innerText()).replace(/\s+/g, " ").trim();
  console.log(`  [${label}] toast: ${text}`);
  await page.screenshot({ path: path.join(OUT, `scan-${label}.png`), fullPage: false });
  return text;
}

/** The count the toast states, or null when it says nothing answered. */
function answeredCount(toast) {
  if (/no devices answered/.test(toast)) return 0;
  const m = toast.match(/(\d+) devices? answered/);
  return m ? Number(m[1]) : null;
}

/**
 * identifyPass checks that a found device can be told apart from an identical
 * sibling, and that the Add-a-rig dialog says which kit it is about to configure.
 *
 * The fakes answer discovery only — they serve no SMB — so the real share probe
 * fails here, which is itself the case worth capturing: a device that cannot be
 * reached must still be configurable by hand. The populated selector is then
 * captured with the shares endpoint stubbed at the network boundary, since
 * standing up an SMB server is not what this seeder is for.
 */
async function identifyPass(page, profile) {
  const tag = (name) => `identify-${profile}-${name}`;
  // The dialog fades in, so a shot taken the instant it exists catches it
  // half-transparent over the page behind it. Settle first.
  const shot = async (name) => {
    await page.waitForTimeout(400);
    await page.screenshot({ path: path.join(OUT, `${tag(name)}.png`), fullPage: true });
  };

  const spareCard = page.locator(".rig-item", { hasText: SPARE_SSID });
  const namelessCard = page.locator(".rig-item", { hasText: NAMELESS_GUID.slice(0, 8) });
  await spareCard.waitFor({ timeout: 15_000 });
  await shot("cards");

  check(
    `${profile}: the found card is titled by the device's Wi-Fi name`,
    (await spareCard.locator(".rig-item__title").innerText()).includes(SPARE_SSID),
    await spareCard.locator(".rig-item__title").innerText(),
  );
  const spareFacts = await spareCard.locator(".rig-item__facts").innerText();
  check(
    `${profile}: the card still shows the address, model and short guid`,
    spareFacts.includes("10.77.0.12") &&
      spareFacts.includes(SHARED_MODEL) &&
      spareFacts.includes(SPARE_GUID),
    spareFacts.replace(/\s+/g, " "),
  );
  check(
    `${profile}: a device with no ssid falls back to the model-plus-guid label`,
    (await namelessCard.locator(".rig-item__title").innerText()).includes(SHARED_MODEL),
    await namelessCard.locator(".rig-item__title").innerText(),
  );

  // --- The dialog, with the real (failing) probe behind it. ---
  await spareCard.getByRole("button", { name: "Set up this rig" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.waitFor({ timeout: 15_000 });
  await shot("dialog-unprobed");

  const name = await dialog.getByLabel("Rig name").inputValue();
  check(
    `${profile}: the rig name is prefilled with the Wi-Fi name, not the shared model`,
    name === SPARE_SSID,
    name,
  );
  const dialogText = (await dialog.innerText()).replace(/\s+/g, " ");
  // The guid sits in a read-only input, whose value innerText does not carry.
  const shownGuid = await dialog.getByRole("textbox", { name: "Device" }).inputValue();
  check(
    `${profile}: the dialog names the kit — Wi-Fi name, address, model and guid`,
    dialogText.includes(SPARE_SSID) &&
      dialogText.includes("10.77.0.12") &&
      dialogText.includes(SHARED_MODEL) &&
      shownGuid === SPARE_GUID,
    `guid=${shownGuid} | ${dialogText.slice(0, 240)}`,
  );
  const probeButton = dialog.getByRole("button", { name: /shares on this device/ });
  check(
    `${profile}: an unprobed dialog offers to find the device's shares`,
    await probeButton.isVisible(),
  );
  check(
    `${profile}: the share name stays typeable when nothing has been probed`,
    await dialog.locator('input[placeholder="Udisk Images"]').isVisible(),
  );
  check(
    `${profile}: the USB-primary advice is still on the shares fieldset`,
    /watch both the Udisk and EMMC shares/i.test(dialogText),
  );

  // --- The same dialog with the probe answering. ---
  await page.route("**/api/discovered/*/shares", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ shares: ["Udisk Images", "EMMC Images"] }),
    }),
  );
  await probeButton.click();
  const select = dialog.locator("select.aac-select").first();
  await select.waitFor({ timeout: 15_000 });
  await shot("dialog-probed");
  const options = await select.locator("option").allInnerTexts();
  check(
    `${profile}: the share field becomes a selector over what the device listed`,
    options.includes("Udisk Images") && options.includes("EMMC Images"),
    options.join(" | "),
  );
  const picked = await dialog.locator("select.aac-select").evaluateAll((els) =>
    els.map((e) => e.value),
  );
  check(
    `${profile}: an untouched list is filled with every share found, not just one`,
    picked.length === 2 &&
      picked.includes("Udisk Images") &&
      picked.includes("EMMC Images"),
    picked.join(" | "),
  );
  check(
    `${profile}: the selector keeps a way out to free text`,
    options.some((o) => /Type a name/.test(o)),
    options.join(" | "),
  );
  check(
    `${profile}: the probe reports what it found`,
    /Found 2 shares on this device/.test((await dialog.innerText()).replace(/\s+/g, " ")),
  );

  // Choosing "Type a name…" must hand back a real text input.
  await select.selectOption({ label: "Type a name…" });
  await dialog.locator('input[placeholder="Udisk Images"]').first().waitFor({ timeout: 5_000 });
  await shot("dialog-free-text");
  check(
    `${profile}: choosing to type a name restores the free-text input`,
    await dialog.locator('input[placeholder="Udisk Images"]').first().isVisible(),
  );

  await page.unroute("**/api/discovered/*/shares");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await dialog.waitFor({ state: "hidden", timeout: 15_000 });
}

/**
 * editPass covers the other way into the dialog: editing a rig that is already
 * set up. Such a rig binds by guid and stores neither an address nor a Wi-Fi
 * name, so both have to come from the device's last scan reply — and the share
 * probe has to be reachable here too, since a rig configured once can need its
 * shares checked again later.
 */
async function editPass(page) {
  await page.getByRole("button", { name: "Edit" }).first().click();
  const dialog = page.getByRole("dialog");
  await dialog.waitFor({ timeout: 15_000 });
  await page.waitForTimeout(400);
  await page.screenshot({ path: path.join(OUT, "identify-edit-dialog.png"), fullPage: true });

  const text = (await dialog.innerText()).replace(/\s+/g, " ");
  check(
    "edit: a configured rig's dialog shows the live Wi-Fi name and address from the scan",
    text.includes("FRA300-Pier") && text.includes("10.77.0.11") && text.includes(SHARED_MODEL),
    text.slice(0, 260),
  );
  check(
    "edit: the shares can be re-checked from the edit dialog",
    await dialog.getByRole("button", { name: /shares on this device/ }).isVisible(),
  );
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await dialog.waitFor({ state: "hidden", timeout: 15_000 });
}

/** The registry as the server reports it, read through the page's session. */
async function registry(page, base) {
  return page.evaluate(async (b) => {
    const res = await fetch(`${b}/api/discovered`, { credentials: "include" });
    return res.json();
  }, base);
}

(async () => {
  if (!DATA_DIR) {
    console.error("AC_DATA_DIR is required — the pass writes the quiet marker into it.");
    process.exit(2);
  }

  const browser = await chromium.launch({ headless: true });

  /** Log a fresh page in and land it on Devices. */
  async function devicesPage(viewport, deviceScaleFactor) {
    const ctx = await browser.newContext({ viewport, deviceScaleFactor });
    const page = await ctx.newPage();
    page.on("console", (m) => {
      if (m.type() === "error") console.log("  [console.error]", m.text());
    });
    // networkidle never settles (the SSE stream stays open), so every wait is
    // domcontentloaded plus an explicit element wait.
    await page.goto(`${BASE}/login`, { waitUntil: "domcontentloaded" });
    await page.waitForSelector('input[type="password"]');
    await page.fill('input[type="password"]', PASSWORD);
    await Promise.all([
      page.waitForURL((u) => !u.pathname.startsWith("/login")),
      page.click('button[type="submit"]'),
    ]);
    await page.goto(`${BASE}/devices`, { waitUntil: "domcontentloaded" });
    await page.waitForSelector("text=Found on your network");
    return { ctx, page };
  }

  const { ctx, page } = await devicesPage({ width: 1440, height: 900 }, 2);

  // --- Round one: both fake devices answering. ---
  const first = await scan(page, "1-both-answering");
  const before = await registry(page, BASE);
  const firstCount = answeredCount(first);
  check(
    "round 1: both seeded devices answered",
    before.some((d) => d.guid === MANAGED_GUID && d.online) &&
      before.some((d) => d.guid === SPARE_GUID && d.online),
    JSON.stringify(before.map((d) => ({ guid: d.guid, online: d.online, managed: d.managed }))),
  );
  check("round 1: the toast states a count", firstCount !== null, first);
  check(
    "round 1: the unadopted device is reported as not set up yet",
    /not set up yet/.test(first),
    first,
  );

  // --- Telling the found devices apart, on both device profiles. ---
  await identifyPass(page, "desktop");
  await editPass(page);
  const mobile = await devicesPage({ width: 393, height: 852 }, 3);
  await identifyPass(mobile.page, "mobile");
  const overflow = await mobile.page.evaluate(() => ({
    docWidth: document.documentElement.scrollWidth,
    viewport: window.innerWidth,
  }));
  check(
    "mobile: the devices page does not scroll horizontally",
    overflow.docWidth <= overflow.viewport + 1,
    `${overflow.docWidth}px content in a ${overflow.viewport}px viewport`,
  );
  await mobile.ctx.close();

  // --- The managed device is powered off. ---
  fs.writeFileSync(path.join(DATA_DIR, "quiet"), "");
  console.log(`  ${MANAGED_GUID} silenced; waiting ${STALE_WAIT_MS / 1000}s for it to age out of the staleness window`);
  await page.waitForTimeout(STALE_WAIT_MS);

  // --- Round two: it is still in the registry, but it did not answer. ---
  const second = await scan(page, "2-managed-powered-off");
  const after = await registry(page, BASE);
  const secondCount = answeredCount(second);
  const stale = after.find((d) => d.guid === MANAGED_GUID);
  check(
    "round 2: the powered-off managed device is still in the registry, marked offline",
    stale && stale.online === false,
    JSON.stringify(after.map((d) => ({ guid: d.guid, online: d.online, managed: d.managed }))),
  );
  check(
    "round 2: the unadopted device is still answering",
    after.some((d) => d.guid === SPARE_GUID && d.online),
  );
  check(
    "round 2: the count dropped by exactly one — the retained device is not counted as having answered",
    firstCount !== null && secondCount !== null && secondCount === firstCount - 1,
    `${firstCount} → ${secondCount}`,
  );

  await ctx.close();
  await browser.close();
  const failed = results.filter((r) => !r.ok);
  console.log(`\n=== ${results.length - failed.length}/${results.length} checks passed ===`);
  process.exit(failed.length ? 1 : 0);
})().catch((e) => {
  console.error("SCRIPT ERROR:", e);
  process.exit(2);
});
