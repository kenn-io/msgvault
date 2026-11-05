import { stubOperationStatus } from './kit-ui';
import { expect, test } from '@playwright/test';
import { selectKitOption, selectWorkspace } from './kit-ui';
import { exploreHistoryState } from './explore-state';

test.beforeEach(async ({ page }) => {
  await page.route('**/api/session', route => route.fulfill({ json: {
    auth_mode: 'session', csrf_token: 'csrf', https: true, plain_http_warning: false
  } }));
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [] } }));
  await stubOperationStatus(page);
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
});

function fileRow(id: number, title: string) {
  return {
    id, key: `source:1:message:m${id}:file:${id}`, entry_key: `source:1:message:m${id}`,
    message_id: id, conversation_id: id,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
    source_identifier: 'archive@example.com', containing_title: title,
    filename: `deep-${id}.pdf`, mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 2048,
    content_state: 'missing_blob', content_available: false
  };
}

const rows = [1, 2].map((id) => ({
  key: `message:${id}`, kind: 'message', message_type: 'email', conversation_type: 'email_thread',
  title: `Presentation message ${id}`, preview: `Pasta analysis ${id}`,
  occurred_at: `2026-07-${20 - id}T12:00:00Z`, source_id: 1,
  source_identifier: 'archive@example.com', source_type: 'synthetic',
  participant_labels: ['Example Person'], participant_ids: [1], attachment_count: 1,
  attachment_size: 2048, has_attachments: true, deleted_from_source: false,
  message_count: 1, anchor_message_id: id, conversation_id: id, match: {}
}));

