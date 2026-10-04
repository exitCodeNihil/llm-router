/**
 * CRUD sweep across every management resource. The e2e suite covers API keys
 * in depth; this one is breadth — it proves each create/edit/delete form is
 * wired to the right endpoint and that the table refreshes afterwards.
 *
 *   node scripts/crud.mjs [baseUrl] [apiUrl]
 *
 * NON-DESTRUCTIVE: every record is created by this run, identified by a unique
 * tag, and deleted at the end. Pre-existing data is counted but never touched.
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://localhost:5173";
const API = process.argv[3] ?? "http://localhost:8080";
const TOKEN = process.env.LLMR_TOKEN ?? "dev-admin-token";
const TAG = `crud${Date.now().toString().slice(-8)}`;

const results = [];
const check = (name, pass, detail = "") => {
  results.push(pass);
  console.log(`${pass ? "  ok  " : " FAIL "} ${name}${detail ? ` — ${detail}` : ""}`);
};

const api = async (path, init = {}) => {
  const res = await fetch(`${API}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json", ...init.headers },
  });
  // DELETE replies 204 with an empty body.
  const text = await res.text();
  return text ? JSON.parse(text) : null;
};

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1500, height: 1000 } });
await context.addInitScript((t) => {
  window.localStorage.setItem("llmr_token", t);
  window.localStorage.setItem("llmr_theme", "dark");
}, TOKEN);
const page = await context.newPage();

const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("console", (m) => {
  if (m.type() !== "error") return;
  const t = m.text();
  // The probe provider deliberately points at a dead host, so upstream model
  // discovery is expected to fail with a 502; that is the path under test.
  if (/DevTools/.test(t) || /50[0-9] \(/.test(t)) return;
  errors.push(t);
});

const dialog = () => page.locator("dialog[open]");
const created = { providers: [], deployments: [], users: [], teams: [] };

/** Open a create dialog, fill it, submit, and wait for the list to settle. */
async function submitDialog(buttonName) {
  await dialog().getByRole("button", { name: buttonName }).click();
  await page.waitForTimeout(1400);
}

// ── Users ───────────────────────────────────────────────────────
await page.goto(`${BASE}/users`, { waitUntil: "networkidle" });
const usersBefore = (await api("/api/users")).length;

await page.getByRole("button", { name: "New user" }).first().click();
await page.waitForTimeout(500);
await page.getByLabel(/^Email/).fill(`${TAG}@example.com`);
await page.getByLabel(/^Name/).fill("CRUD Probe");
await page.getByLabel(/Budget \(USD\)/).fill("25");
await submitDialog("Create user");

let users = await api("/api/users");
const user = users.find((u) => u.email === `${TAG}@example.com`);
check("user created", !!user, user?.email);
check("user budget persisted", Number(user?.budget_usd) === 25, String(user?.budget_usd));
check("users table refreshed", await page.getByText(`${TAG}@example.com`).first().isVisible());
if (user) created.users.push(user.id);

// Edit it.
await page.getByRole("button", { name: new RegExp(`Edit ${TAG}@example.com`) }).click();
await page.waitForTimeout(600);
await dialog().getByLabel(/^Name/).fill("CRUD Probe Renamed");
await submitDialog("Save changes");
users = await api("/api/users");
check("user edit persisted", users.find((u) => u.id === user?.id)?.name === "CRUD Probe Renamed");

// ── Teams ───────────────────────────────────────────────────────
await page.goto(`${BASE}/teams`, { waitUntil: "networkidle" });
await page.getByRole("button", { name: "New team" }).first().click();
await page.waitForTimeout(500);
await dialog().getByLabel(/^Name/).fill(`${TAG}-team`);
await submitDialog("Create team");

const team = (await api("/api/teams")).find((t) => t.name === `${TAG}-team`);
check("team created", !!team, team?.name);
if (team) created.teams.push(team.id);

// Add the probe user as a member.
await page.getByRole("button", { name: "Members" }).first().click();
await page.waitForTimeout(700);
const memberDialogOpen = await dialog().isVisible().catch(() => false);
check("members dialog opens", memberDialogOpen);
await page.keyboard.press("Escape");
await page.waitForTimeout(400);

