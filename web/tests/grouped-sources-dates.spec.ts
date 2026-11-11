import { expect, test } from '@playwright/test';
import { mockArchiveLayout } from './archive-layout';

test.use({ timezoneId: 'Europe/London', colorScheme: 'dark' });

test('source identities and local saved dates survive reload', async ({ page }) => {
  await mockArchiveLayout(page);
  let saved: { schema_version: number; [key: string]: unknown } | undefined;
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [
    { id: 1, source_type: 'slack', identifier: 'alpha.example', active_sync: null },
    { id: 2, source_type: 'slack', identifier: 'beta.example', active_sync: null }
  ] } }));
  await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: { rows: [
    { key: '1', label: 'alpha.example', count: 1, estimated_bytes: 1, latest_at: '2026-07-18T12:00:00Z' }
  ], total_count: 1, cache_revision: 'fixture', search_provenance: {} } }));
  await page.route('**/api/v1/saved-views', async route => {
    if (route.request().method() === 'POST') {
      const body = route.request().postDataJSON();
      saved = { ...body, id: 1, revision: 1, created_at: '2026-07-18T12:00:00Z', updated_at: '2026-07-18T12:00:00Z' };
      await route.fulfill({ status: 201, json: saved });
    } else await route.fulfill({ json: { saved_views: saved ? [saved] : [] } });
  });
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'everything', groupingChain: ['source'] })));
  await expect(page.getByRole('button', { name: 'Show items in Slack (alpha.example)' })).toBeVisible();
  await page.getByRole('button', { name: 'Show items in Slack (alpha.example)' }).click();
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'everything', filters: [
    { dimension: 'after', values: ['2026-07-01T00:00:00-04:00'] },
    { dimension: 'before', values: ['2026-08-01T00:00:00-04:00'] }
  ] })));
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await expect(page.getByLabel('From', { exact: true })).toHaveValue('2026-07-01');
  await page.getByLabel('From', { exact: true }).fill('2026-07-02');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByRole('button', { name: 'Save view…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Save view' });
  await dialog.getByLabel('Name').fill('Local dates');
  await dialog.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => saved?.schema_version).toBe(3);
  await expect(page.getByRole('button', { name: 'Local dates', exact: true })).toBeVisible();
  await page.reload();
  await page.getByRole('button', { name: 'Local dates', exact: true }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await expect(page.getByLabel('From', { exact: true })).toHaveValue('2026-07-02');
  expect(errors).toEqual([]);
});
