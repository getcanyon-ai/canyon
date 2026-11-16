import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The bundle is served from the Go binary at the same origin as the API, so there is no CORS stage
// anywhere (contracts/routes.md, Open group); in development Vite stands in for that origin and
// proxies the two paths the dashboard calls. Everything it emits is hashed into dist/assets.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: { "/api": "http://127.0.0.1:7777", "/health": "http://127.0.0.1:7777" },
    // The dashboard and the reference set-up it shows filled in (deploy/gitops, the hand-off's platform team card).
    fs: { allow: [".", "../deploy/gitops"] },
  },
  test: { environment: "jsdom", setupFiles: ["./src/setup-tests.ts"] },
});
