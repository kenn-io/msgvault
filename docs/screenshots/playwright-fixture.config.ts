const baseURL = process.env.MSGVAULT_DOCS_BASE_URL;
if (!baseURL) throw new Error('MSGVAULT_DOCS_BASE_URL is required');
const compareDir = process.env.MSGVAULT_DOCS_SCREENSHOT_COMPARE_DIR;

const config = {
  testDir: '../../web/tests',
  workers: 1,
  timeout: process.env.CI ? 60_000 : 45_000,
  reporter: [['list']],
  use: {
    baseURL,
    viewport: { width: 1280, height: 720 },
    locale: 'en-US',
    timezoneId: 'UTC',
    contextOptions: { reducedMotion: 'reduce' },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure'
  }
};

if (compareDir) {
  // The capture test compares a single screenshot buffer with the hydrated
  // published asset and then writes that same buffer to its output directory.
  config.snapshotPathTemplate = `${compareDir}/{arg}{ext}`;
}

export default config;
