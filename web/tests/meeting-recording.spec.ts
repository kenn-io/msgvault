import { expect, test } from '@playwright/test';

// A real decodable one-second PCM WAV, generated entirely from synthetic silence.
function recording(): Buffer {
  const dataBytes = 8000 * 2;
  const wav = Buffer.alloc(44 + dataBytes);
  wav.write('RIFF', 0);
  wav.writeUInt32LE(36 + dataBytes, 4);
  wav.write('WAVEfmt ', 8);
  wav.writeUInt32LE(16, 16);
  wav.writeUInt16LE(1, 20);
  wav.writeUInt16LE(1, 22);
  wav.writeUInt32LE(8000, 24);
  wav.writeUInt32LE(16000, 28);
  wav.writeUInt16LE(2, 32);
  wav.writeUInt16LE(16, 34);
  wav.write('data', 36);
  wav.writeUInt32LE(dataBytes, 40);
  return wav;
}

test('a recorded meeting plays local audio and offers a streamed download', async ({ page, baseURL }) => {
  const hash = 'c'.repeat(64);
  const audioRequests: string[] = [];
  const bytes = recording();
  await page.context().addCookies([{
    name: 'msgvault_session', value: 'synthetic-session', url: baseURL!,
  }]);
  await page.route('**/api/session', route => route.fulfill({ json: {
    auth_mode: 'session', https: false, plain_http_warning: false,
  } }));
  const row = {
    key: 'source:1:message:call-1', kind: 'message', message_type: 'meeting_transcript',
    conversation_type: 'meeting', title: 'Recorded example call', preview: 'Call transcript',
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_identifier: 'example-calls',
    source_type: 'synthetic', participant_labels: ['Example caller'], participant_ids: [1],
    attachment_count: 1, attachment_size: bytes.length, has_attachments: true,
    deleted_from_source: false, message_count: 1, anchor_message_id: 42, conversation_id: 7, match: {},
  };
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [row], total_count: 1, cache_revision: 'recorded-calls', search_provenance: {},
  } }));
  await page.route('**/api/v1/conversations/7**', route => route.fulfill({ json: {
    id: 7, anchor_id: 42, messages: [{
      id: 42, conversation_id: 7, subject: row.title, message_type: 'meeting_transcript',
      from: '+12025550100', to: ['+12025550101'], sent_at: row.occurred_at,
      snippet: row.preview, labels: [], has_attachments: true, size_bytes: bytes.length,
      body: 'Example caller: Thank you for calling.',
      attachments: [{ id: 8, filename: 'call.wav', mime_type: 'audio/wav',
        size_bytes: bytes.length, content_hash: hash }],
    }], has_before: false, has_after: false, total: 1,
  } }));
  await page.route(`**/api/v1/attachments/${hash}/content`, route => {
    audioRequests.push(route.request().headers()['cookie'] ?? '');
    return route.fulfill({ contentType: 'audio/wav', body: bytes, headers: {
      'Content-Disposition': 'attachment; filename="call.wav"',
      'Content-Length': String(bytes.length), 'X-Content-Type-Options': 'nosniff',
    } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await page.getByRole('grid', { name: 'Everything results' }).getByText(row.title).click();
  const player = page.getByLabel('Play call.wav');
  await expect(player).toBeVisible();
  await expect(player).toHaveAttribute('preload', 'none');
  expect(audioRequests).toHaveLength(0);
  await player.evaluate(async element => { await (element as HTMLAudioElement).play(); });
  await expect.poll(() => player.evaluate(element => (element as HTMLAudioElement).duration)).toBe(1);
  expect(audioRequests[0]).toContain('msgvault_session=synthetic-session');
  const downloadEvent = page.waitForEvent('download');
  await page.getByRole('link', { name: 'Download call.wav' }).click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe('call.wav');
  expect(await download.failure()).toBeNull();
});
