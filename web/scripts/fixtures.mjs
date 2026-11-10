/**
 * What the suites assume a database already has, so they run against an empty
 * one: a priced streaming stub model, a user, an API key and a little traffic.
 * Anything missing is created and removed again by the returned cleanup
 * (usage rows stay); whatever was already there is left alone.
 *
 *   const cleanup = await fixtures(API, TOKEN);
 *   ...
 *   await cleanup();
 */
import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { fileURLToPath } from "node:url";

const STUB = fileURLToPath(new URL("../../scripts/anthropic-stub.py", import.meta.url));
const NAME = "suite-stub";

const freePort = () =>
  new Promise((resolve) => {
    const s = createServer().listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });

export async function fixtures(API, TOKEN, alias = "claude-test") {
  const admin = async (path, method = "GET", body) => {
    const res = await fetch(`${API}${path}`, {
      method,
      headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text}`);
    return text ? JSON.parse(text) : null;
  };

  // A run that crashed leaves its fixtures behind, pointing at a stub that is gone.
  for (const k of await admin("/api/keys")) if (k.name.startsWith(NAME)) await admin(`/api/keys/${k.id}`, "DELETE");
  for (const d of await admin("/api/deployments"))
    if (d.provider_name === NAME || d.provider === NAME) await admin(`/api/deployments/${d.id}`, "DELETE");
  for (const p of await admin("/api/providers")) if (p.name === NAME) await admin(`/api/providers/${p.id}`, "DELETE");
  for (const t of await admin("/api/teams")) if (t.name === NAME) await admin(`/api/teams/${t.id}`, "DELETE");
  for (const u of await admin("/api/users")) if (u.email === `${NAME}@example.com`) await admin(`/api/users/${u.id}`, "DELETE");

  const undo = [];
  let stub;
  process.on("exit", () => stub?.kill());

  if (!(await admin("/api/deployments")).some((d) => d.model_name === alias && d.enabled)) {
    const port = await freePort();
    stub = spawn("python3", [STUB, String(port)], { stdio: "ignore" });
    const up = `http://127.0.0.1:${port}`;
    for (let i = 0; ; i++) {
      if (await fetch(`${up}/`).then(() => true, () => false)) break;
      if (i > 50) throw new Error("the stub upstream did not start; it needs python3");
      await new Promise((r) => setTimeout(r, 100));
    }
    const p = await admin("/api/providers", "POST", {
      name: NAME,
      type: "anthropic",
      base_url: up,
      auth_mode: "api_key",
      api_key: "stub",
    });
    undo.push(() => admin(`/api/providers/${p.id}`, "DELETE"));
    const d = await admin("/api/deployments", "POST", {
      provider_id: p.id,
      model_name: alias,
      upstream_name: "claude-stub",
      api_flavor: "anthropic",
      input_per_1m: 1,
      output_per_1m: 5,
    });
    undo.push(() => admin(`/api/deployments/${d.id}`, "DELETE"));
  }

  if ((await admin("/api/teams")).length === 0) {
    const t = await admin("/api/teams", "POST", { name: NAME });
    undo.push(() => admin(`/api/teams/${t.id}`, "DELETE"));
  }
  let users = await admin("/api/users");
  if (users.length === 0) {
    const u = await admin("/api/users", "POST", { email: `${NAME}@example.com`, name: NAME, role: "member" });
    undo.push(() => admin(`/api/users/${u.id}`, "DELETE"));
    users = [u];
  }
  if ((await admin("/api/keys")).length === 0) {
    const k = await admin("/api/keys", "POST", { name: NAME, user_id: users[0].id });
    undo.push(() => admin(`/api/keys/${k.id}`, "DELETE"));
  }
  // The control plane picks up a new model within a moment.
  if (undo.length) await new Promise((r) => setTimeout(r, 1500));

  // The Dashboard's flow and health cards only draw with traffic, and the flow
  // needs two keys for one to be unrelated to the other. Deleted keys fold
  // into one node, so these stay until cleanup.
  if ((await admin("/api/usage/requests?limit=1")).length === 0) {
    for (const n of [1, 2]) {
      const k = await admin("/api/keys", "POST", { name: `${NAME}-traffic-${n}`, user_id: users[0].id });
      undo.push(() => admin(`/api/keys/${k.id}`, "DELETE"));
      // A new key works once the control plane has reloaded its config.
      for (let sent = 0, tries = 0; sent < 2 && tries < 20; tries++) {
        const res = await fetch(`${API}/v1/messages`, {
          method: "POST",
          headers: { "x-api-key": k.key, "anthropic-version": "2023-06-01", "Content-Type": "application/json" },
          body: JSON.stringify({ model: alias, max_tokens: 16, messages: [{ role: "user", content: "hi" }] }),
        });
        if (res.ok) sent++;
        else await new Promise((r) => setTimeout(r, 500));
      }
    }
    await new Promise((r) => setTimeout(r, 2500)); // usage is written in batches
  }

  return async () => {
    for (const fn of undo.reverse()) await fn().catch(() => {});
    stub?.kill();
  };
}
