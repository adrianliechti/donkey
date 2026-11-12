import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    // Dev only: vite's own server proxies this while `npm run dev` runs. It
    // is never part of a build and never runs in the cluster, so donkey has
    // nothing to warn about here — flagging it would be a false alarm.
    proxy: { '/api': 'http://localhost:3001' },
  },
})
