import { stubOperationStatus } from './kit-ui';
import { recordingMessage, recordingTranscript, recordingSearchHit, recordingSearchReport } from '../src/lib/archive/recording-fixtures.test-support';
import { sourceGroup } from '../src/lib/explore/fixtures.test-support';
import { expect, test } from '@playwright/test';

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [] } }));
  await stubOperationStatus(page);
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
});

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

test('recordings stay capped in Table and leave Timeline rows reachable', async ({ page }) => {
  // Leave room for 350px of results and the reader's 160px minimum.
  await page.setViewportSize({ width: 1280, height: 844 });
  let resolveReadiness!: () => void;
  const readiness = new Promise<void>(resolve => { resolveReadiness = resolve; });
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: Array.from({ length: 20 }, (_, index) => entry(index + 1)), total_count: 20, cache_revision: 'recording', search_provenance: {} } }));
  await page.route('**/api/v1/media/search/status', async route => {
    await readiness;
    await route.fulfill({ json: { configured: true } });
  });
  await page.route('**/api/v1/media/search?*', route => route.fulfill({ json: recordingSearchReport({
    results: Array.from({ length: 20 }, (_, index) => recordingSearchHit({ containing_title: `Recording ${index + 1}` }))
  }) }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({ query: 'quarterly' })))}`);
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await grid.getByText('Synthetic subject 1', { exact: true }).click();
  const primary = page.locator('.results-split > [data-split-pane] > [data-pane="primary"]');
  const resize = page.getByRole('separator', { name: 'Resize reading pane' });
  const handle = (await resize.boundingBox())!;
  await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
  await page.mouse.down();
  await page.mouse.move(handle.x + handle.width / 2, 0);
  await page.mouse.up();
  await expect.poll(async () => (await primary.boundingBox())!.height).toBe(120);
  resolveReadiness();
  await expect(page.getByRole('heading', { name: /^Results/ })).toBeVisible();
  const recordings = page.getByRole('region', { name: 'Media search', exact: true });
  await expect(recordings).toBeHidden();
  const firstRow = grid.getByRole('row').filter({ hasText: 'Synthetic subject 1' }).first();
  await expect(firstRow).toBeInViewport({ ratio: 1 });
  await expect.poll(async () => (await primary.boundingBox())!.height).toBe(120);
  const readyHandle = (await resize.boundingBox())!;
  const readyPane = (await primary.boundingBox())!;
  await page.mouse.move(readyHandle.x + readyHandle.width / 2, readyHandle.y + readyHandle.height / 2);
  await page.mouse.down();
  await page.mouse.move(readyHandle.x + readyHandle.width / 2, readyHandle.y + readyHandle.height / 2 + 350 - readyPane.height);
  await page.mouse.up();
  await expect.poll(async () => (await primary.boundingBox())!.height).toBe(350);
  await expect(recordings.getByRole('link', { name: 'Recording 1', exact: true })).toBeVisible();
  const results = (await primary.boundingBox())!;
  expect((await recordings.boundingBox())!.height).toBeLessThanOrEqual(Math.min(results.height * 0.4, 320) + 1);
  expect(await recordings.evaluate(element => element.scrollHeight)).toBeGreaterThan(await recordings.evaluate(element => element.clientHeight));
  await recordings.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(recordings.getByRole('link', { name: 'Recording 20', exact: true })).toBeInViewport();
  await expect(grid.getByText('Synthetic subject 1', { exact: true })).toBeInViewport();
  await page.getByRole('combobox', { name: 'Show as: Table' }).click();
  await page.getByRole('option', { name: 'Timeline', exact: true }).click();
  await grid.getByText('Synthetic subject 1', { exact: true }).click();
  await expect.poll(async () => (await primary.boundingBox())!.height).toBe(350);
  await grid.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(grid.getByText('Synthetic subject 20', { exact: true })).toBeInViewport();
});

test('recording reader restores history and focus', async ({ page }) => {
  await page.clock.install();
  const message = recordingMessage;
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [entry(1)], total_count: 1, cache_revision: 'recording', search_provenance: {} } }));
  await page.route('**/api/v1/media/search/status', route => route.fulfill({ json: { configured: true } }));
  await page.route('**/api/v1/media/search?*', route => route.fulfill({ json: recordingSearchReport() }));
  await page.route('**/api/v1/messages/9/recordings', route => route.fulfill({ json: recordingTranscript }));
  await page.route('**/api/v1/messages/9', route => route.fulfill({ json: message }));
  await page.route('**/api/v1/conversations/2?*', route => route.fulfill({ json: { id: 2, messages: [message], anchor_id: 9, has_before: false, has_after: false, total: 1 } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({ query: 'quarterly' })))}`);
  const link = page.getByRole('link', { name: 'Team chat' });
  const reader = page.getByRole('complementary', { name: 'Reading pane: Team chat' });
  await link.click();
  await reader.getByRole('button', { name: 'Close reading pane' }).click();
  await expect(link).toBeFocused();
  await page.goBack();
  await expect(reader).toBeVisible();
  await page.reload();
  await expect(reader).toBeVisible();
  await page.goForward();
  await expect(reader).toHaveCount(0);
  await page.goBack();
  await expect(reader).toBeVisible();
  await expect(page.getByRole('searchbox', { name: 'Search messages' })).toHaveValue('quarterly');
  await expect(link).toBeVisible();
  await page.clock.fastForward(30_001);
  await reader.getByRole('button', { name: 'Close reading pane' }).click();
  await expect(page.locator('[data-recording-hits]')).toBeFocused();
});

