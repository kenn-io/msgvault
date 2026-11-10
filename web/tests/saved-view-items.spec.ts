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
  await page.route('**/api/v1/saved-views/7', route => {
    if (route.request().method() === 'PATCH') { patch = route.request().postDataJSON(); return route.fulfill({ json: { ...view, revision: 2, items } }); }
    return route.fulfill({ json: { ...view, items, include_count: 2, exclude_count: 1 } });
  });
  await page.route('**/api/v1/saved-views/8', route => route.fulfill({ json: { ...view, id: 8, name: 'Other queue', items: { include: [], exclude: [] } } }));
  const predicates: Array<Record<string, unknown>> = [];
  await page.route('**/api/v1/explore', route => { predicates.push(route.request().postDataJSON()); return route.fulfill({ json: { rows: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} } }); });
  await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: { rows: [], total_count: 0 } }));
  await page.goto('/?workspace=everything');
  await selectWorkspace(page, 'Review queue');
  await expect(page.getByText('2 pinned · 1 hidden', { exact: true })).toBeVisible();
  await expect.poll(() => predicates.at(-1)?.include_keys).toEqual(['source:1:message:a', 'source:1:message:b']);
  await expect.poll(() => predicates.at(-1)?.exclude_keys).toEqual(['source:1:message:c']);
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await expect.poll(() => patch).toEqual({ canonical_state: { filters: [], grouping: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }], columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'] } });
  await selectWorkspace(page, 'Other queue');
  await expect(page.getByText('2 pinned · 1 hidden', { exact: true })).toHaveCount(0);
  await page.goBack();
  await expect(page.getByText('2 pinned · 1 hidden', { exact: true })).toBeVisible();
  await expect.poll(() => predicates.at(-1)?.include_keys).toEqual(['source:1:message:a', 'source:1:message:b']);
  patch = undefined;
  const request = page.waitForRequest(req => req.method() === 'PATCH' && req.url().endsWith('/saved-views/7'));
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  expect((await request).headers()['if-match']).toContain('2');
  await expect.poll(() => patch?.canonical_state).toBeDefined();
  await page.screenshot({ path: 'test-results/saved-view-items.png', fullPage: true });
});
