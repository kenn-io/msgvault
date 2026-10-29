import { test, expect, loginToMeetingArchive } from './fixtures/meeting-daemon';
import { selectKitOption, selectWorkspace } from '../kit-ui';

test('Settings saves persist through reload and category history restores Back and Forward', async ({ page, daemon }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ colorScheme: 'dark' });
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await loginToMeetingArchive(page, daemon);
  await selectWorkspace(page, 'Settings');
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Server', exact: true }).click();
  await page.getByRole('textbox', { name: 'Stop when idle after', exact: true }).fill('17m');
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await selectKitOption(page, 'Theme', 'Dark');
  await page.getByRole('button', { name: 'Overview', exact: true }).click();
  const saved = page.waitForResponse(response => response.request().method() === 'PATCH' && response.url().endsWith('/api/v1/settings'));
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  expect((await saved).ok()).toBe(true);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Server', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Stop when idle after', exact: true })).toHaveValue('17m0s');
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await expect(page.getByRole('combobox', { name: /^Theme:/ })).toContainText('Dark');
  await page.goBack();
  await expect(page.getByRole('heading', { name: 'Server', exact: true })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('heading', { name: 'Advanced', exact: true })).toBeVisible();

  expect(errors).toEqual([]);
});
