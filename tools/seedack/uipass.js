// Headless browser pass for the missing panel: acknowledging culled frames (design
// library-reconciliation §10) and triaging and forgetting leftovers (§13, AC-T5).
// Drives the real SPA against the live seeded server, asserts DOM, and screenshots.
// It changes the seeded state (it acknowledges and forgets), so reseed before a
// second run: make demo-down && make demo-up SEED=seedack.
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
  await fraCard.getByRole("button", { name: /^Missing frames\b/ }).click();
  await page.waitForTimeout(700);
  await shot(page, "02-fra-missing-panel.png");

  // --- triage layout: actionable section first, leftovers collapsed ---
  const actionable = fraCard.locator("section.missing-section");
  const leftovers = fraCard.locator("details.missing-leftovers");
  const leadsFirst = await page.evaluate(([a, l]) => !!(a && l && (a.compareDocumentPosition(l) & Node.DOCUMENT_POSITION_FOLLOWING)),
    [await actionable.elementHandle(), await leftovers.elementHandle()]);
  check("actionable section leads the leftovers", leadsFirst);
  check("actionable heading names its count", /Still on the device \(4\)/.test(await actionable.innerText()));
  check("leftovers start collapsed", !(await leftovers.evaluate((d) => d.open)));
  check("leftover summary names its count", /Already gone from the device \(60\)/.test(await leftovers.locator("summary").innerText()));

  // The group button names the acknowledgeable count (2), not the folder's 3 rows,
  // and a folder with nothing acknowledgeable (M31, one held row) has no button.
  const ackAll = actionable.getByRole("button", { name: /^Acknowledge all/ });
  check("group button 'Acknowledge all 2' present", await actionable.getByRole("button", { name: "Acknowledge all 2" }).count() === 1, "ackable count");
  check("no 'Acknowledge all 3' (the folder's raw count)", await actionable.getByRole("button", { name: /Acknowledge all\s*3/ }).count() === 0);
  check("only the acknowledgeable folder offers Acknowledge all", await ackAll.count() === 1, `found ${await ackAll.count()}`);
  // Held row (SII) must offer no per-row Acknowledge — only 2 per-row Acknowledge
  // buttons (Ha, OIII) exist despite 3 missing rows.
  const perRowAck = await fraCard.getByRole("button", { name: /^Acknowledge$/ }).count();
  check("exactly 2 per-row Acknowledge buttons (held row has none)", perRowAck === 2, `found ${perRowAck}`);

  // --- folder click filters the table and shows a removable chip ---
  await actionable.getByRole("button", { name: /M31\/NIGHT_2/ }).click();
  await page.waitForTimeout(600);
  await shot(page, "02b-folder-filter.png");
  const filteredRows = await actionable.locator("tbody tr").allInnerTexts();
  check("folder click filters the table to that folder", filteredRows.length === 1 && /M31/.test(filteredRows[0]), `${filteredRows.length} rows`);
  const chip = actionable.getByRole("button", { name: /Show every folder/ });
  check("the active folder shows as a chip", await chip.count() === 1);
  await chip.click();
  await page.waitForTimeout(600);
  check("removing the chip shows every folder's rows", await actionable.locator("tbody tr").count() === 4 && await chip.count() === 0);

  // --- group dialog: count (2) matches the button, with skipped + dir_gone ---
  await actionable.getByRole("button", { name: "Acknowledge all 2" }).click();
  await page.waitForTimeout(500);
  await shot(page, "03-group-dialog.png");
  const dlg = page.getByRole("dialog");
  const dlgText = await dlg.innerText();
  check("group dialog title says 'Delete 2 frames', as the button", /Delete\s*2\s*frames/i.test(dlgText), "acknowledgeable count");
  check("group dialog shows skipped breakdown, leftovers apart", /Not included: 2 already removed from the device, 1 not verified/i.test(dlgText), dlgText.match(/Not included[^\n]*/)?.[0] || "");
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

  // --- leftovers: expand, search across pages, Forget by age, folder, section ---
  const leftSummary = leftovers.locator("summary");
  await leftSummary.click();
  await page.waitForTimeout(800);
  await shot(page, "07-leftovers-expanded.png");
  const leftPager = leftovers.locator(".pager__status");
  check("expanded leftovers page the section", /60 leftovers · page 1 of 2/.test(await leftPager.innerText()), await leftPager.innerText());
  check("leftover rows offer Forget and Restore, no Acknowledge",
    await leftovers.getByRole("button", { name: /^Forget$/ }).count() === 50 &&
    await leftovers.getByRole("button", { name: /^Restore$/ }).count() === 50 &&
    await leftovers.getByRole("button", { name: /^Acknowledge/ }).count() === 0);
  check("leftover folders carry Forget all N",
    await leftovers.locator("li.missing-folder", { hasText: "IC 434" }).getByRole("button", { name: "Forget all 55" }).count() === 1);

  // M42's three rows sort after IC 434's 55, so they start on page 2.
  const leftSearch = leftovers.getByRole("searchbox");
  await leftSearch.fill("m42");
  await page.waitForTimeout(900);
  await shot(page, "08-leftovers-search.png");
  const searched = await leftovers.locator("tbody tr").allInnerTexts();
  check("search filters across pages", searched.length === 3 && searched.every((t) => /M42/.test(t)) &&
    /3 leftovers · page 1 of 1/.test(await leftPager.innerText()), `${searched.length} rows; ${await leftPager.innerText()}`);
  await leftSearch.fill("");
  await page.waitForTimeout(900);

  async function forget(button, expectTitle, name) {
    await button.click();
    await page.waitForTimeout(500);
    const d = page.getByRole("dialog");
    const t = await d.innerText();
    await shot(page, name);
    check(`forget dialog states the server's count (${expectTitle})`, t.includes(expectTitle), t.split("\n")[0]);
    check("forget dialog states the cost", /pulls it back to the NAS on its next pass/.test(t));
    await d.getByRole("button", { name: /^Forget$/ }).click();
    await page.waitForTimeout(1000);
  }
  await forget(leftovers.getByRole("button", { name: "Forget older than 7 days" }), "Forget 38 frames?", "09-forget-older-7.png");
  check("Forget older than 7 days keeps the recent leftovers", /Already gone from the device \(22\)/.test(await leftSummary.innerText()), await leftSummary.innerText());
  check("recent leftovers in the shared folder remain",
    await leftovers.locator("li.missing-folder", { hasText: "NGC 7000" }).getByRole("button", { name: "Forget all 2" }).count() === 1);

  await forget(leftovers.locator("li.missing-folder", { hasText: "IC 434" }).getByRole("button", { name: /^Forget all/ }),
    "Forget 19 frames?", "10-forget-folder.png");
  check("Forget all on a folder empties it", await leftovers.locator("li.missing-folder", { hasText: "IC 434" }).count() === 0 &&
    /Already gone from the device \(3\)/.test(await leftSummary.innerText()), await leftSummary.innerText());

  await forget(leftovers.getByRole("button", { name: "Forget all", exact: true }), "Forget 3 frames?", "11-forget-section.png");
  await shot(page, "12-leftovers-empty.png");
  check("Forget all empties the section", /Already gone from the device \(0\)/.test(await leftSummary.innerText()) &&
    await leftovers.locator("li.missing-folder").count() === 0, await leftSummary.innerText());
  check("the actionable rows are untouched by Forget", /Still on the device \(4\)/.test(await actionable.innerText()));

  // --- rig-keep: group count shown, but NO Acknowledge control ---
  const keepCard = page.locator(".rig-item", { hasText: "Keep-Originals Rig" });
  await keepCard.getByRole("button", { name: /^Missing frames\b/ }).click();
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
