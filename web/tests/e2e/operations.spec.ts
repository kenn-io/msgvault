import { expect, test, type Page } from '@playwright/test';
import { installOperations } from './fixtures/operations';
import { readFileSync } from 'node:fs';
import type { OperationRunsResponse, OperationRunSummary } from '../../src/lib/api/generated/models';

const historyURL = (state: Record<string, unknown> = {}) => `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true, ...state }))}`;

async function openHistory(page: Page, path = '/') {
  await page.goto(path);
  await page.getByRole('button', { name: /Sync status:/ }).click();
  await page.getByRole('menuitem', { name: 'Past runs' }).click();
  await expect(page.getByRole('dialog', { name: 'Past runs' })).toBeVisible();
}

test('Past runs describes the time and outcome of repeated job kinds', async ({ page }) => {
  await installOperations(page);
  const publicOperations = JSON.parse(readFileSync(new URL('./fixtures/operations-public.json', import.meta.url), 'utf8')) as OperationRunsResponse & { job_runs: OperationRunSummary[] };
  await page.route(url => url.pathname === '/api/v1/operations/runs', route => route.fulfill({ json: {
    runs: [publicOperations.runs.find(run => run.kind === 'source_sync')!, publicOperations.job_runs.find(run => run.kind === 'source_sync')!],
    membership_revision: 1, unavailable_kinds: [],
  } }));
  await openHistory(page);
  const runs = page.getByRole('button', { name: 'Open Source sync run', exact: true });
  await expect(runs.nth(0)).toHaveAccessibleDescription(/Aug 30, 2026, 12:00 PM.*Succeeded/);
  await expect(runs.nth(1)).toHaveAccessibleDescription(/Aug 30, 2026, 11:00 AM.*Failed/);
});

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

test('Past runs keeps focus through detail and history, then restores workspace focus', async ({ page }) => {
  await installOperations(page);
  await openHistory(page, '/?workspace=sources');
  const row = page.getByRole('button', { name: 'Open Document extraction run' });
  for (let index = 0; index < 120 && !(await row.evaluate(element => document.activeElement === element)); index += 1) await page.keyboard.press('Tab');
  await expect(row).toBeFocused();
  await row.press('Enter');
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.goBack();
  await expect(row).toBeFocused();
  await page.goForward();
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.keyboard.press('Tab');
  await page.keyboard.press('Shift+Tab');
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(row).toBeFocused();
  await page.keyboard.press('Enter');
  const link = page.getByRole('button', { name: 'Open Document index status' });
  await link.click();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(link).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(page.getByRole('button', { name: 'Sources', exact: true })).toBeFocused();
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
