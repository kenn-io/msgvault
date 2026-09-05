import { expect, test } from '@playwright/test';
import { selectKitOption } from './kit-ui';
import { installOperations, OPERATION_REFERENCES } from './e2e/fixtures/operations';

function operationsURL(overrides: Record<string, unknown> = {}): string {
  return `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'operations', ...overrides }))}`;
}

test('desktop operations journey restores filters, pages, opaque detail, history, and authority links', async ({ page }) => {
  const fixture = await installOperations(page);
  await page.goto(operationsURL());
  await expect.poll(() => fixture.listQueries.at(-1)?.get('limit')).toBe('25');

  const workspace = page.getByRole('main', { name: 'Operations' });
  await expect(workspace).toBeVisible();
  await expect(page.getByRole('region', { name: 'Operation lanes' }).getByRole('heading', { level: 2 }))
    .toHaveText(['Messages', 'Facts', 'Contacts', 'Documents', 'Attachments']);
  await expect(page.getByRole('status', { name: 'Unavailable operation history' }))
    .toContainText('Person embedding history is unavailable.');

  await selectKitOption(page, 'Lane', 'Documents');
  await expect.poll(() => fixture.listQueries.at(-1)?.get('lane')).toBe('documents');
  await selectKitOption(page, 'Kind', 'Document extraction');
  await expect.poll(() => fixture.listQueries.at(-1)?.get('kind')).toBe('document_extraction');
  await selectKitOption(page, 'State', 'Partial');
  await expect.poll(() => fixture.listQueries.at(-1)?.get('state')).toBe('partial');
  await expect(page.getByRole('button', { name: 'Open Document extraction run' })).toBeVisible();

  await page.goto(operationsURL({
    operationStartedFrom: '2026-08-29T00:00:00Z',
    operationStartedBefore: '2026-08-31T00:00:00Z'
  }));
  await expect.poll(() => fixture.listQueries.at(-1)?.get('started_from')).toBe('2026-08-29T00:00:00Z');
  await expect.poll(() => fixture.listQueries.at(-1)?.get('started_before')).toBe('2026-08-31T00:00:00Z');
  await expect(page.getByRole('button', { name: 'Clear operation dates' })).toBeVisible();

  await page.goto(operationsURL());
  await page.getByRole('button', { name: 'Load more operation history' }).click();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toBeVisible();
  await expect.poll(() => fixture.listQueries.at(-1)?.get('cursor')).toBe('op2.fixture.page-two');

  const sourceRow = page.getByRole('button', { name: 'Open Source sync run' });
  await sourceRow.click();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toContainText('Source sync');
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').operationRunID)
    .toBe(OPERATION_REFERENCES.source);
  const detailURL = page.url();
  await page.reload();
  await expect(page).toHaveURL(detailURL);
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toContainText('24 messages');

  await page.goBack();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toHaveCount(0);
  await page.goForward();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toBeVisible();

  await page.getByRole('region', { name: 'Operation run detail' })
    .getByRole('button', { name: 'Open Sources status' }).click();
  await expect(page.getByRole('main', { name: 'Sources' })).toBeVisible();
  await page.getByRole('button', { name: 'View source operations' }).click();
  await expect(page.getByRole('main', { name: 'Operations' })).toBeVisible();
  await expect.poll(() => fixture.listQueries.at(-1)?.get('kind')).toBe('source_sync');
});

test('narrow operations journey uses focused detail and returns focus to its row', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installOperations(page);
  await page.goto(operationsURL());

  await expect(page.getByRole('list', { name: 'Operation history' })).toBeVisible();
  const sourceRow = page.getByRole('button', { name: 'Open Source sync run' });
  await sourceRow.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('region', { name: 'Operation detail focused content' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Operation lanes' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Back to operation history' }).click();
  await expect(sourceRow).toBeFocused();
});
