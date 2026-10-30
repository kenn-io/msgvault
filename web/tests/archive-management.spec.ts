import { stubOperationStatus } from './kit-ui';
import { expect, test } from '@playwright/test';

test.beforeEach(async ({ page }) => {
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

for (const workspace of ['Sources', 'Staged deletions']) {
  test(`${workspace} keeps long lists scrollable within the workspace`, async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 520 });
    await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
    await page.route('**/api/v1/carddav/connections', route => route.fulfill({ json: { connections: [] } }));
    await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: Array.from({ length: 30 }, (_, index) => ({
      id: index + 1, source_type: 'gmail', identifier: `archive-${index + 1}@example.com`, display_name: `Archive ${index + 1}`,
      last_sync_at: null, updated_at: row.occurred_at, active_sync: null, latest_sync: null, last_successful_sync: null,
      can_sync: false, scheduled: false, sync_unavailable_reason: 'sync_not_configured', next_sync_at: null
    })) } }));
    const batches = Array.from({ length: 30 }, (_, index) => ({
      id: `batch-${index + 1}`, status: 'pending', message_count: index + 1,
      created_at: new Date(Date.UTC(2026, 6, 19, 10, index)).toISOString(), created_by: 'api'
    }));
    await page.route('**/api/v1/deletions', route => route.fulfill({ json: { manifests: batches } }));
    await page.route('**/api/v1/deletions/batch-*', route => route.fulfill({ json: {
      ...batches.find(batch => route.request().url().endsWith(`/${batch.id}`)), account: 'archive@example.com'
    } }));
    await page.goto(`/?workspace=${workspace === 'Sources' ? 'sources' : 'deletions'}`);
    const table = page.getByRole('table', { name: workspace });
    await expect(table.getByRole('row')).toHaveCount(31);
    const lastRow = table.getByRole('row').last();
    const scroller = table.locator('..');
    await expect.poll(() => scroller.evaluate(element => element.scrollHeight - element.clientHeight)).toBeGreaterThan(0);
    await scroller.hover();
    await page.mouse.wheel(0, 5000);
    await expect(lastRow).toBeInViewport();
    if (workspace === 'Staged deletions') await expect(page.getByRole('button', { name: 'Copy msgvault delete-staged', exact: true })).toBeInViewport();
  });
}

function syncRun(status: string, processed: number) {
  return {
    id: 9, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null,
    status, messages_processed: processed, messages_added: processed, messages_updated: 0,
    errors_count: 0, error_message: null, cursor_before: null, cursor_after: null
  };
}

for (const failure of ['mail sync', 'contacts sync', 'contacts status']) {
  test(`Sources keeps ${failure} errors and recovery visible in the narrow panel`, async ({ page }) => {
    await page.setViewportSize({ width: 420, height: 860 });
    let reject = true, attempts = 0, sourceReads = 0;
    const status = { configured: true, available: true, credential_configured: true, enabled: false, scheduled: false, schedule: '',
      account: { base_url: 'https://contacts.example.com/', username: 'contacts@example.com' }, active: null, latest: null, latest_successful: null };
    await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
    await page.route('**/api/v1/settings', route => route.fulfill({ json: { settings: [] } }));
    await page.route('**/api/v1/sources/status', route => {
      sourceReads += 1;
      const active = failure === 'mail sync' && attempts === 2 ? syncRun('running', 5) : null;
      return route.fulfill({ json: { sources: [{ id: 1, source_type: 'gmail', identifier: 'archive@example.com', display_name: 'Archive',
        last_sync_at: null, updated_at: row.occurred_at, active_sync: active, latest_sync: active, last_successful_sync: null,
        can_sync: !active, scheduled: false, next_sync_at: null }] } });
    });
    await page.route('**/api/v1/carddav/connections', route => route.fulfill({ json: { connections: [{ connection: 'default', orphaned: false, status }] } }));
    await page.route('**/api/v1/carddav/status**', route => route.fulfill(failure === 'contacts status' && reject ? { status: 500, json: {} } : { json: status }));
    await page.route('**/api/v1/carddav/books**', route => route.fulfill({ json: { books: [] } }));
    await page.route('**/api/v1/carddav/runs**', route => route.fulfill({ json: { runs: [] } }));
    await page.route('**/api/v1/carddav/sync', route => {
      attempts += 1;
      return route.fulfill(reject ? { status: 400, json: {} } : { json: { status: 'succeeded' } });
    });
    await page.route('**/api/v1/sync/archive%40example.com**', route => {
      attempts += 1;
      return route.fulfill(reject ? { status: 500, json: { message: 'Source sync rejected' } } : { status: 202, json: { status: 'accepted' } });
    });
    await page.goto('/?workspace=sources');
    const label = failure === 'mail sync' ? 'Archive' : 'contacts@example.com';
    await page.getByRole('button', { name: `Details for ${label}`, exact: true }).click();
    const panel = page.getByRole('dialog', { name: label, exact: true });
    if (failure !== 'contacts status') await panel.getByRole('button', { name: 'Sync now', exact: true }).click();
    const message = failure === 'mail sync' ? 'Source sync rejected' : failure === 'contacts sync' ? 'Unable to start CardDAV sync.' : 'Unable to load CardDAV status.';
    const alert = panel.getByRole('alert').filter({ hasText: message });
    await expect(alert).toBeInViewport();
    reject = false;
    if (failure === 'contacts status') {
      await panel.getByRole('button', { name: 'Retry status', exact: true }).click();
      await expect(alert).toHaveCount(0);
      await expect(panel.getByRole('button', { name: 'Sync now', exact: true })).toBeEnabled();
    } else {
      if (failure === 'mail sync') {
        const before = sourceReads;
        await panel.getByRole('button', { name: 'Refresh', exact: true }).click();
        await expect.poll(() => sourceReads).toBeGreaterThan(before);
      }
      await panel.getByRole('button', { name: 'Sync now', exact: true }).click();
      await expect.poll(() => attempts).toBe(2);
      await expect(alert).toHaveCount(0);
      if (failure === 'mail sync') await expect(panel.getByText('Running', { exact: true }).first()).toBeInViewport();
      else await expect(panel.getByRole('button', { name: 'Sync now', exact: true })).toBeEnabled();
    }
  });
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
  await page.getByRole('button', { name: 'Stage 1 message', exact: true }).click();
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
