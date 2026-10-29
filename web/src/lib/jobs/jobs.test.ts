import { describe, expect, it } from 'vitest';
import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import { collectJobs, jobsHealth } from './jobs';

const latest = { id: 'run', lane: 'documents', kind: 'document_extraction', state: 'failed', started_at: '2026-10-28T11:00:00Z', finished_at: '2026-10-28T11:05:00Z', trigger: 'manual', counters: [], error: { code: 'timeout', message: 'Provider request timed out\nprivate detail' } } as const;
const relatedStatus = { source_sync: 'listSourceStatus', carddav_sync: 'getCardDAVStatus', document_extraction: 'getDocumentIndexStatus', document_embedding: 'getDocumentVectorStatus', visual_embedding: 'getVisualAttachmentStatus' } as const;
const lane = (kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus => ({ lane: 'documents', kind, related_status: relatedStatus[kind as keyof typeof relatedStatus], configured: true, history_availability: 'available', supported_actions: [], latest: { ...latest, kind, counters: [] }, ...extra });

describe('jobs', () => {
  it.each([['carddav_sync', 'CardDAV sync'], ['document_embedding', 'Document embedding'], ['document_extraction', 'Document extraction'], ['message_embedding', 'Message embedding'], ['person_embedding', 'Person embedding'], ['person_enrichment', 'Person enrichment'], ['person_sweep', 'Person fact sweep'], ['visual_embedding', 'Visual embedding']] as const)('includes %s with the daemon error first', (kind, name) => {
    const [job] = collectJobs([], [lane(kind)]);
    expect(job?.name).toBe(name);
    expect(job?.detail).toBe('Provider request timed out');
    expect(job?.state).toBe('failed');
  });
  it('routes related status before settings, and uses the owning settings authority otherwise', () => {
    const jobs = collectJobs([], [lane('document_extraction'), lane('message_embedding')]);
    expect(jobs[0]?.target).toEqual({ kind: 'status', authority: 'getDocumentIndexStatus' });
    expect(jobs[1]?.target).toEqual({ kind: 'setting', target: { settingsCategory: 'search', settingsAuthority: 'semantic_search' } });
  });
  it('counts source sync once per source and includes scheduler errors', () => {
    const source = { id: 1, display_name: 'Example archive', identifier: 'archive@example.com', updated_at: '2026-10-28T10:00:00Z', scheduler_last_error: 'Schedule failed\nmore detail' } as SourceStatus;
    expect(collectJobs([source], [lane('source_sync')])).toEqual([{ id: 'source:1', name: 'Example archive', state: 'failed', detail: 'Schedule failed', at: undefined, target: { kind: 'source' } }]);
  });

  it('shows running work and prioritizes a failure in the dot', () => {
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