test.describe('compact toolbar', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  async function expectOneRow(controls: import('@playwright/test').Locator) {
    const boxes = await Promise.all([
      controls.getByRole('button', { name: 'Filter', exact: true }).boundingBox(),
      controls.getByRole('button', { name: 'View', exact: true }).boundingBox(),
      controls.locator('.context-count').boundingBox()
    ]);
    const centers = boxes.map((box) => box!.y + box!.height / 2);
    expect(Math.max(...centers) - Math.min(...centers)).toBeLessThanOrEqual(2);
  }

  async function stubVisualFiles(page: import('@playwright/test').Page, requests?: Array<Record<string, unknown>>) {
    await page.route('**/api/v1/multimodal/status', route => route.fulfill({ json: { generation: { state: 'active', consented: true } } }));
    await page.route('**/api/v1/files/search', async route => {
      requests?.push(route.request().postDataJSON());
      await route.fulfill({ json: { files: [], total_count: 0, cache_revision: 'toolbar' } });
    });
  }

  test('compact Messages and Files toolbars fit one row and View opens Files', async ({ page }) => {
    await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows, total_count: rows.length, cache_revision: 'toolbar' } }));
    await page.route('**/api/v1/files/search', route => route.fulfill({ json: { files: [fileRow(1, 'Presentation message 1')], total_count: 1234567, cache_revision: 'toolbar' } }));
    await page.goto('/?workspace=everything');
    for (const name of ['Messages', 'Files']) {
      const main = page.getByRole('main', { name, exact: true });
      await expect(main.getByRole('grid')).toBeVisible();
      const controls = main.locator('.context-controls');
      await expect(controls.getByRole('button', { name: 'View', exact: true })).toBeVisible();
      await expectOneRow(controls);
      await controls.getByRole('button', { name: 'View', exact: true }).click();
      if (name === 'Messages') {
        await page.getByRole('menuitemradio', { name: 'Files', exact: true }).click();
        await expect(page).toHaveURL(/[?&]workspace=files(&|$)/);
      } else {
        await page.keyboard.press('Escape');
        await controls.getByRole('button', { name: 'Filter', exact: true }).click();
        const dialog = page.getByRole('dialog', { name: 'Filter' });
        await dialog.getByLabel('From', { exact: true }).fill('2026-07-01');
        await dialog.getByLabel('To', { exact: true }).fill('2026-07-31');
        await page.keyboard.press('Escape');
        await expect(controls.getByRole('button', { name: 'Filter', exact: true })).toHaveText('Filter · 2');
        await expect(controls.locator('.context-count')).toHaveText('1,234,567 files');
        await expectOneRow(controls);
      }
    }
  });

  test('compact Files opens visual search setup from Filter', async ({ page }) => {
    await page.route('**/api/v1/files/search', route => route.fulfill({ json: { files: [], total_count: 0, cache_revision: 'toolbar' } }));
    await page.goto('/?workspace=files');
    const controls = page.locator('.context-controls');
    await controls.getByRole('button', { name: 'Filter', exact: true }).click();
    const setup = page.getByRole('dialog', { name: 'Filter' }).getByRole('button', { name: 'Set up', exact: true });
    await expect(setup).toBeVisible();
    await setup.click();
    await expect.poll(async () => (await exploreHistoryState(page)).workspace).toBe('settings');
    expect(await exploreHistoryState(page)).toMatchObject({ settingsCategory: 'search', settingsAuthority: 'visual_attachments' });
  });

  test('compact Files keeps visual search fields and their state in Filter', async ({ page }) => {
    await stubVisualFiles(page);
    await page.goto('/?workspace=files');
    const controls = page.locator('.context-controls');
    const filter = controls.getByRole('button', { name: 'Filter', exact: true });
    await expect(controls.getByRole('switch', { name: 'Visual search' })).toHaveCount(0);
    await filter.click();
    const dialog = page.getByRole('dialog', { name: 'Filter' });
    await dialog.getByRole('switch', { name: 'Visual search' }).click();
    const query = dialog.getByRole('searchbox', { name: 'Search attachment pixels' });
    await expect(query).toBeVisible();
    await expect(page.getByLabel('Query image')).toBeVisible();
    await expect(page.getByText('The query is sent to the configured visual embedding provider.')).toBeVisible();
    const visualControls = dialog.getByLabel('File filters');
    const queryField = query.locator('..');
    const controlsBox = await visualControls.boundingBox();
    const queryBox = await queryField.boundingBox();
    expect(queryBox!.width).toBeCloseTo(controlsBox!.width, 0);
    await expectOneRow(controls);
    await query.fill('blue sky');
    await expect(query).toHaveValue('blue sky');
    await filter.click();
    await expect(dialog).not.toBeVisible();
    await filter.click();
    await expect(query).toHaveValue('blue sky');
    await page.keyboard.press('Escape');
    await page.setViewportSize({ width: 1440, height: 900 });
    await expect(controls.getByRole('searchbox', { name: 'Search attachment pixels' })).toHaveValue('blue sky');
  });

  test('compact Clear filters clears visual text, drafts, and images', async ({ page }) => {
    await page.clock.install();
    const requests: Array<Record<string, unknown>> = [];
    await stubVisualFiles(page, requests);
    await page.goto('/?workspace=files');
    await page.getByRole('button', { name: 'Filter', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Filter' });
    await dialog.getByRole('switch', { name: 'Visual search' }).click();
    const query = dialog.getByRole('searchbox', { name: 'Search attachment pixels' });
    const clear = dialog.getByRole('button', { name: 'Clear filters' });
    await query.fill('blue sky');
    await page.clock.fastForward(600);
    await expect.poll(() => requests.at(-1)?.visual_query).toBe('blue sky');
    await clear.click();
    await expect(query).toHaveValue('');
    await expect.poll(() => requests.at(-1)?.visual_query).toBeUndefined();
    await query.fill('pending draft');
    await clear.click();
    await expect(query).toHaveValue('');
    await page.clock.fastForward(750);
    expect(requests.some(request => request.visual_query === 'pending draft')).toBe(false);
    await dialog.getByLabel('Query image').setInputFiles({ name: 'query.png', mimeType: 'image/png', buffer: Buffer.from('image') });
    await expect.poll(() => requests.at(-1)?.visual_image_base64).toBe(Buffer.from('image').toString('base64'));
    await clear.click();
    await expect(dialog.getByText('query.png', { exact: true })).toHaveCount(0);
    await expect.poll(() => requests.at(-1)?.visual_image_base64).toBeUndefined();
    await page.keyboard.press('Escape');
    await page.setViewportSize({ width: 1440, height: 900 });
    const desktopQuery = page.locator('.context-controls').getByRole('searchbox', { name: 'Search attachment pixels' });
    await desktopQuery.fill('desktop query');
    await page.getByRole('button', { name: 'Filter', exact: true }).click();
    await page.getByRole('dialog', { name: 'Filter' }).getByRole('button', { name: 'Clear filters' }).click();
    await expect(desktopQuery).toHaveValue('desktop query');
  });

  test('compact Filter indicates an active visual query or image', async ({ page }) => {
    const requests: Array<Record<string, unknown>> = [];
    await stubVisualFiles(page, requests);
    await page.goto('/?workspace=files');
    const filter = page.getByRole('button', { name: 'Filter', exact: true });
    await filter.click();
    const dialog = page.getByRole('dialog', { name: 'Filter' });
    const visual = dialog.getByRole('switch', { name: 'Visual search' });
    await visual.click();
    await dialog.getByRole('searchbox', { name: 'Search attachment pixels' }).fill('blue sky');
    await expect.poll(() => requests.at(-1)?.visual_query).toBe('blue sky');
    await filter.click();
    await expect(filter).toHaveText('Filter · 1');
    await expect(filter).toHaveAccessibleDescription('Visual search active');
    await filter.click();
    await visual.click();
    await filter.click();
    await expect(filter).toHaveText('Filter');
    await filter.click();
    await visual.click();
    await dialog.getByLabel('Query image').setInputFiles({ name: 'query.png', mimeType: 'image/png', buffer: Buffer.from('image') });
    await expect.poll(() => requests.at(-1)?.visual_image_base64).toBe(Buffer.from('image').toString('base64'));
    await filter.click();
    await expect(filter).toHaveText('Filter · 1');
    await filter.click();
    await dialog.getByRole('button', { name: 'Clear filters' }).click();
    await filter.click();
    await expect(filter).toHaveText('Filter');
  });

  test('compact grouping and sort shortcuts open View', async ({ page }) => {
    await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows, total_count: rows.length, cache_revision: 'toolbar' } }));
    await page.route('**/api/v1/files/search', route => route.fulfill({ json: { files: [fileRow(1, 'Presentation message 1')], total_count: 1234567, cache_revision: 'toolbar' } }));
    for (const [workspace, gridName] of [['everything', 'Messages results'], ['files', 'Files results']]) {
      await page.goto(`/?workspace=${workspace}`);
      const grid = page.getByRole('grid', { name: gridName });
      await expect(grid).toBeVisible();
      for (const key of ['g', 's']) {
        await grid.focus();
        await page.keyboard.press(key);
        await expect(page.getByRole('menu', { name: 'View' })).toBeVisible();
        await page.keyboard.press('Escape');
      }
    }
  });

  test('compact View keeps global shortcuts suspended while open', async ({ page }) => {
    await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows, total_count: rows.length, cache_revision: 'toolbar' } }));
    await page.goto('/?workspace=everything');
    await page.getByRole('button', { name: 'View', exact: true }).click();
    await page.keyboard.press('f');
    await expect(page.getByRole('menuitemradio', { name: 'Files', exact: true })).toBeFocused();
    await expect(page.getByRole('button', { name: 'Filter', exact: true })).toHaveAttribute('aria-expanded', 'false');
    await page.keyboard.press('/');
    await expect(page.getByRole('menu', { name: 'View' })).toBeVisible();
    await expect(page.getByRole('searchbox', { name: 'Search messages' })).not.toBeFocused();
    await page.keyboard.press('Escape');
    await expect(page.getByRole('button', { name: 'View', exact: true })).toBeFocused();
    await page.keyboard.press('f');
    await expect(page.getByRole('button', { name: 'Filter', exact: true })).toHaveAttribute('aria-expanded', 'true');
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: 'View', exact: true }).click();
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.getByRole('grid', { name: 'Messages results' }).focus();
    await page.keyboard.press('f');
    await expect(page.getByRole('button', { name: 'Filter', exact: true })).toHaveAttribute('aria-expanded', 'true');
    await page.keyboard.press('Escape');
  });

});

