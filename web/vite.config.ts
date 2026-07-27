import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(__dirname, "./src") },
  },
  server: {
    host: true,
    port: 3000,
    // The control plane is a separate origin in development. Proxying keeps the browser on one
    // origin, which means no CORS configuration and no divergence between how the app talks to the
    // API in development and how it will in production behind a single ingress.
    proxy: {
      "/api": { target: process.env.FLEETWARD_API_URL ?? "http://localhost:8080", changeOrigin: true },
      "/readyz": { target: process.env.FLEETWARD_API_URL ?? "http://localhost:8080", changeOrigin: true },
      "/healthz": { target: process.env.FLEETWARD_API_URL ?? "http://localhost:8080", changeOrigin: true },
    },
  },
  build: { outDir: "dist", sourcemap: true },
});
