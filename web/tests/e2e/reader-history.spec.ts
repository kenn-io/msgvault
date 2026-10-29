import type { ConversationResponse } from '../../src/lib/api/generated/models';
import { test, expect, meetingURL, loginToMeetingArchive } from './fixtures/meeting-daemon';

test.use({ viewport: { width: 1440, height: 900 }, colorScheme: 'dark' });
test.beforeEach(async ({ page, daemon }) => {
  await page.addInitScript(() => localStorage.setItem('msgvault.appearance.override', JSON.stringify({ theme: 'dark', density: 'compact' })));
  await loginToMeetingArchive(page, daemon);
});

test('Relationships Back restores a later-page chat row, anchor, and day bounds', async ({ page, daemon }, info) => {
  let visits = 0;
  const row = { key: 'chat:70:day', kind: 'chat_burst', occurred_at: '2026-07-19T10:00:00Z', preview: 'Day preview', source_id: 1, title: 'Saved chat day', has_attachments: false, message_count: 2, anchor_message_id: 9, conversation_id: 70 };
  await page.route('**/api/v1/participants/{1,2}', route => route.fulfill({ json: { id: Number(route.request().url().split('/').at(-1)), display_label: 'Chat person', identifiers: [], activity_count: 2, file_count: 0, source_counts: [], cache_revision: 'cache-rel' } }));
  await page.route('**/api/v1/relationships/{1,2}/timeline', route => route.fulfill({ json: { canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: route.request().url().includes('/1/') && ++visits === 1 ? [row] : [], total_count: 1 } }));
  await page.route('**/api/v1/conversations/70?*', route => route.fulfill({ json: { id: 70, anchor_id: Number(new URL(route.request().url()).searchParams.get('anchor')), messages: [9, 10].map(id => ({ id, conversation_id: 70, message_type: 'whatsapp', from: 'Chat sender', from_participant_id: 1, to: ['Other person'], to_participants: [{ display: 'Other person', participant_id: 2 }], subject: `Chat message ${id}`, body: `Saved body ${id}`, sent_at: row.occurred_at, labels: [], attachments: [] })), total: 2, has_before: false, has_after: false } }));
  await page.goto(meetingURL(daemon, { workspace: 'relationships', relationshipTarget: 'cluster:1', filters: [] }));
  await page.getByText('2 messages in Saved chat day', { exact: true }).click();
  await page.getByRole('button', { name: /Expand message 10/ }).click();
  await page.getByRole('button', { name: 'Other person', exact: true }).last().click();
  await expect(page.getByRole('complementary', { name: /Reading pane/ })).toHaveCount(0);
  const restored = page.waitForRequest(request => request.url().includes('/conversations/70?'));
  await page.goBack();
  await expect(page.getByText('Saved body 10', { exact: true })).toBeVisible();
  const bounds = new URL((await restored).url()).searchParams;
  expect(bounds.get('anchor')).toBe('10');
  expect(bounds.get('start')).toBe('2026-07-19T00:00:00.000Z');
  expect(bounds.get('end')).toBe('2026-07-20T00:00:00.000Z');
  await page.screenshot({ path: info.outputPath('restored-relationship-reader.png') });
  await page.goForward();
  await expect(page.getByRole('complementary', { name: /Reading pane/ })).toHaveCount(0);
});

test('archived email task setup exposes Settings and Back restores its meeting', async ({ page, daemon }, info) => {
  let conversation: ConversationResponse | undefined;
  await page.route('**/api/v1/conversations/*?*', async route => {
    conversation ??= await (await route.fetch()).json();
    const data = { ...conversation, anchor_id: Number(new URL(route.request().url()).searchParams.get('anchor')), messages: [...conversation!.messages] };
    data.messages.push({ ...data.messages[0], id: 999, message_type: 'email', subject: 'Email follow-up', body: 'Synthetic email evidence' });
    await route.fulfill({ json: data });
  });
  await page.route('**/api/v1/messages/999/tasks', route => route.fulfill({ json: { state: 'disabled', tasks: [], complete: false } }));
  await page.goto(meetingURL(daemon, { selectedRow: `archive-meeting:${daemon.meetings.circleback.message_id}` }));
  const reader = page.getByRole('dialog', { name: 'Archived meeting', exact: true });
  await reader.getByRole('button', { name: /Expand message 999/ }).click();
  await reader.getByRole('button', { name: 'Set up', exact: true }).click();
  await expect(reader).toHaveCount(0);
  await expect(page.getByRole('main', { name: 'Settings', exact: true })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Task service', exact: true })).toBeVisible();
  await page.screenshot({ path: info.outputPath('archived-task-settings.png') });
  await page.goBack();
  await expect(reader).toBeVisible();
});
