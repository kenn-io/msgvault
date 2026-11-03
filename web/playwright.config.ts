import { defineConfig, type ReporterDescription } from '@playwright/test';

const previewPort = process.env.MSGVAULT_PLAYWRIGHT_PORT ?? '4173';
const previewURL = `http://127.0.0.1:${previewPort}`;
// In CI, a JSON report records every result for upload; a test that passed on
// retry is marked flaky. CI names each run's report so runs sharing this config
// stay distinct.
const suite = process.env.MSGVAULT_PLAYWRIGHT_SUITE ?? 'browser';
const ciReporters: ReporterDescription[] = process.env.CI
  ? [['json', { outputFile: `test-results/playwright/${suite}.json` }]]
  : [];
const e2eSpecs = ['e2e/**/*.spec.ts', 'conversation-content-frame.spec.ts', 'files-viewer.spec.ts'];
const virtualizerSpecs = ['everything-virtualization.spec.ts', 'files-workspace.spec.ts'];

export default defineConfig({
  testDir: './tests',
  reporter: [
    ['list'],
    ...ciReporters,
    ['html', { open: 'never', outputFolder: 'playwright-report' }]
  ],
  // In CI, retry a failed test twice.
  retries: process.env.CI ? 2 : 0,
  projects: [
    { name: 'browser', testIgnore: [...e2eSpecs, ...virtualizerSpecs] },
    {
      name: 'virtualizer',
      dependencies: ['browser'],
      testMatch: virtualizerSpecs,
      // Deep virtualizer fixtures intentionally exercise tens of thousands of rows.
      // Serial execution keeps their timing and visual rasterization deterministic
      // across developer laptops and the Linux CI runner.
      workers: 1
    },
    { name: 'e2e', testMatch: e2eSpecs, workers: 1 }
  ],
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
