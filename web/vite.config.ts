import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // Local development against a port-forwarded donkey-server.
    proxy: { '/v1': 'http://localhost:8088' },
  },
})
