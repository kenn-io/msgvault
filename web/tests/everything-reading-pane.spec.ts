import { expect, test } from '@playwright/test';

function entry(index: number) {
  return {
    key: `message:${index}`,
    kind: 'message',
    message_type: 'email',
    conversation_type: 'email',
    title: `Synthetic subject ${index}`,
    preview: `Synthetic excerpt ${index}`,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_identifier: 'archive@example.com',
    source_type: 'synthetic',
    participant_labels: ['Example Person'],
    participant_ids: [1],
    attachment_count: 1,
    attachment_size: 128,
    has_attachments: true,
    deleted_from_source: false,
    message_count: 1,
    match: {}
  };
}

function exploreURLState(overrides: Record<string, unknown> = {}) {
  return {
    schemaVersion: 1,
    workspace: 'everything',
    query: '',
    searchMode: 'full_text',
    filters: [],
    groupingChain: [],
    presentation: 'table',
    sort: [{ field: 'occurred_at', direction: 'desc' }],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'],
    columnWidths: {},
    activeRow: null,
    selectedRow: null,
    inspectorPinned: false,
    conversationAnchor: null,
    scrollAnchor: null,
    ...overrides
  };
}

test('pinned inspector width and visibility restore through browser history', async ({ page }) => {
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', (route) => route.fulfill({
    json: {
      rows: [entry(1), entry(2)],
      total_count: 2,
      cache_revision: 'cache-inspector',
      search_provenance: {}
    }
  }));

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();
  await grid.focus();
  await page.keyboard.press('Enter');

  const inspector = page.getByRole('complementary', { name: 'Inspect Synthetic subject 1' });
  await expect(inspector).toBeVisible();
  await expect(page.locator('.kit-detail-drawer-overlay')).toHaveCount(0);
  const resize = page.getByRole('button', { name: 'Resize inspector' });
  await resize.press('ArrowLeft');
  await resize.press('ArrowLeft');
  await expect(inspector).toHaveCSS('width', '428px');

  await page.getByRole('button', { name: 'Close inspector' }).click();
  await expect(inspector).toHaveCount(0);
  await expect(grid).toBeFocused();

  await page.goBack();
  const restored = page.getByRole('complementary', { name: 'Inspect Synthetic subject 1' });
  await expect(restored).toBeVisible();
  await expect(restored).toHaveCSS('width', '428px');

  await page.goForward();
  await expect(restored).toHaveCount(0);
});

test('a direct multi-target inspector URL restores through refresh, Back, and Forward', async ({ page }) => {
  const requests: Array<{ cursor?: string; limit?: number }> = [];
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', async (route) => {
    const body = route.request().postDataJSON() as { cursor?: string; limit?: number };
    requests.push(body);
    const offset = body.cursor ? Number(body.cursor.slice('page:'.length)) : 0;
    await route.fulfill({ json: {
      rows: Array.from({ length: 500 }, (_, index) => entry(offset + index + 1)),
      total_count: 1500,
      cache_revision: 'cache-direct',
      search_provenance: {},
      ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
    } });
  });
  const state = exploreURLState({
    activeRow: 'message:1',
    selectedRow: 'message:1200',
    scrollAnchor: { key: 'message:1', offset: 5 }
  });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(page.getByRole('complementary', { name: 'Inspect Synthetic subject 1200' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  expect(requests).toHaveLength(3);
  expect(requests.every(({ limit }) => (limit ?? 0) <= 500)).toBe(true);

  await page.reload();
  await expect(page.getByRole('complementary', { name: 'Inspect Synthetic subject 1200' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  expect(requests).toHaveLength(6);

  await page.getByRole('button', { name: 'Settings' }).evaluate((button: HTMLButtonElement) => button.click());
  await page.goBack();
  await expect(page.getByRole('complementary', { name: 'Inspect Synthetic subject 1200' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  await expect.poll(() => requests.length).toBe(9);
  await page.goForward();
  await expect(page.getByRole('button', { name: 'Settings', exact: true })).toHaveAttribute('aria-current', 'page');
  await expect(page.getByRole('complementary', { name: 'Inspect Synthetic subject 1200' })).toHaveCount(0);
});

test('a drilled aggregate restores after grouped rows clear and through Back and Forward', async ({ page }) => {
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore/groups', async (route) => {
    const body = route.request().postDataJSON() as {
      filters?: Array<{ dimension: string; values: string[] }>;
    };
    const detail = body.filters?.some((filter) =>
      filter.dimension === 'source' && filter.values.includes('7'));
    await route.fulfill({ json: {
      rows: [{
        key: '7', label: detail ? 'Restored source detail' : 'Example source group',
        count: 12, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z'
      }],
      total_count: 1, cache_revision: 'cache-groups', search_provenance: {}
    } });
  });
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'cache-groups', search_provenance: {}
  } }));
  const state = exploreURLState({ groupingChain: ['source'] });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grouped = page.getByRole('grid', { name: 'Everything grouped by source' });
  await expect(grouped.getByText('Example source group')).toBeVisible();

  await page.getByRole('button', { name: 'Drill into Example source group' }).click();
  await expect(page.getByRole('complementary', { name: 'Inspect Restored source detail' })).toBeVisible();
  await expect(grouped).toHaveCount(0);

  await page.goBack();
  await expect(page.getByRole('grid', { name: 'Everything grouped by source' })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('complementary', { name: 'Inspect Restored source detail' })).toBeVisible();
});

test('global command and Escape shortcuts stay suspended in editable controls', async ({ page }) => {
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'cache-editable', search_provenance: {}
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await page.evaluate(() => {
    const textarea = document.createElement('textarea');
    textarea.id = 'shortcut-textarea';
    const editable = document.createElement('div');
    editable.id = 'shortcut-editable';
    editable.contentEditable = 'true';
    editable.tabIndex = 0;
    document.body.append(textarea, editable);
  });

  for (const locator of [
    page.getByRole('searchbox', { name: 'Search everything' }),
    page.locator('#shortcut-textarea'),
    page.locator('#shortcut-editable')
  ]) {
    await locator.focus();
    await page.keyboard.press('Control+K');
    await page.keyboard.press('Meta+K');
    await expect(page.getByRole('dialog', { name: 'Everything commands' })).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(locator).toBeFocused();
  }
});
