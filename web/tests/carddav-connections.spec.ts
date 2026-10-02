import { expect, test } from '@playwright/test';
import { installCardDAV } from './e2e/fixtures/carddav';

test('switching CardDAV connections clears credentials and uses selected settings and requests', async ({
  page
}, testInfo) => {
  await installCardDAV(page, { configured: true });
  const summaries = ['default', 'work'].map((connection) => ({
    connection,
    account_id: connection === 'default' ? 1 : 2,
    provider: '',
    status: {
      configured: true,
      available: true,
      credential_configured: true,
      enabled: true,
      scheduled: false,
      schedule: '0 2 * * *',
      account: {
        base_url: `https://${connection}.example.test/`,
        username: `${connection}@example.com`
      }
    }
  }));
  await page.route('**/api/v1/carddav/connections', (route) =>
    route.fulfill({ json: { connections: summaries } })
  );
  await page.route('**/api/v1/carddav/account/test', async (route) => {
    const body = route.request().postDataJSON();
    expect(body.connection).toBe('work');
    expect(body.username).toBe('work@example.com');
    expect(body.password).toBeUndefined();
    await route.fulfill({
      json: {
        base_url: body.base_url,
        username: body.username,
        enabled: true,
        books: 2
      }
    });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  await page.getByRole('button', { name: /^CardDAV account/ }).click();
  await expect(page.getByLabel('Username', { exact: true })).toHaveValue('default@example.com');
  await page.getByLabel('Password', { exact: true }).fill('synthetic-default-password');
  await page.getByLabel('Base URL', { exact: true }).fill('https://draft.example.test/');
  await page.getByRole('combobox', { name: /^CardDAV connection/ }).click();
  await page.getByRole('option', { name: 'work', exact: true }).click();
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
  await expect(page.getByLabel('Base URL', { exact: true })).toHaveValue('https://work.example.test/');
  await expect(page.getByLabel('Username', { exact: true })).toHaveValue('work@example.com');
  await page.getByRole('button', { name: 'Test CardDAV connection' }).click();
  await expect(page.getByText('Connection successful. Found 2 address books.')).toBeVisible();
  await expect(page.getByText(/one write target across all CardDAV connections/)).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: testInfo.outputPath('carddav-connections-mobile.png'),
    fullPage: true
  });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
