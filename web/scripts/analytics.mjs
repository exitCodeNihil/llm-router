// Analytics page: one filter set drives the headline, the trend and the
// breakdown, and the drill-down into Requests inherits it. Read-only against
// existing traffic — creates and deletes nothing.
import { chromium } from "playwright";

const BASE = process.env.BASE ?? "http://localhost:5173";
const API = process.env.API ?? "http://localhost:8080";
const TOKEN = process.env.LLMR_ADMIN_TOKEN ?? "dev-admin-token";
const HEAD = { Authorization: `Bearer ${TOKEN}` };

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
const ctx = await browser.newContext({ viewport: { width: 1500, height: 1200 } });
await ctx.addInitScript((t) => {
  window.localStorage.setItem("llmr_token", t);
  window.localStorage.setItem("llmr_theme", "dark");
}, TOKEN);
const api = ctx.request;

try {
  // ── API: filters actually narrow, rather than being ignored ─────
  const stats = async (qs) => (await api.get(`${API}/api/usage/stats?${qs}`, { headers: HEAD })).json();
  const all = await stats("days=90");
  check("stats returns the headline metrics", all.requests > 0 && "p95_ms" in all && "ttft_p50_ms" in all);
  check("stats counts failovers", "failovers" in all);
  // Subscription traffic is unbilled but not valueless: notional must be
  // reported separately and must never be folded into spend.
  check("stats reports notional cost separately", "notional_cost_usd" in all);
  const sub = await stats("days=90&priced=unpriced");
  check(
    "unbilled traffic contributes nothing to spend",
    sub.cost_usd === 0,
    `cost_usd=${sub.cost_usd}`,
  );
  check(
    "unbilled traffic still carries a list value",
    sub.notional_cost_usd > 0,
    `notional=${sub.notional_cost_usd}`,
  );

  const errOnly = await stats("days=90&status=err");
  check(
    "status=err narrows to failures",
    errOnly.requests <= all.requests && errOnly.errors === errOnly.requests,
    `${errOnly.requests} reqs / ${errOnly.errors} errors of ${all.requests}`,
  );

  const oneModel = await stats("days=90&model=claude-test");
  check(
    "model filter narrows the aggregate",
    oneModel.requests > 0 && oneModel.requests < all.requests,
    `${oneModel.requests} of ${all.requests}`,
  );

  const unpriced = await stats("days=90&priced=unpriced");
  check(
    "priced=unpriced isolates subscription traffic",
    unpriced.requests === unpriced.unpriced_requests,
    `${unpriced.requests} reqs / ${unpriced.unpriced_requests} unpriced`,
  );

  // legacy endpoints kept their contract while gaining filters
  const legacy = await (await api.get(`${API}/api/usage/summary?days=90`, { headers: HEAD })).json();
  check("legacy summary still answers", legacy.requests === all.requests, `${legacy.requests} vs ${all.requests}`);
  const legacyFiltered = await (
    await api.get(`${API}/api/usage/summary?days=90&model=claude-test`, { headers: HEAD })
  ).json();
  check("legacy summary now honours filters", legacyFiltered.requests === oneModel.requests);

  // ── breakdown ───────────────────────────────────────────────────
  const bd = async (dim, extra = "") =>
    (await api.get(`${API}/api/usage/breakdown?days=90&group_by=${dim}${extra}`, { headers: HEAD })).json();
  const byModel = await bd("model");
  check("breakdown by model returns buckets", Array.isArray(byModel) && byModel.length > 0);
  check(
    "breakdown request counts sum to the headline",
    byModel.reduce((n, r) => n + Number(r.requests), 0) === all.requests,
    `${byModel.reduce((n, r) => n + Number(r.requests), 0)} vs ${all.requests}`,
  );
  const byErr = await bd("error_code");
  check("breakdown by error reason works", Array.isArray(byErr) && byErr.length > 0);
  check(
    "error reasons are labelled, not raw nulls",
    byErr.every((r) => typeof r.label === "string" && r.label !== ""),
    JSON.stringify(byErr.slice(0, 3)),
  );
  const bad = await api.get(`${API}/api/usage/breakdown?days=90&group_by=; DROP TABLE`, { headers: HEAD });
  check("unknown group_by is rejected, not interpolated", bad.status() === 400, `status ${bad.status()}`);

  // ── timeseries ──────────────────────────────────────────────────
  const ts = await (await api.get(`${API}/api/usage/timeseries?days=90`, { headers: HEAD })).json();
  check("timeseries returns buckets", Array.isArray(ts) && ts.length > 0);
  const tsFiltered = await (
    await api.get(`${API}/api/usage/timeseries?days=90&model=claude-test`, { headers: HEAD })
  ).json();
  check(
    "timeseries honours filters",
    tsFiltered.reduce((n, p) => n + Number(p.requests), 0) === oneModel.requests,
  );

  // ── the page ────────────────────────────────────────────────────
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(`${BASE}/analytics`, { waitUntil: "networkidle" });
  await page.waitForTimeout(1800);

  check("analytics route renders", await page.getByRole("heading", { name: "Analytics" }).isVisible());
  // Eyebrow labels are uppercased in CSS, so the DOM text is title case.
  const eyebrows = (await page.locator(".eyebrow").allTextContents()).map((t) => t.toLowerCase());
  for (const label of ["spend", "requests", "tokens", "errors", "latency", "ttft"]) {
    check(`headline shows ${label}`, eyebrows.includes(label), eyebrows.join(", "));
  }
  check("trend chart renders", (await page.locator("svg.recharts-surface").count()) > 0);
  check("breakdown table renders rows", (await page.locator("tbody tr").count()) > 0);

  // axis labels must be dates, not raw ISO timestamps
  const axis = await page.locator(".recharts-xAxis text").allTextContents();
  check(
    "trend x-axis is formatted, not raw ISO",
    axis.length === 0 || !axis.some((t) => t.includes("T") && t.includes(":")),
    axis.slice(0, 2).join(" | "),
  );

  // filter → drill-down carries it
  // Astryx gives Selector triggers no accessible name of their own (the visible
  // label is not wired to them), and the element type changes once a Selector
  // becomes searchable. Target the popup attribute, which holds in both modes.
  const modelTrigger = page.locator('[aria-haspopup="listbox"]').first();
  await modelTrigger.click();
  await page.waitForTimeout(350);
  await page.getByRole("option", { name: "claude-test", exact: true }).click();
  await page.waitForTimeout(1400);
  check("filter is reflected as active", (await page.getByText(/1 filter active/).count()) > 0);

  await page.getByText("View matching requests").click();
  await page.waitForTimeout(2000);
  check("drill-down lands on requests with the filter", page.url().includes("model=claude-test"), page.url());
  const rows = await page.locator("tbody tr").count();
  check("drill-down shows matching rows", rows > 0, `${rows} rows`);

  // ── Langfuse deep-link ──────────────────────────────────────────
  const tel = await (await api.get(`${API}/api/settings/telemetry`, { headers: HEAD })).json();
  if (tel.trace_base_url) {
    check("telemetry exposes a trace URL prefix", /\/project\/.+\/traces\/$/.test(tel.trace_base_url), tel.trace_base_url);
    const links = await page.getByRole("link", { name: "open" }).count();
    check("request rows deep-link into Langfuse", links > 0, `${links} links`);
    const href = await page.getByRole("link", { name: "open" }).first().getAttribute("href");
    check("the link targets the request's own trace id", !!href && href.startsWith(tel.trace_base_url) && href.includes("req_"), href ?? "");
    const res = await api.get(href);
    check("that trace URL resolves", res.status() === 200, `status ${res.status()}`);
  } else {
    check("no trace link when telemetry is not configured", (await page.getByRole("link", { name: "open" }).count()) === 0);
  }

  check("no page errors", errors.length === 0, errors.slice(0, 2).join(" | "));
} finally {
  await browser.close();
}

console.log(`\n${pass} passed, ${fails.length} failed`);
if (fails.length) {
  for (const f of fails) console.log(`  - ${f}`);
  process.exit(1);
}
