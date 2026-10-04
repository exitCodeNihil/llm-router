/**
 * End-to-end write path: create a real API key through the UI, verify it
 * reaches the gateway, confirm the one-time secret dialog, then delete it.
 * Also exercises the streaming playground.
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://localhost:5173";
const API = process.argv[3] ?? "http://localhost:8080";
// A local stub deployment, so streaming assertions never depend on a real
// provider being reachable or on which alias happens to sort first.
const STUB_ALIAS = process.env.LLMR_STUB_ALIAS ?? "claude-test";
const TOKEN = process.env.LLMR_TOKEN ?? "dev-admin-token";
const NAME = `pilot-e2e-${Date.now()}`;

const results = [];
const check = (name, pass, detail = "") => {
  results.push(pass);
  console.log(`${pass ? "  ok  " : " FAIL "} ${name}${detail ? ` — ${detail}` : ""}`);
};

const apiKeys = async () =>
  (await fetch(`${API}/api/keys`, { headers: { Authorization: `Bearer ${TOKEN}` } })).json();

/** Make the run idempotent: an aborted earlier run can leave keys behind. */
async function purgeLeftovers() {
  for (const k of await apiKeys()) {
    if (!k.name?.startsWith("pilot-e2e-")) continue;
    await fetch(`${API}/api/keys/${k.id}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${TOKEN}` },
    });
  }
}
await purgeLeftovers();

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
await context.addInitScript((t) => {
  window.localStorage.setItem("llmr_token", t);
  window.localStorage.setItem("llmr_theme", "dark");
}, TOKEN);
const page = await context.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));

// ── Create ──────────────────────────────────────────────────────
const startCount = (await apiKeys()).length;
await page.goto(`${BASE}/keys`, { waitUntil: "networkidle" });
await page.getByRole("button", { name: "New key" }).first().click();
await page.waitForTimeout(400);

await page.getByLabel(/^Name/).first().fill(NAME);
await page.locator("dialog button", { hasText: "Select a user" }).first().click();
await page.waitForTimeout(300);
await page.getByRole("option").first().click();
await page.waitForTimeout(200);

// Add a tag through the chip input to exercise its keyboard path.
await page.getByPlaceholder("Add a tag and press Enter").fill("pilot");
await page.keyboard.press("Enter");
await page.waitForTimeout(200);

await page.getByRole("button", { name: "Create key" }).click();
await page.waitForTimeout(1200);

const created = (await apiKeys()).find((k) => k.name === NAME);
check("key created via UI", !!created, created ? created.key_prefix : "not found");
check("key count incremented", (await apiKeys()).length === startCount + 1);
check("tag persisted", !!created?.tags?.includes("pilot"), JSON.stringify(created?.tags));

// ── One-time secret dialog ──────────────────────────────────────
// Both the (closed) confirm dialog and the secret dialog exist in the DOM,
// so scope to the open one rather than the ambiguous dialog role.
const secretDialog = page.locator("dialog[open]");
check("secret dialog shown", await secretDialog.isVisible().catch(() => false));
const secretText = await page.locator("dialog code, dialog pre").first().textContent();
check("full secret revealed once", !!secretText && secretText.startsWith("llmr_"), secretText?.slice(0, 14));

// purpose="required": Escape must NOT dismiss a one-time secret.
await page.keyboard.press("Escape");
await page.waitForTimeout(400);
check(
  "Escape cannot dismiss one-time secret (purpose=required)",
  await secretDialog.isVisible().catch(() => false),
);

await page.getByRole("button", { name: "Done" }).click();
await page.waitForTimeout(400);
check("secret dialog closes on acknowledge", (await page.locator("dialog[open]").count()) === 0);

// New row must appear without a manual refresh (query invalidation).
check("table refreshed with new key", await page.getByText(NAME).first().isVisible());

// ── Delete ──────────────────────────────────────────────────────
const row = page.locator("tr", { hasText: NAME });
await row.getByRole("button", { name: new RegExp(`Delete ${NAME}`) }).click();
await page.waitForTimeout(400);
await page.getByRole("alertdialog").getByRole("button", { name: "Delete" }).click();
await page.waitForTimeout(1200);

const after = await apiKeys();
check("key deleted via UI", !after.find((k) => k.name === NAME));
check("key count restored", after.length === startCount);

// ── Streaming playground ────────────────────────────────────────
await page.goto(`${BASE}/chat`, { waitUntil: "networkidle" });
await waitForComposerReady();

// Pin the stub rather than trusting the default (the first discovered alias):
// once a real deployment sorts ahead of it — an oauth_passthrough model, say,
// which cannot work from an operator session — the default sends this suite
// upstream and it fails for reasons that have nothing to do with the console.
await page.locator('[class*="chat-composer"] button[aria-haspopup="menu"]').click();
await page.waitForTimeout(350);
await page
  .getByRole("menuitemradio", { name: new RegExp(`^${STUB_ALIAS}(\\s|$)`) })
  .click();
await page.waitForTimeout(400);

const PROMPT = "Reply with one word only.";
/** The composer stays contenteditable=false until a model is selected. */
async function waitForComposerReady(timeoutMs = 20000) {
  await page.waitForFunction(
    () => document.querySelector('[aria-label="Message input"]')?.getAttribute("contenteditable") === "true",
    undefined,
    { timeout: timeoutMs },
  );
}

// The composer is a contenteditable div labelled "Message input"; the settings
// panel also renders textboxes (system prompt, schema, tool params), so target
// it by accessible name rather than by position.
const composer = page.getByRole("textbox", { name: /message input/i });
await composer.fill(PROMPT);
await composer.press("Enter");
await page.waitForTimeout(1000);
check(
  "user message rendered in thread",
  await page.locator('[class*="chat-message"]').filter({ hasText: PROMPT }).first().isVisible(),
);

// Wait for the assistant turn to produce text or a surfaced error.
let settled = "";
for (let i = 0; i < 30; i++) {
  await page.waitForTimeout(700);
  // Only inspect text produced *after* the echoed user message, so the
  // prompt itself can never satisfy the assertion.
  settled = await page.evaluate((prompt) => {
    const log = document.querySelector('[class*="chat-message-list"], [role="log"]');
    const all = log ? log.textContent : "";
    const i = all.lastIndexOf(prompt);
    return i >= 0 ? all.slice(i + prompt.length) : "";
  }, PROMPT);
  if (settled.trim().length > 2) break;
}
const surfaced = /failed|error|invalid/i.test(settled);
check(
  "assistant turn streams a reply",
  settled.trim().length > 2 && !surfaced,
  surfaced ? `upstream error: ${settled.trim().slice(0, 60)}` : `reply: ${settled.trim().slice(0, 50)}`,
);

check("no page errors during e2e", errors.length === 0, errors.slice(0, 2).join(" | "));

await browser.close();
const failed = results.filter((r) => !r).length;
console.log(`\n${results.length - failed}/${results.length} e2e checks passed`);
process.exit(failed ? 1 : 0);
