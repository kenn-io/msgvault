import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: 'people-inference-viewport.spec.ts',
  reporter: 'list',
  timeout: 60_000,
  use: { baseURL: 'http://127.0.0.1:4189', browserName: 'chromium' },
  webServer: {
    command: 'bunx vite .. --config people-inference-vite.config.ts --host 127.0.0.1 --port 4189',
    url: 'http://127.0.0.1:4189/tests/fixtures/people-inference.html',
    reuseExistingServer: false,
  },
});
