import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build output (dist/) is embedded into the Go binary via //go:embed in
// serve.go, so it must stay inside this package directory. `base: "./"` makes
// asset URLs relative, so the bundle works regardless of where it is mounted.
//
// In dev, `npm run dev` serves the UI with HMR on :5173 and proxies /api to the
// Go server (start it with `turntable --serve`, which listens on :8080).
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});
