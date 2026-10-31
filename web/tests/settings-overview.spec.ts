import { expect, test } from '@playwright/test';
import { installOperations } from './e2e/fixtures/operations';

test('Build index confirmation preserves feature labels at 768px', async ({ page }) => {
  await page.setViewportSize({ width: 768, height: 844 });
  await installOperations(page);
  await page.route('**/api/v1/search/coverage', route => route.fulfill({ json: {
    eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1',
    status: 'unavailable', actions: ['build_index']
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  await page.getByRole('button', { name: 'Build index, Message search' }).click();
  await expect(page.getByRole('button', { name: 'Confirm full rebuild' })).toBeVisible();
  const rows = page.getByRole('list', { name: 'Features' }).locator('li');
  for (const row of await rows.all()) {
    const geometry = await row.evaluate(element => {
      const range = document.createRange();
      range.selectNodeContents(element.querySelector('.row__name')!);
      const text = range.getBoundingClientRect();
      const chip = element.querySelector('.row__chip')!.getBoundingClientRect();
      return { name: range.toString().trim(), textRight: text.right, chipLeft: chip.left };
    });
    expect(geometry.textRight, geometry.name).toBeLessThanOrEqual(geometry.chipLeft);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(768);
});