test('desktop Files keeps visual search and the three selects in the toolbar', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.route('**/api/v1/multimodal/status', route => route.fulfill({ json: { generation: { state: 'active', consented: true } } }));
  await page.route('**/api/v1/files/search', route => route.fulfill({ json: { files: [], total_count: 0, cache_revision: 'toolbar' } }));
  await page.goto('/?workspace=files');
  const controls = page.locator('.context-controls');
  await expect(controls.getByRole('switch', { name: 'Visual search' })).toBeVisible();
  await expect(controls.getByRole('combobox')).toHaveCount(3);
  await expect(controls.getByRole('combobox', { name: 'Show as: Files' })).toBeVisible();
});

test('Show as Files opens the Files workspace with the same context, and Back returns', async ({ page }) => {
  const filePredicates: Array<Record<string, unknown>> = [];
  const savedViews: Array<Record<string, unknown>> = [];
  await page.route('**/api/v1/files/search', async (route) => {
    const body = route.request().postDataJSON() as { predicate: Record<string, unknown> };
    filePredicates.push(body.predicate);
    await route.fulfill({ json: {
      files: [{ ...fileRow(7, rows[0]!.title), key: 'message:1:file:7', entry_key: 'message:1',
        message_id: 1, conversation_id: 1, filename: 'pasta-analysis.pdf' }],
      total_count: 1, cache_revision: 'presentation-cache', search_provenance: { lexical_index_revision: 'fts-1' }
    } });
  });
  await page.route('**/api/v1/files/7', (route) => route.fulfill({ json: {
    id: 7, message_id: 1, conversation_id: 1, filename: 'pasta-analysis.pdf',
    mime_type: 'application/pdf', size_bytes: 2048,
    content_state: 'missing_blob', content_available: false
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows, total_count: rows.length, cache_revision: 'presentation-cache',
    search_provenance: { lexical_index_revision: 'fts-1' }
  } }));
  await page.route('**/api/v1/explore/match-counts', (route) => route.fulfill({ json: {
    counts: [], cache_revision: 'presentation-cache', lexical_index_revision: 'fts-1',
    canonical_query_hash: 'pasta'
  } }));
  await page.route('**/api/v1/saved-views', async (route) => {
    if (route.request().method() === 'POST') {
      const request = route.request().postDataJSON() as Record<string, unknown>;
      const view = { id: 1, revision: 1, created_at: '2026-07-20T12:00:00Z',
        updated_at: '2026-07-20T12:00:00Z', ...request };
      savedViews.push(view);
      return route.fulfill({ json: view });
    }
    return route.fulfill({ json: { saved_views: savedViews } });
  });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', presentation: 'timeline' }))}`);
  const everything = page.getByRole('grid', { name: 'Messages results' });
  await expect(everything).toBeVisible();
  await page.getByRole('searchbox', { name: 'Search messages' }).fill('pasta');
  await page.getByRole('button', { name: 'Search', exact: true }).click();

  const timeline = page.getByRole('region', { name: 'Canonical activity timeline' });
  await expect(timeline).toBeVisible();
  await expect(timeline.getByText('Presentation message 1')).toBeVisible();
  expect(new URL(page.url()).search).toContain('timeline');

  await selectKitOption(page, 'Show as', 'Files');
  const filesMain = page.getByRole('main', { name: 'Files' });
  const files = page.getByRole('grid', { name: 'Files results' });
  await expect(filesMain).toBeVisible();
  await expect(files.getByText('pasta-analysis.pdf')).toBeVisible();
  await expect(page).toHaveURL(/[?&]workspace=files(&|$)/);
  await expect(page.getByRole('combobox', { name: 'Show as: Files' })).toBeVisible();
  await expect(filesMain.getByText('Full text: “pasta”')).toBeVisible();
  await expect(filesMain.getByText('1 file', { exact: true })).toBeVisible();
  expect(filePredicates.at(-1)).toMatchObject({ query: 'pasta', search_mode: 'full_text' });

  await page.goBack();
  await expect(timeline).toBeVisible();
  await expect(everything).toBeFocused();
  await expect(filesMain).toHaveCount(0);
  await expect(page).toHaveURL(/[?&]workspace=everything(&|$)/);
  await expect(page.getByRole('searchbox', { name: 'Search messages' })).toHaveValue('pasta');
  await page.goForward();
  await expect(files.getByText('pasta-analysis.pdf')).toBeVisible();

  await files.focus();
  await page.keyboard.press('Home');
  await page.keyboard.press('Enter');
  const viewer = page.getByRole('dialog', { name: 'View pasta-analysis.pdf' });
  await expect(viewer).toBeVisible();
  expect(await exploreHistoryState(page)).toMatchObject({ workspace: 'files', selectedRow: 'message:1:file:7' });
  await viewer.getByRole('button', { name: 'Close file viewer' }).click();
  await expect(viewer).not.toBeVisible();
  await expect(files).toBeFocused();
  expect((await exploreHistoryState(page)).selectedRow).toBeNull();

  await files.getByRole('button', { name: 'Open containing item Presentation message 1' }).click();
  await expect(page.getByRole('complementary', { name: 'Reading pane: Presentation message 1' })).toBeVisible();
  expect(await exploreHistoryState(page))
    .toMatchObject({ workspace: 'everything', presentation: 'table', selectedRow: 'message:1' });
  await page.goBack();
  await expect(files.getByText('pasta-analysis.pdf')).toBeVisible();
  await expect(files).toBeFocused();

  await filesMain.getByRole('button', { name: 'Save view…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Save view' });
  await expect(dialog.getByText('Filename, type, and file sort aren’t saved with the view.')).toBeVisible();
  await dialog.getByRole('textbox', { name: 'Name' }).fill('Pasta files');
  await dialog.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(savedViews[0]!.canonical_state).toMatchObject({ query: 'pasta', presentation: 'files' });

  await page.getByRole('button', { name: 'Pasta files', exact: true }).click();
  await expect(files.getByText('pasta-analysis.pdf')).toBeVisible();
  await expect(page).toHaveURL(/[?&]workspace=files(&|$)/);
  await expect(page.getByRole('searchbox', { name: 'Search messages' })).toHaveValue('pasta');
});

