/**
 * Playground feature suite. Signs in as a real user (bootstrap-token sessions
 * cannot persist chats) and exercises the paths the other suites do not touch:
 * conversation persistence, attachments, tool editing and structured output.
 *
 *   node scripts/chat.mjs [baseUrl] [apiUrl]
 *
 * Requires LLMR_USER / LLMR_PASS for an existing password user.
 *
 * NON-DESTRUCTIVE: the account may hold real conversations. Pre-existing chat
 * IDs are snapshotted up front; this suite only ever asserts on, or deletes,
 * chats it created itself.
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://localhost:5173";
const API = process.argv[3] ?? "http://localhost:8080";
const USER = process.env.LLMR_USER ?? "dev@local";
const PASS = process.env.LLMR_PASS ?? "devpassword123";

const TAG = `probe-${Date.now()}`;
const results = [];
const check = (name, pass, detail = "") => {
  results.push(pass);
  console.log(`${pass ? "  ok  " : " FAIL "} ${name}${detail ? ` — ${detail}` : ""}`);
};

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 950 } });
await context.addInitScript(() => window.localStorage.setItem("llmr_theme", "dark"));

// Password login sets an HttpOnly session cookie the context then carries.
const login = await context.request.post(`${API}/auth/password`, {
  data: { email: USER, password: PASS },
});
check("password login", login.ok(), `${login.status()}`);

const listChats = async () => (await context.request.get(`${API}/api/chats`)).json();

// Everything that existed before this run is off-limits.
const preExisting = new Set((await listChats()).map((c) => c.id));
const mine = async () => (await listChats()).filter((c) => !preExisting.has(c.id));

const page = await context.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("console", (m) => {
  if (m.type() === "error" && !/DevTools/.test(m.text())) errors.push(m.text());
});

/** The composer stays contenteditable=false until a model is selected. */
async function waitForComposerReady(timeoutMs = 20000) {
  await page.waitForFunction(
    () => document.querySelector('[aria-label="Message input"]')?.getAttribute("contenteditable") === "true",
    undefined,
    { timeout: timeoutMs },
  );
}

const composer = () => page.getByRole("textbox", { name: /message input/i });
const threadText = () =>
  page.evaluate(() => {
    const log = document.querySelector('[class*="chat-message-list"], [role="log"]');
    return log ? log.textContent : "";
  });

/** Wait until the assistant turn stops growing (stream finished). */
async function waitForReply(timeoutMs = 30000) {
  const started = Date.now();
  let last = "";
  while (Date.now() - started < timeoutMs) {
    await page.waitForTimeout(700);
    const now = await threadText();
    if (now.length > 40 && now === last) return now;
    last = now;
  }
  return last;
}

/** Wait for this run's chat count to reach n. */
async function waitForMine(n, timeoutMs = 15000) {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    const m = await mine();
    if (m.length >= n) return m;
    await page.waitForTimeout(600);
  }
  return mine();
}

// ── Saved conversations ─────────────────────────────────────────
await page.goto(`${BASE}/chat`, { waitUntil: "networkidle" });
await page.waitForTimeout(800);

check("history enabled for a real user session", (await page.getByText(/Sign in to save chats/i).count()) === 0);

await waitForComposerReady();
check("composer enabled once models resolve", true);

const title = `${TAG} first conversation`;
await composer().fill(`${title}. Reply briefly.`);
await composer().press("Enter");
await waitForReply();

const created = await waitForMine(1);
check("conversation persisted to the server", created.length === 1, `${created.length} new chat(s)`);
check("title derived from the first user message", created[0]?.title?.startsWith(TAG), created[0]?.title);
check("pre-existing conversations untouched", (await listChats()).length === preExisting.size + 1);

await page.waitForTimeout(700);
check("saved chat listed in the sidebar", (await page.getByRole("button", { name: new RegExp(TAG) }).count()) > 0);

// Reload and reopen: the transcript must come back from the server.
await page.reload({ waitUntil: "networkidle" });
await page.waitForTimeout(1000);
await page.getByRole("button", { name: new RegExp(TAG) }).first().click();
await page.waitForTimeout(1500);
check("transcript restored after reload", (await threadText()).includes(TAG));

// ── New chat resets state ───────────────────────────────────────
await page.getByRole("button", { name: /^New( chat)?$/ }).first().click();
await page.waitForTimeout(700);
check("new chat clears the thread", !(await threadText()).includes(TAG));

