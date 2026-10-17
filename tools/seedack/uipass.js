// Headless browser pass for the acknowledge-culled-frames UI (design
// library-reconciliation §10). Drives the real SPA against the live seeded server,
// asserts DOM, and screenshots.
const { chromium } = require("playwright");
const path = require("path");

const OUT = process.env.OUT || ".";
const BASE = "http://localhost:8686";
const results = [];
function check(name, cond, detail = "") {
  results.push({ name, ok: !!cond, detail });
  console.log(`${cond ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}
const shot = (page, n) => page.screenshot({ path: path.join(OUT, n), fullPage: false });

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
  page.on("console", (m) => { if (m.type() === "error") console.log("  [console.error]", m.text()); });

  // --- login ---
  await page.goto(BASE, { waitUntil: "networkidle" });
  await page.locator('input[type="password"]').fill("12345678");
  await page.getByRole("button", { name: /sign in/i }).click();
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(400);
  // Client-side nav (a hard reload drops the SPA session here).
  await page.getByRole("link", { name: /^Devices$/ }).click();
  await page.waitForTimeout(700);
  await shot(page, "01-devices.png");
  check("logged in, on /devices", await page.getByText(/FRA300 Rig/).count() > 0);

  // --- open rig-fra missing panel ---
  const fraCard = page.locator(".rig-item", { hasText: "FRA300 Rig" });
  await fraCard.getByRole("button", { name: /^Missing frames$/ }).click();
  await page.waitForTimeout(700);
  await shot(page, "02-fra-missing-panel.png");

  const panelText = await fraCard.innerText();
  // Group button uses the raw per-dir count (3), not the acknowledgeable count.
  check("group button 'Acknowledge all 3' present", /Acknowledge all\s*3/.test(panelText), "raw dir count");
  // Held row (SII) must offer no per-row Acknowledge — only 2 per-row Acknowledge
  // buttons (Ha, OIII) exist despite 3 missing rows.
  const perRowAck = await fraCard.getByRole("button", { name: /^Acknowledge$/ }).count();
  check("exactly 2 per-row Acknowledge buttons (held row has none)", perRowAck === 2, `found ${perRowAck}`);

  // --- group dialog: count (2) differs from button (3), with skipped + dir_gone ---
  await fraCard.getByRole("button", { name: /Acknowledge all\s*3/ }).click();
  await page.waitForTimeout(500);
  await shot(page, "03-group-dialog.png");
  const dlg = page.getByRole("dialog");
  const dlgText = await dlg.innerText();
  check("group dialog title says 'Delete 2 frames'", /Delete\s*2\s*frames/i.test(dlgText), "acknowledgeable count, not 3");
  check("group dialog shows skipped breakdown", /Not included/i.test(dlgText), dlgText.match(/Not included[^\n]*/)?.[0] || "");
  check("group dialog leads with dir_gone notice", /folder itself is gone/i.test(dlgText));
  check("dialog asks the discriminating deleted-not-moved question", /were\s*deleted/i.test(dlgText) && /moved or renamed/i.test(dlgText));
  // cancel (keep waiting)
  await dlg.getByRole("button", { name: /keep waiting/i }).click();
  await page.waitForTimeout(300);

  // --- per-row acknowledge → dialog → confirm → pill ---
  await fraCard.getByRole("button", { name: /^Acknowledge$/ }).first().click();
  await page.waitForTimeout(500);
  await shot(page, "04-frame-dialog.png");
  const fdlg = page.getByRole("dialog");
  check("per-frame dialog title 'Delete this frame from the device?'", /Delete this frame from the device/i.test(await fdlg.innerText()));
  check("per-frame dialog names the device original path", /Light_(Ha|OIII)_/.test(await fdlg.innerText()));
  await fdlg.getByRole("button", { name: /delete from device/i }).click();
  await page.waitForTimeout(900);
  await shot(page, "05-after-confirm-pill.png");
  const after = await fraCard.innerText();
  check("an 'Acknowledged' pill appears after confirm", /Acknowledged/.test(after));
  check("one per-row Acknowledge button remains (was 2, one acked)", await fraCard.getByRole("button", { name: /^Acknowledge$/ }).count() === 1);

  // --- rig-keep: group count shown, but NO Acknowledge control ---
  const keepCard = page.locator(".rig-item", { hasText: "Keep-Originals Rig" });
  await keepCard.getByRole("button", { name: /^Missing frames$/ }).click();
  await page.waitForTimeout(700);
  await shot(page, "06-keep-originals-no-controls.png");
  const keepText = await keepCard.innerText();
  check("keep_originals rig still shows the missing group (count visible)", /IC 1805|NIGHT_5|·\s*1/.test(keepText));
  // Count actual buttons (the panel's explanatory notice contains the word
  // "Acknowledge" but is not a control) — there must be zero.
  const keepAckBtns = await keepCard.getByRole("button", { name: /Acknowledge/ }).count();
  check("keep_originals rig shows NO Acknowledge control (button count 0)", keepAckBtns === 0, `found ${keepAckBtns}`);

  await browser.close();
  const failed = results.filter((r) => !r.ok);
  console.log(`\n=== ${results.length - failed.length}/${results.length} checks passed ===`);
  process.exit(failed.length ? 1 : 0);
})().catch((e) => { console.error("SCRIPT ERROR:", e); process.exit(2); });
