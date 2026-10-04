// Consequential switches must not apply on click. Disabling a key, deployment
// or user changes live behaviour, so each asks first and states what happens.
//
// Non-destructive by construction: for pre-existing rows the toggle is opened
// and then *cancelled*, and the API is checked to prove nothing changed. Only a
// throwaway probe key is actually toggled, and it is deleted afterwards.
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
await ctx.addInitScript((t) => {
  window.localStorage.setItem("llmr_token", t);
  window.localStorage.setItem("llmr_theme", "dark");
}, TOKEN);
const api = ctx.request;
let probeKeyId = null;

try {
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fails.push(`pageerror: ${e.message}`));

  const dialog = () => page.getByRole("alertdialog");

  // ── a throwaway key we may safely toggle for real ───────────────
  const users = await (await api.get(`${API}/api/users`, { headers: HEAD })).json();
  const created = await (
    await api.post(`${API}/api/keys`, {
      headers: HEAD,
      data: { name: "guard-probe-key", user_id: users[0].id },
    })
  ).json();
  probeKeyId = created.id;

  const keyState = async () => {
    const all = await (await api.get(`${API}/api/keys`, { headers: HEAD })).json();
    return all.find((k) => k.id === probeKeyId)?.disabled;
  };

  await page.goto(`${BASE}/keys`, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);

  const row = page.getByRole("row").filter({ hasText: "guard-probe-key" });
  await check("probe key row is present", (await row.count()) > 0);

  // click the switch: must ask, must not apply
  await row.getByRole("switch").first().click();
  await page.waitForTimeout(400);
  check("disabling a key asks first", await dialog().isVisible());
  check(
    "the prompt states the consequence",
    /start failing immediately/i.test((await dialog().textContent()) ?? ""),
    (await dialog().textContent())?.slice(0, 120),
  );
  check("key is untouched while the prompt is open", (await keyState()) === false);

  // cancel → still untouched
  await page.keyboard.press("Escape");
  await page.waitForTimeout(500);
  check("cancelling leaves the key enabled", (await keyState()) === false);
  // Rendered as <input type=checkbox role=switch>, so read .checked — there is
  // no aria-checked attribute to inspect.
  check("switch still reads as enabled after cancelling", await row.getByRole("switch").first().isChecked());

  // confirm → applied
  await row.getByRole("switch").first().click();
  await page.waitForTimeout(400);
  await dialog().getByRole("button", { name: "Disable" }).click();
  await page.waitForTimeout(900);
  check("confirming disables the key", (await keyState()) === true);

  // and the reverse direction asks too, with its own wording
  await row.getByRole("switch").first().click();
  await page.waitForTimeout(400);
  check("re-enabling asks as well", await dialog().isVisible());
  check(
    "the enable prompt states access is restored",
    /regain access/i.test((await dialog().textContent()) ?? ""),
  );
  await dialog().getByRole("button", { name: "Enable" }).click();
  await page.waitForTimeout(900);
  check("confirming re-enables the key", (await keyState()) === false);

  // ── pre-existing rows: open and cancel only ────────────────────
  const pages = [
    {
      path: "/deployments",
      label: "deployment",
      resource: "deployments",
      field: "enabled",
      expect: /stops serving traffic|starts serving traffic/i,
    },
    { path: "/users", label: "user", resource: "users", field: "disabled", expect: /console access/i },
  ];

  for (const p of pages) {
    const before = await (await api.get(`${API}${"/api/" + p.resource}`, { headers: HEAD })).json();
    await page.goto(`${BASE}${p.path}`, { waitUntil: "networkidle" });
    await page.waitForTimeout(700);
    const sw = page.getByRole("switch").first();
    if ((await sw.count()) === 0) {
      check(`${p.label} page has a row to test`, false, "no switch found");
      continue;
    }
    await sw.click();
    await page.waitForTimeout(400);
    check(`toggling a ${p.label} asks first`, await dialog().isVisible());
    check(
      `the ${p.label} prompt states the consequence`,
      p.expect.test((await dialog().textContent()) ?? ""),
      (await dialog().textContent())?.slice(0, 140),
    );
    await page.keyboard.press("Escape");
    await page.waitForTimeout(500);
    const after = await (await api.get(`${API}${"/api/" + p.resource}`, { headers: HEAD })).json();
    check(
      `cancelling changed no ${p.label}`,
      JSON.stringify(before.map((x) => x[p.field])) === JSON.stringify(after.map((x) => x[p.field])),
    );
  }

  // ── capture policy stays a draft ───────────────────────────────
  const tel = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  await page.goto(`${BASE}/observability`, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  const rg = page.locator('[role="radiogroup"]').first();
  const savedMode = tel.telemetry?.mode || "everything";
  await rg.getByRole("radio", { name: savedMode === "off" ? "Everything" : "Off" }).click();
  await page.waitForTimeout(500);
  const still = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  check(
    "changing capture mode does not save on click",
    (still.telemetry?.mode || "everything") === savedMode,
    `server says ${still.telemetry?.mode}`,
  );
  check("an explicit Apply is offered instead", await page.getByRole("button", { name: "Apply policy" }).isVisible());
  await page.getByRole("button", { name: "Discard" }).click();
  await page.waitForTimeout(400);
  const restored = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  check("capture policy still stored as before", (restored.telemetry?.mode || "everything") === savedMode);
} finally {
  if (probeKeyId) {
    await api.delete(`${API}/api/keys/${probeKeyId}`, { headers: HEAD });
    const left = await (await api.get(`${API}/api/keys`, { headers: HEAD })).json();
    const gone = !left.some((k) => k.id === probeKeyId);
    console.log(`  ${gone ? "ok  " : "FAIL"} probe key removed`);
    gone ? pass++ : fails.push("probe key left behind");
  }
  await browser.close();
}

console.log(`\n${pass} passed, ${fails.length} failed`);
if (fails.length) {
  for (const f of fails) console.log(`  - ${f}`);
  process.exit(1);
}
