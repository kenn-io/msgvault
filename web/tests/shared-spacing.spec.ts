import { stubOperationStatus } from './kit-ui';
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
  await page.goto('/?workspace=relationships');
  await expect(page.getByRole('heading', { name: 'People', exact: true })).toBeVisible();
});

test('page content uses its header gap across workspaces', async ({ page }) => {
  await page.route('**/api/v1/operations/runs**', (route) => route.fulfill({ json: { runs: [], unavailable_kinds: [], membership_revision: 1 } }));
  await stubOperationStatus(page);
  await page.route('**/api/v1/carddav/connections', (route) => route.fulfill({ json: { connections: [] } }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
    id: 1, name: 'Synthetic saved view', schema_version: 2, revision: 1, canonical_state: {},
    created_at: row.occurred_at, updated_at: row.occurred_at
  }] } }));
  await page.route('**/api/v1/deletions', (route) => route.fulfill({ json: { manifests: [{
    id: 'spacing-batch', status: 'pending', message_count: 1, created_at: row.occurred_at, created_by: 'api'
  }] } }));
  await page.route('**/api/v1/deletions/spacing-batch', (route) => route.fulfill({ json: {
    id: 'spacing-batch', status: 'pending', message_count: 1, created_at: row.occurred_at, created_by: 'api', account: 'archive@example.com'
  } }));
  await page.route('**/api/v1/identity/match-candidates**', (route) => route.fulfill({ json: { candidates: [], limit: 100, offset: 0 } }));
  await page.route('**/api/v1/person-relationship-reviews**', (route) => route.fulfill({ json: { reviews: [] } }));
  const gaps: Record<string, number> = {};
  for (const label of ['People', 'Messages', 'Files', 'Sources', 'Deletions', 'Settings']) {
    await selectWorkspace(page, label);
    if (label === 'People') await expect(page.getByText('No people yet', { exact: true })).toBeVisible();
    if (label === 'Messages') await expect(page.getByRole('grid', { name: 'Messages results' }).getByText(row.title, { exact: true })).toBeVisible();
    if (label === 'Files') await expect(page.getByText('SyntheticArchiveAttachmentWithAnUnbrokenDescriptiveFilename.pdf', { exact: true })).toBeVisible();
    if (label === 'Deletions') {
      await page.getByRole('table', { name: 'Staged deletions' }).getByRole('button', { name: /^Open batch from/ }).click();
      const panel = page.getByRole('dialog', { name: /^Staged / });
      await expect(panel.getByText('archive@example.com', { exact: true })).toBeVisible();
      await panel.getByRole('button', { name: 'Close', exact: true }).click();
    }
    if (label === 'Sources') await expect(page.getByRole('table', { name: 'Sources' }).getByText('Archive', { exact: true })).toBeVisible();
    if (label === 'Settings') await expect(page.locator('.kit-settings')).toBeVisible();
    const header = page.locator('.kit-page-header:visible');
    await expect(header).toHaveCount(1);
    gaps[label] = await header.evaluate((element) => {
      const owner = element.parentElement!;
      let next = element.nextElementSibling;
      if (!next) next = owner.nextElementSibling;
      while (next && (next.getBoundingClientRect().height === 0 || next.classList.contains('kit-sr-only'))) next = next.nextElementSibling;
      return next!.getBoundingClientRect().top - element.getBoundingClientRect().bottom;
    });
  }
  expect(gaps).toEqual(Object.fromEntries(Object.keys(gaps).map((label) => [label, ['People', 'Settings'].includes(label) ? 12 : 20])));
});

