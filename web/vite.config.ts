import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

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
    proxy: { "/api": "http://127.0.0.1:7777" },
  },
});
