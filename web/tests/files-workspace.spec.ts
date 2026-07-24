import { expect, test } from '@playwright/test';

function row(id: number) {
  return {
    id,
    key: `file:${id}`,
    entry_key: `message:${id}`,
    message_id: id,
    conversation_id: id,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_type: 'synthetic',
    source_identifier: 'archive@example.com',
    containing_title: `Containing item ${id}`,
    filename: `file-${id}.pdf`,
    mime_type: 'application/pdf',
    mime_family: 'pdf',
    size_bytes: id,
    content_state: 'missing_blob',
    content_available: false
  };
}

test('Files restores a deep URL through bounded 500-row pages with bounded DOM', async ({ page }) => {
  const total = 50_000;
  const target = 1_200;
  const requests: Array<{ cursor?: string; limit?: number }> = [];
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/files/search', async (route) => {
    const body = route.request().postDataJSON() as { cursor?: string; limit?: number };
    requests.push(body);
    const offset = body.cursor ? Number(body.cursor.slice('files:'.length)) : 0;
    const count = Math.min(500, total - offset);
    await route.fulfill({ json: {
      files: Array.from({ length: count }, (_, index) => row(offset + index + 1)),
      total_count: total,
      cache_revision: 'cache-files-deep',
      search_provenance: {},
      ...(offset + count < total ? { next_cursor: `files:${offset + count}` } : {})
    } });
  });
  await page.route(`**/api/v1/files/${target}`, (route) => route.fulfill({ json: {
    id: target,
    message_id: target,
    conversation_id: target,
    filename: `file-${target}.pdf`,
    mime_type: 'application/pdf',
    size_bytes: target,
    content_state: 'missing_blob',
    content_available: false
  } }));
  const state = {
    schemaVersion: 2,
    workspace: 'files',
    query: '',
    searchMode: 'full_text',
    filters: [],
    groupingChain: [],
    presentation: 'table',
    sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' },
    fileFilenameQuery: '',
    fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'],
    columnWidths: {},
    activeRow: `file:${target}`,
    selectedRow: `file:${target}`,
    inspectorPinned: false,
    conversationAnchor: null,
    scrollAnchor: null
  };

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  await expect(page.getByRole('dialog', { name: `View file-${target}.pdf` })).toBeVisible();
  expect(requests).toHaveLength(3);
  expect(requests.every(({ limit }) => (limit ?? 0) <= 500)).toBe(true);
  expect(await page.getByRole('grid', { name: 'Files results' }).locator('[role="row"]').count()).toBeLessThan(80);
});
