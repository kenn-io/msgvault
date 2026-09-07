import { expect, test } from '@playwright/test';
import {
  installOperations,
  OPERATION_PRIVACY_SENTINELS,
  OPERATION_REFERENCES
} from './fixtures/operations';

const operationsURL = `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'operations' }))}`;

test('operations recovers from revision drift and restarts page one', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.driftNextPage();
  await page.goto(operationsURL);
  await page.getByRole('button', { name: 'Load more operation history' }).click();

  const conflict = page.getByRole('alert', { name: 'Operation history conflict' });
  await expect(conflict).toContainText('Operation history changed. Restart from the first page.');
  await conflict.getByRole('button', { name: 'Restart operation history' }).click();
  await expect(conflict).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Open Source sync run' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toHaveCount(0);
});

test('operations invokes only advertised CardDAV and visual actions and surfaces conflicts', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(operationsURL);

  await page.getByRole('button', { name: 'Open CardDAV sync run' }).click();
  const cardDAVDetail = page.getByRole('region', { name: 'Operation run detail' });
  await expect(cardDAVDetail.getByRole('button', { name: 'Start CardDAV sync' })).toBeVisible();
  await expect(cardDAVDetail.getByRole('button', { name: /visual index/i })).toHaveCount(0);
  await cardDAVDetail.getByRole('button', { name: 'Start CardDAV sync' }).click();
  await expect.poll(() => fixture.actionRequests).toEqual(['carddav_sync']);
  await expect(page.locator('[aria-live="polite"]')).toContainText('CardDAV sync request completed');

  fixture.conflictNextCardDAV();
  await cardDAVDetail.getByRole('button', { name: 'Start CardDAV sync' }).click();
  await expect(page.getByRole('alert', { name: 'Operation action conflict' }))
    .toContainText('current server state changed');
  await expect.poll(() => fixture.actionRequests).toEqual(['carddav_sync', 'carddav_sync']);

  await page.getByRole('button', { name: 'Close operation detail' }).click();
  await page.getByRole('button', { name: 'Open Visual embedding run' }).click();
  const visualDetail = page.getByRole('region', { name: 'Operation run detail' });
  await expect(visualDetail.getByRole('button', { name: 'Resume visual index' })).toBeVisible();
  await expect(visualDetail.getByRole('button', { name: 'Build visual index' })).toHaveCount(0);
  await expect(visualDetail.getByRole('button', { name: 'Start CardDAV sync' })).toHaveCount(0);
});

test('operations direct opaque detail never renders private native values', async ({ page }) => {
  await installOperations(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'operations', operationRunID: OPERATION_REFERENCES.document
  }))}`);
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toContainText('Document extraction');

  const rendered = await page.locator('body').innerText();
  for (const sentinel of OPERATION_PRIVACY_SENTINELS) {
    expect(rendered, `rendered private sentinel: ${sentinel}`).not.toContain(sentinel);
  }
});

test('operations related links fetch and render their live status authorities', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(operationsURL);

  for (const [linkName, statusName, expectedCopy, endpoint] of [
    ['Open Document index status', 'Document index status', '4 of 5 owners ready', '/api/v1/documents/status/current'],
    ['Open Document vector status', 'Document vector status', '7 of 9 chunks ready', '/api/v1/documents/vectors/status'],
    ['Open Visual attachment status', 'Visual attachment status', '8 of 10 attachments current', '/api/v1/multimodal/status']
  ] as const) {
    const link = page.getByRole('button', { name: linkName });
    await link.click();
    await expect(page.getByRole('region', { name: statusName })).toContainText(expectedCopy);
    await expect.poll(() => fixture.statusRequests).toContain(endpoint);
    await page.getByRole('button', { name: 'Back to operations' }).click();
    await expect(link).toBeFocused();
  }
});

test('operations sends confirmed unconfigured document and visual status links to Settings without fetching them', async ({ page }) => {
  const fixture = await installOperations(page);
  for (const [kind, linkName, settingsName, endpoint] of [
    ['document_extraction', 'Open Document index status', 'Open document index settings', '/api/v1/documents/status/current'],
    ['visual_embedding', 'Open Visual attachment status', 'Open visual attachment settings', '/api/v1/multimodal/status']
  ] as const) {
    fixture.setOperationConfigured(kind, false);
    await page.goto(operationsURL);
    await page.getByRole('button', { name: linkName }).click();
    await expect(page.getByRole('button', { name: settingsName })).toBeVisible();
    expect(fixture.statusRequests).not.toContain(endpoint);
    await page.getByRole('button', { name: settingsName }).click();
    await expect(page.getByRole('main', { name: 'Settings' })).toBeVisible();
  }
});

test('operations keeps a configured but unavailable document authority retry-only', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.failNextDocumentStatus();
  await page.goto(operationsURL);
  await page.getByRole('button', { name: 'Open Document index status' }).click();

  const status = page.getByRole('region', { name: 'Document index status' });
  await expect(status.getByRole('alert')).toContainText('Unable to load document index status.');
  await expect(status.getByRole('button', { name: 'Retry document index status' })).toBeVisible();
  await expect(status.getByRole('button', { name: /settings/i })).toHaveCount(0);
  await status.getByRole('button', { name: 'Retry document index status' }).click();
  await expect(status).toContainText('4 of 5 owners ready');
});

test('desktop refresh preserves selected detail identity and row focus when opaque references rotate', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(operationsURL);

  const sourceRow = page.getByRole('button', { name: 'Open Source sync run' });
  await sourceRow.click();
  const selectedReference = () => JSON.parse(
    new URL(page.url()).searchParams.get('explore') ?? '{}'
  ).operationRunID as string | undefined;
  const before = selectedReference();
  const listedBefore = await sourceRow.getAttribute('data-run-id');
  expect(listedBefore).toBe(before);
  fixture.rotateReferencesOnNextRefresh();
  await page.getByRole('button', { name: 'Refresh operations' }).click();
  await expect(sourceRow).not.toHaveAttribute('data-run-id', listedBefore!);
  expect(selectedReference()).toBe(before);
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toContainText('Source sync');
  await page.getByRole('button', { name: 'Close operation detail' }).click();
  await expect(sourceRow).toBeFocused();
});

test('narrow action reconciliation preserves selected detail identity when opaque references rotate', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const fixture = await installOperations(page);
  await page.goto(operationsURL);

  const visualRow = page.getByRole('button', { name: 'Open Visual embedding run' });
  await visualRow.click();
  const selectedReference = () => JSON.parse(
    new URL(page.url()).searchParams.get('explore') ?? '{}'
  ).operationRunID as string | undefined;
  const before = selectedReference();
  fixture.rotateReferencesOnNextRefresh();
  await page.getByRole('button', { name: 'Resume visual index' }).click();
  await expect(page.getByRole('status', { name: 'Operation status' }))
    .toContainText('Visual index resume request completed; current operation state was refreshed.');
  expect(selectedReference()).toBe(before);
  await expect(page.getByRole('region', { name: 'Operation detail focused content' })).toContainText('Visual embedding');
  await page.getByRole('button', { name: 'Back to operation history' }).click();
  await expect(visualRow).toBeFocused();
});