test('an old Everything-as-Files link opens Files at its deep anchor with bounded paging and a focused fallback', async ({ page }) => {
  const pageSize = 100;
  const total = 650;
  let requests = 0;
  await page.route('**/api/v1/files/search', async (route) => {
    requests += 1;
    const body = route.request().postDataJSON() as { cursor?: string };
    const pageIndex = body.cursor ? Number(body.cursor.replace('page-', '')) : 0;
    const start = pageIndex * pageSize + 1;
    const end = Math.min(total, start + pageSize - 1);
    await route.fulfill({ json: {
      files: Array.from({ length: end - start + 1 }, (_, offset) => fileRow(start + offset, `Containing item ${start + offset}`)),
      total_count: total, cache_revision: 'deep-files', search_provenance: {},
      ...(end < total ? { next_cursor: `page-${pageIndex + 1}` } : {})
    } });
  });

  // Everything-as-Files and Files share the daemon's `<entry>:file:<id>` row keys.
  const state = {
    schemaVersion: 2, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
    groupingChain: [], presentation: 'files', sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' }, fileFilenameQuery: '', fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
    activeRow: 'source:1:message:m550:file:550', selectedRow: null, inspectorPinned: false,
    conversationAnchor: null,
    scrollAnchor: { key: 'source:1:message:m540:file:540', offset: 5 }
  };
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Files results' });
  await expect(page.getByRole('main', { name: 'Files' })).toBeVisible();
  await expect(page).toHaveURL(/[?&]workspace=files(&|$)/);
  expect(await exploreHistoryState(page)).toMatchObject({ workspace: 'files', presentation: 'files' });
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-550');
  await expect(grid.getByText('deep-550.pdf')).toBeVisible();
  expect(await grid.getByRole('row').count()).toBeLessThan(80);
  expect(requests).toBe(6);
  await expect(grid).toBeFocused();

  // Reaching the last loaded row prefetches exactly one more page; the next
  // ArrowDown moves into it without another request.
  await page.keyboard.press('End');
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-600');
  await expect.poll(() => requests).toBe(7);
  await page.keyboard.press('ArrowDown');
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-601');
  await expect(grid.getByText('deep-601.pdf')).toBeVisible();
  expect(requests).toBe(7);
  expect(await grid.getByRole('row').count()).toBeLessThan(80);

  const missing = { ...state, activeRow: 'source:1:message:m999:file:999', scrollAnchor: null };
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(missing))}`);
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-1');
  await expect(grid.getByText('deep-1.pdf')).toBeVisible();
  await expect(grid).toBeFocused();
});

test('attachment deep links restore with one bounded metadata lookup', async ({ page }) => {
  let filePageRequests = 0;
  let metadataRequests = 0;
  await page.route('**/api/v1/files/search', (route) => {
    filePageRequests += 1;
    return route.fulfill({ json: {
      files: [fileRow(filePageRequests, `Containing item ${filePageRequests}`)],
      total_count: 10_000, next_cursor: `page-${filePageRequests}`,
      cache_revision: 'deep-file-viewer', search_provenance: {}
    } });
  });
  await page.route('**/api/v1/files/640', (route) => {
    metadataRequests += 1;
    return route.fulfill({ json: {
      id: 640, message_id: 640, conversation_id: 64, filename: 'deep-linked.pdf',
      mime_type: 'application/pdf', size_bytes: 4096,
      content_state: 'missing_blob', content_available: false
    } });
  });

  const state = {
    schemaVersion: 2, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
    groupingChain: [], presentation: 'files', sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' }, fileFilenameQuery: '', fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
    activeRow: null, selectedRow: 'attachment:640', inspectorPinned: false,
    conversationAnchor: null, scrollAnchor: null
  };
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);

  await expect(page.getByRole('dialog', { name: 'View deep-linked.pdf' })).toBeVisible();
  const grid = page.getByRole('grid', { name: 'Files results' });
  await expect(grid.getByText('deep-1.pdf')).toBeAttached();
  await expect(grid).toHaveAttribute('aria-busy', 'false');
  expect(filePageRequests).toBe(1);
  expect(metadataRequests).toBe(1);
});
