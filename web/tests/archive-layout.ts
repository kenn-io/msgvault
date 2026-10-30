import type { Page } from '@playwright/test';

export const row = {
  key: 'message:1',
  kind: 'message',
  message_type: 'email',
  conversation_type: 'email',
  title: 'Synthetic archive subject',
  preview: 'Synthetic analytical excerpt for visual verification.',
  occurred_at: '2026-07-18T12:00:00Z',
  source_id: 1,
  source_identifier: 'archive@example.com',
  source_type: 'synthetic',
  participant_labels: ['Example Person With A Long Archive Display Name'],
  participant_ids: [1],
  attachment_count: 1,
  attachment_size: 2048,
  has_attachments: true,
  deleted_from_source: false,
  message_count: 1,
  match: {}
};

export async function mockArchiveLayout(page: Page) {
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/settings', (route) => route.fulfill({
    headers: { ETag: '"synthetic-settings"' },
    json: {
      groups: [{ id: 'browser', label: 'Appearance', description: 'How the web app looks.' }],
      settings: [
        { key: 'web.theme', group: 'browser', label: 'Theme', description: 'Light, dark, or follow the system.', kind: 'string', value: { string: 'system' }, options: ['system', 'light', 'dark'], restart_required: true },
        { key: 'web.density', group: 'browser', label: 'Density', description: 'Spacing of tables and toolbars.', kind: 'string', value: { string: 'compact' }, options: ['compact', 'comfortable'], restart_required: true }
      ],
      pending_restart: true
    }
  }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({
    json: { rows: [row], total_count: 1, cache_revision: 'cache-theme', search_provenance: {} }
  }));
  await page.route('**/api/v1/explore/groups', (route) => route.fulfill({
    json: {
      rows: [{ key: '1', label: 'Synthetic source', count: 1, estimated_bytes: 2048, latest_at: '2026-07-18T12:00:00Z' }],
      total_count: 1, cache_revision: 'cache-theme', search_provenance: {}
    }
  }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({
    json: { saved_views: [] }
  }));
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [{
    id: 1, source_type: 'gmail', identifier: 'archive@example.com', display_name: 'Archive',
    last_sync_at: null, updated_at: row.occurred_at,
    active_sync: null, latest_sync: null, last_successful_sync: null,
    can_sync: false, scheduled: false, sync_unavailable_reason: 'sync_not_configured', next_sync_at: null
  }] } }));
  await page.route('**/api/v1/files/search', (route) => route.fulfill({
    json: {
      files: [{
        id: 1, key: 'file:1', entry_key: 'message:1', message_id: 1, conversation_id: 1,
        occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
        source_identifier: 'archive@example.com', containing_title: 'Synthetic archive subject',
        filename: 'SyntheticArchiveAttachmentWithAnUnbrokenDescriptiveFilename.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      }],
      total_count: 1, cache_revision: 'cache-theme', search_provenance: {}
    }
  }));
}
