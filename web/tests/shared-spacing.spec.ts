import { expect, test } from '@playwright/test';
import { selectWorkspace } from './kit-ui';

import { mockArchiveLayout, row } from './archive-layout';
import { installOperations } from './e2e/fixtures/operations';

test.beforeEach(async ({ page }) => {
  await mockArchiveLayout(page);
  await page.route('**/api/v1/people**', (route) => route.fulfill({ json: { people: [], total_count: 0 } }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'spacing', identity_revision: 1
  } }));
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Relationships', exact: true })).toBeVisible();
});

test('page content sits one header gap below the divider, and full-width panes meet it', async ({ page }) => {
  await page.route('**/api/v1/operations/runs**', (route) => route.fulfill({ json: { runs: [], unavailable_kinds: [], membership_revision: 1 } }));
  await page.route('**/api/v1/operations/status', (route) => route.fulfill({ json: { lanes: [] } }));
  await page.route('**/api/v1/sources**', (route) => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
    id: 1, name: 'Synthetic saved view', schema_version: 2, revision: 1, canonical_state: {},
    created_at: row.occurred_at, updated_at: row.occurred_at
  }] } }));
  await page.route('**/api/v1/deletions', (route) => route.fulfill({ json: { manifests: [] } }));
  await page.route('**/api/v1/identity/match-candidates**', (route) => route.fulfill({ json: { candidates: [], limit: 100, offset: 0 } }));
  await page.route('**/api/v1/person-relationship-reviews**', (route) => route.fulfill({ json: { reviews: [] } }));
  const gaps: Record<string, number> = {};
  for (const label of ['Relationships', 'Directory', 'Reviews', 'Everything', 'Files', 'Saved views', 'Sources', 'Operations', 'Deletions', 'Settings']) {
    await selectWorkspace(page, label);
    if (label === 'Relationships') await expect(page.getByText('No relationships found', { exact: true })).toBeVisible();
    if (label === 'Directory') await expect(page.getByText('No people found', { exact: true })).toBeVisible();
    if (label === 'Everything') await expect(page.getByRole('grid', { name: 'Everything results' }).getByText(row.title, { exact: true })).toBeVisible();
    if (label === 'Files') await expect(page.getByText('SyntheticArchiveAttachmentWithAnUnbrokenDescriptiveFilename.pdf', { exact: true })).toBeVisible();
    if (label === 'Operations') await expect(page.getByRole('status', { name: 'Operation history state' })).toHaveText('No operation runs match the current filters.');
    if (label === 'Reviews') await expect(page.getByText('No identity matches in this queue.', { exact: true })).toBeVisible();
    if (label === 'Deletions') await expect(page.getByText('No staged deletions', { exact: true })).toBeVisible();
    if (label === 'Sources') await expect(page.getByText('No archived sources are available.')).toBeVisible();
    if (label === 'Saved views') await expect(page.getByText('Synthetic saved view', { exact: true })).toBeVisible();
    if (label === 'Settings') await expect(page.locator('.kit-settings')).toBeVisible();
    const header = page.locator('.page-header:visible');
    await expect(header).toHaveCount(1);
    gaps[label] = await header.evaluate((element) => {
      const divider = element.getBoundingClientRect().bottom - Number.parseFloat(getComputedStyle(element, '::before').bottom);
      const owner = element.parentElement!;
      let next = element.nextElementSibling;
      if (!next) next = owner.nextElementSibling;
      while (next && next.getBoundingClientRect().height === 0) next = next.nextElementSibling;
      return next!.getBoundingClientRect().top - divider;
    });
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await installOperations(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'operations' }))}`);
  await page.getByRole('button', { name: 'Open Source sync run' }).click();
  await expect(page.getByRole('region', { name: 'Operation detail focused content' })).toBeVisible();
  gaps['Focused Operations'] = await page.locator('.focused-detail .page-header').evaluate((header) => {
    const divider = header.getBoundingClientRect().bottom - Number.parseFloat(getComputedStyle(header, '::before').bottom);
    return header.nextElementSibling!.getBoundingClientRect().top - divider;
  });
  expect(gaps).toEqual(Object.fromEntries(Object.keys(gaps).map((label) => [label, ['Relationships', 'Settings'].includes(label) ? 0 : 8])));
});

test('selection bar uses the shared inset and gap', async ({ page }) => {
  await selectWorkspace(page, 'Everything');
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
});

for (const width of [1280, 420]) {
  test.describe(`saved queries at ${width}px`, () => {
    test.use({ viewport: { width, height: 720 } });

    test(`long saved queries keep row actions visible at ${width}px`, async ({ page }) => {
      const query = 'project planning meeting notes and follow up tasks '.repeat(7).trim();
      await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
        id: 1, name: 'Long archive search', schema_version: 2, revision: 1,
        canonical_state: { query, search_mode: 'full_text' },
        created_at: row.occurred_at, updated_at: row.occurred_at
      }] } }));
      await selectWorkspace(page, 'Saved views');
      const view = page.locator('.view-list article').filter({ hasText: 'Long archive search' });
      await expect(view).toBeVisible();
      for (const name of ['Open', 'Edit', 'Delete']) {
        const action = view.getByRole('button', { name: `${name} Long archive search`, exact: true });
        await expect(action).toBeInViewport({ ratio: 1 });
        await action.click({ trial: true });
      }
      const label = view.locator('.summary .kit-chip__label').filter({ hasText: query });
      expect(await label.evaluate((element) => element.scrollWidth <= element.clientWidth && element.scrollHeight <= element.clientHeight)).toBe(true);
    });
  });
}

test('save-view dialog shares header and field insets', async ({ page }) => {
  await selectWorkspace(page, 'Everything');
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

test('loaded message sender, body, and attachment edges align with the reader', async ({ page }) => {
  await page.route('**/api/v1/messages/1/recordings', (route) => route.fulfill({ json: { message_id: 1, recordings: [] } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [{ ...row, message_type: 'meeting_transcript', conversation_id: 11, anchor_message_id: 1 }], total_count: 1, cache_revision: 'spacing', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
    id: 11, anchor_id: 1, has_before: false, has_after: false, total: 1,
    messages: [{ id: 1, conversation_id: 11, subject: row.title, message_type: 'meeting_transcript',
      from: 'Example Person', to: [], sent_at: row.occurred_at, snippet: row.preview,
      body: 'A plain text archive message.', body_html: '',
      attachments: [{ id: 1, filename: 'Meeting-notes.txt', stored: false }] }]
  } }));
  await selectWorkspace(page, 'Everything');
  await page.getByText(row.title, { exact: true }).click();
  const reader = page.getByRole('complementary', { name: `Reading pane: ${row.title}` });
  await expect(reader.getByText('A plain text archive message.')).toBeVisible();
  await expect(reader.getByText('Meeting-notes.txt (not downloaded)')).toBeVisible();
  const inset = await reader.locator('.pane-header').evaluate((element) => {
    const box = element.getBoundingClientRect();
    const heading = element.querySelector('.pane-heading')!.getBoundingClientRect();
    const style = getComputedStyle(element);
    return { left: heading.left - box.left, top: style.paddingTop, gap: style.columnGap };
  });
  expect(inset).toEqual({ left: 12, top: '8px', gap: '16px' });
  const insets = await reader.evaluate((element) => [
    '.card-header', '.card-body', '.attachments'
  ].map((selector) => {
    const section = element.querySelector(selector)!;
    return section.firstElementChild!.getBoundingClientRect().left - section.getBoundingClientRect().left;
  }));
  expect(insets).toEqual([12, 12, 12]);
});

test('chips keep pill corners and removable chips keep visible insets', async ({ page }) => {
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
    id: 1, name: 'Archive search', schema_version: 2, revision: 1,
    canonical_state: { query: 'archive', search_mode: 'full_text' },
    created_at: row.occurred_at, updated_at: row.occurred_at
  }] } }));
  await selectWorkspace(page, 'Everything');
  await page.getByRole('searchbox', { name: 'Search everything' }).fill('archive');
  await page.getByRole('combobox', { name: 'Group by', exact: true }).click();
  await page.getByRole('option', { name: 'Source', exact: true }).click();
  const chips = page.locator('.context-chips .chip');
  await expect(chips).toHaveCount(2);
  const geometry = await chips.evaluateAll((elements) => elements.map((element) => {
    const range = document.createRange();
    range.selectNodeContents(element.querySelector('.kit-chip__label')!);
    const box = element.getBoundingClientRect();
    const icon = element.querySelector('svg')!.getBoundingClientRect();
    const border = Number.parseFloat(getComputedStyle(element).borderLeftWidth);
    return { height: box.height, left: range.getBoundingClientRect().left - box.left - border,
      right: box.right - border - icon.right };
  }));
  expect(geometry).toEqual([{ height: 26, left: 8, right: 8 }, { height: 26, left: 8, right: 8 }]);
  for (const chip of await chips.all()) {
    const shape = await chip.evaluate((element) => ({ radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height }));
    expect.soft(shape.radius).toBeGreaterThanOrEqual(shape.height / 2);
  }
  const longQuery = 'archive '.repeat(100).trim();
  await page.getByRole('searchbox', { name: 'Search everything' }).fill(longQuery);
  const queryChip = chips.filter({ hasText: longQuery });
  const wrapping = await queryChip.locator('.kit-chip__label').evaluate((element) => ({
    clipped: element.scrollWidth > element.clientWidth,
    wrapped: element.getBoundingClientRect().height > Number.parseFloat(getComputedStyle(element).lineHeight)
  }));
  expect(wrapping).toEqual({ clipped: false, wrapped: true });
  await expect(queryChip.getByRole('button', { name: 'Remove search', exact: true })).toBeVisible();
  await selectWorkspace(page, 'Directory');
  await page.getByRole('button', { name: 'Filters', exact: true }).click();
  await page.getByRole('textbox', { name: 'Category filter' }).fill('Archive');
  const filter = page.locator('.filter-chips .chip');
  await expect(filter).toBeVisible();
  const directoryGeometry = await filter.evaluate((element) => {
    const range = document.createRange(); range.selectNodeContents(element.querySelector('.kit-chip__label')!);
    const box = element.getBoundingClientRect();
    const icon = element.querySelector('svg')!.getBoundingClientRect();
    const border = Number.parseFloat(getComputedStyle(element).borderLeftWidth);
    return { height: box.height, left: range.getBoundingClientRect().left - box.left - border,
      right: box.right - border - icon.right };
  });
  expect(directoryGeometry).toEqual({ height: 26, left: 8, right: 8 });
  const directoryShape = await filter.evaluate((element) => ({ radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height }));
  expect.soft(directoryShape.radius).toBeGreaterThanOrEqual(directoryShape.height / 2);
  await selectWorkspace(page, 'Saved views');
  const summary = page.locator('.summary .kit-chip').filter({ hasText: 'archive' });
  await expect(summary).toBeVisible();
  const savedShape = await summary.evaluate((element) => {
    return { radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height };
  });
  expect.soft(savedShape.radius).toBeGreaterThanOrEqual(savedShape.height / 2);
  const savedGeometry = await summary.evaluate((element) => {
    const range = document.createRange(); range.selectNodeContents(element.querySelector('.kit-chip__label')!);
    const box = element.getBoundingClientRect(); const text = range.getBoundingClientRect();
    return { centered: Math.abs((text.top + text.bottom) / 2 - (box.top + box.bottom) / 2) };
  });
  expect(savedGeometry.centered).toBeLessThanOrEqual(1);
  await selectWorkspace(page, 'Sources');
  const status = page.locator('.kit-chip').filter({ hasText: 'Never synced' });
  await expect(status).toBeVisible();
  const statusShape = await status.evaluate((element) => ({ radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height }));
  expect.soft(statusShape.radius).toBeGreaterThanOrEqual(statusShape.height / 2);
});

test('linked message uses the page title text scale', async ({ page }) => {
  const message = {
    id: 1, conversation_id: 11, subject: row.title, message_type: 'email',
    from: 'sender@example.com', to: ['archive@example.com'], sent_at: row.occurred_at,
    snippet: row.preview, body: 'A linked archive message.', body_html: '',
    labels: [], has_attachments: false, size_bytes: 25, attachments: []
  };
  await page.route('**/api/v1/messages/1', (route) => route.fulfill({ json: message }));
  await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
    id: 11, anchor_id: 1, messages: [message], has_before: false, has_after: false, total: 1
  } }));
  await page.goto('/messages/1');
  await expect(page.getByRole('heading', { level: 1, name: row.title })).toHaveCSS('font-size', '18px');
});

test('settings cards keep 12px control insets', async ({ page }) => {
  await selectWorkspace(page, 'Settings');
  const control = page.locator('.kit-card .row__control').first();
  await expect(control).toBeVisible();
  const inset = await control.evaluate((element) => {
    const card = element.closest('.kit-card')!;
    return card.getBoundingClientRect().right - Number.parseFloat(getComputedStyle(card).borderRightWidth) - element.getBoundingClientRect().right;
  });
  expect(inset).toBe(12);
});

test('app scrollbars use the shared 8px style', async ({ page }) => {
  await selectWorkspace(page, 'Settings');
  const size = await page.locator('.kit-settings__scroll').evaluate((element) => getComputedStyle(element, '::-webkit-scrollbar').width);
  expect(size).toBe('8px');
});
