import { expect, test } from '@playwright/test';

test('interactive login refreshes daemon appearance once while the session override wins', async ({ page }) => {
  let settingsRequests = 0;
  await page.addInitScript(() => {
    sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ theme: 'light' }));
  });
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'required', https: true, plain_http_warning: false }
  }));
  await page.route('**/api/session/login', (route) => route.fulfill({
    json: { auth_mode: 'session', csrf_token: 'csrf-token', https: true, plain_http_warning: false }
  }));
  await page.route('**/api/v1/settings', (route) => {
    settingsRequests += 1;
    return route.fulfill({ json: {
      settings: [
        { key: 'web.theme', value: { string: 'dark' } },
        { key: 'web.density', value: { string: 'comfortable' } }
      ], pending_restart: false
    } });
  });
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'login', search_provenance: {}
  } }));

  await page.goto('/');
  await page.getByLabel('API key').fill('test-key');
  await page.getByRole('button', { name: 'Log in' }).click();

  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('html')).toHaveAttribute('data-density', 'comfortable');
  await expect.poll(() => settingsRequests).toBe(1);
});