// ── Text attachment ─────────────────────────────────────────────
await page.setInputFiles('input[type="file"]', {
  name: "notes.txt",
  mimeType: "text/plain",
  buffer: Buffer.from("PROJECT_CODENAME=falcon\n"),
});
await page.waitForTimeout(600);
const tokenCount = () =>
  page.locator('[class*="astryx-token"]', { hasText: "notes.txt" }).count();
check("text attachment shows as a token", (await tokenCount()) > 0);

await waitForComposerReady();
await composer().fill("What is the codename?");
await composer().press("Enter");
const withFile = await waitForReply();
// The stub echoes the prompt, so the inlined file content lands in the turn.
check("text file inlined into the request", /falcon|notes\.txt/.test(withFile));
check("attachment cleared after send", (await tokenCount()) === 0);

// ── Unsupported attachment is rejected ──────────────────────────
await page.getByRole("button", { name: /^New( chat)?$/ }).first().click();
await page.waitForTimeout(500);
await page.setInputFiles('input[type="file"]', {
  name: "bad.bin",
  mimeType: "application/octet-stream",
  buffer: Buffer.from([0, 1, 2, 3]),
});
await page.waitForTimeout(800);
check("unsupported file type rejected with a message", (await page.getByText(/unsupported type/i).count()) > 0);

// ── Structured output ───────────────────────────────────────────
/** Request settings live in a collapsible rail; open it on demand. */
async function openSettingsRail() {
  if (await page.locator('[aria-label="Request settings"]').count()) return;
  await page.getByRole("button", { name: /Show request settings/i }).click();
  await page.waitForTimeout(600);
}
await openSettingsRail();
check("request settings rail opens", (await page.locator('[aria-label="Request settings"]').count()) > 0);
await page.getByRole("radio", { name: "Schema", exact: true }).click();
await page.waitForTimeout(500);
const schemaBox = page.getByLabel(/JSON schema/i);
await schemaBox.fill("{ not json");
await page.waitForTimeout(600);
check("invalid JSON schema is flagged", (await page.getByText(/Not valid JSON/i).count()) > 0);
const composerEditable = () =>
  page.evaluate(
    () => document.querySelector('[aria-label="Message input"]')?.getAttribute("contenteditable") === "true",
  );
check("send blocked while the schema is invalid", !(await composerEditable()));

await schemaBox.fill('{"type":"object","properties":{"answer":{"type":"string"}}}');
await page.waitForTimeout(600);
check("valid schema re-enables sending", await composerEditable());
await page.getByRole("radio", { name: "None", exact: true }).click();

// ── Tools ───────────────────────────────────────────────────────
await page.getByRole("button", { name: "Add tool" }).click();
await page.waitForTimeout(500);
const params = page.getByLabel(/Parameters \(JSON Schema\)/i);
check("tool editor added", (await params.count()) > 0);
check(
  "tool choice control appears",
  (await page.getByRole("radio", { name: "Required", exact: true }).count()) > 0,
);

await params.fill("{ broken");
await page.waitForTimeout(500);
check("invalid tool parameters flagged", (await page.getByText(/Not valid JSON/i).count()) > 0);
await params.fill('{"type":"object","properties":{"city":{"type":"string"}}}');
await page.waitForTimeout(400);

await page.getByRole("button", { name: /Remove tool/i }).first().click();
await page.waitForTimeout(500);
check("tool removed", (await page.getByLabel(/Parameters \(JSON Schema\)/i).count()) === 0);

// ── Delete a conversation (only ones this run made) ─────────────
const before = (await mine()).length;
await page.getByRole("button", { name: new RegExp(`Delete .*${TAG}|Delete ${TAG}`) }).first().click();
await page.waitForTimeout(600);
await page.getByRole("alertdialog").getByRole("button", { name: "Delete" }).click();
await page.waitForTimeout(1500);
check("conversation deleted", (await mine()).length === before - 1, `${before} → ${(await mine()).length}`);

check("no console/page errors", errors.length === 0, errors.slice(0, 3).join(" | "));

// Remove only what this run created; never touch pre-existing chats.
for (const c of await mine()) await context.request.delete(`${API}/api/chats/${c.id}`);
const leftover = (await listChats()).length;
check("cleanup left pre-existing chats intact", leftover === preExisting.size, `${leftover} chats remain`);

await browser.close();
const failed = results.filter((r) => !r).length;
console.log(`\n${results.length - failed}/${results.length} playground checks passed`);
process.exit(failed ? 1 : 0);
