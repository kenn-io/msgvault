import { expect, test } from '@playwright/test';
import { selectWorkspace, setKitTheme, setTemporaryDensity } from './kit-ui';

const row = {
  key: 'message:1', kind: 'message', message_type: 'email', conversation_type: 'email',
  title: 'Synthetic archive subject', preview: 'An archive message for layout checks.',
  occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
  source_identifier: 'archive@example.com', source_type: 'synthetic',
  participant_labels: ['Example Person'], participant_ids: [1],
  attachment_count: 0, attachment_size: 0, has_attachments: false,
  deleted_from_source: false, message_count: 1, match: {}
};

test.beforeEach(async ({ page }) => {
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'spacing', identity_revision: 1
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [row], total_count: 1, cache_revision: 'spacing', search_provenance: {}
  } }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [] } }));
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Relationships', exact: true })).toBeVisible();
});

for (const theme of ['light', 'dark'] as const) {
  for (const density of ['Compact', 'Comfortable'] as const) {
    test(`${theme} ${density}: header band ends at its reserved margin`, async ({ page }) => {
      await setKitTheme(page, theme);
      await setTemporaryDensity(page, density);
      // The extra four pixels used to paint over the page's own content gap.
      const header = page.locator('.page-header');
      const band = await header.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const style = getComputedStyle(element);
        const background = getComputedStyle(element, '::before');
        return {
          top: Number.parseFloat(background.top),
          bottom: Number.parseFloat(background.bottom),
          end: box.bottom - Number.parseFloat(background.bottom),
          reservedEnd: box.bottom + Number.parseFloat(style.marginBottom)
        };
      });
      expect(band.top).toBe(-12);
      expect(band.bottom).toBe(-12);
      expect(band.end).toBe(band.reservedEnd);
      await expect(page.getByRole('grid', { name: 'Relationship results' })).toBeVisible();
    });

    test(`${theme} ${density}: reader, selection, and dialog share inset edges`, async ({ page }) => {
      await setKitTheme(page, theme);
      await setTemporaryDensity(page, density);
      await selectWorkspace(page, 'Everything');
      await page.getByText(row.title, { exact: true }).click();
      const reader = page.getByRole('complementary', { name: `Reading pane: ${row.title}` });
      await expect(reader).toBeVisible();
      const inset = await reader.locator('.pane-header').evaluate((element) => {
        const box = element.getBoundingClientRect();
        const heading = element.querySelector('.pane-heading')!.getBoundingClientRect();
        const style = getComputedStyle(element);
        return { left: heading.left - box.left, top: style.paddingTop, gap: style.columnGap };
      });
      expect(inset).toEqual({ left: 12, top: '8px', gap: '16px' });
      await page.getByRole('button', { name: 'Close reading pane' }).click();
      const grid = page.getByRole('grid', { name: 'Everything results' });
      await grid.focus();
      await page.keyboard.press('Shift+A');
      const bar = page.locator('.selection-bar');
      await expect(bar).toBeVisible();
      const selectionInset = await bar.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const count = element.querySelector('.selection-count')!.getBoundingClientRect();
        const style = getComputedStyle(element);
        return { left: count.left - box.left, top: style.paddingTop, gap: style.columnGap };
      });
      expect(selectionInset).toEqual({ left: 12, top: '8px', gap: '8px' });
      await page.getByRole('button', { name: 'Clear selection', exact: true }).click();
      await page.getByRole('button', { name: 'Save view…' }).click();
      const dialog = page.getByRole('dialog', { name: 'Save view' });
      await expect(dialog).toBeVisible();
      const edges = await dialog.evaluate((element) => {
        const headerElement = element.querySelector<HTMLElement>('.kit-modal-header')!;
        const header = headerElement.getBoundingClientRect();
        const body = element.querySelector('.kit-modal-body')!.getBoundingClientRect();
        const heading = element.querySelector('.kit-modal-title')!.getBoundingClientRect();
        const input = element.querySelector('.kit-text-input')!.getBoundingClientRect();
        return { heading: heading.left - header.left - headerElement.clientLeft, field: input.left - body.left };
      });
      expect(edges).toEqual({ heading: 12, field: 12 });
      await dialog.getByRole('button', { name: 'Close', exact: true }).click();
    });
  }
}

test('narrow page keeps the header band inside the viewport', async ({ page }) => {
  await page.setViewportSize({ width: 600, height: 720 });
  const band = await page.locator('.page-header').evaluate((element) => {
    const box = element.getBoundingClientRect();
    const style = getComputedStyle(element, '::before');
    return { left: box.left + Number.parseFloat(style.left), right: box.right - Number.parseFloat(style.right) };
  });
  expect(band.left).toBe(0);
  expect(band.right).toBe(600);
});