test('the bottom reading pane opens on a single click, resizes, and persists its height', async ({ page }) => {
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', (route) => route.fulfill({
    json: {
      rows: [entry(1), entry(2)],
      total_count: 2,
      cache_revision: 'cache-reading-pane',
      search_provenance: {}
    }
  }));

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();

  // A single click opens the reading pane as a bottom split, never a drawer.
  await grid.getByText('Synthetic subject 1').click();
  const reading = page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });
  await expect(reading).toBeVisible();
  await expect(page.locator('.kit-detail-drawer-overlay')).toHaveCount(0);
  const paneBox = await page.locator('[data-pane="secondary"]').boundingBox();
  const gridBox = await grid.boundingBox();
  expect(paneBox!.y).toBeGreaterThan(gridBox!.y);

  const resize = page.getByRole('separator', { name: 'Resize reading pane' });
  const beforeResize = paneBox!.height;
  // The handle spans the split, so a pointer can grab and drag it.
  await expect(resize).toBeVisible();
  const handleBox = (await resize.boundingBox())!;
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + handleBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + handleBox.height / 2 - 48);
  await page.mouse.up();
  await expect.poll(async () => (await page.locator('[data-pane="secondary"]').boundingBox())!.height)
    .toBe(beforeResize + 48);

  await page.getByRole('button', { name: 'Close reading pane' }).click();
  await expect(reading).toHaveCount(0);
  await expect(grid).toBeFocused();

  await page.goBack();
  const restored = page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });
  await expect(restored).toBeVisible();
  // The dragged height persists locally across close and reopen.
  await expect.poll(async () => (await page.locator('[data-pane="secondary"]').boundingBox())!.height)
    .toBe(beforeResize + 48);

  await page.goForward();
  await expect(restored).toHaveCount(0);
});

test('the bottom reading pane stays within a short Messages viewport', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 400 });
  await page.route('**/api/session', route => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [entry(1), entry(2)], total_count: 2,
    cache_revision: 'cache-reading-pane', search_provenance: {}
  } }));

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Synthetic subject 1').click();
  const reading = page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });
  await expect(reading).toBeVisible();
  await expect.poll(async () => {
    const box = (await reading.boundingBox())!;
    return box.y + box.height;
  }).toBeLessThanOrEqual(400);
});

test('a direct multi-target reading-pane URL restores through refresh, Back, and Forward', async ({ page }) => {
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
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  expect(requests).toHaveLength(3);
  expect(requests.every(({ limit }) => (limit ?? 0) <= 500)).toBe(true);

  await page.reload();
  await expect(page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  expect(requests).toHaveLength(6);

  await page.getByRole('button', { name: 'Settings' }).evaluate((button: HTMLButtonElement) => button.click());
  await page.goBack();
  await expect(page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  await expect.poll(() => requests.length).toBe(9);
  await page.goForward();
  await expect(page.getByRole('button', { name: 'Settings', exact: true })).toHaveAttribute('aria-current', 'page');
  await expect(page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toHaveCount(0);
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
  const grouped = page.getByRole('grid', { name: 'Messages grouped by source' });
  await expect(grouped.getByText('Example source group')).toBeVisible();

  await page.getByRole('button', { name: 'Show items in Example source group' }).click();
  await expect(page.getByRole('complementary', { name: 'Reading pane: Restored source detail' })).toBeVisible();
  await expect(grouped).toHaveCount(0);

  await page.goBack();
  await expect(page.getByRole('grid', { name: 'Messages grouped by source' })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('complementary', { name: 'Reading pane: Restored source detail' })).toBeVisible();
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
    page.getByRole('searchbox', { name: 'Search messages' }),
    page.locator('#shortcut-textarea'),
    page.locator('#shortcut-editable')
  ]) {
    await locator.focus();
    await page.keyboard.press('Control+K');
    await page.keyboard.press('Meta+K');
    await expect(page.getByRole('dialog', { name: 'Messages commands' })).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(locator).toBeFocused();
  }
});

test('desktop deep link leaves focus on the page', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [entry(1)], total_count: 1, cache_revision: 'cache-desktop', search_provenance: {}
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({ selectedRow: 'message:1' })))}`);
  await expect(page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1' })).toBeVisible();
  await expect(page.locator('body')).toBeFocused();
});

test('phone reader replaces results and Back restores the focused row', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route('**/api/session', route => route.fulfill({ json: {
    auth_mode: 'loopback', https: false, plain_http_warning: false
  } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: Array.from({ length: 120 }, (_, index) => entry(index + 1)), total_count: 120,
    cache_revision: 'cache-phone', search_provenance: {}
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState()))}`);
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(grid.getByText('Synthetic subject 1', { exact: true })).toBeVisible();
  await grid.evaluate(element => { element.scrollTop = 1800; });
  let key: string | null = null;
  await expect.poll(async () => {
    key = await grid.evaluate(element => {
      const bounds = element.getBoundingClientRect();
      return Array.from(element.querySelectorAll<HTMLElement>('.data-row')).find(row => {
        const box = row.getBoundingClientRect();
        return box.top >= bounds.top && box.bottom <= bounds.bottom;
      })?.dataset.rowKey ?? null;
    });
    return Number(key?.split(':')[1]);
  }).toBeGreaterThan(1);
  const row = grid.locator(`[data-row-key="${key}"]`);
  const index = Number(key?.split(':')[1]);
  expect(index).toBeGreaterThan(1);
  await row.getByText('Example Person').click();
  const reading = page.getByRole('complementary', { name: `Reading pane: Synthetic subject ${index}` });
  await expect(reading).toBeVisible();
  await expect(grid).toBeHidden();
  const resultsBox = (await page.locator('.results-split').boundingBox())!;
  expect(Math.abs((await reading.boundingBox())!.width - resultsBox.width)).toBeLessThanOrEqual(1);
  await expect(reading.locator('.pane-title')).toHaveText(`Synthetic subject ${index}`);
  await expect(reading.getByRole('button', { name: 'Back to results' })).toBeFocused();
  await expect(reading.getByRole('button', { name: 'Close reading pane' })).toHaveCount(0);
  await reading.getByRole('button', { name: 'Back to results' }).click();
  await expect(grid).toBeVisible();
  await expect(grid).toBeFocused();
  await expect(grid.locator(`[data-row-key="${key}"]`)).toBeInViewport();
  await grid.locator(`[data-row-key="${key}"]`).getByText('Example Person').click();
  await page.keyboard.press('Escape');
  await expect(grid).toBeFocused();
  await expect(grid.locator(`[data-row-key="${key}"]`)).toBeInViewport();
  await grid.locator(`[data-row-key="${key}"]`).getByText('Example Person').click();
  await page.goBack();
  await expect(grid).toBeVisible();
  await expect(grid).toBeFocused();
  await expect(grid.locator(`[data-row-key="${key}"]`)).toBeInViewport();
});

