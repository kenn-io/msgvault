const baseURL = process.env.MSGVAULT_DOCS_BASE_URL;
if (!baseURL) throw new Error('MSGVAULT_DOCS_BASE_URL is required');

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

export default config;
