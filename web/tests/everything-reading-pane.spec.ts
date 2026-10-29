import { stubOperationStatus } from './kit-ui';
import { recordingMessage, recordingTranscript, recordingSearchHit, recordingSearchReport } from '../src/lib/archive/recording-fixtures.test-support';
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
  const pane = (await primary.boundingBox())!;
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
  await page.mouse.move(readyHandle.x + readyHandle.width / 2, readyHandle.y + readyHandle.height / 2);
  await page.mouse.down();
  await page.mouse.move(handle.x + handle.width / 2, pane.y + 350);
  await page.mouse.up();
  await expect.poll(async () => (await primary.boundingBox())!.height).toBeCloseTo(350, -1);
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
  await expect.poll(async () => (await primary.boundingBox())!.height).toBeCloseTo(350, -1);
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

  await page.getByRole('button', { name: 'Drill into Example source group' }).click();
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

test('message menu creates a task, reloads its link, unlinks it, and opens a person with Back', async ({ page }) => {
  let linked = false;
  const writes: unknown[] = [];
  const message = { id: 42, conversation_id: 7, message_type: 'email', subject: 'Synthetic subject 1', from: 'Alex Example <alex@example.com>', from_participant_id: 19, to: ['owner@example.com'], to_participants: [{ display: 'owner@example.com', participant_id: 20 }], sent_at: '2026-07-18T12:00:00Z', body: 'Please review the notes.', body_html: '<p>Please review the notes.</p>', attachments: [{ id: 5, filename: 'notes.txt', mime_type: 'text/plain', size: 128, content_hash: 'a'.repeat(64) }] };
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [{ ...entry(1), anchor_message_id: 42, conversation_id: 7 }], total_count: 1, cache_revision: 'menu', search_provenance: {} } }));
  await page.route('**/api/v1/conversations/7?*', route => route.fulfill({ json: { id: 7, anchor_id: 42, messages: [message], has_before: false, has_after: false, total: 1 } }));
  await page.route('**/api/v1/messages/42/recordings', route => route.fulfill({ json: { recordings: [] } }));
  await page.route('**/api/v1/integrations/kata/status', route => route.fulfill({ json: { state: 'disabled' } }));
  await page.route('**/api/v1/integrations/tasks/status', route => route.fulfill({ json: { state: 'ready', project: 'example' } }));
  await page.route('**/api/v1/messages/42/tasks', async route => {
    if (route.request().method() === 'POST') {
      writes.push(route.request().postDataJSON()); linked = true;
      return route.fulfill({ status: 201, json: { task: { id: 'task-1', title: 'Synthetic subject 1', revision: 'r1' } } });
    }
    await route.fulfill({ json: { state: 'ready', complete: true, tasks: linked ? [{ id: 'task-1', title: 'Synthetic subject 1', revision: 'r1' }] : [], outbound_metadata: { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: message.subject, from: message.from, sent_at: message.sent_at } } });
  });
  await page.route('**/api/v1/messages/42/tasks/task-1', route => { linked = false; return route.fulfill({ json: { task: { id: 'task-1' } } }); });
  await page.goto('/?workspace=everything');
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Synthetic subject 1', { exact: true }).click();
  const card = page.getByRole('article', { name: 'Message 42', exact: true });
  await expect(card.getByRole('link', { name: 'notes.txt' })).toBeVisible();
  await card.getByRole('button', { name: 'More actions for message 42' }).click();
  await card.getByRole('menuitem', { name: 'Show plain text' }).click();
  await expect(card.getByText('Please review the notes.', { exact: true })).toBeVisible();
  await card.getByRole('button', { name: 'More actions for message 42' }).click();
  await card.getByRole('menuitem', { name: 'Add to task…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Add to task', exact: true });
  await dialog.getByText('Metadata leaving the archive').click();
  await expect(dialog.getByText('archive-a', { exact: true })).toBeVisible();
  await dialog.getByRole('button', { name: 'Create task', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(card.getByRole('button', { name: 'Unlink Synthetic subject 1' })).toBeVisible();
  expect(writes).toEqual([expect.objectContaining({ title: 'Synthetic subject 1' })]);
  await page.reload();
  await card.getByRole('button', { name: 'Unlink Synthetic subject 1' }).click();
  await expect(card.getByText('No linked tasks.', { exact: true })).toBeVisible();
  await card.getByRole('button', { name: 'Alex Example <alex@example.com>', exact: true }).click();
  await expect(page).toHaveURL(url => url.searchParams.get('workspace') === 'relationships' && JSON.parse(url.searchParams.get('explore') ?? '{}').relationshipTarget === 'cluster:19');
  await page.goBack();
  await expect(card).toBeVisible();
});

for (const standalone of [false, true]) {
  test(`exact participants and task setup work from ${standalone ? 'a linked message' : 'Messages'}`, async ({ page }) => {
    const message = { id: 42, conversation_id: 7, message_type: 'email', subject: 'Identity follow-up', from: 'Alex Example', from_participant_id: 29, to: ['Alex Example'], to_participants: [{ display: 'Alex Example', participant_id: 39 }], body: 'Exact archived identities.', sent_at: '2026-07-18T12:00:00Z', attachments: [] };
    await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false } }));
    await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [{ ...entry(1), title: message.subject, anchor_message_id: 42, conversation_id: 7 }], total_count: 1, cache_revision: 'identity', search_provenance: {} } }));
    await page.route('**/api/v1/messages/42', route => route.fulfill({ json: message }));
    await page.route('**/api/v1/conversations/7?*', route => route.fulfill({ json: { id: 7, anchor_id: 42, messages: [message], has_before: false, has_after: false, total: 1 } }));
    await page.route('**/api/v1/messages/42/recordings', route => route.fulfill({ json: { recordings: [] } }));
    await page.route('**/api/v1/integrations/kata/status', route => route.fulfill({ json: { state: 'disabled' } }));
    await page.route('**/api/v1/integrations/tasks/status', route => route.fulfill({ json: { state: 'disabled' } }));
    await page.route('**/api/v1/messages/42/tasks', route => route.fulfill({ json: { state: 'disabled', tasks: [], complete: true } }));
    await page.goto(standalone ? '/messages/42' : '/?workspace=everything');
    if (!standalone) await page.getByRole('grid', { name: 'Messages results' }).getByText(message.subject, { exact: true }).click();
    const card = page.getByRole('article', { name: 'Message 42', exact: true });
    for (const [index, participantID] of [[0, 29], [1, 39]]) {
      await card.getByRole('button', { name: 'Alex Example', exact: true }).nth(index!).click();
      await expect(page).toHaveURL(url => url.searchParams.get('workspace') === 'relationships' && JSON.parse(url.searchParams.get('explore') ?? '{}').relationshipTarget === `cluster:${participantID}`);
      await page.goBack();
      await expect(card).toBeVisible();
    }
    await card.getByRole('button', { name: 'Set up', exact: true }).click();
    await expect(page).toHaveURL(url => url.searchParams.get('workspace') === 'settings' && JSON.parse(url.searchParams.get('explore') ?? '{}').settingsCategory === 'tasks');
  });
}
