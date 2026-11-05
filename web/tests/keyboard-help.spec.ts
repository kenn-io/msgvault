import { expect, test } from '@playwright/test';

import { mockArchiveLayout } from './archive-layout';
import { selectWorkspace, setKitTheme } from './kit-ui';

test.use({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, colorScheme: 'dark' });

test.beforeEach(async ({ page }) => {
  await mockArchiveLayout(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await expect(page.getByText('Synthetic archive subject')).toBeVisible();
  await setKitTheme(page, 'dark');
});

test('help keeps all shortcuts visible in two columns and stays put while filtering', async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.getByRole('grid', { name: 'Messages results' }).focus();
  await page.keyboard.press('Shift+/');
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const search = help.getByRole('searchbox', { name: 'Search keyboard shortcuts' });
  await expect(search).toBeFocused();
  const bounds = await help.boundingBox();
  const list = help.locator('.keyboard-help-list');
  const rows = help.locator('dl > div');
  await expect(rows).toHaveCount(23);
  const geometry = await list.evaluate(element => {
    const bounds = element.getBoundingClientRect();
    return {
      overflow: element.scrollHeight - element.clientHeight,
      horizontalOverflow: element.scrollWidth - element.clientWidth,
      rowsVisible: [...element.querySelectorAll('dl > div')].every(row => {
        const rect = row.getBoundingClientRect();
        return rect.top >= bounds.top && rect.bottom <= bounds.bottom;
      })
    };
  });
  expect(geometry).toEqual({ overflow: 0, horizontalOverflow: 0, rowsVisible: true });
  const navigate = await help.getByRole('heading', { name: 'Navigate', exact: true }).boundingBox();
  const selection = await help.getByRole('heading', { name: 'Selection', exact: true }).boundingBox();
  expect(selection!.x).toBeGreaterThan(navigate!.x + 400);
  expect(selection!.y).toBe(navigate!.y);
  const paletteRow = rows.filter({ hasText: 'Open command palette' });
  await expect(paletteRow.locator('kbd[aria-label="Ctrl K"] > kbd')).toHaveCount(2);
  await page.screenshot({ path: testInfo.outputPath('artifacts/03-shell-keyboard-help.png') });
  await help.screenshot({ path: testInfo.outputPath('artifacts/03-shell-keyboard-help-full.png') });

  await search.fill('deletion');
  expect(await help.boundingBox()).toEqual(bounds);
  expect((await help.getByRole('heading', { name: 'Safety', exact: true }).boundingBox())!.x).toBe(selection!.x);
  await search.fill('zzzxxyy');
  await expect(help.getByText('No shortcuts match "zzzxxyy"')).toBeVisible();
  expect(await help.boundingBox()).toEqual(bounds);
  await page.screenshot({ path: testInfo.outputPath('artifacts/06-shell-help-no-results.png') });
  await help.getByRole('button', { name: 'Search commands instead' }).click();
  await expect(page.getByRole('dialog', { name: 'Messages commands' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Messages commands' }).getByRole('combobox')).toHaveValue('zzzxxyy');
  await page.keyboard.press('Escape');
  await page.keyboard.press('Escape');
  await page.route('**/api/v1/deletions', route => route.fulfill({ json: { manifests: [] } }));
  await selectWorkspace(page, 'Staged deletions');
  await expect(page.getByRole('heading', { name: 'Staged deletions', exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test('help scrolls vertically on narrow screens and formats Mac shortcuts', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.evaluate(() => Object.defineProperty(navigator, 'platform', { value: 'MacIntel' }));
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.getByRole('button', { name: 'Help', exact: true }).click();
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const list = help.locator('.keyboard-help-list');
  expect(await list.evaluate(element => element.scrollHeight - element.clientHeight)).toBeGreaterThan(0);
  expect(await list.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
  await help.getByRole('searchbox', { name: 'Search keyboard shortcuts' }).fill('palette');
  await expect(help.locator('dl kbd[aria-label="Command K"] > kbd')).toHaveCount(2);
  await page.screenshot({ path: testInfo.outputPath('artifacts/keyboard-help-narrow-mac.png') });
  await help.getByRole('button', { name: 'Close' }).click();
  await expect(help).toHaveCount(0);
});

test('help wraps long unmatched queries on a phone', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.getByRole('button', { name: 'Help', exact: true }).click();
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const query = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
  await help.getByRole('searchbox', { name: 'Search keyboard shortcuts' }).fill(query);
  const title = help.getByText(`No shortcuts match "${query}"`, { exact: true });
  await expect(title).toBeVisible();
  const list = help.locator('.keyboard-help-list');
  expect(await list.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
  const action = help.getByRole('button', { name: 'Search commands instead' });
  await expect(action).toBeInViewport({ ratio: 1 });
  await page.screenshot({ path: testInfo.outputPath('artifacts/keyboard-help-long-query.png') });
  await action.click();
  await expect(page.getByRole('dialog', { name: 'Messages commands' }).getByRole('combobox')).toHaveValue(query);
  expect(errors).toEqual([]);
});

test('short laptop windows scroll every shortcut inside Help', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 720 });
  await page.getByRole('button', { name: 'Help', exact: true }).click();
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const list = help.locator('.keyboard-help-list');
  const bounds = await help.boundingBox();
  expect(await list.evaluate(element => element.scrollHeight - element.clientHeight)).toBeGreaterThan(0);
  expect(await list.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
  const last = help.locator('dl > div').filter({ hasText: 'Open command palette' });
  await last.scrollIntoViewIfNeeded();
  const geometry = await last.evaluate(row => {
    const list = row.closest('.keyboard-help-list')!.getBoundingClientRect();
    const body = row.closest('.kit-modal-body')!.getBoundingClientRect();
    const bounds = row.getBoundingClientRect();
    return bounds.top >= Math.max(list.top, body.top) && bounds.bottom <= Math.min(list.bottom, body.bottom);
  });
  expect(geometry).toBe(true);
  await expect(help.getByRole('button', { name: /^Open command palette/ })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('artifacts/keyboard-help-short.png') });
  await help.getByRole('searchbox', { name: 'Search keyboard shortcuts' }).fill('shift');
  expect(await help.boundingBox()).toEqual(bounds);
  await page.keyboard.press('Control+k');
  await expect(help).toHaveCount(0);
  await expect(page.getByRole('dialog', { name: 'Messages commands' })).toBeVisible();
});
