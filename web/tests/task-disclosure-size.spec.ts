import { expect, test } from '@playwright/test';
import { recordingMessage } from '../src/lib/archive/recording-fixtures.test-support';
import { taskLookup } from '../src/lib/components/tasks/fixtures.test-support';
import { stubOperationStatus } from './kit-ui';

test('task disclosure is as readable as the form labels', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const message = { ...recordingMessage, id: 42, conversation_id: 7, subject: 'Follow up', body: 'A short passage.', from: 'sender@example.com', to: ['team@example.com'], sent_at: '2026-07-18T12:00:00Z', message_type: 'email', has_attachments: false, attachments: [] };
  const entry = { key: 'message:42', kind: 'message', message_type: 'email', conversation_type: 'email', title: 'Follow up', preview: 'A short passage.', occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_identifier: 'archive@example.com', source_type: 'synthetic', participant_labels: ['Example'], participant_ids: [1], conversation_id: 7, anchor_message_id: 42, message_count: 1, match: {} };
  const state = { schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [], groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }], columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {}, activeRow: null, selectedRow: null, inspectorPinned: false, conversationAnchor: null, scrollAnchor: null };
  await page.route('**/api/session', route => route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [] } }));
  await stubOperationStatus(page);
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: { rows: [entry], total_count: 1, cache_revision: 'fixture', search_provenance: {} } }));
  await page.route('**/api/v1/conversations/7?*', route => route.fulfill({ json: { id: 7, messages: [message], anchor_id: 42, has_before: false, has_after: false, total: 1 } }));
  await page.route('**/api/v1/integrations/{tasks,kata}/status', route => route.fulfill({ json: { state: 'ready', project: 'example' } }));
  await page.route('**/api/v1/messages/42/tasks', route => route.fulfill({ json: taskLookup() }));
  await page.route('**/api/v1/integrations/kata/open-issues', route => route.fulfill({ json: { issues: [{ uid: 'one', ref: 'abcd', qualified_ref: 'example#abcd', project: 'example', title: 'Follow up', status: 'open', revision: '1' }], truncated: false } }));
  await page.route('**/api/v1/integrations/kata/evidence/prepare', route => route.fulfill({ json: { evidence: [{ id: 'one', passage: 'one', excerpt: 'A short passage.', reference: { version: 1, kind: 'message', archive_uid: 'archive-example', message_id: 42, source_type: 'email', source_identifier: 'archive@example.com', source_message_id: '42', message: { body_sha256: 'a'.repeat(64), start_rune: 0, end_rune: 16 } }, display: {}, content_trust: 'untrusted' }] } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Follow up', { exact: true }).click();
  await page.getByRole('article', { name: 'Message 42' }).getByRole('button', { name: /More actions for/ }).click();
  await page.getByRole('menuitem', { name: 'Add to task…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Add to task' });
  const disclosure = dialog.getByText('Bodies and attachments are never sent.');
  const project = dialog.getByText('Project', { exact: true });
  await expect(disclosure).toHaveCSS('font-size', await project.evaluate(el => getComputedStyle(el).fontSize));
});
