import { expect, test, type Page } from '@playwright/test';
import { installOperations, OPERATION_PRIVACY_SENTINELS, OPERATION_REFERENCES } from './fixtures/operations';

const historyURL = (state: Record<string, unknown> = {}) => `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true, ...state }))}`;

async function openHistory(page: Page) {
  await page.goto('/');
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

test('operations direct opaque detail never renders private native values', async ({ page }) => {
  await installOperations(page);
  await page.goto(historyURL({ operationRunID: OPERATION_REFERENCES.document }));
  await expect(page.getByRole('dialog', { name: 'Document extraction', exact: true })).toBeVisible();
  const rendered = await page.locator('body').innerText();
  for (const sentinel of OPERATION_PRIVACY_SENTINELS) expect(rendered).not.toContain(sentinel);
});

test('Back returns focus to the related status link', async ({ page }) => {
  await installOperations(page);
  await page.goto(historyURL({ operationRunID: OPERATION_REFERENCES.document }));
  const link = page.getByRole('button', { name: 'Open Document index status' });
  await link.click();
  await expect(page.getByRole('region', { name: 'Document index status' })).toContainText('4 of 5 owners ready');
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(link).toBeFocused();
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
