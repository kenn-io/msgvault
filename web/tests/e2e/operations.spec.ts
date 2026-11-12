import { expect, test, type Page } from '@playwright/test';
import { installOperations } from './fixtures/operations';

const historyURL = (state: Record<string, unknown> = {}) => `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true, ...state }))}`;

async function openHistory(page: Page, path = '/') {
  await page.goto(path);
  await page.getByRole('button', { name: /Sync status:/ }).click();
  await page.getByRole('menuitem', { name: 'Past runs' }).click();
  await expect(page.getByRole('dialog', { name: 'Past runs' })).toBeVisible();
}

test('Past runs offers advertised actions and announces completion', async ({ page }) => {
  await installOperations(page);
  await openHistory(page);
  await page.getByRole('button', { name: 'Open CardDAV sync run' }).click();
  const detail = page.getByRole('region', { name: 'Operation run detail' });
  const action = detail.getByRole('button', { name: 'Start CardDAV sync' });
  await expect(action).toBeVisible();
  await expect(detail.getByRole('button', { name: /visual index/i })).toHaveCount(0);
  await action.click();
  await expect(page.getByRole('status', { name: 'Operation status' })).toContainText('CardDAV sync request completed');
  await page.getByRole('button', { name: 'All runs' }).click();
  await page.getByRole('button', { name: 'Open Visual embedding run' }).click();
  await expect(detail.getByRole('button', { name: 'Resume visual index' })).toBeVisible();
  await expect(detail.getByRole('button', { name: 'Build visual index' })).toHaveCount(0);
  await expect(detail.getByRole('button', { name: 'Start CardDAV sync' })).toHaveCount(0);
});

test('Past runs keeps focus through history and related status, and preserves Messages', async ({ page }) => {
  await installOperations(page);
  await openHistory(page, '/?workspace=sources');
  const sourceRow = page.getByRole('button', { name: 'Open Visual embedding run' });
  for (let index = 0; index < 120 && !(await sourceRow.evaluate(element => document.activeElement === element)); index += 1) await page.keyboard.press('Tab');
  await expect(sourceRow).toBeFocused();
  await sourceRow.press('Enter');
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.goBack();
  await expect(sourceRow).toBeFocused();
  await page.goForward();
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.keyboard.press('Tab');
  await page.keyboard.press('Shift+Tab');
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(sourceRow).toBeFocused();
  await page.keyboard.press('Enter');
  const link = page.getByRole('button', { name: 'Open Visual attachment status' });
  await link.click();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(link).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(page.getByRole('button', { name: 'Sources', exact: true })).toBeFocused();

  let exploreRequests = 0;
  await page.route('**/api/v1/explore', route => {
    exploreRequests += 1;
    return route.fulfill({ json: {
      rows: [1, 2].map(index => ({
        key: `message:${index}`, kind: 'message', message_type: 'email', conversation_type: 'email',
        title: `Synthetic subject ${index}`, preview: `Synthetic excerpt ${index}`,
        occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_identifier: 'archive@example.com',
        source_type: 'synthetic', participant_labels: ['Example Person'], participant_ids: [1],
        attachment_count: 0, attachment_size: 0, has_attachments: false, deleted_from_source: false,
        message_count: 1, match: {}
      })), total_count: 2, cache_revision: 'operations-messages', search_provenance: {}
    } });
  });
  await openHistory(page);
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();
  await expect(grid.getByText('Synthetic subject 2')).toBeVisible();
  expect(exploreRequests).toBe(1);
  const row = page.getByRole('button', { name: 'Open Visual embedding run' });
  await row.click();
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await expect(page.getByRole('dialog')).toHaveAccessibleName('Past runs');
  await expect(page.getByRole('region', { name: 'Operation run detail' }).getByText('Visual embedding', { exact: true })).toBeVisible();
  await page.goBack();
  await expect(row).toBeFocused();
  expect(exploreRequests).toBe(1);
  await page.goForward();
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  expect(exploreRequests).toBe(1);
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();
  expect(exploreRequests).toBe(1);
});

test('unconfigured visual status opens Search & AI settings', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.setOperationConfigured('visual_embedding', false);
  await page.goto(historyURL({ operationStatus: 'getVisualAttachmentStatus' }));
  const configure = page.getByRole('button', { name: 'Open visual attachment settings' });
  await expect(configure).toBeVisible();
  await configure.click();
  const settings = page.getByRole('main', { name: 'Settings' });
  await expect(settings).toBeVisible();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search & AI' })).toBeVisible();
});

test('Refresh waits for completion and returns focus to its button', async ({ page }) => {
  const fixture = await installOperations(page);
  await openHistory(page);
  const refresh = page.getByRole('button', { name: 'Refresh', exact: true });
  const release = fixture.holdNextOperationStatus();
  const reads = fixture.operationStatusReads;
  await refresh.click();
  await expect.poll(() => fixture.operationStatusReads).toBeGreaterThan(reads);
  await expect(refresh).toBeDisabled();
  release();
  await expect(refresh).toBeEnabled();
  await expect(refresh).toBeFocused();
});
