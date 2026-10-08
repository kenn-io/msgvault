import { test, expect, loginToMeetingArchive } from './fixtures/meeting-daemon';

test('call and meeting links round trip through the real archive', async ({ page, daemon }) => {
  await loginToMeetingArchive(page, daemon);
  const [callID, meetingID] = daemon.chatwoot;
  await page.goto(`${daemon.origin}/messages/${callID}`);
  const call = page.getByRole('article', { name: `Message ${callID}`, exact: true });
  await expect(call).toContainText('Voice call with Example Customer');
  await call.getByRole('link', { name: 'Open meeting', exact: true }).click();
  const meeting = page.getByRole('article', { name: `Message ${meetingID}`, exact: true });
  await expect(meeting).toContainText('Please send the meeting recap');
  const download = page.waitForEvent('download');
  await meeting.getByRole('list', { name: 'Attachments' }).getByRole('link').click();
  expect(await (await download).failure()).toBeNull();
  await meeting.getByRole('link', { name: 'Open conversation', exact: true }).click();
  await expect(page).toHaveURL(`${daemon.origin}/messages/${callID}`);
  await expect(call).toHaveAttribute('aria-current', 'true');
  await expect(page.getByText('Could we discuss the next delivery?', { exact: true })).toBeVisible();
});
