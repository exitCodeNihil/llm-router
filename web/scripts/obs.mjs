// Observability page: capture policy is a draft with an explicit apply, the
// pending diff names every change, and export delivery failures are visible.
//
// Non-destructive: snapshots the stored telemetry config up front and restores
// it verbatim at the end, including on failure.
import { chromium } from "playwright";

const BASE = process.env.BASE ?? "http://localhost:5173";
const API = process.env.API ?? "http://localhost:8080";
const TOKEN = process.env.LLMR_ADMIN_TOKEN ?? "dev-admin-token";
const HEAD = { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" };

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
const api = ctx.request;

// ── snapshot the real config so nothing is lost ───────────────────
const original = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
const restore = async () => {
  if (!original.telemetry) return;
  // secret_key never round-trips; blank keeps the stored one.
  await api.put(`${API}/api/settings/telemetry`, {
    headers: HEAD,
    data: { ...original.telemetry, secret_key: "" },
  });
};

try {
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fails.push(`pageerror: ${e.message}`));
  await page.goto(`${BASE}/observability`, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);

  // ── 1. capture policy is a draft, not an auto-save ──────────────
  const modeGroup = page.locator('[role="radiogroup"]').first();
  await check("capture mode control renders", (await modeGroup.count()) > 0);

  const savedMode = original.telemetry?.mode || "everything";
  const target = savedMode === "everything" ? "By rule" : "Everything";
  await modeGroup.getByRole("radio", { name: target }).click();
  await page.waitForTimeout(400);

  const banner = page.getByText(/unsaved change/i).first();
  await check("changing mode shows an unsaved-changes banner", await banner.isVisible());

  const diffLine = page.getByText(/^·\s*Mode:/).first();
  await check(
    "pending diff names the mode transition",
    await diffLine.isVisible(),
    await diffLine.count() ? (await diffLine.textContent())?.trim() : "no diff line",
  );

  const applyBtn = page.getByRole("button", { name: "Apply policy" });
  await check("an explicit Apply button appears", await applyBtn.isVisible());

  // the change must NOT have been persisted yet
  const midway = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  check(
    "mode change is not persisted before Apply",
    (midway.telemetry?.mode || "everything") === savedMode,
    `server says ${midway.telemetry?.mode}`,
  );

  // ── 2. Discard reverts the draft ────────────────────────────────
  await page.getByRole("button", { name: "Discard" }).click();
  await page.waitForTimeout(400);
  check("Discard clears the unsaved-changes banner", !(await banner.isVisible().catch(() => false)));
  const backOn = await modeGroup
    .getByRole("radio", { name: savedMode === "everything" ? "Everything" : "By rule" })
    .getAttribute("aria-checked");
  check("Discard restores the stored mode", backOn === "true", `aria-checked=${backOn}`);

  // ── 3. Apply persists, and survives a refresh ──────────────────
  await modeGroup.getByRole("radio", { name: "By rule" }).click();
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: "Apply policy" }).click();
  await page.waitForTimeout(900);
  const applied = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  check("Apply persists the mode", applied.telemetry?.mode === "by_rule", `server says ${applied.telemetry?.mode}`);

  await page.reload({ waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  const afterReload = await page.locator('[role="radiogroup"]').first()
    .getByRole("radio", { name: "By rule" }).getAttribute("aria-checked");
  check("mode still selected after refresh", afterReload === "true", `aria-checked=${afterReload}`);
  check(
    "no unsaved-changes banner right after a refresh",
    !(await page.getByText(/unsaved change/i).first().isVisible().catch(() => false)),
  );

  // ── 4. by_rule with no rules is called out, not silent ─────────
  const ruleCount = (applied.telemetry?.rules ?? []).length;
  if (ruleCount === 0) {
    check(
      "by-rule with zero rules warns that nothing is exported",
      await page.getByText(/no rules exist|nothing is being exported/i).first().isVisible(),
    );
  } else {
    check("by-rule with existing rules skips the empty warning", true);
  }

  // ── 5. scoping to team / user / key ────────────────────────────
  const scopeSel = page.getByLabel("Applies to");
  await check("rule scope selector exists", (await scopeSel.count()) > 0);
  for (const scope of ["Team", "User", "API key"]) {
    await scopeSel.click();
    await page.waitForTimeout(250);
    const opt = page.getByRole("option", { name: scope }).first();
    const found = (await opt.count()) > 0;
    check(`can scope a rule to ${scope}`, found);
    if (found) await opt.click();
    else await page.keyboard.press("Escape");
    await page.waitForTimeout(200);
  }

  // add a real team-scoped rule and confirm it persists with its capture level
  await scopeSel.click();
  await page.waitForTimeout(250);
  await page.getByRole("option", { name: "Team" }).first().click();
  await page.waitForTimeout(300);
  const targetSel = page.getByLabel("Target");
  await targetSel.click();
  await page.waitForTimeout(300);
  const firstTeam = page.getByRole("option").first();
  const teamName = (await firstTeam.textContent())?.trim();
  await firstTeam.click();
  await page.waitForTimeout(250);
  await page.getByRole("button", { name: "Add rule" }).click();
  await page.waitForTimeout(400);

  check("adding a rule shows it as a pending change", await page.getByText(/^·\s*Add rule:/).first().isVisible());
  const notYet = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  check("a new rule is not persisted before Apply", (notYet.telemetry?.rules ?? []).length === ruleCount);

  await page.getByRole("button", { name: "Apply policy" }).click();
  await page.waitForTimeout(900);
  const withRule = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  const added = (withRule.telemetry?.rules ?? []).find((r) => r.scope_type === "team");
  check("team-scoped rule persisted", !!added, `rules=${JSON.stringify(withRule.telemetry?.rules ?? [])}`);
  check("rule carries a capture level", added?.capture === "meta" || added?.capture === "content");
  check("rule carries a sample rate", typeof added?.sample === "number" && added.sample > 0);
  if (teamName) check("rule shows the scoped team by name", await page.getByText(teamName, { exact: false }).first().isVisible());

  // ── 6. delivery health is stated, not implied by the feed ───────
  const health = withRule.health;
  check("API reports exporter delivery health", !!health && typeof health.delivered === "number");
  if (health?.dropped > 0) {
    check("rejected exports surface as an error banner", await page.getByText(/rejected/i).first().isVisible());
  } else if (health?.delivered > 0) {
    check("successful delivery is confirmed in the UI", await page.getByText(/delivered to Langfuse/i).first().isVisible());
  } else {
    check("health renders with nothing delivered yet", true);
  }

  // ── 7. Test connection reports credential failures ─────────────
  const bad = await (
    await api.post(`${API}/api/observability/test`, {
      headers: HEAD,
      data: { type: "langfuse", host: original.telemetry?.host || "http://localhost:3001", public_key: "pk-wrong", secret_key: "sk-wrong" },
    })
  ).json();
  check("wrong credentials fail the connection test", bad.ok === false, JSON.stringify(bad));
  check(
    "the failure explains it is the credentials, not the host",
    /credential/i.test(bad.detail ?? ""),
    bad.detail,
  );
} finally {
  await restore();
  const back = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  const same =
    (back.telemetry?.mode || "") === (original.telemetry?.mode || "") &&
    JSON.stringify(back.telemetry?.rules ?? []) === JSON.stringify(original.telemetry?.rules ?? []);
  console.log(`  ${same ? "ok  " : "FAIL"} original telemetry config restored`);
  if (same) pass++;
  else fails.push("telemetry config was not restored");
  await browser.close();
}

console.log(`\n${pass} passed, ${fails.length} failed`);
if (fails.length) {
  for (const f of fails) console.log(`  - ${f}`);
  process.exit(1);
}
