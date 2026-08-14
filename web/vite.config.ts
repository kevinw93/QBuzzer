import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// Same variable and default as cmd/server.
const goServer = `http://${process.env.QBUZZER_ADDR ?? '127.0.0.1:8080'}`

export default defineConfig({
  plugins: [react()],
  test: { environment: 'jsdom', include: ['src/**/*.test.{ts,tsx}'] },
  server: {
    proxy: {
      '/api': {
        target: goServer,
        changeOrigin: true,
        ws: true,
        // The Go server rejects WebSockets whose Origin differs from Host.
        configure: (proxy) => {
          proxy.on('proxyReqWs', (proxyReq) => proxyReq.setHeader('origin', goServer))
        },
      },
    },
  },
})
