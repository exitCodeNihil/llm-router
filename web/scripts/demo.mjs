/**
 * Records the README demo GIFs (docs/media/*.gif). Run it through `make demo-gifs`, which
 * resets the stack to an empty database and starts scripts/anthropic-stub.py first.
 *
 *   node web/scripts/demo.mjs [base-url] [scene ...]     scenes: quickstart keys-connect dashboard
 *
 * Needs ffmpeg on PATH. Scenes after "quickstart" log in as the admin it creates, so they
 * can be re-recorded on their own once that scene has run once.
 */
import { chromium } from "playwright";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const BASE = process.argv[2] ?? "http://localhost:8080";
const ONLY = process.argv.slice(3);
// The gateway runs in a container, so the host's stub is reached by the engine's host alias.
const UPSTREAM = process.env.DEMO_UPSTREAM ?? "http://host.containers.internal:9901";
const OUT = fileURLToPath(new URL("../../docs/media/", import.meta.url));
const TMP = mkdtempSync(join(tmpdir(), "llmr-demo-"));
const ADMIN = { name: "Ada Lovelace", email: "ada@example.com", password: "demo-password-1" };
const MODEL = "claude-sonnet-5";
// Narrow enough that UI text stays readable at README width, wide enough that the Dashboard
// KPI row and the Requests table do not wrap or scroll.
const SIZE = { width: 1200, height: 750 };

// Playwright videos have no pointer; draw one, with a ripple on click, so viewers can follow.
const POINTER = () => {
  addEventListener("DOMContentLoaded", () => {
    // Fades and slides are the expensive frames in a GIF; let UI state changes land instantly.
    const css = document.createElement("style");
    css.textContent = "*,*::before,*::after{transition:none!important;animation-duration:0s!important;animation-delay:0s!important}";
    document.head.append(css);
    const dot = document.createElement("div");
    dot.style.cssText =
      "position:fixed;z-index:2147483647;pointer-events:none;width:20px;height:20px;margin:-10px 0 0 -10px;" +
      "border-radius:50%;background:rgba(255,255,255,.92);border:2px solid #111;box-shadow:0 2px 8px rgba(0,0,0,.55)";
    const at = (x, y) => Object.assign(dot.style, { left: x + "px", top: y + "px" });
    const [sx, sy] = (sessionStorage.getItem("__ptr") ?? "-40,-40").split(",").map(Number);
    at(sx, sy);
    document.body.append(dot);
    addEventListener("mousemove", (e) => {
      at(e.clientX, e.clientY);
      sessionStorage.setItem("__ptr", `${e.clientX},${e.clientY}`);
    }, true);
    addEventListener("mousedown", (e) => {
      const ring = document.createElement("div");
      ring.style.cssText =
        `position:fixed;z-index:2147483646;pointer-events:none;left:${e.clientX - 18}px;top:${e.clientY - 18}px;` +
        "width:36px;height:36px;border-radius:50%;border:3px solid #f5a524";
      document.body.append(ring);
      ring.animate([{ transform: "scale(.4)", opacity: 1 }, { transform: "scale(1.5)", opacity: 0 }], { duration: 450 })
        .finished.then(() => ring.remove());
    }, true);
  });
};

const pause = (page, ms) => page.waitForTimeout(ms);

async function click(page, loc, after = 350) {
  await loc.scrollIntoViewIfNeeded();
  const b = await loc.boundingBox();
  await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2, { steps: 12 });
  await pause(page, 100);
  await page.mouse.down();
  await page.mouse.up();
  await pause(page, after);
}

async function type(page, loc, text) {
  await click(page, loc, 120);
  await page.keyboard.type(text, { delay: 24 });
  await pause(page, 200);
}

async function pick(page, combo, option) {
  await click(page, combo, 250);
  await click(page, page.getByRole("option", { name: option, exact: true }), 300);
}

const nav = (page, name) => click(page, page.getByRole("link", { name, exact: true }), 700);

function toGif(webm, name, seconds, speed) {
  const gif = join(OUT, `${name}.gif`);
  // The video starts late (first paint) but ends when the context closes, so trim from the end.
  // One filtergraph, two passes: palettegen sees the whole clip, paletteuse then maps to it.
  const vf =
    `setpts=PTS/${speed},fps=10,scale=1100:-1:flags=lanczos,split[a][b];` +
    `[a]palettegen=max_colors=48:stats_mode=full[p];[b][p]paletteuse=dither=none:diff_mode=rectangle`;
  execFileSync("ffmpeg", ["-v", "error", "-y", "-sseof", `-${seconds.toFixed(2)}`, "-i", webm, "-vf", vf, gif]);
  const kb = Math.round(statSync(gif).size / 1024);
  console.log(`  ${name}.gif  ${kb} KB  ${(seconds / speed).toFixed(1)} s`);
}

const browser = await chromium.launch();

