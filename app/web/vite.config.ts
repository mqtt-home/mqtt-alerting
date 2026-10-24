import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { homeUi } from '@mqtt-home/ui/vite'
import path from 'path'

export default defineConfig({
  // homeUi: dark mode + Wall Display detection before first paint
  plugins: [react(), tailwindcss(), homeUi()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
