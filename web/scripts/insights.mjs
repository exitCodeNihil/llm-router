/**
 * Operator readouts in the playground: routing attribution, latency, cost,
 * request-id correlation, message actions, send-as-key policy enforcement,
 * the context meter and curl export.
 *
 *   node scripts/insights.mjs [baseUrl] [apiUrl]
 *
 * Needs a password user (LLMR_USER / LLMR_PASS). Non-destructive: creates its
 * own API key, deployment context value and chats, and removes them all.
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://localhost:5173";
const API = process.argv[3] ?? "http://localhost:8080";
const TOKEN = process.env.LLMR_TOKEN ?? "dev-admin-token";
const USER = process.env.LLMR_USER ?? "dev@local";
const PASS = process.env.LLMR_PASS ?? "devpassword123";

const results = [];
const check = (name, pass, detail = "") => {
  results.push(pass);
  console.log(`${pass ? "  ok  " : " FAIL "} ${name}${detail ? ` — ${detail}` : ""}`);
};

const admin = async (path, init = {}) => {
  const res = await fetch(`${API}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json", ...init.headers },
  });
  const text = await res.text();
  return text ? JSON.parse(text) : null;
};

// ── Fixtures: a model-restricted key and a known context window ──
const stubAlias = "claude-test";
const deployments = await admin("/api/deployments");
const stub = deployments.find((d) => d.model_name === stubAlias && d.enabled);
const other = deployments.find((d) => d.model_name !== stubAlias && d.enabled);
const originalContext = stub?.context_tokens ?? null;
await admin(`/api/deployments/${stub.id}`, {
  method: "PATCH",
  body: JSON.stringify({ context_tokens: 8192 }),
});

const users = await admin("/api/users");
const minted = await admin("/api/keys", {
  method: "POST",
  body: JSON.stringify({
    name: `insights-probe-${Date.now()}`,
    user_id: users[0].id,
    allowed_models: [stubAlias],
  }),
});
const probeKey = minted.key;

// Sweep keys stranded by an earlier crashed run: cleanup lives at the end of the
// script, so a failed assertion mid-run leaves a live credential behind.
for (const k of await admin("/api/keys")) {
  if (k.name.startsWith("insights-probe-") && k.id !== minted.id) {
    await admin(`/api/keys/${k.id}`, { method: "DELETE" });
    console.log(`  (swept a probe key stranded by an earlier run: ${k.name})`);
  }
}

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1500, height: 1000 } });
await context.addInitScript(() => {
  window.localStorage.setItem("llmr_theme", "dark");
  window.localStorage.setItem("llmr_chat_history", "0");
  window.localStorage.setItem("llmr_chat_settings", "1"); // settings rail open
});
await context.grantPermissions(["clipboard-read", "clipboard-write"]);
await context.request.post(`${API}/auth/password`, { data: { email: USER, password: PASS } });

const listChats = async () => (await context.request.get(`${API}/api/chats`)).json();
const preExisting = new Set((await listChats()).map((c) => c.id));
const mine = async () => (await listChats()).filter((c) => !preExisting.has(c.id));

const page = await context.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));

const composer = () => page.getByRole("textbox", { name: /message input/i });
const ready = () =>
  page.waitForFunction(
    () => document.querySelector('[aria-label="Message input"]')?.getAttribute("contenteditable") === "true",
    undefined,
    { timeout: 20000 },
  );
// Menu items are labelled "<alias> <provider>", and real deployments share
// prefixes (claude-haiku vs claude-haiku-4-5), so anchor on a following space or
// the label end: a bare ^alias matches every longer sibling and trips strict mode.
const aliasPattern = (alias) =>
  new RegExp(`^${alias.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(\\s|$)`);
const pickModel = async (alias) => {
  const trigger = page.locator('[class*="chat-composer"] button[aria-haspopup="menu"]');
  await trigger.click();
  await page.waitForTimeout(350);
  await page.getByRole("menuitemradio", { name: aliasPattern(alias) }).click();
  await page.waitForTimeout(500);
};
const send = async (text, waitMs = 9000) => {
  await composer().fill(text);
  await composer().press("Enter");
  await page.waitForTimeout(waitMs);
};

await page.goto(`${BASE}/chat`, { waitUntil: "networkidle" });
await ready();
await pickModel(stubAlias);

// ── Context meter (needs an operator-set context_tokens) ────────
check("context meter shown when a context window is configured", (await page.getByText(/^Context$/).count()) > 0);
check("context meter reports the configured limit", (await page.getByText(/8\.2K|8192/).count()) > 0);

// ── One turn, then the operator readouts ────────────────────────
await send("Say hi.");

const footer = (await page.locator('[class*="chat-message"]').last().textContent()) ?? "";
check("routing attribution: provider shown", /foundry|lm-studio|stub/i.test(footer), footer.slice(-90));
check("time to first token reported", /to first token/.test(footer));
check("total latency reported", /ms total/.test(footer));
check("token counts reported", /\d+\s*in\s*·\s*\d+\s*out/.test(footer));

// The stub deployment is priced, so a cost must appear and not be "unpriced".
check("cost per turn reported", /\$\d/.test(footer), (footer.match(/\$[\d.]+/) ?? [""])[0]);

// request_id correlation: shown, and resolvable on the Requests page.
const reqIdText = await page
  .locator('[class*="chat-message"] code')
  .last()
  .textContent()
  .catch(() => null);
check("request id shown for correlation", !!reqIdText && reqIdText.length >= 6, reqIdText ?? "none");

// ── Message actions ─────────────────────────────────────────────
check("copy action present on the assistant turn", (await page.getByRole("button", { name: "Copy", exact: true }).count()) > 0);
// Clear the clipboard first: a stale value from an earlier step would let a
// broken copy button pass.
await page.evaluate(() => navigator.clipboard.writeText("__cleared__").catch(() => {}));
await page.getByRole("button", { name: "Copy", exact: true }).last().click();
await page.waitForTimeout(500);
const clip = await page.evaluate(() => navigator.clipboard.readText().catch(() => ""));
// Compare on collapsed whitespace: the rendered Markdown does not match the
// raw text character for character.
// Read the whole list: .last() on [class*="chat-message"] resolves to the
// metadata footer, whose class also contains "chat-message".
const answer = ((await page.locator('[class*="chat-message-list"]').textContent()) ?? "").replace(/\s+/g, " ");
const clipNorm = clip.replace(/\s+/g, " ").trim();
check(
  "copy puts the answer on the clipboard",
  clip !== "__cleared__" && !clip.startsWith("curl") && clipNorm.length > 10 && answer.includes(clipNorm.slice(0, 25)),
  `${clip.slice(0, 40)}…`,
);

const before = await page.locator('[class*="chat-message"]').count();
check("regenerate offered on the last turn", (await page.getByRole("button", { name: "Regenerate" }).count()) > 0);
await page.getByRole("button", { name: "Regenerate" }).last().click();
await page.waitForTimeout(9000);
check(
  "regenerate replaces the answer rather than appending a turn",
  (await page.locator('[class*="chat-message"]').count()) <= before,
);

// ── Copy as curl ────────────────────────────────────────────────
await page.evaluate(() => navigator.clipboard.writeText("__cleared__").catch(() => {}));
await page.getByRole("button", { name: "Copy as curl" }).click();
await page.waitForTimeout(500);
const curl = await page.evaluate(() => navigator.clipboard.readText().catch(() => ""));
check("curl targets the public client surface", curl.includes("/v1/chat/completions"));
check("curl leaves the key as a shell variable", curl.includes("$LLMR_API_KEY") && !curl.includes("llmr_"));
check("curl carries the model and messages", curl.includes(stubAlias) && curl.includes("messages"));

// ── Send as a virtual key: policy must now apply ────────────────
await page.getByLabel(/API key/i).fill(probeKey);
await page.getByRole("button", { name: /Use this key/i }).click();
await page.waitForTimeout(600);
check("send-as-key mode is indicated", (await page.getByText("as key").count()) > 0);

await page.getByRole("button", { name: "New chat" }).first().click();
await page.waitForTimeout(600);
await ready();
await send("Still fine?", 9000);
const allowed = (await page.locator('[class*="chat-message"]').last().textContent()) ?? "";
check("allowed model still works through the key", !/Request failed/i.test(allowed));
check("turn is attributed to the key", (await page.getByText(/key llmr_/).count()) > 0);

// The key only allows the stub alias, so another model must be refused —
// this is the check the console session could never make.
if (other) {
  await pickModel(other.model_name);
  await send("Should be refused.", 8000);
  const refused = (await page.locator('[class*="chat-message-list"]').textContent()) ?? "";
  check(
    "disallowed model is refused for the key",
    /not available for this API key|model_not_found/i.test(refused),
    refused.slice(-90),
  );
}

check("no page errors", errors.length === 0, errors.slice(0, 2).join(" | "));

// ── Cleanup ─────────────────────────────────────────────────────
for (const c of await mine()) await context.request.delete(`${API}/api/chats/${c.id}`);
await admin(`/api/keys/${minted.id}`, { method: "DELETE" });
await admin(`/api/deployments/${stub.id}`, {
  method: "PATCH",
  body: JSON.stringify({ context_tokens: originalContext }),
});
const keysLeft = (await admin("/api/keys")).filter((k) => k.name.startsWith("insights-probe-"));
check("probe key removed", keysLeft.length === 0);
check("pre-existing chats intact", (await listChats()).length === preExisting.size);

await browser.close();
const failed = results.filter((r) => !r).length;
console.log(`\n${results.length - failed}/${results.length} operator-readout checks passed`);
process.exit(failed ? 1 : 0);
