import { stubOperationStatus } from './kit-ui';
import { expect, test } from '@playwright/test';

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/settings', route => route.fulfill({ json: { settings: [], pending_restart: false } }));
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [] } }));
  await stubOperationStatus(page);
});

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
  let staged = false;
  const sourceRequests: Array<{ method: string; accept: string | null }> = [];
  let syncStarted = false;

  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/explore/preflight', (route) => {
    preflights.push(route.request().postDataJSON());
    return route.fulfill({ json: {
      count: 1, deletable_count: 1, estimated_bytes: 20, cache_revision: 'cache-management', search_provenance: {},
      unavailable_actions: [{ action: 'open_in_source', reason: 'trusted_source_link_unavailable' }],
      action_targets: [{ action: 'export', message_id: 1, filename: 'message-1.eml' }],
      operation_token: 'operation-1', expires_at: '2026-07-19T10:05:00Z'
    } });
  });
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [row], total_count: 1, cache_revision: 'cache-management', search_provenance: {}
  } }));
  const batch = { id: 'batch-1', status: 'pending', message_count: 1, created_at: '2026-07-19T10:00:00Z', created_by: 'api', description: 'Reviewed selection' };
  await page.route('**/api/v1/deletions', route => {
    if (route.request().method() === 'POST') {
      const body = route.request().postDataJSON();
      if (body.dry_run) return route.fulfill({ json: { dry_run: true, message_count: 1 } });
      staged = true;
      return route.fulfill({ status: 201, json: { id: 'batch-1', message_count: 1 } });
    }
    return route.fulfill({ json: { manifests: staged ? [batch] : [] } });
  });
  await page.route('**/api/v1/deletions/batch-1', route => {

    return route.fulfill({ json: { ...batch, account: 'archive@example.com' } });
  });
  await page.route('**/api/v1/sources/status', (route) => {
    sourceRequests.push({ method: route.request().method(), accept: route.request().headers().accept ?? null });
    const active = syncStarted ? syncRun('running', 5) : null;
    return route.fulfill({ json: { sources: [{
      id: 1, source_type: 'gmail', identifier: 'archive@example.com', display_name: 'Archive',
      last_sync_at: null, updated_at: '2026-07-19T10:00:00Z', active_sync: active,
      latest_sync: active, last_successful_sync: null, can_sync: !active,
      ...(active ? { sync_unavailable_reason: 'sync_already_running' } : {})
    }] } });
  });
  await page.route('**/api/v1/sync/archive%40example.com**', (route) => {
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
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(grid.getByText('Reviewed message')).toBeVisible();
  await grid.focus();
  await page.keyboard.press('Space');
  await expect(page.getByRole('status', { name: 'Selection status' })).toHaveText('1 selected');
  await expect(page.getByRole('button', { name: 'Export selection' })).toBeVisible();
  await page.getByRole('button', { name: 'More selection actions' }).click();
  await expect(page.getByRole('menuitem', { name: 'Open selection in source' })).toBeDisabled();
  await expect(page.getByText('Your sources don’t provide links to open these items.')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menuitem', { name: 'Open selection in source' })).toHaveCount(0);
  await grid.focus();
  await expect.poll(() => preflights[0]).toMatchObject({ selection: {
    mode: 'explicit', row_keys: ['source:1:message:m1'], cache_revision: 'cache-management'
  } });

  await page.keyboard.press('d');
  await expect(page.getByRole('main', { name: 'Messages' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Review deletion' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Stage 1 message' })).toBeVisible();
  await page.keyboard.press('Escape');

  await page.getByRole('button', { name: 'Select all 1 matching items' }).click();
  await page.keyboard.press('Shift+d');
  await expect(page.getByRole('dialog', { name: 'Review deletion' })).toBeVisible();
  await expect(page.getByRole('status', { name: 'Selection status' })).toHaveText('All 1 matching items selected');
  await page.keyboard.press('Escape');

  await page.getByRole('button', { name: 'Review deletion…' }).click();
  await expect(page.getByRole('dialog', { name: 'Review deletion' })).toBeVisible();
  await page.getByRole('button', { name: 'Dry run', exact: true }).click();
  await page.getByRole('button', { name: 'Stage 1 message', exact: true }).click();
  await expect(page.getByText('Batch ID:')).toBeVisible();
  await page.getByRole('button', { name: 'Done', exact: true }).click();
  await expect(page.getByRole('dialog', { name: 'Review deletion' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Staged deletions', exact: true }).click();
  await expect(page.getByRole('table', { name: 'Staged deletions' })).toBeVisible();

  await page.getByRole('button', { name: 'Sources' }).click();
  await page.getByRole('button', { name: 'Sync now Archive' }).click();
  await expect(page.getByText('5 processed')).toBeVisible();
  await expect.poll(() => sourceRequests.some((request) => request.method === 'POST')).toBe(true);
  expect(sourceRequests.every((request) => request.accept !== 'text/event-stream')).toBe(true);

  await page.getByRole('button', { name: 'Invoices' }).click();
  await expect(page.getByRole('searchbox', { name: 'Search messages' })).toHaveValue('invoice');
  await expect(page.getByRole('grid', { name: 'Messages results' })).toBeFocused();
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').query).toBe('invoice');
});

test('Sources opens a read-only panel and routes to its existing settings editor', async ({ page }) => {
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback' } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} } }));
  await page.route('**/api/v1/deletions', route => route.fulfill({ json: { manifests: [] } }));
  await page.route('**/api/v1/settings', route => route.fulfill({ json: { pending_restart: false, settings: [] } }));
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [{
    id: 1, source_type: 'beeper', identifier: 'beeper', display_name: 'Example chats', can_sync: true, latest_sync: null, last_successful_sync: null
  }] } }));
  await page.route('**/api/v1/carddav/connections', route => route.fulfill({ json: { connections: [] } }));
  await page.goto('/?workspace=sources');
  await page.getByRole('button', { name: 'Details for Example chats' }).click();
  await expect(page.getByRole('region', { name: 'Schedule' })).toBeVisible();
  await page.getByRole('button', { name: 'Edit in Settings › Sources' }).click();
  await expect(page.getByRole('heading', { name: 'Sources', exact: true })).toBeVisible();
});
