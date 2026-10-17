// Headless browser pass for the retarget preview's night projection.
//
// The preview card only exists after an operator selects frames on Tools, types a
// target and clicks Preview, so scripts/screenshots.mjs — which drives one URL per
// surface — can never reach it. This drives the real SPA against a server seeded by
// tools/seednight, asserts the rendered sub-lines, and captures the card on both
// device profiles.
//
// Playwright lives in the GLOBAL node_modules on this machine.
//   go run ./tools/seednight /tmp/acnight
//   go run ./cmd/asiair-commander --data-dir /tmp/acnight run     # :8788
//   OUT=<dir> NODE_PATH=$(npm root -g) node tools/seednight/uipass.js
const { chromium } = require("playwright");
const path = require("path");

const OUT = process.env.OUT || ".";
const BASE = process.env.TARGET || "http://localhost:8788";
const PASSWORD = process.env.AC_PASSWORD || "12345678";

const results = [];
function check(name, cond, detail = "") {
  results.push({ name, ok: !!cond, detail });
  console.log(`${cond ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}

const IPHONE_16_PRO = {
  viewport: { width: 393, height: 852 },
  deviceScaleFactor: 3,
  isMobile: true,
  hasTouch: true,
  userAgent:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 " +
    "(KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
};
const DESKTOP = { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 };

// The same probe scripts/screenshots.mjs uses, so a mobile reading here is
// comparable with the ones in the standard report.
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
  return { deviceWidth, innerWidth, scrollWidth, overflow: widest > deviceWidth, overflowBy: widest - deviceWidth, topOffenders: offenders.slice(0, 5) };
}

async function run(browser, device) {
  const ctx = await browser.newContext(device.profile);
  const page = await ctx.newPage();
  page.on("console", (m) => {
    if (m.type() === "error") console.log(`  [${device.name} console.error]`, m.text());
  });

  // networkidle never settles (the SSE stream stays open), so every wait below is
  // domcontentloaded plus an explicit element wait.
  await page.goto(`${BASE}/login`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector('input[type="password"]');
  await page.fill('input[type="password"]', PASSWORD);
  await Promise.all([
    page.waitForURL((u) => !u.pathname.startsWith("/login")),
    page.click('button[type="submit"]'),
  ]);
  await page.waitForSelector("nav, main, h1");

  await page.goto(`${BASE}/tools`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("table tbody tr");
  const rows = page.locator("table tbody tr");
  await page.waitForFunction(() => document.querySelectorAll("table tbody tr").length === 9);

  // Rows come back newest id first: the three mis-labelled NGC 7830 frames, then the
  // NGC7380 archive. Index 8 is the oldest NGC7380 frame, selected so the card also
  // carries an ineligible row for contrast.
  const wanted = [0, 1, 2, 8];
  for (const i of wanted) {
    await rows.nth(i).locator('input[type="checkbox"]').check();
  }
  check(
    `${device.name}: selected the three mis-labelled frames plus one already on target`,
    (await page.getByText("4 selected of 9 shown").count()) > 0,
  );

  await page.getByLabel("New target").fill("NGC7380");
  // Two cards on this page have a Preview button — Retarget's comes first in the
  // DOM, Re-file's is further down.
  await page.getByRole("button", { name: "Preview" }).first().click();
  await page.waitForSelector(".retarget-preview__item");
  await page.waitForFunction(
    () => document.querySelectorAll(".retarget-preview__item").length === 4,
  );

  const card = page.locator(".retarget-preview").locator("..");
  await card.scrollIntoViewIfNeeded();
  await page.waitForTimeout(400);

  const items = page.locator(".retarget-preview__item");
  const texts = [];
  for (let i = 0; i < 4; i++) texts.push((await items.nth(i).innerText()).replace(/\s+/g, " "));
  console.log(`\n--- ${device.name}: preview rows as rendered ---`);
  texts.forEach((t, i) => console.log(`  [${i}] ${t}`));
  console.log("---\n");

  check(
    `${device.name}: NIGHT_4 row carries the shift caveat`,
    /NIGHT_4/.test(texts[0]) &&
      /Night number may change — another frame filed before you apply can shift it\./.test(texts[0]),
  );
  check(
    `${device.name}: NIGHT_3 row names the spelling already filed there`,
    /NIGHT_3/.test(texts[1]) &&
      /Already filed here as "NGC 7380" — applying will leave two spellings of this target in one folder\./.test(
        texts[1],
      ),
  );
  check(
    `${device.name}: NIGHT_3 row also carries the caveat (an adoption can still shift)`,
    /Night number may change/.test(texts[1]),
  );
  check(
    `${device.name}: NIGHT_1 row is the ledger-exact one — no caveat, no spelling line`,
    /NIGHT_1/.test(texts[2]) &&
      !/Night number may change/.test(texts[2]) &&
      !/Already filed here/.test(texts[2]),
  );
  check(
    `${device.name}: the ineligible row states its refusal`,
    /Ineligible/.test(texts[3]) && /new target equals the current target/.test(texts[3]),
  );
  const all = texts.join(" ");
  check(`${device.name}: no NIGHT_0 anywhere in the card`, !/NIGHT_0/.test(all));

  const pageText = await page.locator("main").innerText();
  check(`${device.name}: no NIGHT_0 anywhere on the page`, !/NIGHT_0/.test(pageText));

  // The colour the sub-lines actually resolve to, so "informational, not an error"
  // is a measurement rather than an impression.
  const colours = await items.nth(1).evaluate((li) => {
    const out = [];
    for (const p of li.querySelectorAll("p")) {
      out.push({ text: p.innerText.slice(0, 40), colour: getComputedStyle(p).color, cls: p.className });
    }
    return out;
  });
  const problem = await items.nth(3).evaluate((li) => {
    const p = li.querySelector("p:last-of-type");
    return p ? { text: p.innerText.slice(0, 40), colour: getComputedStyle(p).color } : null;
  });
  console.log(`  ${device.name} sub-line colours:`, JSON.stringify(colours));
  console.log(`  ${device.name} ineligible reason colour:`, JSON.stringify(problem));
  check(
    `${device.name}: the informational sub-lines are muted, not the problem colour`,
    colours.every((c) => c.cls.includes("text-muted") && c.colour !== problem?.colour),
  );

  const base = path.join(OUT, `preview-${device.name}`);
  await card.screenshot({ path: `${base}-card.png` });
  await page.screenshot({ path: `${base}-full.png`, fullPage: true });

  // The card is taller than a phone viewport, and an element screenshot of it is
  // stitched — which paints the sticky header over the seam. Walk the card past the
  // viewport instead, so what is captured is what an operator actually sees.
  const box = await card.boundingBox();
  const scrollY = await page.evaluate(() => window.scrollY);
  const top = box.y + scrollY;
  const step = Math.round(device.profile.viewport.height * 0.8);
  const shots = Math.max(1, Math.ceil(box.height / step));
  for (let k = 0; k < shots; k++) {
    await page.evaluate((y) => window.scrollTo(0, y), top + k * step - 8);
    await page.waitForTimeout(250);
    await page.screenshot({ path: `${base}-viewport-${k}.png`, fullPage: false });
  }

  if (device.name === "mobile") {
    const report = await page.evaluate(overflowProbe, device.profile.viewport.width);
    console.log("  mobile overflow:", JSON.stringify(report, null, 2));
    check(
      "mobile: the preview card does not push the page wider than the screen",
      !report.overflow,
      `${report.scrollWidth}px vs ${report.deviceWidth}px`,
    );
  }

  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  for (const device of [
    { name: "mobile", profile: IPHONE_16_PRO },
    { name: "desktop", profile: DESKTOP },
  ]) {
    await run(browser, device);
  }
  await browser.close();
  const failed = results.filter((r) => !r.ok);
  console.log(`\n=== ${results.length - failed.length}/${results.length} checks passed ===`);
  process.exit(failed.length ? 1 : 0);
})().catch((e) => {
  console.error("SCRIPT ERROR:", e);
  process.exit(2);
});
