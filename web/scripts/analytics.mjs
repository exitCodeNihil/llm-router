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

  // ── Trace links ─────────────────────────────────────────────────
  // Each enabled destination with a link template ({trace_id} in trace_url)
  // links every row that has a trace id; Langfuse also links older rows by
  // their request id, which its old exporter used as the trace id.
  const destsRes = await api.get(`${API}/api/telemetry/destinations`, { headers: HEAD });
  check("destinations list loads", destsRes.ok(), `status ${destsRes.status()}`);
  const dests = destsRes.ok() ? await destsRes.json() : [];
  const linked = dests.filter((d) => d.enabled && d.trace_url);
  const openLinks = page.locator("tbody").getByRole("link", { name: "open", exact: true });
  const openMenus = page.locator("tbody").getByRole("button", { name: "open", exact: true });
  if (linked.length === 0) {
    check("no trace column without a linked destination", (await page.getByRole("columnheader", { name: "Trace" }).count()) === 0);
    check("no trace link without a linked destination", (await openLinks.count()) + (await openMenus.count()) === 0);
  } else {
    for (const d of linked) {
      check(`${d.name}'s trace link is a template`, d.trace_url.includes("{trace_id}"), d.trace_url);
    }
    // The same rows the page shows: its own query string, as the console sends it.
    const qs = new URL(page.url()).searchParams;
    qs.set("limit", "50");
    const reqRows = await (await api.get(`${API}/api/usage/requests?${qs}`, { headers: HEAD })).json();
    const idsFor = (d) =>
      reqRows.map((r) => r.trace_id ?? (d.kind === "langfuse" ? r.request_id : null)).filter(Boolean);
    const expected = new Set(
      linked.flatMap((d) => idsFor(d).map((id) => d.trace_url.replaceAll("{trace_id}", encodeURIComponent(id)))),
    );
    check("the trace column is shown", (await page.getByRole("columnheader", { name: "Trace" }).count()) > 0);
    if (linked.length === 1) {
      const linkable = idsFor(linked[0]).length;
      const hrefs = await openLinks.evaluateAll((as) => as.map((a) => a.getAttribute("href")));
      check(
        "every row with a trace id links to it, and only those",
        hrefs.length === linkable,
        `${hrefs.length} links for ${linkable} of ${reqRows.length} rows with a trace id`,
      );
      const stray = hrefs.filter((h) => !expected.has(h));
      check("each link fills the template with the row's own trace id", stray.length === 0, stray.slice(0, 2).join(" | "));
      if (hrefs[0]) {
        const res = await api.get(hrefs[0], { maxRedirects: 5 }).catch((e) => ({ status: () => e.message }));
        check("that trace URL resolves", Number(res.status()) < 400, `status ${res.status()}`);
      }
    } else {
      const menus = await openMenus.count();
      const traced = reqRows.filter((r) => r.trace_id).length;
      check("rows with a trace id offer a link per destination", menus === traced, `${menus} menus for ${traced} traced rows`);
      if (menus > 0) {
        await openMenus.first().click();
        await page.waitForTimeout(300);
        const items = await page.getByRole("menuitem").allTextContents();
        check(
          "the menu names each linked destination",
          linked.every((d) => items.some((t) => t.includes(d.name))),
          items.join(", "),
        );
        await page.keyboard.press("Escape");
      }
    }
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