test('resizing an open reader preserves expanded messages and focus', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [{ ...entry(1), conversation_id: 7, anchor_message_id: 1 }, { ...entry(2), conversation_id: 7, anchor_message_id: 2 }],
    total_count: 2, cache_revision: 'cache-resize', search_provenance: {}
  } }));
  let requests = 0;
  await page.route('**/api/v1/conversations/7?*', route => {
    requests += 1;
    return route.fulfill({ json: { id: 7, anchor_id: Number(new URL(route.request().url()).searchParams.get('anchor')), has_before: false, has_after: false, total: 3, messages: [1, 2, 3].map(id => ({ ...recordingMessage, id, conversation_id: 7, message_type: 'email', subject: `Message ${id}`, body: `Synthetic body ${id}`, body_html: '', has_attachments: false, attachments: [] })) } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState()))}`);
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await grid.getByText('Synthetic subject 1', { exact: true }).click();
  await page.getByRole('button', { name: /Expand message 2/ }).click();
  const expanded = page.getByRole('article', { name: 'Message 2' });
  await expect(expanded).toBeVisible();
  await expect(page.getByText('Synthetic body 1', { exact: true })).toBeVisible();
  const collapse = expanded.getByRole('button', { name: /Collapse message 2/ });
  await collapse.focus();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(expanded).toBeVisible();
  await expect(collapse).toBeFocused();
  expect(requests).toBe(1);
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(expanded).toBeVisible();
  await expect(collapse).toBeFocused();
  expect(requests).toBe(1);
  await grid.focus();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole('button', { name: 'Back to results' })).toBeFocused();
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(page.getByRole('button', { name: 'Close reading pane' })).toBeFocused();
  await page.evaluate(() => { Object.defineProperty(Element.prototype, 'checkVisibility', { value: undefined, configurable: true }); });
  await grid.focus();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole('button', { name: 'Back to results' })).toBeFocused();
});

test('phone recording Back keeps loaded hits and restores the opened link during an outage', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [entry(1)], total_count: 1, cache_revision: 'recording', search_provenance: {} } }));
  await page.route('**/api/v1/media/search/status', route => route.fulfill({ json: { configured: true } }));
  let requests = 0;
  await page.route('**/api/v1/media/search?*', route => {
    requests += 1;
    return requests === 1 ? route.fulfill({ json: recordingSearchReport() }) : route.fulfill({ status: 503, json: { message: 'Search unavailable' } });
  });
  await page.route('**/api/v1/messages/9', route => route.fulfill({ json: recordingMessage }));
  await page.route('**/api/v1/conversations/2?*', route => route.fulfill({ json: { id: 2, messages: [recordingMessage], anchor_id: 9, has_before: false, has_after: false, total: 1 } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({ query: 'quarterly' })))}`);
  const link = page.getByRole('link', { name: 'Team chat' });
  await link.click();
  await page.getByRole('complementary', { name: 'Reading pane: Team chat' }).getByRole('button', { name: 'Back to results' }).click();
  await expect(link).toBeVisible();
  await expect(link).toBeFocused();
  expect(requests).toBe(1);
});

