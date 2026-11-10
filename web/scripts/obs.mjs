// Observability page: telemetry destinations (add, test, edit headers, scope,
// failing health, disable, delete), trace links on the Requests page, and the
// capture policy draft with its explicit apply.
//
//   BASE=http://localhost:5173 API=http://localhost:8080 LLMR_ADMIN_TOKEN=… node scripts/obs.mjs
//
// Needs real backends: an OTLP/HTTP collector (OTLP, default
// http://localhost:4318, with Tempo's API at TEMPO, default
// http://localhost:3200) and Langfuse (LANGFUSE, default http://localhost:3001,
// keys LANGFUSE_PK / LANGFUSE_SK). SHOTS=<dir> saves screenshots.
//
// Non-destructive: the capture policy is snapshotted and restored verbatim;
// every destination, provider, deployment and key it creates carries a unique
// tag and is deleted at the end, including on failure. Traffic goes to a stub
// upstream this script serves itself.
import http from "node:http";
import { chromium } from "playwright";
import { fixtures } from "./fixtures.mjs";

const BASE = process.argv[2] ?? process.env.BASE ?? "http://localhost:5173";
const API = process.argv[3] ?? process.env.API ?? "http://localhost:8080";
const TOKEN = process.env.LLMR_ADMIN_TOKEN ?? "dev-admin-token";
const OTLP = process.env.OTLP ?? "http://localhost:4318";
const TEMPO = process.env.TEMPO ?? "http://localhost:3200";
const LANGFUSE = process.env.LANGFUSE ?? "http://localhost:3001";
const LANGFUSE_PK = process.env.LANGFUSE_PK ?? "pk-lf-dev-1234";
const SHOTS = process.env.SHOTS;
const HEAD = { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" };
const TAG = `obs${Date.now().toString().slice(-6)}`;

let pass = 0;
const fails = [];
const check = (name, cond, detail = "") => {
  if (cond) {
    pass++;
    console.log(`  ok   ${name}`);
  } else {
    fails.push(`${name}${detail ? ` — ${detail}` : ""}`);
    console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ""}`);
  }
};
const shot = async (page, name, opts = {}) => {
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/${name}.png`, ...opts });
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const api = async (path, init = {}) => {
  const res = await fetch(`${API}${path}`, { ...init, headers: { ...HEAD, ...init.headers } });
  const text = await res.text();
  return text ? JSON.parse(text) : null;
};
const destinations = () => api("/api/telemetry/destinations");
const byName = async (name) => (await destinations()).find((d) => d.name === name);

// ── stub upstream: an OpenAI-compatible endpoint that always answers ──
const upstream = http.createServer((req, res) => {
  req.resume();
  req.on("end", () => {
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        id: "chatcmpl-obs",
        object: "chat.completion",
        created: Math.floor(Date.now() / 1000),
        model: "stub",
        choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
        usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 },
      }),
    );
  });
});
await new Promise((r) => upstream.listen(0, "127.0.0.1", r));

const created = { providers: [], deployments: [], keys: [] };
const original = await api("/api/settings/telemetry");

const cleanup = await fixtures(API, TOKEN);
const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
// The console reads its admin token from localStorage; without it every page
// bounces to login and nothing under test renders.
await ctx.addInitScript(
  ([t, m]) => {
    window.localStorage.setItem("llmr_token", t);
    window.localStorage.setItem("llmr_theme", m);
  },
  [TOKEN, "dark"],
);

