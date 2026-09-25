import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: '../internal/server/ui/dist',
    emptyOutDir: true,
  },
  server: {
    // `jin server` must be running; log in once at http://localhost:7420/?token=... (cookies are shared across ports).
    proxy: { '/api': 'http://localhost:7420' },
  },
})
