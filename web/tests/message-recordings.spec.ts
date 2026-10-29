import { mkdir } from 'node:fs/promises';
import path from 'node:path';

import { expect, test } from '@playwright/test';

import { captureScreenshot } from './docs-fixture-screenshot';

// Set to a directory to keep one screenshot per width and theme.
const screenshotDir = process.env.MSGVAULT_RECORDINGS_SCREENSHOT_DIR ?? '';

const row = {
  key: 'source:1:message:source-1',
  kind: 'message',
  message_type: 'beeper',
  conversation_type: 'direct_chat',
  title: 'Voice note thread',
  preview: 'A synthetic voice note',
  occurred_at: '2026-07-18T12:00:00Z',
  source_id: 1,
  source_identifier: 'signal',
  source_type: 'beeper',
  participant_labels: ['alice'],
  participant_ids: [1],
  attachment_count: 1,
  attachment_size: 48213,
  has_attachments: true,
  deleted_from_source: false,
  message_count: 1,
  anchor_message_id: 42,
  conversation_id: 7,
  match: {}
};

const longFilename = `synthetic-voice-note-${'recorded-during-a-very-long-walk-'.repeat(6)}at-dusk.ogg`;

for (const theme of screenshotDir ? ['light', 'dark'] as const : ['light'] as const) {
  for (const width of [1280, 800, 700, 620]) {
    test(`recordings lay out at ${width}px in ${theme}`, async ({ page, baseURL }) => {
      if (!baseURL) throw new Error('Playwright baseURL is required');
      await page.setViewportSize({ width, height: 720 });
      await page.addInitScript((selected) => {
        sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ theme: selected, density: 'compact' }));
      }, theme);
      await page.context().addCookies([{
        name: 'msgvault_session', value: 'synthetic-session', url: new URL(baseURL).origin
      }]);
      await page.route('**/api/session', (route) =>
        route.fulfill({ json: { auth_mode: 'session', https: false, plain_http_warning: false } })
      );
      await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
        rows: [row], total_count: 1, cache_revision: 'cache-recordings', search_provenance: {}
      } }));
      await page.route('**/api/v1/conversations/7**', (route) => route.fulfill({ json: {
        id: 7,
        anchor_id: 42,
        messages: [{
          id: 42,
          conversation_id: 7,
          subject: '',
          message_type: 'beeper',
          from: 'alice@example.com',
          to: ['user@example.com'],
          sent_at: row.occurred_at,
          snippet: row.preview,
          labels: [],
          has_attachments: true,
          size_bytes: 10,
          body: 'Sent a voice note',
          attachments: [
            { id: 5, filename: longFilename, mime_type: 'audio/ogg', size_bytes: 48213, content_hash: '' }
          ]
        }],
        has_before: false,
        has_after: false,
        total: 1
      } }));
      await page.route('**/api/v1/messages/42/recordings', (route) => route.fulfill({ json: {
        message_id: 42,
        recordings: [
          {
            attachment_id: 5,
            filename: longFilename,
            size_bytes: 48213,
            state: 'ready',
            transcript: {
              origin: 'generated',
              partial: true,
              units: [
                { text: 'synthetic transcript of the first part of the walk, long enough to wrap across several lines at narrow widths', start_ms: 0, end_ms: 4200, speaker: 'alice' },
                { text: 'second synthetic line without timing' }
              ]
            }
          },
          { attachment_id: 6, filename: 'late.ogg', size_bytes: 1200, state: 'processing' }
        ]
      } }));

      const explore = encodeURIComponent(JSON.stringify({ workspace: 'everything' }));
      await page.goto(`/?explore=${explore}`);
      const grid = page.getByRole('grid', { name: 'Everything results' });
      await expect(grid.getByText(row.title)).toBeVisible();
      await grid.focus();
      await page.keyboard.press('Enter');

      const section = page.getByRole('region', { name: 'Recordings' });
      await expect(section).toBeVisible();
      await expect(section.getByText('synthetic transcript of the first part')).toBeVisible();
      await section.scrollIntoViewIfNeeded();

      expect(await section.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      const filename = section.locator('.recording-file strong').first();
      expect(await filename.evaluate((element) => getComputedStyle(element).textOverflow)).toBe('ellipsis');
      expect(await filename.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
      const size = section.locator('.recording-file span').first();
      await expect(size).toHaveText('47 KB');
      expect(await size.evaluate((element) => element.getBoundingClientRect().height)).toBeLessThan(24);

      if (screenshotDir) {
        await mkdir(screenshotDir, { recursive: true });
        await captureScreenshot(page, path.join(screenshotDir, `recordings-${width}-${theme}.png`));
      }
    });
  }
}
