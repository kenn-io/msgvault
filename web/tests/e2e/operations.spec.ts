import { expect, test, type Page } from '@playwright/test';
import { installOperations } from './fixtures/operations';

const historyURL = (state: Record<string, unknown> = {}) => `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true, ...state }))}`;

async function openHistory(page: Page, path = '/') {
  await page.goto(path);
  await page.getByRole('button', { name: /Sync status:/ }).click();
  await page.getByRole('menuitem', { name: 'Past runs' }).click();
  await expect(page.getByRole('dialog', { name: 'Past runs' })).toBeVisible();
}

test('operations invokes only advertised CardDAV and visual actions and surfaces conflicts', async ({ page }) => {
  const fixture = await installOperations(page);
  await openHistory(page);
  await page.getByRole('button', { name: 'Open CardDAV sync run' }).click();
  const detail = page.getByRole('region', { name: 'Operation run detail' });
  const action = detail.getByRole('button', { name: 'Start CardDAV sync' });
  await expect(action).toBeVisible();
  await expect(detail.getByRole('button', { name: /visual index/i })).toHaveCount(0);
  await action.click();
  await expect.poll(() => fixture.actionRequests).toEqual(['carddav_sync']);
  await expect(action).toBeEnabled();
  await expect(page.getByRole('status', { name: 'Operation status' })).toContainText('CardDAV sync request completed');
  fixture.conflictNextCardDAV();
  await action.click();
  await expect(page.getByRole('alert').filter({ hasText: 'current server state changed' })).toBeVisible();
  await expect.poll(() => fixture.actionRequests).toEqual(['carddav_sync', 'carddav_sync']);
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
  await row.focus();
  await row.press('Enter');
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  await page.goBack();
  await expect(row).toBeFocused();
  await page.goForward();
  await expect(page.getByRole('button', { name: 'All runs' })).toBeFocused();
  const link = page.getByRole('button', { name: 'Open Document index status' });
  await link.click();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(link).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(page.getByRole('button', { name: 'Sources', exact: true })).toBeFocused();
});

test('Past runs exposes the dirty Settings leave confirmation before opening Sources', async ({ page }) => {
  await installOperations(page);
  await page.goto(`/?workspace=settings&explore=${encodeURIComponent(JSON.stringify({ settingsCategory: 'search' }))}`);
  const settings = page.getByRole('main', { name: 'Settings' });
  const semanticSearch = settings.getByRole('switch', { name: 'Semantic search', exact: true });
  await semanticSearch.click();
  await expect(semanticSearch).toBeChecked();
  for (const choice of ['Stay', 'Discard and leave']) {
    await page.getByRole('button', { name: /Sync status:/ }).click();
    await page.getByRole('menuitem', { name: 'Past runs' }).click();
    await page.getByRole('button', { name: 'Open Source sync run' }).click();
    await page.getByRole('button', { name: 'Open Sources status' }).click();
    await expect(page.getByRole('dialog', { name: 'Source sync', exact: true })).toBeHidden();
    await expect(settings.getByText('Leave Settings and discard 1 unsaved change?', { exact: true })).toBeVisible();
    await settings.getByRole('button', { name: choice, exact: true }).click();
    if (choice === 'Stay') {
      await expect(settings).toBeVisible();
      await expect(semanticSearch).toBeChecked();
    }
  }
  await expect(page.getByRole('main', { name: 'Sources' })).toBeVisible();
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

test('action reconciliation preserves selected detail identity when opaque references rotate', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(historyURL());
  const visualRow = page.getByRole('button', { name: 'Open Visual embedding run' });
  await visualRow.click();
  const selected = () => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').operationRunID;
  const before = selected();
  fixture.rotateReferencesOnNextRefresh();
  await page.getByRole('button', { name: 'Resume visual index' }).click();
  await expect.poll(() => fixture.actionRequests).toEqual(['visual_resume']);
  await expect(page.getByRole('button', { name: 'Resume visual index' })).toBeEnabled();
  await expect(page.getByRole('status', { name: 'Operation status' })).toContainText('Visual index resume request completed');
  expect(selected()).toBe(before);
  await expect(page.getByRole('dialog', { name: 'Visual embedding', exact: true })).toBeVisible();
});
