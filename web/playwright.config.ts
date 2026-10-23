import { defineConfig } from '@playwright/test';

const previewPort = process.env.MSGVAULT_PLAYWRIGHT_PORT ?? '4173';
const previewURL = `http://127.0.0.1:${previewPort}`;
// The web-e2e job owns these specs; the frontend job runs the rest.
const e2eSpecs = [
  '**/e2e/**/*.@(spec|test).?(c|m)[jt]s?(x)',
  '**/conversation-content-frame.spec.ts',
  '**/files-viewer.spec.ts'
];

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
  projects: [
    { name: 'browser', testIgnore: e2eSpecs },
    { name: 'e2e', testMatch: e2eSpecs }
  ],
  webServer: {
    command: `bun run build && bunx vite preview --host 127.0.0.1 --port ${previewPort}`,
    url: previewURL,
    reuseExistingServer: false
  }
});