for (const grouped of [false, true]) {
  test(`phone keeps a failed ${grouped ? 'group' : 'message'} list visible until Retry succeeds`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
    let requests = 0;
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    await page.route(grouped ? '**/api/v1/explore/groups' : '**/api/v1/explore', async route => {
      requests += 1;
      if (requests === 1) {
        await pending;
        return route.fulfill({ status: 503, json: { message: 'Search unavailable' } });
      }
      return route.fulfill({ json: {
        rows: grouped ? [sourceGroup] : [entry(1)],
        total_count: 1, cache_revision: 'retry', search_provenance: {}
      } });
    });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({
      query: 'alpha', groupingChain: grouped ? ['source'] : [], selectedRow: grouped ? 'group:source:7' : 'message:1'
    })))}`);
    await page.getByRole('button', { name: 'Back to results' }).focus();
    release();
    await expect(page.getByRole('alert').filter({ hasText: 'Search unavailable' })).toBeVisible();
    const grid = page.getByRole('grid', { name: grouped ? 'Messages grouped by source' : 'Messages results' });
    await expect(grid).toBeVisible();
    await expect(page.getByRole('button', { name: 'Close reading pane' })).toBeFocused();
    await page.getByRole('button', { name: /Retry (restoration|request)/ }).click();
    await expect(page.getByRole('button', { name: 'Back to results' })).toBeVisible();
    await expect(grid).toBeHidden();
    expect(requests).toBeGreaterThanOrEqual(2);
  });
}

test('right preview resizes, restores its width, and falls back below on narrow windows', async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1080 });
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [entry(1), entry(2)], total_count: 2,
    cache_revision: 'cache-layout', search_provenance: {}
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await grid.getByText('Synthetic subject 1').click();
  const reading = page.getByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });
  const resize = page.getByRole('separator', { name: 'Resize reading pane' });
  const position = page.getByRole('group', { name: 'Reader position' });
  const moveReader = async (name: string) => {
    await page.getByRole('button', { name: 'Display', exact: true }).click();
    await page.getByRole('menuitemradio', { name, exact: true }).click();
    await page.keyboard.press('Escape');
  };
  await moveReader('Right');
  await expect(resize).toHaveAttribute('aria-orientation', 'vertical');
  const primary = page.locator('.results-split > [data-split-pane] > [data-pane="primary"]');
  const secondary = page.locator('.results-split > [data-split-pane] > [data-pane="secondary"]');
  const listBox = (await primary.boundingBox())!;
  const previewBox = (await secondary.boundingBox())!;
  expect(previewBox.x).toBeGreaterThanOrEqual(listBox.x + listBox.width);
  expect(previewBox.y).toBe(listBox.y);
  await expect(reading.getByText('Synthetic excerpt 1')).toBeVisible();

  const handleBox = (await resize.boundingBox())!;
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + 50);
  await page.mouse.down();
  await page.mouse.move(handleBox.x + handleBox.width / 2 - 80, handleBox.y + 50);
  await page.mouse.up();
  await expect.poll(async () => (await secondary.boundingBox())!.width).toBeCloseTo(previewBox.width + 80, 0);
  await resize.press('ArrowLeft');
  const resizedWidth = previewBox.width + 104;
  await expect.poll(async () => (await secondary.boundingBox())!.width).toBeCloseTo(resizedWidth, 0);
  await page.reload();
  await expect(reading).toBeVisible();
  await expect.poll(async () => (await secondary.boundingBox())!.width).toBeCloseTo(resizedWidth, 0);

  await page.setViewportSize({ width: 760, height: 900 });
  await expect(resize).toHaveAttribute('aria-orientation', 'horizontal');
  await page.getByRole('button', { name: 'Display', exact: true }).click();
  await expect(position).toHaveCount(0);
  await page.keyboard.press('Escape');
  const narrowList = (await primary.boundingBox())!;
  expect((await secondary.boundingBox())!.y).toBeGreaterThanOrEqual(narrowList.y + narrowList.height);
  await expect(reading).toBeVisible();

  await page.setViewportSize({ width: 1920, height: 1080 });
  await expect(resize).toHaveAttribute('aria-orientation', 'vertical');
  await expect.poll(async () => (await secondary.boundingBox())!.width).toBeCloseTo(resizedWidth, 0);
  await moveReader('Below');
  await expect(resize).toHaveAttribute('aria-orientation', 'horizontal');
  await resize.press('ArrowUp');
  const bottomHeight = (await secondary.boundingBox())!.height;
  await moveReader('Right');
  await expect.poll(async () => (await secondary.boundingBox())!.width).toBeCloseTo(resizedWidth, 0);
  await moveReader('Below');
  await expect.poll(async () => (await secondary.boundingBox())!.height).toBe(bottomHeight);
  await moveReader('Right');
  await page.getByRole('button', { name: 'Close reading pane' }).click();
  await expect(grid).toBeFocused();
  await expect.poll(async () => (await primary.boundingBox())!.width)
    .toBeCloseTo((await page.locator('.results-split').boundingBox())!.width, 0);
});

test('aligns group actions and activity in wide and inspected views', async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 900 });
  const year = new Date().getFullYear();
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore/groups', route => {
    const body = route.request().postDataJSON();
    const label = body.grouping[0] === 'source' ? 'Example source' : 'Person Example';
    const overview = { first_at: `${year}-05-03T12:00:00Z`, activity: [{ bucket: `${year}-05`, count: 1 }, { bucket: `${year}-06`, count: 2 }], co_participants: [] };
    return route.fulfill({ json: {
      rows: [{ key: '7', label, count: 3, estimated_bytes: 512, latest_at: `${year}-06-30T12:00:00Z` }],
      ...(body.group_key ? { overview } : {}),
      total_count: 1, cache_revision: 'cache-groups', search_provenance: {}
    } });
  });
  await page.route('**/api/v1/explore/files', route => route.fulfill({ json: { files: [], total_count: 0, cache_revision: 'cache-groups', search_provenance: {} } }));
  await page.route('**/api/v1/meetings/metrics', route => route.fulfill({ json: {
    schema_version: 1, archive_uid: 'synthetic', scope: { kind: 'explore' },
    totals: { meeting_count: 0, known_duration_count: 0, unknown_duration_count: 0, total_known_seconds: 0, average_known_seconds: 0 },
    first_meeting_at: null, last_meeting_at: null, undated_count: 0, duration_by_basis: [], months: []
  } }));
  await page.route('**/api/v1/meetings/actions', route => route.fulfill({ json: {
    schema_version: 1, archive_uid: 'synthetic', scope: { kind: 'explore' }, rows: [], total_count: 0,
    coverage: { meeting_count: 0, available: 0, partial: 0, unsupported: 0, unavailable: 0 }
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({ groupingChain: ['source'] })))}`);
  const sources = page.getByRole('grid', { name: 'Messages grouped by source' });
  const sourceAction = sources.getByRole('row', { name: /Example source/ }).getByRole('button', { name: 'Show items in Example source' });
  await expect(sourceAction).toBeVisible();
  const actionHeader = (await sources.getByRole('columnheader', { name: 'Show items' }).boundingBox())!;
  const actionButton = (await sourceAction.boundingBox())!;
  expect(Math.abs(actionHeader.x + actionHeader.width / 2 - actionButton.x - actionButton.width / 2)).toBeLessThanOrEqual(1);
  const sourceGrid = (await sources.boundingBox())!;
  const sourceCell = (await sourceAction.locator('..').boundingBox())!;
  expect.soft(sourceGrid.x + sourceGrid.width - sourceCell.x - sourceCell.width, 'wide Source action inset').toBeLessThanOrEqual(2);

  const width = await sources.evaluate(element => element.clientWidth);
  await page.setViewportSize({ width: page.viewportSize()!.width + 640 - width, height: 900 });
  await expect.poll(async () => (await sources.boundingBox())!.width).toBeCloseTo(640, 0);
  const narrowGrid = (await sources.boundingBox())!;
  const narrowAction = (await sourceAction.boundingBox())!;
  expect(narrowAction.x + narrowAction.width).toBeLessThanOrEqual(narrowGrid.x + narrowGrid.width);
  await page.setViewportSize({ width: 1920, height: 900 });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState({ groupingChain: ['participant'] })))}`);
  const people = page.getByRole('grid', { name: 'Messages grouped by participant' });
  await people.getByText('Person Example', { exact: true }).click();
  await expect(page.getByRole('region', { name: 'Group overview' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Group overview' }).getByText('May 3 – Jun 30')).toBeVisible();
  const personAction = people.getByRole('row', { name: /Person Example/ }).getByRole('button', { name: 'Show items in Person Example' });
  const personGrid = (await people.boundingBox())!;
  const personCell = (await personAction.locator('..').boundingBox())!;
  expect.soft(personGrid.x + personGrid.width - personCell.x - personCell.width, 'inspected Person action inset').toBeLessThanOrEqual(2);
  const personSpacing = await people.getByRole('row', { name: /Person Example/ }).evaluate(row => {
    const count = row.querySelector('.group-count')!;
    const countRange = document.createRange();
    countRange.selectNodeContents(count);
    return countRange.getBoundingClientRect().left - row.getBoundingClientRect().left;
  });
  expect.soft(personSpacing, 'inspected Person count stays beside the capped name column').toBeLessThanOrEqual(560);
  const activity = page.getByRole('img', { name: /Activity by month/ });
  expect((await activity.boundingBox())!.width).toBeLessThanOrEqual(320);
  expect((await activity.locator('rect').first().boundingBox())!.width).toBeLessThanOrEqual(8);
  const personActivityGap = await page.getByRole('img', { name: /Activity by month/ }).evaluate(svg => {
    const lastBar = svg.querySelector('rect:last-child')!;
    return svg.getBoundingClientRect().right - lastBar.getBoundingClientRect().right;
  });
  expect.soft(personActivityGap, 'last activity bar aligns with its endpoint').toBeLessThanOrEqual(1);
  await page.getByRole('button', { name: 'Display', exact: true }).click();
  await page.getByRole('menuitemradio', { name: 'Right', exact: true }).click();
  await page.keyboard.press('Escape');
  expect(await people.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
});

test('shows the full message time beside a long sender on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const message = {
    id: 42, conversation_id: 7, subject: 'Garden plans',
    body: 'Please send the revised budget by Friday.',
    from: 'Mira Example <mira@example.com>', to: ['team@example.com'],
    sent_at: `${new Date().getUTCFullYear()}-07-18T12:00:00Z`, message_type: 'email', attachments: []
  };
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [{ ...entry(42), title: 'Garden plans', conversation_id: 7, anchor_message_id: 42 }],
    total_count: 1, cache_revision: 'fixture', search_provenance: {}
  } }));
  await page.route('**/api/v1/messages/42', route => route.fulfill({ json: message }));
  await page.route('**/api/v1/conversations/7?*', route => route.fulfill({ json: {
    id: 7, messages: [message], anchor_id: 42, has_before: false, has_after: false, total: 1
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState()))}`);
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Garden plans', { exact: true }).click();
  const card = page.getByRole('article', { name: 'Message 42' });
  const time = card.locator('time');
  const menu = card.getByRole('button', { name: /More actions for/ });
  await expect(time).toHaveText('Jul 18, 12:00 PM');
  const timeBox = (await time.boundingBox())!;
  const menuBox = (await menu.boundingBox())!;
  expect(timeBox.x + timeBox.width <= menuBox.x || timeBox.y >= menuBox.y + menuBox.height).toBe(true);
});