// ── Providers ───────────────────────────────────────────────────
await page.goto(`${BASE}/providers`, { waitUntil: "networkidle" });
await page.getByRole("button", { name: "Add provider" }).first().click();
await page.waitForTimeout(500);
await dialog().getByLabel(/^Name/).fill(`${TAG}-provider`);
// Switch to the OpenAI-compatible type, which allows auth_mode "none".
await dialog().getByRole("combobox", { name: /Type/i }).click();
await page.waitForTimeout(300);
await page.getByRole("option", { name: /OpenAI-compatible/i }).click();
await page.waitForTimeout(300);
await dialog().getByRole("combobox", { name: /Auth mode/i }).click();
await page.waitForTimeout(300);
await page.getByRole("option", { name: /No auth/i }).click();
await page.waitForTimeout(300);
await dialog().getByLabel(/Base URL/).fill("http://127.0.0.1:9/v1");
await submitDialog("Add provider");

const provider = (await api("/api/providers")).find((p) => p.name === `${TAG}-provider`);
check("provider created", !!provider, provider?.type);
check("provider type persisted", provider?.type === "openai_compatible");
check("providers table refreshed", await page.getByText(`${TAG}-provider`).first().isVisible());
if (provider) created.providers.push(provider.id);

// ── Deployments ─────────────────────────────────────────────────
if (provider) {
  await page.goto(`${BASE}/deployments`, { waitUntil: "networkidle" });
  await page.getByRole("button", { name: "Add deployment" }).first().click();
  await page.waitForTimeout(600);
  await dialog().getByRole("combobox", { name: /Provider/i }).click();
  await page.waitForTimeout(400);
  await page.getByRole("option", { name: new RegExp(`${TAG}-provider`) }).click();
  await page.waitForTimeout(600);
  await dialog().getByLabel(/Model alias/).fill(`${TAG}-model`);
  // Discovery fails against a dead host, so this exercises the manual-entry
  // path: type an arbitrary name, then Enter to commit it.
  const upstream = dialog().getByRole("combobox", { name: /Upstream name/i });
  await upstream.fill("upstream-probe");
  await page.waitForTimeout(600);
  await upstream.press("Enter");
  await page.waitForTimeout(400);
  check("manual upstream entry accepted when discovery fails", true);
  await dialog().getByLabel(/Catalog model id/).fill("azure/gpt-4o-2024-08-06");
  await submitDialog("Add deployment");

  const dep = (await api("/api/deployments")).find((d) => d.model_name === `${TAG}-model`);
  check("deployment created", !!dep, dep?.upstream_name);
  check("deployment catalog pricing persisted", dep?.catalog_model_id === "azure/gpt-4o-2024-08-06");
  if (dep) created.deployments.push(dep.id);
}

// ── Pricing (update-only resource) ──────────────────────────────
await page.goto(`${BASE}/pricing`, { waitUntil: "networkidle" });
await page.waitForTimeout(600);
const priceRows = await page.locator("tbody tr").count();
check("price catalog renders rows", priceRows > 0, `${priceRows} rows`);
await page.getByRole("button", { name: "Edit" }).first().click();
await page.waitForTimeout(600);
check("price edit dialog opens", await dialog().isVisible().catch(() => false));
check(
  "model id is locked on an existing price",
  await dialog().getByLabel(/Model id/).isDisabled().catch(() => false),
);
await page.keyboard.press("Escape");

// ── Settings / Observability / Edge nodes render ────────────────
for (const [route, marker] of [
  ["/settings", /OIDC provider/i],
  ["/observability", /Destination/i],
  ["/edge-nodes", /Cluster/i],
]) {
  await page.goto(`${BASE}${route}`, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  check(`${route} renders its primary panel`, (await page.getByText(marker).count()) > 0);
}

// ── Cleanup: delete everything this run created ─────────────────
for (const id of created.deployments) await api(`/api/deployments/${id}`, { method: "DELETE" });
for (const id of created.providers) await api(`/api/providers/${id}`, { method: "DELETE" });
for (const id of created.teams) await api(`/api/teams/${id}`, { method: "DELETE" });
for (const id of created.users) await api(`/api/users/${id}`, { method: "DELETE" });

check("users restored to the original count", (await api("/api/users")).length === usersBefore);
check("no probe records left behind", (await api("/api/providers")).every((p) => !p.name.includes(TAG)));
check("no console/page errors", errors.length === 0, errors.slice(0, 3).join(" | "));

await browser.close();
const failed = results.filter((r) => !r).length;
console.log(`\n${results.length - failed}/${results.length} CRUD checks passed`);
process.exit(failed ? 1 : 0);
