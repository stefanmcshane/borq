import { defineConfig } from "astro/config";
import svelte from "@astrojs/svelte";

export default defineConfig({
  output: "static",
  integrations: [svelte()],
  vite: { server: { proxy: { "/api": "http://127.0.0.1:7833" } } },
});