/** Record one scene. `run(page, mark)` calls mark() once the first screen is worth showing. */
async function scene(name, { speed = 1, hold = 1800 }, run) {
  if (ONLY.length && !ONLY.includes(name)) return;
  console.log(`recording ${name}`);
  const ctx = await browser.newContext({ viewport: SIZE, recordVideo: { dir: TMP, size: SIZE } });
  await ctx.addInitScript(POINTER);
  const page = await ctx.newPage();
  const t0 = Date.now();
  let start = 0;
  await run(page, ctx, () => (start = (Date.now() - t0) / 1000));
  await pause(page, hold);
  const seconds = (Date.now() - t0) / 1000 - start;
  await ctx.close();
  toGif(await page.video().path(), name, seconds, speed);
}

async function login(ctx) {
  const r = await ctx.request.post(`${BASE}/auth/password`, { data: ADMIN });
  if (!r.ok()) throw new Error(`login failed (${r.status()}): run quickstart on an empty database first`);
}

async function fire(key, n) {
  for (let i = 0; i < n; i++) {
    const r = await fetch(`${BASE}/v1/messages`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${key}`, "anthropic-version": "2023-06-01" },
      body: JSON.stringify({ model: MODEL, max_tokens: 64, stream: i % 3 === 0, messages: [{ role: "user", content: `hello ${i}` }] }),
    });
    if (!r.ok) throw new Error(`request through the key failed: ${r.status} ${await r.text()}`);
    await r.text();
  }
}

let key = process.env.DEMO_KEY ?? "";

mkdirSync(OUT, { recursive: true });

await scene("quickstart", { speed: 1.5, hold: 1500 }, async (page, _ctx, mark) => {
  await page.goto(BASE);
  await page.getByText("Create the first administrator").waitFor();
  mark();
  await pause(page, 600);
  await type(page, page.getByLabel("Name"), ADMIN.name);
  await type(page, page.getByLabel("Email"), ADMIN.email);
  await type(page, page.getByLabel("Password"), ADMIN.password);
  await click(page, page.getByRole("button", { name: "Set up llm-router" }), 200);

  await page.getByRole("link", { name: "Providers", exact: true }).waitFor();
  await nav(page, "Providers");
  await click(page, page.getByRole("button", { name: "Add provider" }).first());
  const dlg = page.getByRole("dialog");
  await type(page, dlg.getByLabel("Name"), "demo-stub");
  await pick(page, dlg.getByRole("combobox", { name: "Type" }), "OpenAI-compatible");
  await pick(page, dlg.getByRole("combobox", { name: "Auth mode" }), "No auth");
  await type(page, dlg.getByLabel("Base URL"), UPSTREAM);
  await click(page, dlg.getByRole("button", { name: "Add provider" }), 1000);

  await nav(page, "Models");
  await click(page, page.getByRole("button", { name: "Add model" }).first());
  await pick(page, dlg.getByRole("combobox", { name: "Provider" }), "demo-stub");
  await type(page, dlg.getByLabel("Upstream model"), MODEL);
  await click(page, page.getByRole("option").first(), 400);
  await click(page, dlg.getByRole("radio", { name: "Custom" }), 300);
  await type(page, dlg.getByLabel("Input $/1M", { exact: true }), "3");
  await type(page, dlg.getByLabel("Output $/1M", { exact: true }), "15");
  await click(page, dlg.getByRole("button", { name: "Add model" }), 1200);
});

await scene("keys-connect", { speed: 1.15, hold: 2200 }, async (page, ctx, mark) => {
  await login(ctx);
  await page.goto(`${BASE}/keys`);
  await page.getByRole("button", { name: "New key" }).first().waitFor();
  mark();
  await pause(page, 500);
  await click(page, page.getByRole("button", { name: "New key" }).first());
  const dlg = page.getByRole("dialog");
  await type(page, dlg.getByLabel("Name"), "my-laptop");
  await click(page, dlg.getByRole("button", { name: "Create key" }), 1000);

  // The one-time-secret dialog is "required", which Astryx exposes as an alertdialog.
  const connect = page.getByRole("alertdialog");
  key = (await connect.innerText()).match(/llmr_\S+/)[0];
  const landed = fire(key, 1); // usage is flushed every second; send it while the dialog is up
  await pause(page, 1000);
  await click(page, connect.getByRole("radio", { name: /My claude.ai subscription/ }), 1400);
  await click(page, connect.getByRole("radio", { name: "cURL" }), 1500);
  await click(page, connect.getByRole("button", { name: "Done" }), 500);

  await landed;
  await nav(page, "Requests");
});

await scene("dashboard", { speed: 1, hold: 2000 }, async (page, ctx, mark) => {
  await login(ctx);
  if (!key) throw new Error("no key: run keys-connect first or set DEMO_KEY");
  await fire(key, 40);
  await page.goto(BASE);
  await page.getByText("Spend by model").waitFor();
  await pause(page, 1500);
  mark();
  await pause(page, 3200);
  await nav(page, "Requests");
});

await browser.close();
if (process.env.DEMO_KEEP) console.log(`kept raw videos in ${TMP}`);
else rmSync(TMP, { recursive: true, force: true });
