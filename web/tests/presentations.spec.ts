import { expect, test } from '@playwright/test';

const rows = [1, 2].map((id) => ({
  key: `message:${id}`, kind: 'message', message_type: 'email', conversation_type: 'email_thread',
  title: `Presentation message ${id}`, preview: `Pasta analysis ${id}`,
  occurred_at: `2026-07-${20 - id}T12:00:00Z`, source_id: 1,
  source_identifier: 'archive@example.com', source_type: 'synthetic',
  participant_labels: ['Example Person'], participant_ids: [1], attachment_count: 1,
  attachment_size: 2048, has_attachments: true, deleted_from_source: false,
  message_count: 1, anchor_message_id: id, conversation_id: id, match: {}
}));

test('Show as preserves analytical meaning, keyboard focus, history, and Saved Views', async ({ page }) => {
  const predicates: Array<Record<string, unknown>> = [];
  const savedViews: Array<Record<string, unknown>> = [];
  await page.route('**/api/session', (route) => route.fulfill({ json: {
    auth_mode: 'session', csrf_token: 'csrf', https: true, plain_http_warning: false
  } }));
  await page.route('**/api/v1/explore/files', async (route) => {
    const body = route.request().postDataJSON() as { predicate: Record<string, unknown> };
    predicates.push(body.predicate);
    await route.fulfill({ json: {
      files: [{ id: 7, key: 'message:1:file:7', entry_key: 'message:1', message_id: 1, conversation_id: 1,
        occurred_at: rows[0]!.occurred_at, source_id: 1,
        source_identifier: 'archive@example.com', title: rows[0]!.title,
        filename: 'pasta-analysis.pdf', mime_type: 'application/pdf', size: 2048 }],
      total_count: 1, cache_revision: 'presentation-cache', search_provenance: { lexical_index_revision: 'fts-1' }
    } });
  });
  await page.route('**/api/v1/files/7', (route) => route.fulfill({ json: {
    id: 7, message_id: 1, conversation_id: 1, filename: 'pasta-analysis.pdf',
    mime_type: 'application/pdf', size_bytes: 2048,
    content_state: 'missing_blob', content_available: false
  } }));
  await page.route('**/api/v1/explore', async (route) => {
    const body = route.request().postDataJSON() as Record<string, unknown>;
    predicates.push(body);
    await route.fulfill({ json: {
      rows, total_count: rows.length, cache_revision: 'presentation-cache',
      search_provenance: { lexical_index_revision: 'fts-1' }
    } });
  });
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

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await expect(page.getByRole('grid', { name: 'Everything results' })).toBeVisible();
  await page.getByRole('searchbox', { name: 'Search everything' }).fill('pasta');
  await page.getByRole('button', { name: 'Search', exact: true }).click();

  const showAs = page.getByRole('combobox', { name: 'Show as' });
  await showAs.selectOption('timeline');
  const timeline = page.getByRole('region', { name: 'Canonical activity timeline' });
  await expect(timeline).toBeVisible();
  await expect(timeline.getByText('Presentation message 1')).toBeVisible();
  expect(new URL(page.url()).search).toContain('timeline');

  await showAs.selectOption('files');
  const files = page.getByRole('grid', { name: 'Files in current context' });
  await expect(files).toBeVisible();
  await expect(files.getByText('pasta-analysis.pdf')).toBeVisible();
  expect(predicates.at(-1)).toMatchObject({ query: 'pasta', search_mode: 'full_text', presentation: 'files' });

  await files.focus();
  await page.keyboard.press('Home');
  await page.keyboard.press('Enter');
  const viewer = page.getByRole('dialog', { name: 'View pasta-analysis.pdf' });
  await expect(viewer).toBeVisible();
  expect(JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}'))
    .toMatchObject({ selectedRow: 'attachment:7', activeRow: 'message:1:file:7' });

  await page.goBack();
  await expect(viewer).not.toBeVisible();
  await expect(files).toBeFocused();
  await page.goForward();
  await expect(viewer).toBeVisible();
  await page.reload();
  await expect(viewer).toBeVisible();

  await viewer.getByRole('button', { name: 'Close file viewer' }).click();
  await expect(viewer).not.toBeVisible();
  await expect(files).toBeFocused();
  expect(JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').selectedRow).toBeNull();

  await files.focus();
  await page.keyboard.press('Enter');
  await expect(viewer).toBeVisible();
  await viewer.getByRole('button', { name: 'Open containing item' }).click();
  await expect(page.getByRole('complementary', { name: 'Inspect Presentation message 1' })).toBeVisible();
  const tableState = JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}');
  expect(tableState).toMatchObject({ presentation: 'table', activeRow: 'message:1', scrollAnchor: null });
  expect(tableState.activeRow).not.toContain(':file:');

  await page.goBack();
  await expect(files).toBeVisible();
  await expect(files).toBeFocused();
  await page.goBack();
  await expect(timeline).toBeVisible();
  await expect(page.getByRole('grid', { name: 'Everything results' })).toBeFocused();
  await page.goForward();
  await expect(files).toBeVisible();
  await expect(files).toBeFocused();

  await page.getByRole('button', { name: 'Saved Views', exact: true }).click();
  await page.getByLabel('Name').fill('Pasta files');
  await page.getByRole('button', { name: 'Save current view' }).click();
  expect((savedViews[0]!.canonical_state as Record<string, unknown>).presentation).toBe('files');
  await page.getByRole('button', { name: 'Open Pasta files' }).click();
  await expect(files).toBeVisible();
  await expect(files.getByText('pasta-analysis.pdf')).toBeVisible();
  expect(new URL(page.url()).search).toContain('files');
});

