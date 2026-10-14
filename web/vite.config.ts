import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// During development the API is proxied to a manager on :8080 (IBS_API to
// change it; an https manager's certificate is not checked).
export default defineConfig({
  plugins: [react()],
  server: { proxy: { '/api': { target: process.env.IBS_API ?? 'http://127.0.0.1:8080', secure: false } } },
  build: { outDir: 'dist', emptyOutDir: true },
})
