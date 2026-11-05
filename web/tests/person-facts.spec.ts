import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';

for (const width of [1440, 390]) {
  test(`Facts rows and searchable fields fit at ${width}px`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    await page.setViewportSize({ width, height: 900 });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: 42 }))}`);
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    const facts = page.getByRole('region', { name: 'Facts', exact: true });
    await facts.getByRole('button', { name: 'Show', exact: true }).click();
    await expect(facts.getByRole('button', { name: 'Reveal sensitive fact value' })).toBeVisible();
    await expect(facts.getByRole('heading', { name: 'Claims', exact: true })).toBeVisible();
    if (width === 1440) {
      const tabs = await facts.getByRole('radiogroup').boundingBox();
      const picker = await facts.getByRole('button', { name: 'Search fields: All fields' }).boundingBox();
      expect(Math.abs(tabs!.y + tabs!.height / 2 - picker!.y - picker!.height / 2)).toBeLessThanOrEqual(1);
    }
    await facts.getByRole('button', { name: 'Search fields: All fields' }).click();
    const bounds = await page.evaluate(() => {
      const column = document.querySelector('.ledger')!.getBoundingClientRect();
      const panel = document.querySelector('.kit-typeahead__panel')!.getBoundingClientRect();
      const trigger = document.querySelector('.kit-typeahead')!.getBoundingClientRect();
      return { left: panel.left, right: panel.right, columnLeft: column.left, columnRight: column.right, triggerRight: trigger.right };
    });
    expect(bounds.left).toBeGreaterThanOrEqual(bounds.columnLeft - 1);
    expect(bounds.right).toBeLessThanOrEqual(bounds.columnRight + 1);
    expect(Math.abs(bounds.right - bounds.triggerRight)).toBeLessThanOrEqual(1);
    await page.getByRole('combobox', { name: 'Search fields', exact: true }).fill('extension');
    await expect(page.getByRole('option', { name: /Forbidden extension field Sensitive/ })).toBeVisible();
    await page.keyboard.press('Escape');
    await facts.locator('summary').filter({ hasText: 'About these fields' }).click();
    await expect(facts.locator('details.about')).toHaveAttribute('open', '');
    await facts.locator('summary').filter({ hasText: 'About these fields' }).click();
    await expect(facts.locator('details.about')).not.toHaveAttribute('open', '');
    await facts.getByRole('radio', { name: 'Evidence', exact: true }).click();
    await expect(facts.getByRole('button', { name: 'Reveal evidence excerpt' })).toBeVisible();
    await expect(facts.getByRole('navigation')).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}
