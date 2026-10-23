import { expect, test } from '@playwright/test';
import { installOperations } from './fixtures/operations';

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

test('operations related links fetch and render their live status authorities', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(operationsURL);

  for (const [linkName, statusName, expectedCopy, endpoint] of [
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

test('operations sends unconfigured visual status links to settings without fetching', async ({ page }) => {
  const fixture = await installOperations(page);
  fixture.setOperationConfigured('visual_embedding', false);
  await page.goto(operationsURL);
  await page.getByRole('button', { name: 'Open Visual attachment status' }).click();
  await expect(page.getByRole('button', { name: 'Open visual attachment settings' })).toBeVisible();
  expect(fixture.statusRequests).not.toContain('/api/v1/multimodal/status');
  await page.getByRole('button', { name: 'Open visual attachment settings' }).click();
  const settings = page.getByRole('main', { name: 'Settings' });
  await expect(settings).toBeVisible();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search' })).toBeVisible();
});

test('the refresh control reloads status only and keeps paged history, detail, and focus', async ({ page }) => {
  await page.clock.install();
  const fixture = await installOperations(page);
  await page.goto(operationsURL);
  await page.getByRole('button', { name: 'Load more operation history' }).click();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toBeVisible();
  const sourceRow = page.getByRole('button', { name: 'Open Source sync run' });
  await sourceRow.click();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toBeVisible();
  await sourceRow.focus();
  const listed = fixture.listQueries.length;
  const reads = fixture.operationStatusReads;

  await page.clock.fastForward('05:00');
  await expect.poll(() => fixture.operationStatusReads).toBe(reads + 1);
  expect(fixture.listQueries.length).toBe(listed);
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toBeVisible();
  await expect(sourceRow).toBeFocused();

  const refresh = page.getByRole('button', { name: 'Refresh operation status' });
  const releaseStatus = fixture.holdNextOperationStatus();
  await refresh.focus();
  await refresh.click();
  await expect.poll(() => fixture.operationStatusReads).toBe(reads + 2);
  await expect(refresh).toBeDisabled();
  expect(fixture.listQueries.length).toBe(listed);
  releaseStatus();
  await expect(refresh).toBeEnabled();
  await expect(refresh).toBeFocused();

  await page.getByRole('button', { name: 'Reload run history' }).click();
  await expect.poll(() => fixture.operationStatusReads).toBe(reads + 3);
  await expect.poll(() => fixture.listQueries.length).toBe(listed + 1);
  expect(fixture.listQueries.at(-1)?.get('cursor')).toBeNull();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toHaveCount(0);
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
  await page.getByRole('button', { name: 'Reload run history' }).click();
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
