/**
 * Browser audit for the Astryx pilot.
 *
 * Visits every route at three viewports, captures console errors / page
 * errors / failed requests, checks for horizontal overflow, and screenshots.
 *
 *   node scripts/audit.mjs [baseUrl] [outDir]
 */
import { chromium } from "playwright";
import { mkdirSync } from "node:fs";

const BASE = process.argv[2] ?? "http://localhost:5173";
const OUT = process.argv[3] ?? "/tmp/astryx-shots";
const TOKEN = process.env.LLMR_TOKEN ?? "dev-admin-token";
// Colour mode(s) to audit: "dark" (default), "light" or "both".
// Deliberately NOT called LLMR_MODE — that is the gateway's own run mode
// (all | gateway) and clashing with it breaks `make dev`.
const AUDIT_MODE = process.env.AUDIT_MODE ?? "dark";
const MODES = AUDIT_MODE === "both" ? ["dark", "light"] : [AUDIT_MODE];

const ROUTES = [
  ["/", "dashboard"],
  ["/requests", "requests"],
  ["/chat", "chat"],
  ["/keys", "keys"],
  ["/teams", "teams"],
  ["/users", "users"],
  ["/providers", "providers"],
  ["/deployments", "deployments"],
  ["/pricing", "pricing"],
  ["/edge-nodes", "edge-nodes"],
  ["/observability", "observability"],
  ["/settings", "settings"],
  ["/account", "account"],
];

const VIEWPORTS = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "tablet", width: 834, height: 1000 },
  { name: "mobile", width: 390, height: 844 },
];

// React logs key/prop warnings via console.error — worth failing on.
const IGNORE = [/Download the React DevTools/i, /\[vite\]/i];

mkdirSync(OUT, { recursive: true });

const browser = await chromium.launch();
const problems = [];

for (const mode of MODES) {
for (const vp of VIEWPORTS) {
  const context = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: 1,
  });
  // Seed the bootstrap admin token so the app skips the login screen.
  await context.addInitScript(
    ({ t, m }) => {
      window.localStorage.setItem("llmr_token", t);
      window.localStorage.setItem("llmr_theme", m);
    },
    { t: TOKEN, m: mode },
  );

  const page = await context.newPage();

  for (const [route, name] of ROUTES) {
    const found = [];
    const onConsole = (m) => {
      if (m.type() !== "error" && m.type() !== "warning") return;
      const text = m.text();
      if (IGNORE.some((r) => r.test(text))) return;
      found.push(`${m.type()}: ${text}`);
    };
    const onPageError = (e) => found.push(`pageerror: ${e.message}`);
    const onFailed = (r) => {
      if (r.url().includes("/api/") || r.url().includes("/v1/"))
        found.push(`requestfailed: ${r.url()} ${r.failure()?.errorText ?? ""}`);
    };

    page.on("console", onConsole);
    page.on("pageerror", onPageError);
    page.on("requestfailed", onFailed);

    await page.goto(BASE + route, { waitUntil: "networkidle", timeout: 30000 });
    // Let charts/animations settle before measuring and shooting.
    await page.waitForTimeout(700);

    const metrics = await page.evaluate(() => {
      const de = document.documentElement;
      const overflow = de.scrollWidth - de.clientWidth;
      // Find the widest offender when the page scrolls sideways.
      let culprit = null;
      if (overflow > 1) {
        let worst = 0;
        for (const el of document.querySelectorAll("*")) {
          const r = el.getBoundingClientRect();
          if (r.right > de.clientWidth + 1 && r.width > worst) {
            worst = r.width;
            culprit = `${el.tagName.toLowerCase()}.${(el.className || "").toString().slice(0, 60)}`;
          }
        }
      }
      return {
        overflow,
        culprit,
        text: document.body.innerText.slice(0, 200),
        // Astryx's AppShell exposes the main landmark as role="main" on a
        // div rather than a <main> element — equivalent for assistive tech.
        hasMain: !!document.querySelector('main, [role="main"]'),
      };
    });

    if (metrics.overflow > 1)
      found.push(`h-overflow: ${metrics.overflow}px (widest: ${metrics.culprit})`);
    if (!metrics.hasMain) found.push("no <main> landmark rendered");
    if (!metrics.text.trim()) found.push("blank page (no text)");

    await page.screenshot({
      path: `${OUT}/${mode}-${vp.name}-${name}.png`,
      fullPage: vp.name === "desktop",
    });

    page.off("console", onConsole);
    page.off("pageerror", onPageError);
    page.off("requestfailed", onFailed);

    const status = found.length ? "FAIL" : "ok";
    console.log(`[${mode}/${vp.name}] ${route.padEnd(16)} ${status}`);
    for (const f of found) {
      console.log(`    ${f}`);
      problems.push({ mode, viewport: vp.name, route, issue: f });
    }
  }

  await context.close();
}
}

await browser.close();

console.log(`\n${problems.length} problem(s) across ${ROUTES.length * VIEWPORTS.length * MODES.length} page loads`);
console.log(`screenshots: ${OUT}`);
process.exit(problems.length ? 1 : 0);
