import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // `npm run dev` against a locally running backend.
    proxy: { '/api': 'http://localhost:8000' },
  },
})