test('Files presentation restores a deep paged anchor with bounded DOM and visible fallback', async ({ page }) => {
  const pageSize = 100;
  const total = 650;
  let requests = 0;
  await page.route('**/api/session', (route) => route.fulfill({ json: {
    auth_mode: 'loopback', https: false, plain_http_warning: false
  } }));
  await page.route('**/api/v1/explore/files', async (route) => {
    requests += 1;
    const body = route.request().postDataJSON() as { cursor?: string };
    const pageIndex = body.cursor ? Number(body.cursor.replace('page-', '')) : 0;
    const start = pageIndex * pageSize + 1;
    const end = Math.min(total, start + pageSize - 1);
    await route.fulfill({ json: {
      files: Array.from({ length: end - start + 1 }, (_, offset) => {
        const id = start + offset;
        return {
          id, key: `message:${id}:file:${id}`, entry_key: `message:${id}`,
          message_id: id, conversation_id: id, occurred_at: '2026-07-18T12:00:00Z',
          source_id: 1, source_identifier: 'archive@example.com',
          title: `Containing item ${id}`, filename: `deep-${id}.pdf`,
          mime_type: 'application/pdf', size: 2048
        };
      }),
      total_count: total, cache_revision: 'deep-files', search_provenance: {},
      ...(end < total ? { next_cursor: `page-${pageIndex + 1}` } : {})
    } });
  });

  const state = {
    schemaVersion: 2, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
    groupingChain: [], presentation: 'files', sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' }, fileFilenameQuery: '', fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
    activeRow: 'message:550:file:550', selectedRow: null, inspectorPinned: false,
    inspectorWidth: 380, conversationAnchor: null,
    scrollAnchor: { key: 'message:540:file:540', offset: 5 }
  };
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Files in current context' });
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-550/);
  await expect(grid.getByText('deep-550.pdf')).toBeVisible();
  expect(await grid.getByRole('row').count()).toBeLessThan(80);
  expect(requests).toBe(6);

  await grid.focus();
  await page.keyboard.press('End');
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-600/);
  expect(requests).toBe(6);

  await page.keyboard.press('ArrowDown');
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-601/);
  expect(requests).toBe(7);
  expect(await grid.getByRole('row').count()).toBeLessThan(80);

  const invalid = { ...state, activeRow: 'message:999:file:999', scrollAnchor: null };
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(invalid))}`);
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1/);
  await expect(grid.getByText('deep-1.pdf')).toBeVisible();
  await expect(grid).toBeFocused();
});

test('attachment deep links restore with one bounded metadata lookup', async ({ page }) => {
  let filePageRequests = 0;
  let metadataRequests = 0;
  await page.route('**/api/session', (route) => route.fulfill({ json: {
    auth_mode: 'loopback', https: false, plain_http_warning: false
  } }));
  await page.route('**/api/v1/explore/files', (route) => {
    filePageRequests += 1;
    return route.fulfill({ json: {
      files: [], total_count: 10_000, next_cursor: 'page-1',
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
    inspectorWidth: 380, conversationAnchor: null, scrollAnchor: null
  };
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);

  await expect(page.getByRole('dialog', { name: 'View deep-linked.pdf' })).toBeVisible();
  expect(filePageRequests).toBe(1);
  expect(metadataRequests).toBe(1);
});
