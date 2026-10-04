/**
 * Composer input paths: clipboard paste (image + long text), drag & drop, the
 * inline model switcher, and an image round-trip through the gateway.
 *
 *   node scripts/attach.mjs [baseUrl] [apiUrl]
 *
 * Needs a password user (LLMR_USER / LLMR_PASS) so turns can be persisted and
 * inspected. Non-destructive: removes any conversation it creates.
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://localhost:5173";
const API = process.argv[3] ?? "http://localhost:8080";
const USER = process.env.LLMR_USER ?? "dev@local";
const PASS = process.env.LLMR_PASS ?? "devpassword123";

const results = [];
const check = (name, pass, detail = "") => {
  results.push(pass);
  console.log(`${pass ? "  ok  " : " FAIL "} ${name}${detail ? ` — ${detail}` : ""}`);
};

// A 1x1 red PNG, small enough to paste inline.
const PNG_B64 =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==";

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1500, height: 950 } });
await context.addInitScript(() => {
  window.localStorage.setItem("llmr_theme", "dark");
  window.localStorage.setItem("llmr_chat_history", "0");
  window.localStorage.setItem("llmr_chat_settings", "0");
});
const login = await context.request.post(`${API}/auth/password`, {
  data: { email: USER, password: PASS },
});
check("password login", login.ok(), `${login.status()}`);

const listChats = async () => (await context.request.get(`${API}/api/chats`)).json();
const preExisting = new Set((await listChats()).map((c) => c.id));
const mine = async () => (await listChats()).filter((c) => !preExisting.has(c.id));

const page = await context.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("console", (m) => {
  if (m.type() === "error" && !/DevTools/.test(m.text())) errors.push(m.text());
});
// Record the URL and body of any failed API call so a bad request is
// attributable rather than just "400".
page.on("response", async (r) => {
  if (r.ok() || !/\/(api|v1)\//.test(r.url())) return;
  let body = "";
  try {
    body = (await r.text()).slice(0, 200);
  } catch {}
  errors.push(`${r.request().method()} ${r.url().replace(BASE, "")} -> ${r.status()} ${body}`);
});

const composer = () => page.getByRole("textbox", { name: /message input/i });
const tokens = (name) => page.locator('[class*="astryx-token"]', { hasText: name }).count();

await page.goto(`${BASE}/chat`, { waitUntil: "networkidle" });
await page.waitForFunction(
  () => document.querySelector('[aria-label="Message input"]')?.getAttribute("contenteditable") === "true",
  undefined,
  { timeout: 20000 },
);

// ── Inline model switcher ───────────────────────────────────────
const aliases = await (await context.request.get(`${API}/api/chat/models`)).json();
// The switcher is the composer's only menu trigger (aria-haspopup="menu").
const trigger = page.locator('[class*="chat-composer"] button[aria-haspopup="menu"]');
await trigger.click();
await page.waitForTimeout(400);
const radios = await page.getByRole("menuitemradio").count();
check("model switcher lists every alias", radios === aliases.length, `${radios} of ${aliases.length}`);
check(
  "switcher shows the serving provider",
  (await page.getByRole("menuitemradio").first().textContent())?.length > (aliases[0] ?? "").length,
);

// Pick a different alias and confirm it takes effect.
const current = ((await trigger.textContent()) ?? "").trim();
const target = aliases.find((a) => a !== current) ?? aliases[0];
await page.getByRole("menuitemradio", { name: new RegExp(`^${target}`) }).click();
await page.waitForTimeout(600);
await page.waitForTimeout(300);
check(
  "switching the model updates the composer",
  ((await trigger.textContent()) ?? "").trim() === target,
  `${current} → ${target}`,
);

// ── Clipboard paste: image ──────────────────────────────────────
await composer().click();
await page.evaluate(async (b64) => {
  const bin = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const dt = new DataTransfer();
  dt.items.add(new File([bin], "pasted.png", { type: "image/png" }));
  document
    .querySelector('[aria-label="Message input"]')
    .dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
}, PNG_B64);
await page.waitForTimeout(700);
check("pasted image becomes an attachment", (await tokens("pasted.png")) > 0);

// ── Clipboard paste: long text becomes a document ───────────────
await page.evaluate(() => {
  const dt = new DataTransfer();
  dt.setData("text/plain", "x".repeat(4000));
  document
    .querySelector('[aria-label="Message input"]')
    .dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
});
await page.waitForTimeout(700);
check("long paste becomes a text attachment", (await tokens("pasted-1.txt")) > 0);
check("composer not flooded by the long paste", ((await composer().textContent()) ?? "").length < 200);

// ── Short paste still goes into the input ───────────────────────
await page.evaluate(() => {
  const dt = new DataTransfer();
  dt.setData("text/plain", "short paste");
  document
    .querySelector('[aria-label="Message input"]')
    .dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
});
await page.waitForTimeout(500);
check("short paste is left to the browser", (await tokens("pasted-2.txt")) === 0);

// ── Drag & drop ─────────────────────────────────────────────────
await page.evaluate(() => {
  const dt = new DataTransfer();
  dt.items.add(new File(["dropped body"], "dropped.txt", { type: "text/plain" }));
  const el = document.querySelector('.astryx-chat-composer').parentElement;
  el.dispatchEvent(new DragEvent("dragover", { dataTransfer: dt, bubbles: true }));
  el.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true }));
});
await page.waitForTimeout(700);
check("dropped file becomes an attachment", (await tokens("dropped.txt")) > 0);

// ── Image round-trip through the gateway ────────────────────────
// Clear the composer, keep only the image, and send it.
await page.reload({ waitUntil: "networkidle" });
await page.waitForFunction(
  () => document.querySelector('[aria-label="Message input"]')?.getAttribute("contenteditable") === "true",
  undefined,
  { timeout: 20000 },
);
const stub = aliases.find((a) => a.includes("claude-test")) ?? aliases[0];
await trigger.click();
await page.waitForTimeout(400);
await page.getByRole("menuitemradio", { name: new RegExp(`^${stub}`) }).click();
await page.waitForTimeout(500);

await composer().click();
await page.evaluate(async (b64) => {
  const bin = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const dt = new DataTransfer();
  dt.items.add(new File([bin], "shot.png", { type: "image/png" }));
  document
    .querySelector('[aria-label="Message input"]')
    .dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
}, PNG_B64);
await page.waitForTimeout(600);
await composer().fill("What colour is this image?");
await composer().press("Enter");
await page.waitForTimeout(9000);

// The user turn must render the image, proving it went out as a content part.
const imgs = await page.locator('[class*="chat-message"] img').count();
check("image rendered in the transcript", imgs > 0, `${imgs} img element(s)`);

const saved = await mine();
if (saved.length) {
  const detail = await (await context.request.get(`${API}/api/chats/${saved[0].id}`)).json();
  const userMsg = (detail.messages ?? []).find((m) => m.role === "user");
  const parts = Array.isArray(userMsg?.content) ? userMsg.content : [];
  check(
    "request carried an image_url content part",
    parts.some((p) => p.type === "image_url" && String(p.image_url?.url).startsWith("data:image/")),
    parts.map((p) => p.type).join("+") || "none",
  );
}

check("no console/page errors", errors.length === 0, errors.slice(0, 3).join(" | "));

for (const c of await mine()) await context.request.delete(`${API}/api/chats/${c.id}`);
check("cleanup left pre-existing chats intact", (await listChats()).length === preExisting.size);

await browser.close();
const failed = results.filter((r) => !r).length;
console.log(`\n${results.length - failed}/${results.length} composer input checks passed`);
process.exit(failed ? 1 : 0);
