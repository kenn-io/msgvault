import { expect, test, type Page } from '@playwright/test';
import { installOperations, OPERATION_PRIVACY_SENTINELS, OPERATION_REFERENCES } from './fixtures/operations';

const historyURL = (state: Record<string, unknown> = {}) => `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true, ...state }))}`;

async function openHistory(page: Page) {
  await page.goto('/');
  await page.getByRole('button', { name: /Sync status:/ }).click();
  await page.getByRole('menuitem', { name: 'Past runs' }).click();
  await expect(page.getByRole('dialog', { name: 'Past runs' })).toBeVisible();
}

test('operations recovers from revision drift and restarts page one', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.driftNextPage();
  await openHistory(page);
  await page.getByRole('button', { name: 'Show more' }).click();
  const conflict = page.getByRole('alert').filter({ hasText: 'Operation history changed' });
  await expect(conflict).toContainText('Restart from the first page.');
  await page.getByRole('button', { name: 'Restart', exact: true }).click();
  await expect(conflict).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Open Source sync run' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toHaveCount(0);
});

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

test('operations related links fetch and render their live status authorities', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(historyURL({ operationRunID: OPERATION_REFERENCES.document }));
  const link = page.getByRole('button', { name: 'Open Document index status' });
  await link.click();
  await expect(page.getByRole('region', { name: 'Document index status' })).toContainText('4 of 5 owners ready');
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(link).toBeFocused();
  for (const [authority, statusName, copy, endpoint] of [
    ['getDocumentVectorStatus', 'Document vector status', '7 of 9 chunks ready', '/api/v1/documents/vectors/status'],
    ['getVisualAttachmentStatus', 'Visual attachment status', '8 of 10 attachments current', '/api/v1/multimodal/status']
  ]) {
    await page.goto(historyURL({ operationStatus: authority }));
    await expect(page.getByRole('region', { name: statusName })).toContainText(copy!);
    await expect.poll(() => fixture.statusRequests).toContain(endpoint);
    await page.getByRole('button', { name: 'Back', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Open Source sync run' })).toBeVisible();
  }
});

test('operations sends confirmed unconfigured document and visual status links to setup without fetching them', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.setOperationConfigured('document_extraction', false);
  await page.goto(historyURL({ operationStatus: 'getDocumentIndexStatus' }));
  const guide = page.getByRole('link', { name: 'Document indexing setup' });
  await expect(guide).toBeVisible();
  await expect(guide).toHaveAttribute('target', '_blank');
  await expect(guide).toHaveAttribute('href', 'https://msgvault.io/docs/usage/document-indexing/#configure-the-policy');
  await expect(page.getByText('Configured in config.toml on the daemon host.').first()).toBeVisible();
  expect(fixture.statusRequests).not.toContain('/api/v1/documents/status/current');
  fixture.setOperationConfigured('visual_embedding', false);
  await page.goto(historyURL({ operationStatus: 'getVisualAttachmentStatus' }));
  const configure = page.getByRole('button', { name: 'Open visual attachment settings' });
  await expect(configure).toBeVisible();
  expect(fixture.statusRequests).not.toContain('/api/v1/multimodal/status');
  await configure.click();
  const settings = page.getByRole('main', { name: 'Settings' });
  await expect(settings).toBeVisible();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search & AI' })).toBeVisible();
});

test('operations keeps a configured but unavailable document authority retry-only', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.failNextDocumentStatus();
  await page.goto(historyURL({ operationStatus: 'getDocumentIndexStatus' }));
  const status = page.getByRole('region', { name: 'Document index status' });
  await expect(status.getByRole('alert')).toContainText('Unable to load document index status.');
  await expect(status.getByRole('button', { name: /settings/i })).toHaveCount(0);
  await status.getByRole('button', { name: 'Retry document index status' }).click();
  await expect(status).toContainText('4 of 5 owners ready');
});

test('Refresh reloads page one and retries failures while retaining selected detail', async ({ page }) => {
  const fixture = await installOperations(page);
  await openHistory(page);
  await page.getByRole('button', { name: 'Show more' }).click();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toBeVisible();
  fixture.failNextHistory();
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'Unable to load operation history.' })).toBeVisible();
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toHaveCount(0);
  expect(fixture.listQueries.at(-1)?.get('cursor')).toBeNull();
  await page.getByRole('button', { name: 'Open Source sync run' }).click();
  const refresh = page.getByRole('button', { name: 'Refresh', exact: true });
  const release = fixture.holdNextOperationStatus();
  const reads = fixture.operationStatusReads;
  await refresh.click();
  await expect.poll(() => fixture.operationStatusReads).toBeGreaterThan(reads);
  await expect(refresh).toBeDisabled();
  release();
  await expect(refresh).toBeEnabled();
  await expect(refresh).toBeFocused();
  await expect(page.getByRole('dialog', { name: 'Source sync', exact: true })).toBeVisible();
});

test('desktop refresh preserves selected detail identity and row focus when opaque references rotate', async ({ page }) => {
  const fixture = await installOperations(page);
  await openHistory(page);
  const sourceRow = page.getByRole('button', { name: 'Open Source sync run' });
  const listedBefore = await sourceRow.getAttribute('data-run-id');
  await sourceRow.click();
  const selected = () => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').operationRunID;
  const before = selected();
  expect(listedBefore).toBe(before);
  fixture.rotateReferencesOnNextRefresh();
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.getByRole('dialog', { name: 'Source sync', exact: true })).toBeVisible();
  expect(selected()).toBe(before);
  await page.getByRole('button', { name: 'All runs' }).click();
  await expect(sourceRow).not.toHaveAttribute('data-run-id', listedBefore!);
  await expect(sourceRow).toBeFocused();
});

test('narrow action reconciliation preserves selected detail identity when opaque references rotate', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
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
  await page.getByRole('button', { name: 'All runs' }).click();
  await expect(visualRow).toBeFocused();
});