test('keeps Kata issue choices focused while navigating with arrow keys', async ({ page }) => {
  const message = {
    id: 42, conversation_id: 7, subject: 'Garden plans', body: 'Please send notes.',
    from: 'Mira Example <mira@example.com>', to: ['team@example.com'],
    sent_at: '2026-07-18T12:00:00Z', message_type: 'email', attachments: []
  };
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [{ ...entry(42), title: 'Garden plans', conversation_id: 7, anchor_message_id: 42 }],
    total_count: 1, cache_revision: 'fixture', search_provenance: {}
  } }));
  await page.route('**/api/v1/messages/42', route => route.fulfill({ json: message }));
  await page.route('**/api/v1/conversations/7?*', route => route.fulfill({ json: {
    id: 7, messages: [message], anchor_id: 42, has_before: false, has_after: false, total: 1
  } }));
  await page.route('**/api/v1/integrations/tasks/status', route => route.fulfill({ json: { state: 'disabled' } }));
  await page.route('**/api/v1/messages/42/tasks', route => route.fulfill({ json: { state: 'disabled', complete: false, tasks: [], outbound_metadata: {} } }));
  await page.route('**/api/v1/integrations/kata/status', route => route.fulfill({ json: { state: 'ready', project: 'garden' } }));
  await page.route('**/api/v1/integrations/kata/issues?*', route => route.fulfill({ json: { issues: [], truncated: false } }));
  await page.route('**/api/v1/integrations/kata/open-issues', route => route.fulfill({ json: {
    issues: [
      { uid: 'one', qualified_ref: 'garden#one', title: 'First', status: 'open' },
      { uid: 'two', qualified_ref: 'garden#two', title: 'Second', status: 'open' },
      { uid: 'three', qualified_ref: 'garden#three', title: 'Third', status: 'open' }
    ], truncated: false
  } }));
  await page.route('**/api/v1/integrations/kata/evidence/prepare', route => route.fulfill({ json: { evidence: [{
    id: 'a'.repeat(64), passage: 'b'.repeat(64),
    reference: { version: 1, kind: 'message', archive_uid: 'archive-example', message_id: 42, message: { body_sha256: 'c'.repeat(64), start_rune: 0, end_rune: 18 } },
    excerpt: message.body, display: {}, content_trust: 'untrusted'
  }] } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState()))}`);
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Garden plans', { exact: true }).click();
  const card = page.getByRole('article', { name: 'Message 42' });
  await expect(card.getByRole('region', { name: 'Tasks', exact: true })).toHaveCount(0);
  await expect(card.locator('.task-notices')).toHaveCSS('padding-bottom', '0px');
  await card.getByRole('button', { name: /More actions for/ }).click();
  await page.getByRole('menuitem', { name: 'Add to task…' }).click();
  await page.getByRole('radio', { name: 'Kata', exact: true }).click();
  await page.getByRole('radio', { name: 'Existing', exact: true }).click();
  const first = page.getByRole('radio', { name: 'First · garden#one' });
  const second = page.getByRole('radio', { name: 'Second · garden#two' });
  const third = page.getByRole('radio', { name: 'Third · garden#three' });
  await first.focus();
  await page.keyboard.press('ArrowDown');
  await expect(second).toBeChecked();
  await expect(second).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await expect(third).toBeChecked();
  await expect(third).toBeFocused();
  await expect(first).toBeVisible();
  await expect(page.getByRole('searchbox', { name: 'Filter open issues' })).toBeVisible();
});

test('keeps the new task form inside the dialog on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const message = { id: 42, conversation_id: 7, subject: 'Garden plans', body: 'Please send notes.', from: 'Mira Example <mira@example.com>', to: ['team@example.com'], sent_at: '2026-07-18T12:00:00Z', message_type: 'email', attachments: [] };
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [{ ...entry(42), title: 'Garden plans', conversation_id: 7, anchor_message_id: 42 }], total_count: 1, cache_revision: 'fixture', search_provenance: {} } }));
  await page.route('**/api/v1/messages/42', route => route.fulfill({ json: message }));
  await page.route('**/api/v1/conversations/7?*', route => route.fulfill({ json: { id: 7, messages: [message], anchor_id: 42, has_before: false, has_after: false, total: 1 } }));
  await page.route('**/api/v1/integrations/tasks/status', route => route.fulfill({ json: { state: 'ready', project: 'garden' } }));
  await page.route('**/api/v1/messages/42/tasks', route => route.fulfill({ json: { state: 'ready', complete: true, tasks: [{ id: 'task-1', title: 'Garden task' }], outbound_metadata: { archive_uid: 'archive-example', message_id: 42, conversation_id: 7, source_type: 'email', source_identifier: 'archive@example.com', source_message_id: 'message-example', subject: message.subject, from: message.from, sent_at: message.sent_at } } }));
  await page.route('**/api/v1/integrations/kata/status', route => route.fulfill({ json: { state: 'ready', project: 'garden' } }));
  await page.route('**/api/v1/integrations/kata/issues?*', route => route.fulfill({ json: { issues: [{ uid: 'one', qualified_ref: 'garden#one', title: 'Garden issue', status: 'open', web_url: 'https://example.org/issue' }], truncated: false } }));
  await page.route('**/api/v1/integrations/kata/open-issues', route => route.fulfill({ json: { issues: [{ uid: 'one', qualified_ref: 'garden#one', title: 'First', status: 'open' }], truncated: false } }));
  await page.route('**/api/v1/integrations/kata/evidence/prepare', route => route.fulfill({ json: { evidence: [{ id: 'a'.repeat(64), passage: 'b'.repeat(64), reference: { version: 1, kind: 'message', archive_uid: 'archive-example', message_id: 42, message: { body_sha256: 'c'.repeat(64), start_rune: 0, end_rune: 18 } }, excerpt: message.body, display: {}, content_trust: 'untrusted' }] } }));
  await page.route('**/api/v1/integrations/kata/issues', route => route.fulfill({ json: { issue: { uid: 'one', qualified_ref: 'garden#one', title: 'First', status: 'open', web_url: 'https://example.org/issue' }, replayed: false } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreURLState()))}`);
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Garden plans', { exact: true }).click();
  const unlink = page.getByRole('button', { name: 'Unlink Garden task' });
  await expect(unlink).toBeVisible();
  const taskRow = unlink.locator('xpath=ancestor::div[contains(@class, "detail-row")]');
  expect.soft(await unlink.evaluate(el => parseFloat(getComputedStyle(el).borderTopWidth))).toBeGreaterThan(0);
  expect.soft((await unlink.boundingBox())!.x - (await taskRow.boundingBox())!.x).toBeLessThan(30);
  const openKata = page.getByRole('link', { name: 'Open in Kata' });
  await expect(openKata).toBeVisible();
  expect.soft(await openKata.evaluate(el => parseFloat(getComputedStyle(el).borderTopWidth))).toBeGreaterThan(0);
  const issueRow = openKata.locator('xpath=ancestor::div[contains(@class, "detail-row")]');
  expect.soft((await openKata.boundingBox())!.x - (await issueRow.boundingBox())!.x).toBeLessThan(30);
  await page.getByRole('article', { name: 'Message 42' }).getByRole('button', { name: /More actions for/ }).click();
  await page.getByRole('menuitem', { name: 'Add to task…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Add to task' });
  await expect(dialog.getByText('garden', { exact: true })).toBeVisible();
  const context = dialog.getByText(/Garden plans\s· Mira Example/);
  await expect(context).toContainText('Mira Example <mira@example.com> · Jul 18');
  expect.soft(await context.evaluate(el => el.scrollWidth <= el.clientWidth && el.scrollHeight <= el.clientHeight)).toBe(true);
  expect.soft(await context.evaluate(el => Number(getComputedStyle(el).fontWeight))).toBeLessThan(600);
  expect.soft(await context.locator('..').evaluate(el => parseFloat(getComputedStyle(el).borderRadius))).toBeLessThanOrEqual(8);
  expect.soft(await context.evaluate(el => getComputedStyle(el.closest('.kit-chip')!).paddingTop)).not.toBe('0px');
  expect.soft(await context.evaluate(el => el.textContent?.includes('\u00a0· Jul 18'))).toBe(true);
  const title = dialog.getByRole('textbox', { name: 'Task title' });
  expect.soft(await context.evaluate(el => getComputedStyle(el.closest('.kit-chip')!).backgroundColor)).not.toBe(await title.evaluate(el => getComputedStyle(el).backgroundColor));
  const bounds = (await dialog.boundingBox())!;
  for (const control of [dialog.getByText('garden', { exact: true }), dialog.getByText(/Garden plans\s· Mira Example/), dialog.getByRole('textbox', { name: 'Task title' }), dialog.getByRole('textbox', { name: 'Description' }), dialog.getByRole('combobox', { name: 'Priority: No priority' }), dialog.getByRole('textbox', { name: 'Labels' }), dialog.getByRole('button', { name: 'Create task' })]) {
    const box = (await control.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(bounds.x);
    expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width);
  }
  await page.getByRole('radio', { name: 'Kata', exact: true }).click();
  const kata = page.getByRole('dialog', { name: 'Add to Kata issue' });
  await expect(kata.getByRole('button', { name: 'Create issue' })).toBeEnabled();
  const earlier = kata.getByRole('button', { name: 'Earlier passage' });
  const later = kata.getByRole('button', { name: 'Later passage' });
  const quote = kata.getByRole('button', { name: 'Quote selection' });
  expect.soft(await quote.innerText()).toBe('Quote selection');
  expect.soft(await earlier.innerText()).toContain('passage');
  expect.soft(await later.innerText()).toContain('passage');
  const cancel = kata.getByRole('button', { name: 'Cancel' });
  expect.soft(await quote.evaluate(el => getComputedStyle(el).fontSize)).toBe(await cancel.evaluate(el => getComputedStyle(el).fontSize));
  expect.soft((await quote.boundingBox())!.height).toBeGreaterThanOrEqual((await cancel.boundingBox())!.height);
  const passage = kata.getByRole('region', { name: 'Quoted passage' }).locator('pre');
  expect.soft((await quote.boundingBox())!.y).toBe((await earlier.boundingBox())!.y);
  expect.soft((await later.boundingBox())!.y).toBeGreaterThan((await earlier.boundingBox())!.y);
  expect.soft((await quote.boundingBox())!.y - ((await passage.boundingBox())!.y + (await passage.boundingBox())!.height)).toBeGreaterThanOrEqual(12);
  for (const control of [kata.getByRole('textbox', { name: 'Issue title' }), kata.getByRole('textbox', { name: 'Brief' }), kata.getByRole('button', { name: 'Later passage' }), kata.getByRole('button', { name: 'Create issue' })]) {
    const box = (await control.boundingBox())!;
    expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width);
  }
  await kata.getByRole('radio', { name: 'Existing', exact: true }).click();
  await expect(kata.getByRole('radio', { name: 'First · garden#one' })).toBeVisible();
  expect.soft(await quote.innerText()).toBe('Quote selection');
  expect.soft((await quote.boundingBox())!.y).toBe((await earlier.boundingBox())!.y);
  expect.soft((await later.boundingBox())!.y).toBeGreaterThan((await earlier.boundingBox())!.y);
  expect.soft((await quote.boundingBox())!.y - ((await passage.boundingBox())!.y + (await passage.boundingBox())!.height)).toBeGreaterThanOrEqual(12);
  const results = kata.locator('.results');
  expect.soft((await results.boundingBox())!.height).toBeLessThan(100);
  const hint = kata.getByText('Search by whole word. The list is capped at 25.');
  expect.soft((await hint.boundingBox())!.y - ((await results.boundingBox())!.y + (await results.boundingBox())!.height)).toBeGreaterThanOrEqual(12);
  for (const control of [kata.getByRole('searchbox', { name: 'Filter open issues' }), kata.getByRole('button', { name: 'Later passage' }), kata.getByRole('button', { name: 'Add quote' })]) {
    const box = (await control.boundingBox())!;
    expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width);
  }
  await page.setViewportSize({ width: 1280, height: 900 });
  expect.soft((await results.boundingBox())!.height).toBeLessThan(100);
  expect.soft((await hint.boundingBox())!.y - ((await results.boundingBox())!.y + (await results.boundingBox())!.height)).toBeGreaterThanOrEqual(12);
  await page.setViewportSize({ width: 390, height: 844 });
  await kata.getByRole('radio', { name: 'New', exact: true }).click();
  await kata.getByRole('button', { name: 'Create issue' }).click();
  const saved = kata.getByText('Saved.', { exact: true });
  const destination = kata.getByText('Kata · garden#one');
  const receiptLink = kata.getByRole('link', { name: 'Open in Kata' });
  const done = kata.getByRole('button', { name: 'Done' });
  await expect(saved).toBeVisible();
  for (const viewport of [{ width: 390, height: 844 }, { width: 1280, height: 900 }]) {
    await page.setViewportSize(viewport);
    expect.soft((await destination.boundingBox())!.y - ((await saved.boundingBox())!.y + (await saved.boundingBox())!.height)).toBeGreaterThanOrEqual(4);
    expect.soft((await done.boundingBox())!.y - ((await receiptLink.boundingBox())!.y + (await receiptLink.boundingBox())!.height)).toBeLessThanOrEqual(16);
  }

});
