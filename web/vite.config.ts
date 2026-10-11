import { defineConfig } from "vite"
import { devtools } from "@tanstack/devtools-vite"
import { tanstackStart } from "@tanstack/react-start/plugin/vite"
import viteReact from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { nitro } from "nitro/vite"

const config = defineConfig({
  resolve: { tsconfigPaths: true },
  server: { host: "127.0.0.1", strictPort: true },
  preview: { host: "127.0.0.1", strictPort: true },
  plugins: [devtools(), tailwindcss(), tanstackStart(), nitro({ preset: "node-server" }), viteReact()],
})

export default config
