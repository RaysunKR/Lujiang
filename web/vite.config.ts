import { defineConfig } from "vite";
import solid from "vite-plugin-solid";
import tailwindcss from "@tailwindcss/vite";
import { resolve } from "node:path";

export default defineConfig({
  plugins: [solid(), tailwindcss()],
  build: {
    outDir: resolve(__dirname, "../internal/server/web/dist"),
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:7443",
      "/healthz": "http://127.0.0.1:7443",
    },
  },
});
