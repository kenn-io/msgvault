import { test, expect, loginToMeetingArchive, meetingURL } from './fixtures/meeting-daemon';

test.use({ hasTouch: true });

test('real daemon Operations history and keyboard navigation preserve the underlying reader', async ({ page, daemon }) => {
  await loginToMeetingArchive(page, daemon);
  await page.goto(meetingURL(daemon, { filters: [], selectedRow: `message:${daemon.chatwoot[0]}` }));
  const reader = page.getByRole('complementary', { name: /Reading pane/ });
  await expect(reader).toBeVisible();
  const status = page.getByRole('button', { name: /Sync status:/ });
  await status.focus();
  await page.keyboard.press('Enter');
  await page.keyboard.press('End');
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: 'Past runs' });
  await expect(dialog).toBeVisible();
  const kind = dialog.getByRole('combobox', { name: /^Kind:/ });
  await kind.focus();
  await page.keyboard.press('Enter');
  await page.getByRole('option', { name: 'Source sync', exact: true }).press('Enter');
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').operationKind).toBe('source_sync');
  await expect(dialog.getByRole('button', { name: 'Refresh', exact: true })).toBeEnabled();
  const reloaded = page.waitForResponse(response => response.url().includes('/operations/runs?') && response.ok());
  await dialog.getByRole('button', { name: 'Refresh', exact: true }).press('Enter');
  await reloaded;
  await expect(dialog.getByRole('button', { name: 'Refresh', exact: true })).toBeEnabled();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(reader).toBeVisible();
  await expect(status).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(reader).toBeHidden();
  await page.getByRole('button', { name: 'Files', exact: true }).press('Enter');
  await expect(page.getByRole('main', { name: 'Files', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Messages', exact: true }).press('Enter');
  await expect(page.getByRole('main', { name: 'Messages', exact: true })).toBeVisible();
});

test('touch Help opens Commands for Reviews and Directory against the real daemon', async ({ page, daemon }) => {
  await loginToMeetingArchive(page, daemon);
  for (const [command, landmark] of [['Reviews', 'Reviews'], ['Directory', 'Directory']]) {
    await page.getByRole('button', { name: 'Help', exact: true }).tap();
    await page.getByRole('button', { name: 'Commands', exact: true }).tap();
    await page.getByRole('combobox', { name: 'Messages commands' }).fill(command!);
    await page.getByRole('option', { name: command === 'Reviews' ? 'Open Reviews' : 'Directory', exact: true }).tap();
    await expect(page.getByRole('main', { name: landmark!, exact: true })).toBeVisible();
  }
});
