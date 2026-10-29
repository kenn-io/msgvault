import { test, expect, loginToMeetingArchive, meetingURL } from './fixtures/meeting-daemon';

test.use({ hasTouch: true });

test('Escape closes Past runs before the underlying reader', async ({ page, daemon }) => {
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
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(reader).toBeVisible();
  await expect(status).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(reader).toBeHidden();

});

test('touch Help opens Commands against the real daemon', async ({ page, daemon }) => {
  await loginToMeetingArchive(page, daemon);
  await page.getByRole('button', { name: 'Help', exact: true }).tap();
  await page.getByRole('button', { name: 'Commands', exact: true }).tap();
  await expect(page.getByRole('combobox', { name: 'Messages commands' })).toBeVisible();
});