try {
  // Traffic must reach the destinations while they are tested.
  await api("/api/settings/telemetry", {
    method: "PUT",
    body: JSON.stringify({ mode: "everything", capture_content: false, rules: original.rules }),
  });
  const team = (await api("/api/teams"))[0];
  if (!team) throw new Error("needs at least one team");

  const page = await ctx.newPage();
  page.on("pageerror", (e) => fails.push(`pageerror: ${e.message}`));
  await page.goto(`${BASE}/observability`, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  await shot(page, "01-page-before", { fullPage: true });

  // ── 1. add an OTLP destination and test it before saving ────────
  const otlpName = `${TAG} tempo`;
  await page.getByRole("button", { name: "Add destination" }).first().click();
  const dialog = page.getByRole("dialog");
  await dialog.waitFor();
  await dialog.getByLabel("Name", { exact: false }).first().fill(otlpName);
  await dialog.getByText("OpenTelemetry (OTLP/HTTP)").click();
  check("the OTLP kind card shows Beta", await dialog.getByText("Beta").first().isVisible());
  await dialog.getByLabel("Endpoint").fill(OTLP);
  await dialog.getByRole("button", { name: "Advanced" }).click();
  await dialog.getByLabel("Trace link").fill("http://localhost:3000/explore?traceId={trace_id}");
  await dialog.getByRole("button", { name: "Add header" }).click();
  await dialog.getByLabel("Header 1 name").fill("X-Scope-OrgID");
  await dialog.getByLabel("Header 1 value").fill("tenant-a");
  await dialog.getByRole("button", { name: "Test", exact: true }).click();
  const accepted = dialog.getByText("Test span accepted");
  await accepted.waitFor({ timeout: 30_000 }).catch(() => {});
  check("testing an unsaved OTLP draft succeeds", await accepted.isVisible());
  const link = dialog.getByRole("link", { name: "Open the test trace" });
  const href = (await link.count()) ? await link.getAttribute("href") : "";
  const traceId = href?.match(/[0-9a-f]{32}/)?.[0];
  check("the test result links to the test trace", !!traceId, href ?? "no link");
  await shot(page, "02-dialog-otlp-tested");

  if (traceId) {
    let found = false;
    for (let i = 0; i < 15 && !found; i++) {
      const r = await fetch(`${TEMPO}/api/v2/traces/${traceId}`).catch(() => null);
      found = r?.ok ?? false;
      if (!found) await sleep(1000);
    }
    check("the test span arrived in Tempo", found, traceId);
  }

  await dialog.getByRole("button", { name: "Add destination" }).click();
  await dialog.waitFor({ state: "hidden" });
  await page.waitForTimeout(500);
  let otlp = await byName(otlpName);
  check("OTLP destination saved", !!otlp);
  check("its header is stored by name only", JSON.stringify(otlp?.headers) === '["X-Scope-OrgID"]', JSON.stringify(otlp?.headers));
  check("its trace link template is stored", !!otlp?.config?.trace_link?.includes("{trace_id}"));
  const row = () => page.getByRole("row").filter({ hasText: otlpName });
  check("it shows in the table", await row().isVisible());

  // ── 2. edit headers: stored value unchanged, replace one, add one ──
  await row().getByRole("button", { name: `Edit ${otlpName}` }).click();
  await dialog.waitFor();
  const storedValue = dialog.getByLabel("Header 1 value");
  check("a stored header value shows as unchanged", (await storedValue.getAttribute("placeholder")) === "unchanged");
  await storedValue.fill("tenant-b");
  await dialog.getByRole("button", { name: "Add header" }).click();
  await dialog.getByLabel("Header 2 name").fill("Authorization");
  await dialog.getByLabel("Header 2 value").fill("Bearer obs-test");
  await dialog.getByRole("button", { name: "Test", exact: true }).click();
  await dialog.getByText(/Test span accepted|Test failed/).waitFor({ timeout: 30_000 }).catch(() => {});
  check("testing an edit fills in stored values and succeeds", await dialog.getByText("Test span accepted").isVisible());
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await dialog.waitFor({ state: "hidden" });
  otlp = await byName(otlpName);
  check(
    "edit keeps both headers",
    JSON.stringify([...(otlp?.headers ?? [])].sort()) === '["Authorization","X-Scope-OrgID"]',
    JSON.stringify(otlp?.headers),
  );

  await row().getByRole("button", { name: `Edit ${otlpName}` }).click();
  await dialog.waitFor();
  await dialog.getByRole("button", { name: "Remove header X-Scope-OrgID" }).click();
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await dialog.waitFor({ state: "hidden" });
  otlp = await byName(otlpName);
  check("removing a header deletes it", JSON.stringify(otlp?.headers) === '["Authorization"]', JSON.stringify(otlp?.headers));

  // ── 3. scope: selected teams, groups except the control plane ───
  await row().getByRole("button", { name: `Edit ${otlpName}` }).click();
  await dialog.waitFor();
  await dialog.getByRole("radiogroup", { name: "Teams scope" }).getByRole("radio", { name: "Selected" }).click();
  check("selected with nothing chosen warns", await dialog.getByText(/No teams selected/).isVisible());
  await dialog.getByLabel("Teams to export").click();
  await page.getByRole("option", { name: team.name }).click();
  await page.keyboard.press("Escape");
  await dialog.getByRole("radiogroup", { name: "Gateway groups scope" }).getByRole("radio", { name: "All except" }).click();
  await dialog.getByLabel("Gateway groups to leave out").click();
  check("the group list offers the control plane", await page.getByRole("option", { name: "Control plane" }).isVisible());
  await page.getByRole("option", { name: "Control plane" }).click();
  await page.keyboard.press("Escape");
  await dialog.getByText("Full content", { exact: true }).click();
  check("full content shows the size cap", await dialog.getByLabel("Content cap (KiB)").isVisible());
  await page.waitForTimeout(400);
  await shot(page, "03-dialog-scope-content");
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await dialog.waitFor({ state: "hidden" }).catch(() => {});
  otlp = await byName(otlpName);
  check(
    "team scope saved",
    otlp?.team_scope.mode === "selected" && JSON.stringify(otlp.team_scope.values) === JSON.stringify([team.id]),
    JSON.stringify(otlp?.team_scope),
  );
  check(
    "group scope saved",
    otlp?.group_scope.mode === "except" && JSON.stringify(otlp.group_scope.values) === '["_control_plane"]',
    JSON.stringify(otlp?.group_scope),
  );
  check("content mode saved", otlp?.content_mode === "content", otlp?.content_mode);
  check("headers survive an edit that leaves them alone", JSON.stringify(otlp?.headers) === '["Authorization"]');
  await page.waitForTimeout(300);
  const scopeCell = (await row().textContent()) ?? "";
  check("the table summarises the scope", scopeCell.includes(team.name) && scopeCell.includes("except Control plane"), scopeCell);

  // Probe traffic is served by the control plane itself: put the group
  // scope back so it reaches this destination.
  await api(`/api/telemetry/destinations/${otlp.id}`, {
    method: "PUT",
    body: JSON.stringify({ ...otlp, group_scope: { mode: "all", values: [] }, headers: undefined, secrets: {} }),
  });

  // ── 4. a Langfuse destination with the wrong secret ─────────────
  const lfName = `${TAG} langfuse-bad`;
  await page.getByRole("button", { name: "Add destination" }).first().click();
  await dialog.waitFor();
  await dialog.getByLabel("Name", { exact: false }).first().fill(lfName);
  await dialog.getByText("Langfuse", { exact: true }).click();
  await dialog.getByLabel("Host").fill(LANGFUSE);
  await dialog.getByLabel("Public key").fill(LANGFUSE_PK);
  await dialog.getByLabel("Secret key").fill("sk-lf-wrong");
  await dialog.getByRole("button", { name: "Test", exact: true }).click();
  const failed = dialog.getByText(/^Test failed/);
  await failed.waitFor({ timeout: 30_000 }).catch(() => {});
  check("a wrong Langfuse secret fails the test", await failed.isVisible());
  check("the failure names the HTTP status", /HTTP 401/.test((await failed.textContent().catch(() => "")) ?? ""));
  await shot(page, "04-dialog-langfuse-failed");
  await dialog.getByRole("button", { name: "Add destination" }).click();
  await dialog.waitFor({ state: "hidden" });
  const lf = await byName(lfName);
  check("Langfuse destination saved", !!lf);
  check(
    "its secret is reported as set, never returned",
    JSON.stringify(lf?.secret_names) === '["secret_key"]' && !JSON.stringify(lf).includes("sk-lf-wrong"),
  );

  await page.getByRole("row").filter({ hasText: lfName }).getByRole("button", { name: `Edit ${lfName}` }).click();
  await dialog.waitFor();
  check("a stored secret shows as set", (await dialog.getByLabel("Secret key").getAttribute("placeholder")) === "set (unchanged)");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await dialog.waitFor({ state: "hidden" });

  // ── 5. real traffic: failing health and trace links ─────────────
  const prov = await api("/api/providers", {
    method: "POST",
    body: JSON.stringify({
      name: `${TAG}-stub`,
      type: "openai_compatible",
      base_url: `http://127.0.0.1:${upstream.address().port}/v1`,
      auth_mode: "bearer",
      api_key: "stub",
    }),
  });
  created.providers.push(prov.id);
  const dep = await api("/api/deployments", {
    method: "POST",
    body: JSON.stringify({ provider_id: prov.id, model_name: `${TAG}-model`, upstream_name: "stub" }),
  });
  created.deployments.push(dep.id);
  const key = await api("/api/keys", { method: "POST", body: JSON.stringify({ name: `${TAG}-key`, team_id: team.id }) });
  created.keys.push(key.id);
  await sleep(1500); // snapshot reload

  const traceIds = [];
  for (let i = 0; i < 3; i++) {
    const r = await fetch(`${API}/v1/chat/completions`, {
      method: "POST",
      headers: { Authorization: `Bearer ${key.key}`, "Content-Type": "application/json" },
      body: JSON.stringify({ model: `${TAG}-model`, messages: [{ role: "user", content: "hello" }] }),
    });
    await r.text();
    traceIds.push(r.headers.get("x-llmr-trace-id"));
    check(`probe request ${i + 1} answered`, r.ok, String(r.status));
  }

  let lfNow = null;
  for (let i = 0; i < 30; i++) {
    lfNow = await byName(lfName);
    if (lfNow?.health.status === "failing") break;
    await sleep(1000);
  }
  check("the wrong-secret destination turns failing", lfNow?.health.status === "failing", JSON.stringify(lfNow?.health));
  let otlpNow = null;
  for (let i = 0; i < 30; i++) {
    otlpNow = await byName(otlpName);
    if (otlpNow?.health.status === "healthy") break;
    await sleep(1000);
  }
  check("the OTLP destination turns healthy", otlpNow?.health.status === "healthy", JSON.stringify(otlpNow?.health));

  await page.reload({ waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  const lfRow = page.getByRole("row").filter({ hasText: lfName });
  check("the table marks it Failing", await lfRow.getByText("Failing").isVisible());
  check("the OTLP row reads Healthy", await row().getByText("Healthy").isVisible());
  check("a failing banner names it", await page.getByText(`${lfName} is failing`).isVisible());
  await lfRow.getByText("Failing").hover();
  await page.waitForTimeout(800);
  // Other destinations' tooltips hold a last error too, hidden: look for a shown one.
  const shown = await Promise.all((await page.getByText(/^Last error/).all()).map((l) => l.isVisible().catch(() => false)));
  check("hovering the status shows the last error", shown.includes(true));
  await shot(page, "05-card-failing", { fullPage: true });

  await page.goto(`${BASE}/requests`, { waitUntil: "networkidle" });
  await page.waitForTimeout(800);
  const reqRow = page.getByRole("row").filter({ hasText: `${TAG}-model` }).first();
  const traceLink = reqRow.getByRole("link", { name: "open" });
  const menu = reqRow.getByRole("button", { name: "open" });
  if ((await traceLink.count()) > 0) {
    const h = (await traceLink.getAttribute("href")) ?? "";
    check("a request row links to its own trace id", traceIds.some((t) => t && h.includes(t)), h);
    check("the link is filled from the destination's template", h.startsWith("http://localhost:3000/explore?traceId="), h);
  } else {
    check("a request row offers its trace links", (await menu.count()) > 0);
    if (await menu.count()) {
      await menu.click();
      check("the menu lists the OTLP destination", await page.getByRole("menuitem", { name: otlpName }).isVisible());
      await page.keyboard.press("Escape");
    }
  }
  await shot(page, "06-requests-trace-link");

  // ── 6. disable one, delete one ──────────────────────────────────
  await page.goto(`${BASE}/observability`, { waitUntil: "networkidle" });
  await page.waitForTimeout(600);
  await row().getByRole("switch", { name: `Disable ${otlpName}` }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Disable" }).click();
  await page.waitForTimeout(900);
  otlp = await byName(otlpName);
  check("disabling persists", otlp?.enabled === false);
  check("disabled reads as Disabled", await row().getByText("Disabled").isVisible());
  check("disabling keeps the stored header", JSON.stringify(otlp?.headers) === '["Authorization"]');

  await page.getByRole("row").filter({ hasText: lfName }).getByRole("button", { name: `Delete ${lfName}` }).click();
  const confirm = page.getByRole("alertdialog");
  await confirm.waitFor();
  check("delete asks for confirmation", await confirm.isVisible());
  await shot(page, "07-delete-confirm");
  await confirm.getByRole("button", { name: "Delete" }).click();
  await page.waitForTimeout(900);
  check("delete removes it", !(await byName(lfName)));
  check("and the table drops the row", (await page.getByRole("row").filter({ hasText: lfName }).count()) === 0);
  await shot(page, "08-card-after", { fullPage: true });

  // ── 7. capture policy: a draft with an explicit apply ───────────
  const modeGroup = page.getByRole("radiogroup", { name: "Capture mode" });
  check("capture policy says it applies before every destination", await page.getByText(/Applies before every destination/).isVisible());
  await modeGroup.getByRole("radio", { name: "By rule" }).click();
  await page.waitForTimeout(300);
  check("changing mode shows an unsaved-changes banner", await page.getByText(/unsaved change/i).first().isVisible());
  check("pending diff names the mode transition", await page.getByText(/^·\s*Mode:/).first().isVisible());
  const midway = await api("/api/settings/telemetry");
  check("mode change is not persisted before Apply", midway.mode === "everything", midway.mode);

  await page.getByRole("button", { name: "Discard" }).click();
  await page.waitForTimeout(300);
  check(
    "Discard restores the stored mode",
    (await modeGroup.getByRole("radio", { name: "Everything" }).getAttribute("aria-checked")) === "true",
  );

  await modeGroup.getByRole("radio", { name: "By rule" }).click();
  await page.getByLabel("Applies to").click();
  await page.getByRole("option", { name: "Team" }).first().click();
  await page.getByLabel("Target").click();
  await page.getByRole("option", { name: team.name }).first().click();
  await page.getByRole("button", { name: "Add rule" }).click();
  await page.waitForTimeout(300);
  check("adding a rule shows it as a pending change", await page.getByText(/^·\s*Add rule:/).first().isVisible());
  await page.getByRole("button", { name: "Apply policy" }).click();
  await page.waitForTimeout(900);
  const applied = await api("/api/settings/telemetry");
  check(
    "Apply persists the policy-only shape",
    applied.mode === "by_rule" && applied.rules.some((r) => r.scope_value === team.id),
    JSON.stringify(applied),
  );
  check("the policy carries no destination fields", !("host" in applied) && !("telemetry" in applied));
  await page.reload({ waitUntil: "networkidle" });
  await page.waitForTimeout(600);
  check(
    "mode still selected after refresh",
    (await page.getByRole("radiogroup", { name: "Capture mode" }).getByRole("radio", { name: "By rule" }).getAttribute("aria-checked")) === "true",
  );
} finally {
  await cleanup();
  for (const d of (await destinations()) ?? []) {
    if (d.name.startsWith(TAG)) await api(`/api/telemetry/destinations/${d.id}`, { method: "DELETE" });
  }
  for (const id of created.keys) await api(`/api/keys/${id}`, { method: "DELETE" });
  for (const id of created.deployments) await api(`/api/deployments/${id}`, { method: "DELETE" });
  for (const id of created.providers) await api(`/api/providers/${id}`, { method: "DELETE" });
  await api("/api/settings/telemetry", { method: "PUT", body: JSON.stringify(original) });
  const back = await api("/api/settings/telemetry");
  const left = ((await destinations()) ?? []).filter((d) => d.name.startsWith(TAG));
  check(
    "original capture policy restored",
    back.mode === original.mode &&
      back.capture_content === original.capture_content &&
      JSON.stringify(back.rules) === JSON.stringify(original.rules),
  );
  check("no probe destinations left behind", left.length === 0);
  await browser.close();
  upstream.close();
}

console.log(`\n${pass} passed, ${fails.length} failed`);
if (fails.length) {
  for (const f of fails) console.log(`  - ${f}`);
  process.exit(1);
}
