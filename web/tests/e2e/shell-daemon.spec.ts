import { test, expect, loginToMeetingArchive, meetingURL } from './fixtures/meeting-daemon';

test.use({ hasTouch: true });

test('closing Past runs returns focus to the status trigger', async ({ page, daemon }) => {
  await loginToMeetingArchive(page, daemon);
  await page.goto(meetingURL(daemon, { filters: [], selectedRow: `message:${daemon.chatwoot[0]}` }));
  await expect(page.getByRole('complementary', { name: /Reading pane/ })).toBeVisible();
  const status = page.getByRole('button', { name: /Sync status:/ });
  await status.focus();
  await page.keyboard.press('Enter');
  await page.keyboard.press('End');
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: 'Past runs' });
  await expect(dialog).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(status).toBeFocused();
});
