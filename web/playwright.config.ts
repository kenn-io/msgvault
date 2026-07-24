import { defineConfig } from '@playwright/test';

const previewPort = process.env.MSGVAULT_PLAYWRIGHT_PORT ?? '4173';
const previewURL = `http://127.0.0.1:${previewPort}`;

export default defineConfig({
  testDir: './tests',
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: 'playwright-report' }]
  ],
  // Deep virtualizer fixtures intentionally exercise tens of thousands of rows.
  // Serial execution keeps their timing and visual rasterization deterministic
  // across developer laptops and the Linux CI runner.
  workers: 1,
  // CI containers run the archive-fixture generation and first renders much
  // slower than developer machines; keep the strict budget locally.
  timeout: process.env.CI ? 60_000 : 30_000,
  use: {
    baseURL: previewURL,
    viewport: { width: 1280, height: 720 },
    locale: 'en-US',
    timezoneId: 'UTC',
    colorScheme: 'light',
    contextOptions: { reducedMotion: 'reduce' },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure'
  },
  webServer: {
    command: `bun run build && bunx vite preview --host 127.0.0.1 --port ${previewPort}`,
    url: previewURL,
    reuseExistingServer: false
  }
});
