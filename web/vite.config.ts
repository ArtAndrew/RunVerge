import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      // Local bootstrap development only. OIDC requires its configured HTTPS
      // public origin; this HTTP proxy is not an SSO deployment profile.
      '/auth': { target: 'http://127.0.0.1:8080', changeOrigin: true },
    },
  },
})
