import { expect, test } from '@playwright/test';

const row = {
  key: 'source:1:message:m1', kind: 'message', message_type: 'email', conversation_type: 'email',
  title: 'Reviewed message', preview: 'Exact selection', occurred_at: '2026-07-19T10:00:00Z',
  source_id: 1, source_identifier: 'archive@example.com', source_type: 'gmail',
  participant_labels: ['Archive Owner'], participant_ids: [1], attachment_count: 1,
  attachment_size: 20, has_attachments: true, deleted_from_source: false,
  message_count: 1, match: {}
};

function syncRun(status: string, processed: number) {
  return {
    id: 9, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null,
    status, messages_processed: processed, messages_added: processed, messages_updated: 0,
    errors_count: 0, error_message: null, cursor_before: null, cursor_after: null
  };
}

test('archive management workspaces preserve reviewed authority and daemon job boundaries', async ({ page }) => {
  const preflights: unknown[] = [];
  const sourceRequests: Array<{ method: string; accept: string | null }> = [];
  let syncStarted = false;
  let sourceReads = 0;

  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/explore/preflight', (route) => {
    preflights.push(route.request().postDataJSON());
    return route.fulfill({ json: {
      count: 1, estimated_bytes: 20, cache_revision: 'cache-management', search_provenance: {},
      unavailable_actions: [{ action: 'open_in_source', reason: 'trusted_source_link_unavailable' }],
      action_targets: [{ action: 'export', message_id: 1, filename: 'message-1.eml' }],
      operation_token: 'operation-1', expires_at: '2026-07-19T10:05:00Z'
    } });
  });
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [row], total_count: 1, cache_revision: 'cache-management', search_provenance: {}
  } }));
  await page.route('**/api/v1/deletions', (route) => route.fulfill({ json: { manifests: [] } }));
  await page.route('**/api/v1/sources/status', (route) => {
    sourceRequests.push({ method: route.request().method(), accept: route.request().headers().accept ?? null });
    sourceReads += 1;
    const active = syncStarted && sourceReads === 2 ? syncRun('running', 5) : null;
    return route.fulfill({ json: { sources: [{
      id: 1, source_type: 'gmail', identifier: 'archive@example.com', display_name: 'Archive',
      last_sync_at: null, updated_at: '2026-07-19T10:00:00Z', active_sync: active,
      latest_sync: active, last_successful_sync: null, can_sync: !active,
      ...(active ? { sync_unavailable_reason: 'sync_already_running' } : {})
    }] } });
  });
  await page.route('**/api/v1/sync/archive%40example.com', (route) => {
    sourceRequests.push({ method: route.request().method(), accept: route.request().headers().accept ?? null });
    syncStarted = true;
    return route.fulfill({ status: 202, json: { status: 'accepted', message: 'started' } });
  });
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
    id: 7, name: 'Invoices', description: 'Canonical saved view', schema_version: 1, revision: 2,
    created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z',
    canonical_state: { query: 'invoice', search_mode: 'full_text', filters: [], grouping: [],
      presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'title'], inspector_pinned: false }
  }] } }));

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid.getByText('Reviewed message')).toBeVisible();
  await grid.focus();
  await page.keyboard.press('Space');
  await expect(page.getByRole('button', { name: 'Export selection' })).toBeVisible();
  await expect(page.getByText('Open in source: trusted_source_link_unavailable')).toBeVisible();
  await expect.poll(() => preflights[0]).toMatchObject({ selection: {
    mode: 'explicit', row_keys: ['source:1:message:m1'], cache_revision: 'cache-management'
  } });

  await page.keyboard.press('d');
  await expect(page.getByRole('main', { name: 'Deletions' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Confirm selected deletion' })).toBeVisible();
  await expect(page.getByText(/creates a staged manifest; it does not execute deletion/i)).toBeVisible();
  await page.getByRole('button', { name: 'Cancel' }).click();

  await page.getByRole('button', { name: 'Everything' }).click();
  await page.getByRole('button', { name: 'Select all 1 matching items' }).click();
  await page.keyboard.press('Shift+d');
  await expect(page.getByRole('dialog', { name: 'Confirm matching deletion' })).toBeVisible();
  await page.getByRole('button', { name: 'Cancel' }).click();

  await page.getByRole('button', { name: 'Sources' }).click();
  await page.getByRole('button', { name: 'Sync now Archive' }).click();
  await expect(page.getByText('5 processed')).toBeVisible();
  await expect.poll(() => sourceRequests.some((request) => request.method === 'POST')).toBe(true);
  expect(sourceRequests.every((request) => request.accept !== 'text/event-stream')).toBe(true);

  await page.getByRole('button', { name: 'Saved Views' }).click();
  await page.getByRole('button', { name: 'Open Invoices' }).click();
  await expect(page.getByRole('searchbox', { name: 'Search everything' })).toHaveValue('invoice');
  await expect(page.getByRole('grid', { name: 'Everything results' })).toBeFocused();
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').query).toBe('invoice');
});
