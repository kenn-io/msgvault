import { describe, expect, it } from 'vitest';
import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import { collectJobs, jobsHealth } from './jobs';

const latest = { id: 'run', lane: 'documents', kind: 'document_extraction', state: 'failed', started_at: '2026-10-28T11:00:00Z', finished_at: '2026-10-28T11:05:00Z', trigger: 'manual', counters: [], error: { code: 'timeout', message: 'Provider request timed out\nprivate detail' } } as const;
const lane = (kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus => ({ lane: 'documents', kind, configured: true, history_availability: 'available', supported_actions: [], latest: { ...latest, kind, counters: [] }, ...extra });

describe('jobs', () => {
  it.each(['carddav_sync', 'document_embedding', 'document_extraction', 'message_embedding', 'person_embedding', 'person_enrichment', 'person_sweep', 'visual_embedding'] as const)('includes %s with the daemon error first', kind => {
    const [job] = collectJobs([], [lane(kind)]);
    expect(job?.detail).toBe('Provider request timed out');
    expect(job?.state).toBe('failed');
  });
  it('routes related status before settings, and uses the owning settings authority otherwise', () => {
    const jobs = collectJobs([], [lane('document_extraction', { related_status: 'getDocumentIndexStatus' }), lane('message_embedding')]);
    expect(jobs[0]?.target).toEqual({ kind: 'status', authority: 'getDocumentIndexStatus' });
    expect(jobs[1]?.target).toEqual({ kind: 'setting', target: { settingsCategory: 'search', settingsAuthority: 'semantic_search' } });
  });
  it('counts source sync once per source and includes scheduler errors', () => {
    const source = { id: 1, display_name: 'Example archive', identifier: 'archive@example.com', updated_at: '2026-10-28T10:00:00Z', scheduler_last_error: 'Schedule failed\nmore detail' } as SourceStatus;
    expect(collectJobs([source], [lane('source_sync')])).toEqual([{ id: 'source:1', name: 'Example archive', state: 'failed', detail: 'Schedule failed', at: undefined, target: { kind: 'source' } }]);
  });
  it('keeps unresolved source and lane failures regardless of age', () => {
    const source = { id: 1, identifier: 'archive@example.com', latest_sync: { status: 'failed', started_at: '2026-10-28T10:00:00Z', error_message: 'Authorization expired\nmore detail' } } as SourceStatus;
    expect(collectJobs([source], [])[0]?.detail).toBe('Authorization expired');
    expect(collectJobs([], [lane('document_extraction', { latest: { ...latest, counters: [], finished_at: '2026-10-01T12:00:00Z' } })])[0]?.detail).toBe('Provider request timed out');
    source.latest_sync!.started_at = '2026-10-01T12:00:00Z';
    expect(collectJobs([source], [])[0]?.state).toBe('failed');
  });
  it('keeps active work regardless of age and prioritizes a failure in the dot', () => {
    const running = collectJobs([], [lane('document_extraction', { active: { ...latest, state: 'running', counters: [] } })]);
    expect(running[0]?.state).toBe('running');
    expect(jobsHealth(running)).toBe('running');
    expect(jobsHealth([...running, ...collectJobs([], [lane('message_embedding')])])).toBe('failed');
    expect(jobsHealth([])).toBe('ok');
  });
});

it('keeps standing scheduler errors and clears them only when the producer recovers', () => {
  const source = { id: 1, identifier: 'archive@example.com', updated_at: '2026-01-01T00:00:00Z', scheduler_last_error: 'Token expired' } as SourceStatus;
  expect(collectJobs([source], [])[0]?.detail).toBe('Token expired');
  source.scheduler_last_error = '';
  expect(collectJobs([source], [])).toEqual([]);
});
it.each([
  { name: 'old failed import with item errors', source: { latest_sync: { status: 'failed', started_at: '2020-01-01T00:00:00Z', errors_count: 2, item_errors: [{ error_message: 'Invalid MIME header' }, { error_message: 'Missing attachment' }] } }, lanes: [], expected: [['failed', 'Invalid MIME header · Missing attachment']] },
  { name: 'completed sync and partial lane warnings', source: { latest_sync: { status: 'completed', errors_count: 2 } }, lanes: [lane('document_extraction', { latest: { ...latest, state: 'partial', counters: [] } })], expected: [] },
  { name: 'completed sync with scheduler failure', source: { latest_sync: { status: 'completed' }, scheduler_last_error: 'Token expired' }, lanes: [], expected: [['failed', 'Token expired']] },
  { name: 'disabled lane with a stale failure', lanes: [lane('document_extraction', { configured: false })], expected: [] },
  { name: 'disabled lane still running', lanes: [lane('document_extraction', { configured: false, active: { ...latest, state: 'running', counters: [] } })], expected: [['running', 'Running']] },
  { name: 'scheduler running before a sync row exists', source: { sync_unavailable_reason: 'sync_already_running' }, lanes: [], expected: [['running', 'Syncing']] },
])('$name', ({ source, lanes, expected }) => {
  expect(collectJobs(source ? [{ id: 1, identifier: 'archive@example.com', ...source } as SourceStatus] : [], lanes).map(({ state, detail }) => [state, detail])).toEqual(expected);
});