test('Past runs keeps its detail below the dialog header on mobile', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installOperations(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true }))}`);
  const dialog = page.getByRole('dialog', { name: 'Past runs' });
  await dialog.getByRole('button', { name: 'Open Source sync run' }).click();
  await expect(dialog.getByRole('region', { name: 'Operation run detail' })).toBeVisible();
  const gap = await dialog.locator('.kit-modal-header').evaluate((header) => (
    header.nextElementSibling!.firstElementChild!.getBoundingClientRect().top - header.getBoundingClientRect().bottom
  ));
  expect(gap).toBe(12);
});

test('selection bar uses the shared inset and gap', async ({ page }) => {
  await selectWorkspace(page, 'Messages');
  const grid = page.getByRole('grid', { name: 'Messages results' });
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
      await page.reload();
      const opener = page.getByRole('button', { name: 'Open navigation' });
      if (await opener.isVisible()) await opener.click();
      const view = page.locator('.saved-view').filter({ hasText: 'Long archive search' });
      await expect(view).toBeVisible();
      const open = view.getByRole('button', { name: 'Long archive search', exact: true });
      await expect(open).toBeInViewport({ ratio: 1 });
      await open.click({ trial: true });
      await view.hover();
      await view.getByRole('button', { name: 'Long archive search actions', exact: true }).click();
      for (const name of ['Rename', 'Edit description…', 'Delete']) {
        const action = page.getByRole('menuitem', { name, exact: true });
        await expect(action).toBeInViewport({ ratio: 1 });
        await action.click({ trial: true });
      }
      await page.keyboard.press('Escape');
      if (await opener.isVisible()) await opener.click();
      await open.click();
      const label = page.locator('.context-chips .kit-chip__label').filter({ hasText: query });
      await expect(label).toBeVisible();
      expect(await label.evaluate((element) => element.scrollWidth <= element.clientWidth && element.scrollHeight <= element.clientHeight)).toBe(true);
    });
  });
}

test('a long sidebar saved view list keeps every row at its full height and scrolls', async ({ page }) => {
  const description = 'Synthetic description that wraps across several lines of the saved view row. '.repeat(4).trim();
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: Array.from({ length: 40 }, (_, index) => ({
    id: index + 1, name: `Saved view ${index + 1}`, description, schema_version: 1, revision: 1,
    canonical_state: { filters: [{ field: 'source', operator: 'eq', values: ['1'] }], grouping: ['year'], presentation: index < 20 ? 'table' : 'files' },
    created_at: row.occurred_at, updated_at: row.occurred_at
  })) } }));
  await page.reload();
  const sidebar = page.getByRole('complementary', { name: 'Sidebar', exact: true });
  const views = sidebar.locator('.saved-view');
  await expect(views).toHaveCount(40);
  const rows = await views.evaluateAll((elements) => elements.map((element) => {
    const box = element.getBoundingClientRect();
    return { top: box.top, bottom: box.bottom, overflow: element.scrollHeight - element.clientHeight };
  }));
  for (const [index, current] of rows.entries()) {
    expect(current.overflow, `row ${index + 1} content overflows its row`).toBeLessThanOrEqual(0);
    expect(current.bottom - current.top, `row ${index + 1} is squashed`).toBeGreaterThanOrEqual(28);
    if (index > 0) expect(current.top, `row ${index + 1} overlaps the row above`).toBeGreaterThanOrEqual(rows[index - 1]!.bottom - 1);
  }
  const last = sidebar.getByRole('button', { name: 'Saved view 40', exact: true });
  await last.scrollIntoViewIfNeeded();
  await expect(last).toBeInViewport({ ratio: 1 });
  expect(await sidebar.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
});

test('save-view dialog shares header and field insets', async ({ page }) => {
  await selectWorkspace(page, 'Messages');
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
  await selectWorkspace(page, 'Messages');
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
  await selectWorkspace(page, 'Messages');
  await page.getByRole('searchbox', { name: 'Search messages' }).fill('archive');
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
  await page.getByRole('searchbox', { name: 'Search messages' }).fill(longQuery);
  const queryChip = chips.filter({ hasText: longQuery });
  const wrapping = await queryChip.locator('.kit-chip__label').evaluate((element) => ({
    clipped: element.scrollWidth > element.clientWidth,
    wrapped: element.getBoundingClientRect().height > Number.parseFloat(getComputedStyle(element).lineHeight)
  }));
  expect(wrapping).toEqual({ clipped: false, wrapped: true });
  await expect(queryChip.getByRole('button', { name: 'Remove search', exact: true })).toBeVisible();
  await selectWorkspace(page, 'People');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Profile filters', { exact: true }).click();
  await page.getByRole('textbox', { name: 'Category filter' }).fill('Archive');
  const filter = page.locator('.chips .kit-chip').filter({ hasText: 'Archive' });
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

test('app scrollbars use the shared 8px style', async ({ page }) => {
  await selectWorkspace(page, 'Settings');
  const size = await page.locator('.kit-settings__scroll').evaluate((element) => getComputedStyle(element, '::-webkit-scrollbar').width);
  expect(size).toBe('8px');
});
test.describe('touch reading', () => {
  test.use({ isMobile: true, hasTouch: true, viewport: { width: 620, height: 900 } });

  test('body and recording transcript share the reading size in one card', async ({ page }) => {
    await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
      rows: [{ ...row, message_type: 'beeper', conversation_type: 'direct_chat', conversation_id: 11, anchor_message_id: 1 }],
      total_count: 1, cache_revision: 'reading-size', search_provenance: {}
    } }));
    await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
      id: 11, anchor_id: 1, has_before: false, has_after: false, total: 1,
      messages: [{ id: 1, conversation_id: 11, subject: row.title, message_type: 'beeper',
        from: 'Example Person', to: [], sent_at: row.occurred_at, body: 'A synthetic voice note body.',
        attachments: [{ id: 1, filename: 'Voice-note.ogg', mime_type: 'audio/ogg', size_bytes: 2048, stored: false }] }]
    } }));
    await page.route('**/api/v1/messages/1/recordings', (route) => route.fulfill({ json: {
      message_id: 1, recordings: [{ attachment_id: 1, filename: 'Voice-note.ogg', size_bytes: 2048, state: 'ready',
        transcript: { origin: 'generated', units: [{ text: 'A synthetic voice note transcript.' }] } }]
    } }));
    await selectWorkspace(page, 'Messages');
    await page.getByText(row.title, { exact: true }).click();
    const card = page.locator('.message-card');
    await expect(card.locator('.card-body pre')).toHaveText('A synthetic voice note body.');
    await expect(card.locator('.transcript')).toContainText('A synthetic voice note transcript.');
    await expect(card.locator('.card-body pre')).toHaveCSS('font-size', '14px');
    await expect(card.locator('.transcript')).toHaveCSS('font-size', '14px');
  });
});
