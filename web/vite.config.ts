// Vite configuration for the KubeNeuron control panel (Phase 2).
// The dev server proxies API calls to a locally running controller.
import { defineConfig } from "vite";

export default defineConfig({
  server: {
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
  build: {
    outDir: "dist",
  },
});
