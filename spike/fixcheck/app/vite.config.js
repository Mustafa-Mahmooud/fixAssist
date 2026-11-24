import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Production build without source maps, like a default Lovable deploy.
export default defineConfig({
  plugins: [react()],
  build: { sourcemap: false, minify: "esbuild" },
});
