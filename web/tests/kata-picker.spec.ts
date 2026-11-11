import { expect, test } from '@playwright/test';
import { row } from './archive-layout';
import { disabledTaskLookup } from '../src/lib/components/tasks/fixtures.test-support';

test('Kata search guidance sits below the issue results', async ({ page }) => {
  const message = { id: 42001, source_id: 3, source_message_id: 'source-message', conversation_id: 71, subject: 'Budget', message_type: 'email', from: 'sender@example.com', to: ['reader@example.com'], sent_at: '2020-01-01T12:00:00Z', snippet: 'Please review the budget', labels: [], has_attachments: false, size_bytes: 20, body: 'Please review the budget', attachments: [] };
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/v1/settings') return route.fulfill({ json: { settings: [], pending_restart: false } });
    if (url.pathname === '/api/v1/telemetry/events') return route.fulfill({ status: 202, json: { status: 'disabled' } });
    if (url.pathname === '/api/v1/integrations/kata/status') return route.fulfill({ json: { state: 'ready', project: 'example' } });
    if (url.pathname === '/api/v1/integrations/tasks/status') return route.fulfill({ json: { state: 'disabled' } });
    if (url.pathname === '/api/v1/messages/42001/tasks') return route.fulfill({ json: disabledTaskLookup(42001, 71) });
    if (url.pathname === '/api/v1/explore') return route.fulfill({ json: { rows: [{ ...row, key: 'message:42001', title: 'Budget', preview: 'Please review the budget', occurred_at: '2026-01-01T12:00:00Z', source_id: 3, participant_labels: [], participant_ids: [], attachment_count: 0, attachment_size: 0, has_attachments: false }], total_count: 1, cache_revision: 'test', search_provenance: {} } });
    if (url.pathname === '/api/v1/messages/42001') return route.fulfill({ json: message });
    if (url.pathname === '/api/v1/conversations/71') return route.fulfill({ json: { id: 71, anchor_id: 42001, messages: [message], has_before: false, has_after: false, total: 1 } });
    if (url.pathname === '/api/v1/integrations/kata/open-issues') return route.fulfill({ json: { issues: [{ uid: 'one', ref: 'abcd', qualified_ref: 'example#abcd', project: 'example', title: 'Budget review', status: 'open', revision: '1' }], truncated: false } });
    if (url.pathname === '/api/v1/integrations/kata/evidence/prepare') return route.fulfill({ json: { evidence: [{ id: 'one', passage: 'one', reference: { version: 1, kind: 'message', archive_uid: 'archive-example', message_id: 42001, source_type: 'email', source_identifier: 'example', source_message_id: 'source-message', message: { body_sha256: 'a'.repeat(64), start_rune: 0, end_rune: 24 } }, excerpt: 'Please review the budget', display: {}, content_trust: 'untrusted' }] } });
    return route.fulfill({ status: 404, json: { message: 'Unexpected archive request' } });
  });
  await page.goto('/messages/42001');
  await page.getByRole('button', { name: /^More actions for / }).click();
  await page.getByRole('menuitem', { name: 'Add to task…' }).click();
  await page.getByRole('radio', { name: 'Kata' }).click();
  await page.getByRole('radio', { name: 'Existing' }).click();
  const result = page.getByRole('radio', { name: 'Budget review · example#abcd' });
  await expect(result).toBeVisible();
  const bounds = await result.locator('xpath=ancestor::div[contains(@class,"results")][1]').boundingBox();
  const hint = await page.getByText('Search by whole word or enter a ref.').boundingBox();
  const gap = hint!.y - (bounds!.y + bounds!.height);
  expect(gap).toBeGreaterThanOrEqual(0);
  expect(gap).toBeLessThanOrEqual(12);
});
