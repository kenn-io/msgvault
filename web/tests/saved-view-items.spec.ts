import { expect, test } from '@playwright/test';
import { selectWorkspace, stubOperationStatus } from './kit-ui';

// Synthetic wire fixture: store/API integration tests verify membership itself.
test('opens live Saved View items and saves only canonical state', async ({ page }) => {
  await stubOperationStatus(page);
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'session', csrf_token: 'synthetic-csrf', https: true, plain_http_warning: false } }));
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/v1/cli/accounts', route => route.fulfill({ json: { accounts: [] } }));
  const view = { id: 7, name: 'Review queue', canonical_state: {}, schema_version: 2, revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' };
  const items = { include: ['a', 'b'].map((id, i) => ({ entry_key: `source:1:message:${id}`, message_id: i + 1, source_id: 1, added_by: 'owner', kind: 'message' })), exclude: [{ entry_key: 'source:1:message:c', message_id: 3, source_id: 1, added_by: 'owner', kind: 'message' }] };
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [view, { ...view, id: 8, name: 'Other queue' }] } }));
  let patch: Record<string, unknown> | undefined;
  let revision = 1;
  let gets = 0;
  let liveItems = items;
  await page.route('**/api/v1/saved-views/7', route => {
    if (route.request().method() === 'PATCH') { patch = route.request().postDataJSON(); return route.fulfill({ json: { ...view, revision: ++revision, items: liveItems } }); }
    gets += 1;
    return route.fulfill({ json: { ...view, revision, items: liveItems, include_count: 2, exclude_count: 1 } });
  });
  await page.route('**/api/v1/saved-views/8', route => route.fulfill({ json: { ...view, id: 8, name: 'Other queue', items: { include: [], exclude: [] } } }));
  const predicates: Array<Record<string, unknown>> = [];
  await page.route('**/api/v1/explore', route => { predicates.push(route.request().postDataJSON()); return route.fulfill({ json: { rows: ['a', 'b'].map((id, index) => ({
    key: `source:1:message:${id}`, kind: 'email', anchor_message_id: index + 1,
    source_id: 1, source_type: 'gmail', source_identifier: 'review@example.test',
    message_type: 'email', conversation_type: 'email',
    title: ['Review release notes', 'Confirm documentation update'][index],
    preview: 'Synthetic pinned review item', occurred_at: '2026-11-01T12:00:00Z',
    inbox_state: 'inbox', read_state: 'unread', match: {}, message_count: 1,
    has_attachments: false, attachment_count: 0, attachment_size: 0,
    deleted_from_source: false, matched_sender_identities: [], matched_recipient_identities: []
  })), total_count: 2, cache_revision: 'synthetic', search_provenance: {} } }); });
  await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: { rows: [], total_count: 0 } }));
  await page.goto('/?workspace=everything');
  await selectWorkspace(page, 'Review queue');
  await expect(page.getByText('2 pinned · 1 hidden', { exact: true })).toBeVisible();
  await expect.poll(() => predicates.at(-1)?.include_keys).toEqual(['source:1:message:a', 'source:1:message:b']);
  await expect.poll(() => predicates.at(-1)?.exclude_keys).toEqual(['source:1:message:c']);
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await expect.poll(() => patch).toEqual({ canonical_state: { filters: [], grouping: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }], columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'] } });
  const search = page.getByRole('searchbox', { name: 'Search messages', exact: true });
  await search.fill('review');
  await search.press('Enter');
  await selectWorkspace(page, 'Other queue');
  await expect(page.getByText('2 pinned · 1 hidden', { exact: true })).toHaveCount(0);
  // A source merge while this entry is in history changes live keys and ETag.
  liveItems = {
    include: items.include.map(item => ({ ...item, source_id: 2, entry_key: item.entry_key.replace('source:1:', 'source:2:') })),
    exclude: items.exclude.map(item => ({ ...item, source_id: 2, entry_key: item.entry_key.replace('source:1:', 'source:2:') })),
  };
  revision = 3;
  await page.goBack();
  await expect(page.getByText('2 pinned · 1 hidden', { exact: true })).toBeVisible();
  await expect.poll(() => predicates.at(-1)?.include_keys).toEqual(['source:2:message:a', 'source:2:message:b']);
  await expect.poll(() => predicates.at(-1)?.exclude_keys).toEqual(['source:2:message:c']);
  await expect(search).toHaveValue('review');
  patch = undefined;
  const request = page.waitForRequest(req => req.method() === 'PATCH' && req.url().endsWith('/saved-views/7'));
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  expect((await request).headers()['if-match']).toBe('"saved-view-7-r3"');
  await expect.poll(() => (patch?.canonical_state as { query?: string } | undefined)?.query).toBe('review');
  const beforeReload = gets;
  await page.reload();
  await expect.poll(() => gets).toBeGreaterThan(beforeReload);
  await expect(search).toHaveValue('review');
  await expect.poll(() => predicates.at(-1)?.exclude_keys).toEqual(['source:2:message:c']);

  await expect(page.getByText('Review release notes', { exact: true })).toBeVisible();
  await expect(page.getByText('Confirm documentation update', { exact: true })).toBeVisible();
  await page.screenshot({ path: 'test-results/saved-view-items.png', fullPage: true });
});
