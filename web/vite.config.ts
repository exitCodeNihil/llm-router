import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

/**
 * Dev: Vite serves the console with HMR on :5173 and proxies API traffic to the
 * Go gateway on :8080 (started by `make dev`, which runs air for live reload).
 * Prod: `npm run build` emits dist/, which web/embed.go bakes into the binary.
 *
 * Override the backend with LLMR_DEV_BACKEND when running the gateway elsewhere.
 */
const backend = process.env.LLMR_DEV_BACKEND ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      // ws: the workspace IDE's terminal and language servers are websockets
      // under /api; without it they never connect through the dev server.
      // Object form keeps the browser's Host header, which the gateway's
      // CSRF guard compares against Origin (the string form rewrites it).
      "/api": { target: backend, ws: true },
      "/auth": { target: backend },
      "/v1": { target: backend },
    },
  },
});
