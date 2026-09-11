import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';
import { installCardDAV } from './e2e/fixtures/carddav';

test('Google Contacts opens sign-in and returns authorization to settings', async ({ page }, testInfo) => {
  await installMixedArchive(page);
  await installCardDAV(page);
  let completed = false;
  await page.route('**/api/v1/carddav/google/authorize', async (route) => {
    const body = route.request().postDataJSON();
    expect(body.email).toBe('person@example.com');
    expect(body.oauth_app).toBe('contacts');
    const callback = new URL(body.redirect_uri);
    callback.searchParams.set('state', 'msgvault-carddav-synthetic-state');
    callback.searchParams.set('code', 'synthetic-code');
    await route.fulfill({ json: { url: callback.href, state: 'msgvault-carddav-synthetic-state' } });
  });
  await page.route('**/api/v1/carddav/google/callback', async (route) => {
    expect(route.request().postDataJSON()).toEqual({ state: 'msgvault-carddav-synthetic-state', code: 'synthetic-code' });
    completed = true;
    await route.fulfill({ json: { status: 'ok', message: 'Google Contacts authorized' } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  await page.getByRole('button', { name: /^CardDAV account/ }).click();
  await page.getByRole('combobox', { name: /^CardDAV provider/ }).click();
  await page.getByRole('option', { name: 'Google Contacts' }).click();
  await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0);
  await expect(page.getByLabel('Base URL', { exact: true })).toHaveCount(0);
  await page.getByLabel('Google account email').fill('person@example.com');
  await page.getByLabel('OAuth app', { exact: true }).fill('contacts');
  await page.screenshot({ path: testInfo.outputPath('google-carddav-desktop.png'), fullPage: true });
  const popupOpened = page.waitForEvent('popup');
  await page.getByRole('button', { name: 'Connect Google' }).click();
  await popupOpened;
  await expect(page.getByRole('status').filter({ hasText: 'Google Contacts authorized' })).toBeVisible();
  expect(completed).toBe(true);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: testInfo.outputPath('google-carddav-mobile.png'), fullPage: true });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
