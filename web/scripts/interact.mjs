/**
 * Interaction tests for the Astryx pilot: dialog focus management, keyboard
 * dismissal, form validation gating, live search, and colour-mode switching.
 * Verifies the accessibility behaviour the hand-rolled console was missing.
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://localhost:5173";
const TOKEN = process.env.LLMR_TOKEN ?? "dev-admin-token";
const results = [];
const check = (name, pass, detail = "") => {
  results.push({ name, pass, detail });
  console.log(`${pass ? "  ok  " : " FAIL "} ${name}${detail ? ` — ${detail}` : ""}`);
};

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
await context.addInitScript((t) => {
  window.localStorage.setItem("llmr_token", t);
  window.localStorage.setItem("llmr_theme", "dark");
}, TOKEN);
const page = await context.newPage();

const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("console", (m) => {
  if (m.type() === "error" && !/DevTools/.test(m.text())) errors.push(m.text());
});

// ── Dialog focus management ─────────────────────────────────────
await page.goto(`${BASE}/keys`, { waitUntil: "networkidle" });
await page.getByRole("button", { name: "New key" }).first().click();
await page.waitForTimeout(400);

const dialog = page.getByRole("dialog");
check("dialog opens", await dialog.isVisible());

// Focus must move into the dialog, not stay on the page behind it.
const focusInside = await page.evaluate(() => {
  const d = document.querySelector('dialog[open], [role="dialog"]');
  return !!d && d.contains(document.activeElement);
});
check("focus moves into dialog", focusInside);

// A native modal dialog cycles focus dialog -> document root -> dialog, so
// transiently landing on <body> is correct. What must never happen is focus
// reaching an interactive control *behind* the dialog.
let leaked = null;
for (let i = 0; i < 40; i++) {
  await page.keyboard.press("Tab");
  const hit = await page.evaluate(() => {
    const d = document.querySelector('dialog[open], [role="dialog"]');
    const a = document.activeElement;
    if (!d || !a || a === document.body || a === document.documentElement) return null;
    if (d.contains(a)) return null;
    return `${a.tagName}:${(a.textContent || "").trim().slice(0, 24)}`;
  });
  if (hit) {
    leaked = hit;
    break;
  }
}
check("focus never leaks to background controls (40 tabs)", !leaked, leaked ?? "");

// showModal() (not just the open attribute) is what makes the rest of the
// page inert for pointer and assistive technology.
const isModal = await page.evaluate(() => {
  const d = document.querySelector("dialog");
  return !!d && d.matches(":modal");
});
check("native showModal() — background inert", isModal);

// Submit must stay disabled until every required field is valid. With a
// bootstrap-admin token there is no "self" owner, so a user must be chosen.
const submit = page.getByRole("button", { name: "Create key" });
check("submit gated while name empty", await submit.isDisabled());

await page.getByLabel(/^Name/).first().fill("pilot-test-key");
await page.waitForTimeout(200);
check("submit still gated without an owner", await submit.isDisabled());

// With hasSearch enabled Astryx puts role="combobox" on the search input and
// leaves the trigger as a button with aria-haspopup/-expanded/-controls,
// which is the correct ARIA 1.2 combobox-with-textbox pattern.
const userTrigger = page.locator("dialog button", { hasText: "Select a user" }).first();
check("searchable selector trigger is announced", (await userTrigger.getAttribute("aria-haspopup")) === "listbox");
await userTrigger.click();
await page.waitForTimeout(350);
check("search input owns the combobox role", await page.getByRole("combobox", { name: /search/i }).isVisible());
await page.getByRole("option").first().click();
await page.waitForTimeout(300);
check("submit enabled once name + owner set", await submit.isEnabled());

// Escape closes a form dialog and restores focus to the trigger.
await page.keyboard.press("Escape");
await page.waitForTimeout(400);
check("Escape closes dialog", !(await dialog.isVisible().catch(() => false)));
const restored = await page.evaluate(
  () => document.activeElement?.textContent?.includes("New key") ?? false,
);
check("focus restored to trigger", restored);

// ── Destructive confirmation ────────────────────────────────────
await page.getByRole("button", { name: /^Delete / }).first().click();
await page.waitForTimeout(400);
const alert = page.getByRole("alertdialog");
check("delete uses alertdialog role", await alert.isVisible().catch(() => false));
await page.keyboard.press("Escape");
await page.waitForTimeout(300);

// ── Live search (Pricing) ───────────────────────────────────────
await page.goto(`${BASE}/pricing`, { waitUntil: "networkidle" });
const before = await page.locator("tbody tr").count();
await page.getByPlaceholder("Search models…").fill("gpt-4o");
await page.waitForTimeout(400);
const after = await page.locator("tbody tr").count();
check("search filters table", after > 0 && after < before, `${before} → ${after} rows`);

// ── Colour mode ─────────────────────────────────────────────────
await page.goto(`${BASE}/`, { waitUntil: "networkidle" });
const darkBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
await page.getByRole("button", { name: /Switch to light mode/i }).click();
await page.waitForTimeout(500);
const lightBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
check("colour mode switches", darkBg !== lightBg, `${darkBg} → ${lightBg}`);
const persisted = await page.evaluate(() => localStorage.getItem("llmr_theme"));
check("colour mode persisted", persisted === "light");

// ── Selector keyboard operation ─────────────────────────────────
await page.goto(`${BASE}/requests`, { waitUntil: "networkidle" });
// Astryx moves role="combobox" onto the search input once a Selector is
// searchable, which Requests turns on above 8 options. Locating the trigger by
// role therefore breaks purely by adding a ninth deployment, so match the
// listbox-popup attribute the trigger carries in both modes.
const selector = page.locator('[aria-haspopup="listbox"]').first();
await selector.click();
await page.waitForTimeout(300);
const listbox = await page.getByRole("listbox").isVisible().catch(() => false);
check("selector opens a listbox", listbox);
const expanded = await selector.getAttribute("aria-expanded");
check("selector trigger reports aria-expanded", expanded === "true");
// Twice: in searchable mode focus starts in the search box, so one ArrowDown
// only reaches the first option — which is the "All models" default already
// selected, making a successful selection indistinguishable from a no-op.
await page.keyboard.press("ArrowDown");
await page.keyboard.press("ArrowDown");
await page.keyboard.press("Enter");
await page.waitForTimeout(400);
check("selector is keyboard-operable", (await selector.textContent())?.trim() !== "All models");

// ── Routing-flow tracing ────────────────────────────────────────
// Selecting a node must dim what it does not touch, and must not flood the
// whole connected component: a key's siblings share upstreams, so a traversal
// that walks both directions from every reached node lights up everything.
await page.goto(`${BASE}/`, { waitUntil: "networkidle" });
await page.waitForTimeout(1200);
const chips = page.getByRole("button", { name: /trace connections/ });
const chipCount = await chips.count();
check("routing-flow chips are focusable buttons", chipCount > 0, `${chipCount} chips`);

if (chipCount > 2) {
  const dimmed = () =>
    page.evaluate(
      () =>
        [...document.querySelectorAll('g[role="button"]')].filter(
          (g) => parseFloat(getComputedStyle(g).opacity) < 0.5,
        ).length,
    );
  check("nothing dimmed before a selection", (await dimmed()) === 0);

  await chips.first().click();
  await page.waitForTimeout(400);
  const afterClick = await dimmed();
  check("selecting a node dims unrelated nodes", afterClick > 0, `${afterClick} dimmed`);
  check(
    "selection does not dim everything (traversal kept the route)",
    afterClick < chipCount,
    `${afterClick} of ${chipCount}`,
  );
  check("selected node reports aria-pressed", (await chips.first().getAttribute("aria-pressed")) === "true");

  // keyboard: Enter toggles the same selection off
  await chips.first().press("Enter");
  await page.waitForTimeout(400);
  check("Enter toggles the selection off", (await dimmed()) === 0);
}

check("no console/page errors during interactions", errors.length === 0, errors.slice(0, 3).join(" | "));

await browser.close();

const failed = results.filter((r) => !r.pass);
console.log(`\n${results.length - failed.length}/${results.length} interaction checks passed`);
process.exit(failed.length ? 1 : 0);
