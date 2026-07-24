import { execFileSync } from 'node:child_process';
import { readFileSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Page, Route } from '@playwright/test';
import type { components } from '../../../src/lib/api/generated/schema';

export const CHAT_CONVERSATION_COUNT = 100;
export const RAW_CHAT_MESSAGE_COUNT = 100_000;
const FIXTURE_TIME = '2026-01-03T12:00:00Z';

const archivePerson = {
  id: 12,
  display_label: 'Archive Person',
  display_name: 'Archive Person',
  partial_label: false,
  identifiers: [{
    type: 'email', value: 'person@example.com', display_value: 'person@example.com',
    is_primary: true, provenance: 'participant_identifiers'
  }],
  activity_count: 3,
  file_count: 1,
  source_counts: [{ source_type: 'synthetic', count: 3 }],
  first_at: FIXTURE_TIME,
  last_at: FIXTURE_TIME,
  cache_revision: 'mixed-100k'
};

const archiveDomain = {
  domain: 'example.com', activity_count: 3, person_count: 1, file_count: 1,
  source_counts: [{ source_type: 'synthetic', count: 3 }], first_at: FIXTURE_TIME,
  last_at: FIXTURE_TIME, cache_revision: 'mixed-100k'
};

type MixedArchiveFixture = {
  rawChatMessageCount: number;
  chatConversationCount: number;
  logicalRows: components['schemas']['EntryRow'][];
  firstPage: components['schemas']['ExploreHTTPResponse'];
};

let fixturePromise: Promise<MixedArchiveFixture> | undefined;

export function loadMixedArchive(): Promise<MixedArchiveFixture> {
  fixturePromise ??= Promise.resolve().then(() => {
    const fixturePath = join(tmpdir(), `msgvault-mixed-archive-${process.pid}.json`);
    const repositoryRoot = dirname(fileURLToPath(new URL('../../../../package.json', import.meta.url)));
    execFileSync('go', [
      'test', '-tags', 'fts5 sqlite_vec', './internal/query',
      '-run', '^TestWriteMixedArchiveBrowserFixture$', '-count=1'
    ], {
      cwd: repositoryRoot,
      env: { ...process.env, MSGVAULT_MIXED_ARCHIVE_FIXTURE: fixturePath },
      stdio: ['ignore', 'pipe', 'pipe']
    });
    const fixture = JSON.parse(readFileSync(fixturePath, 'utf8')) as MixedArchiveFixture;
    unlinkSync(fixturePath);
    return fixture;
  });
  return fixturePromise;
}

export async function installMixedArchive(page: Page) {
  const fixture = await loadMixedArchive();
  await page.route('**/api/session', sessionRoute);
  await page.route('**/api/v1/settings', (route) => route.fulfill({ json: {
    settings: [
      { key: 'web.theme', value: { string: 'light' } },
      { key: 'web.density', value: { string: 'compact' } }
    ], pending_restart: false
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: fixture.firstPage }));
  await page.route('**/api/v1/explore/groups', (route) => route.fulfill({ json: {
    rows: [{ key: 'synthetic_chat', label: 'Synthetic chat', count: CHAT_CONVERSATION_COUNT,
      estimated_bytes: RAW_CHAT_MESSAGE_COUNT * 64, latest_at: '2026-01-02T12:00:00Z' }],
    total_count: 1, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/explore/preflight', (route) => route.fulfill({ json: {
    count: 1, estimated_bytes: 2048, cache_revision: 'mixed-100k', search_provenance: {},
    unavailable_actions: [], action_targets: [], operation_token: 'synthetic-operation',
    expires_at: '2026-01-03T12:05:00Z'
  } }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [{
      canonical_id: 12, display_label: 'Archive Person', last_at: FIXTURE_TIME, member_ids: [12], score: 1,
      signals: {
        last_interaction_at: FIXTURE_TIME, meeting_count: 0, meetings_together: 0, modalities: 1,
        received_from_them: 1, sent_count: 2, sent_to_them: 1
      }
    }],
    total_count: 1, cache_revision: 'mixed-100k', identity_revision: 1
  } }));
  await page.route('**/api/v1/relationships/12/timeline', (route) => route.fulfill({ json: {
    canonical_id: 12, identity_revision: 1, cache_revision: 'mixed-100k',
    rows: [fixture.logicalRows[0]], total_count: 1
  } }));
  await page.route('**/api/v1/people/search', (route) => route.fulfill({ json: {
    rows: [archivePerson], total_count: 1, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/people/12', (route) => route.fulfill({ json: archivePerson }));
  await page.route('**/api/v1/people/12/summary', (route) => route.fulfill({ json: {
    summary: archivePerson, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/people/12/timeline', (route) => route.fulfill({ json: {
    rows: [fixture.logicalRows[0]], total_count: 1,
    cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/people/12/files/search', (route) => route.fulfill({ json: {
    files: [], total_count: 0, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/domains/search', (route) => route.fulfill({ json: {
    rows: [archiveDomain], total_count: 1, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/domains/example.com', (route) => route.fulfill({ json: archiveDomain }));
  await page.route('**/api/v1/domains/example.com/summary', (route) => route.fulfill({ json: {
    summary: archiveDomain, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/domains/example.com/timeline', (route) => route.fulfill({ json: {
    rows: [fixture.logicalRows[0]], total_count: 1,
    cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/domains/example.com/files/search', (route) => route.fulfill({ json: {
    files: [], total_count: 0, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
    files: [{ id: 1, key: 'file:1', entry_key: 'message:100001', message_id: 100001,
      conversation_id: 100001, occurred_at: '2026-01-03T12:00:00Z', source_id: 1,
      source_type: 'synthetic', source_identifier: 'archive@example.com',
      containing_title: 'Synthetic email', filename: 'synthetic.txt', mime_type: 'text/plain',
      mime_family: 'text', size_bytes: 2048, content_state: 'unsupported', content_available: true }],
    total_count: 1, cache_revision: 'mixed-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/files/1', (route) => route.fulfill({ json: {
    id: 1, message_id: 100001, conversation_id: 100001, filename: 'synthetic.txt',
    mime_type: 'text/plain', size_bytes: 2048, content_hash: 'a'.repeat(64),
    content_state: 'local_content', content_available: true
  } }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [] } }));
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/v1/deletions', (route) => route.fulfill({ json: { manifests: [] } }));
  return fixture;
}

function sessionRoute(route: Route) {
  return route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } });
}
