import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';

for (const width of [1440, 390]) {
  test(`Facts rows and searchable fields fit at ${width}px`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    const longValue = 'x'.repeat(200);
    await page.route('**/api/v1/people/42/attributes*', route => route.fulfill({ json: {
      person_id: 42, attributes: [{
        definition: {
          id: 8, universal_id: 'forbidden-target-key', object_type: 'person', slug: 'forbidden-extension-field', label: 'Forbidden extension field',
          value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 10,
          is_required: false, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true,
          is_searchable: true, is_sensitive: false, is_audited: true, is_deletable: true, history_exempt: false,
          is_active: true, revision: 1, created_at: '2026-08-01', updated_at: '2026-08-01',
        },
        current: [{ id: 1, person_id: 42, definition_id: 8, definition_slug: 'forbidden-extension-field', ordinal: 0,
          value: { type: 'text', text: longValue }, source: 'user', created_at: '2026-08-01', active_from: '2026-08-01' }],
        history: [],
      }],
    } }));
    await page.setViewportSize({ width, height: 900 });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: 42 }))}`);
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    const facts = page.getByRole('region', { name: 'Facts', exact: true });
    await facts.getByRole('button', { name: 'Show', exact: true }).click();
    await expect(facts.getByRole('region', { name: 'Claims', exact: true })).toBeVisible();
    const saved = facts.getByText(longValue, { exact: true });
    await expect(saved).toBeVisible();
    const savedBounds = await saved.boundingBox();
    const columnBounds = await facts.locator('.ledger').boundingBox();
    expect(savedBounds!.x + savedBounds!.width).toBeLessThanOrEqual(columnBounds!.x + columnBounds!.width);
    const pickerGeometry = await facts.locator('.kit-typeahead__trigger').evaluate(trigger => {
      const label = trigger.querySelector('.kit-typeahead__value-text')!;
      return { left: trigger.getBoundingClientRect().left, labelLeft: label.getBoundingClientRect().left, width: trigger.getBoundingClientRect().width, parentWidth: trigger.parentElement!.parentElement!.getBoundingClientRect().width };
    });
    expect(pickerGeometry.labelLeft - pickerGeometry.left).toBeLessThan(20);
    if (width === 390) expect(pickerGeometry.width).toBe(pickerGeometry.parentWidth);
    const savedRow = saved.locator('..');
    const provenance = await savedRow.locator('.meta').boundingBox();
    const details = await savedRow.locator('summary').boundingBox();
    expect(provenance!.y + provenance!.height).toBeLessThanOrEqual(details!.y);
    if (width === 390) {
      const claim = facts.locator('[aria-label="Fact claim"]').first();
      const claimSource = await claim.locator('.meta').boundingBox();
      const claimDetails = await claim.locator('summary').boundingBox();
      expect(claimSource!.y + claimSource!.height).toBeLessThanOrEqual(claimDetails!.y);
    }
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
    await page.keyboard.press('Escape');
    await facts.getByRole('radio', { name: 'Evidence', exact: true }).click();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}
