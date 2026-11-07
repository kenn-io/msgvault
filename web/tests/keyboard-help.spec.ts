import { expect, test } from '@playwright/test';

import { mockArchiveLayout } from './archive-layout';
import { setKitTheme } from './kit-ui';

test.use({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, colorScheme: 'dark' });

test.beforeEach(async ({ page }) => {
  await mockArchiveLayout(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await expect(page.getByText('Synthetic archive subject')).toBeVisible();
  await setKitTheme(page, 'dark');
});

test('help keeps all shortcuts visible in two columns and stays put while filtering', async ({ page }, testInfo) => {
  await page.getByRole('grid', { name: 'Messages results' }).focus();
  await page.keyboard.press('Shift+/');
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const search = help.getByRole('searchbox', { name: 'Search keyboard shortcuts' });
  await expect(search).toBeFocused();
  const bounds = await help.boundingBox();
  const list = help.locator('.keyboard-help-list');
  const rows = help.locator('dl > div');
  await expect(rows).not.toHaveCount(0);
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
  await page.screenshot({ path: testInfo.outputPath('artifacts/03-shell-keyboard-help.png') });
  await help.screenshot({ path: testInfo.outputPath('artifacts/03-shell-keyboard-help-full.png') });

  await search.fill('deletion');
  expect(await help.boundingBox()).toEqual(bounds);
  expect((await help.getByRole('heading', { name: 'Safety', exact: true }).boundingBox())!.x).toBe(navigate!.x);
  await page.screenshot({ path: testInfo.outputPath('artifacts/keyboard-help-filtered.png') });
  await search.fill('zzzxxyy');
  await expect(help.getByText('No shortcuts match "zzzxxyy"')).toBeVisible();
  expect(await help.boundingBox()).toEqual(bounds);
  await page.screenshot({ path: testInfo.outputPath('artifacts/06-shell-help-no-results.png') });
});

test('help wraps long unmatched queries on a phone', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.getByRole('button', { name: 'Help', exact: true }).click();
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const list = help.locator('.keyboard-help-list');
  expect(await list.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
  const query = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
  await help.getByRole('searchbox', { name: 'Search keyboard shortcuts' }).fill(query);
  const title = help.getByText(`No shortcuts match "${query}"`, { exact: true });
  await expect(title).toBeVisible();
  expect(await list.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
  const action = help.getByRole('button', { name: 'Search commands instead' });
  await expect(action).toBeInViewport({ ratio: 1 });
  await page.screenshot({ path: testInfo.outputPath('artifacts/keyboard-help-long-query.png') });
});

test('short laptop windows scroll every shortcut inside Help', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 720 });
  await page.getByRole('button', { name: 'Help', exact: true }).click();
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  const list = help.getByRole('region', { name: 'Keyboard shortcut list' });
  expect(await list.evaluate(element => element.scrollHeight - element.clientHeight)).toBeGreaterThan(0);
  expect(await list.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
  const last = help.locator('dl > div').filter({ hasText: 'Open searchable keyboard help' });
  await page.keyboard.press('Tab');
  await expect(list).toBeFocused();
  await page.keyboard.press('End');
  await expect.poll(() => list.evaluate(element => element.scrollTop)).toBeGreaterThan(0);
  await expect.poll(() => last.evaluate(row => {
    const list = row.closest('.keyboard-help-list')!.getBoundingClientRect();
    const body = row.closest('.kit-modal-body')!.getBoundingClientRect();
    const bounds = row.getBoundingClientRect();
    const clipping = Math.max(Math.max(list.top, body.top) - bounds.top, bounds.bottom - Math.min(list.bottom, body.bottom));
    return clipping;
  })).toBeLessThanOrEqual(1);
  await expect(help.getByRole('button', { name: /^Open command palette/ })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('artifacts/keyboard-help-short.png') });
});
