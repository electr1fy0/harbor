import { defineConfig } from "vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"
import viteReact from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"

const apiProxy = {
  "/api": {
    target: "http://127.0.0.1:3000",
    changeOrigin: false,
  },
}

const config = defineConfig({
  resolve: { tsconfigPaths: true },
  server: { host: "127.0.0.1", port: 5173, strictPort: true, proxy: apiProxy },
  preview: { host: "127.0.0.1", strictPort: true, proxy: apiProxy },
  plugins: [tanstackRouter({ target: "react" }), tailwindcss(), viteReact()],
})

export default config
