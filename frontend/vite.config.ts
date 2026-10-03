import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The dev server proxies /api to the Go backend so the browser sees a single
// origin. That matters for more than convenience: the SSE stream is a
// long-lived response, and a dev-server proxy forwards it without buffering,
// so live updates arrive the same way they will behind a real reverse proxy.
//
// A proxy also means the app is developed against same-origin requests, so it
// cannot accidentally depend on CORS succeeding. The backend still sends CORS
// headers and still restricts them to configured origins, because that is what
// a deployed build depends on.
const BACKEND = process.env.ITSM_BACKEND ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    // Bind on all interfaces so the dev server is reachable from a container
    // or a phone on the same network, which is handy for checking the layout.
    host: true,
    proxy: {
      '/api': {
        target: BACKEND,
        changeOrigin: true,
        // Server-sent events must not be buffered by the proxy or they arrive
        // in one lump when the connection closes.
        configure: (proxy) => {
          proxy.on('proxyRes', (proxyRes) => {
            if (proxyRes.headers['content-type']?.includes('text/event-stream')) {
              proxyRes.headers['x-accel-buffering'] = 'no'
            }
          })
        },
      },
    },
  },
})