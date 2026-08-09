import { defineConfig } from '@playwright/test'

const addr = '127.0.0.1:18090'

export default defineConfig({
  testDir: 'e2e',
  timeout: 30_000,
  use: { baseURL: `http://${addr}` },
  webServer: {
    // The real binary with the freshly built frontend embedded.
    command: 'npm run build && cd .. && go run ./cmd/server',
    env: { QBUZZER_ADDR: addr },
    url: `http://${addr}/healthz`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
})
