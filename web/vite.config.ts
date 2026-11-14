import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// apiTarget is `jig serve`'s default listen address.
const apiTarget = "http://127.0.0.1:8383";

// The UI is built into web/dist and committed, so `go build` embeds a
// reviewed production bundle and an operator never needs Node (R18).
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // No sourcemaps: dist is a committed artifact, and a map would double
    // the diff for nothing an operator can act on.
    sourcemap: false,
  },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
    css: false,
    exclude: ["node_modules/**", "dist/**"],
  },
  server: {
    fs: { allow: [fileURLToPath(new URL(".", import.meta.url)), fileURLToPath(new URL("../examples/definitions", import.meta.url))] },
    // `vite dev` proxies the API to a local `jig serve` on its default
    // port. The control plane fences mutations on Host and Origin (R20):
    // changeOrigin makes the Host the listener's own, and an Origin that is
    // this dev server's own origin is rewritten to the listener's, so the
    // UI's mutations pass as same-origin. Any other Origin is forwarded
    // unchanged, so a foreign page reaching the dev server is still refused.
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyRequest, request) => {
            const origin = request.headers.origin;
            if (origin !== undefined && origin === `http://${request.headers.host}`) {
              proxyRequest.setHeader("origin", apiTarget);
            }
          });
        },
      },
    },
  },
});
