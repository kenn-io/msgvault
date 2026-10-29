import { describe, expect, it } from 'vitest';
import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import { collectJobs, jobsHealth } from './jobs';

const now = Date.parse('2026-10-28T12:00:00Z');
const latest = { id: 'run', lane: 'documents', kind: 'document_extraction', state: 'failed', started_at: '2026-10-28T11:00:00Z', finished_at: '2026-10-28T11:05:00Z', trigger: 'manual', counters: [], error: { code: 'timeout', message: 'Provider request timed out\nprivate detail' } } as const;
const lane = (kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus => ({ lane: 'documents', kind, configured: true, history_availability: 'available', supported_actions: [], latest: { ...latest, kind, counters: [] }, ...extra });

describe('jobs', () => {
  it.each(['carddav_sync', 'document_embedding', 'document_extraction', 'message_embedding', 'person_embedding', 'person_enrichment', 'person_sweep', 'visual_embedding'] as const)('includes %s with the daemon error first', kind => {
    const [job] = collectJobs([], [lane(kind)], now);
    expect(job?.detail).toBe('Provider request timed out');
    expect(job?.state).toBe('failed');
  });
  it('routes related status before settings, and uses the owning settings authority otherwise', () => {
    const jobs = collectJobs([], [lane('document_extraction', { related_status: 'getDocumentIndexStatus' }), lane('message_embedding')], now);
    expect(jobs[0]?.target).toEqual({ kind: 'status', authority: 'getDocumentIndexStatus' });
    expect(jobs[1]?.target).toEqual({ kind: 'setting', target: { settingsCategory: 'search', settingsAuthority: 'semantic_search' } });
  });
  it('counts source sync once per source and includes scheduler errors', () => {
    const source = { id: 1, display_name: 'Example archive', identifier: 'archive@example.com', updated_at: '2026-10-28T10:00:00Z', scheduler_last_error: 'Schedule failed\nmore detail' } as SourceStatus;
    expect(collectJobs([source], [lane('source_sync')], now)).toEqual([{ id: 'source:1', name: 'Example archive', state: 'failed', detail: 'Schedule failed', at: source.updated_at, target: { kind: 'source', id: 1 } }]);
  });
  it('uses raw source errors and excludes failures older than seven days', () => {
    const source = { id: 1, identifier: 'archive@example.com', latest_sync: { status: 'failed', started_at: '2026-10-28T10:00:00Z', error_message: 'Authorization expired\nmore detail' } } as SourceStatus;
    expect(collectJobs([source], [], now)[0]?.detail).toBe('Authorization expired');
    expect(collectJobs([], [lane('document_extraction', { latest: { ...latest, counters: [], finished_at: '2026-10-20T12:00:00Z' } })], now)).toEqual([]);
  });
  it('keeps active work regardless of age and prioritizes a failure in the dot', () => {
    const running = collectJobs([], [lane('document_extraction', { active: { ...latest, state: 'running', counters: [] } })], now);
    expect(running[0]?.state).toBe('running');
    expect(jobsHealth(running)).toBe('running');
    expect(jobsHealth([...running, ...collectJobs([], [lane('message_embedding')], now)])).toBe('failed');
    expect(jobsHealth([])).toBe('ok');
  });
});
