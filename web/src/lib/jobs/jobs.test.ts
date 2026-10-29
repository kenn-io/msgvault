import { describe, expect, it } from 'vitest';
import type { SourceStatus } from '../api/generated/models';
import { collectJobs, jobsHealth } from './jobs';

import { jobLane as lane, jobRun, sourceStatus, syncRunStatus } from './fixtures.test-support';
import canonical from '../../../tests/e2e/fixtures/operations-public.json';
const activeRun = canonical.runs.find(run => run.kind === 'visual_embedding') as import('../api/generated/models').OperationRunSummary;
const partial = canonical.runs.find(run => run.kind === 'document_extraction') as import('../api/generated/models').OperationRunSummary;

describe('jobs', () => {
  it.each(['carddav_sync', 'document_embedding', 'document_extraction', 'message_embedding', 'person_embedding', 'person_enrichment', 'person_sweep', 'visual_embedding'] as const)('includes failed %s with its daemon error', kind => {
    const [job] = collectJobs([], [lane(kind)]);
    expect(job?.detail).toBe(jobRun(kind).error!.message);
    expect(job?.state).toBe('failed');
    expect(job?.target).toEqual({ kind: 'history', operationKind: kind, operationRunID: jobRun(kind).id });
  });
  it.each([
    ['message_embedding', { settingsCategory: 'search', settingsAuthority: 'semantic_search' }],
    ['person_enrichment', { settingsCategory: 'enrichment', settingsAuthority: '' }],
  ] as const)('routes related status before %s settings', (kind, target) => {
    const jobs = collectJobs([], [lane('document_extraction', { active: { ...jobRun('document_extraction'), state: 'running', finished_at: undefined, error: undefined } }), lane(kind, { active: { ...jobRun(kind), state: kind === 'person_enrichment' ? 'queued' : 'running', finished_at: undefined, error: undefined } })]);
    expect(jobs[0]?.target).toEqual({ kind: 'status', authority: 'getDocumentIndexStatus' });
    expect(jobs[1]?.target).toEqual({ kind: 'setting', target });
  });
  it('counts source sync once per source and includes scheduler errors', () => {
    const source = sourceStatus({ scheduler_last_error: 'Schedule failed\nmore detail' });
    expect(collectJobs([source], [lane('source_sync')])).toEqual([{ id: 'source:1', name: 'Example archive', state: 'failed', detail: 'Schedule failed', at: undefined, target: { kind: 'source' } }]);
  });

  it('shows running work and prioritizes a failure in the dot', () => {
    const running = collectJobs([], [lane('visual_embedding', { active: activeRun })]);
    const queued = collectJobs([], [lane('person_enrichment', { active: { ...jobRun('person_enrichment'), state: 'queued', finished_at: undefined, error: undefined } })]);
    expect(queued[0]?.detail).toBe('Queued');
    expect(jobsHealth(queued)).toBe('queued');
    expect(jobsHealth([...queued, ...running])).toBe('running');
    expect(jobsHealth([...running, ...collectJobs([], [lane('message_embedding')])])).toBe('failed');
    expect(jobsHealth([])).toBe('ok');
  });
});


it.each([
  { name: 'old failed import with item errors', source: { latest_sync: syncRunStatus({ status: 'failed', started_at: '2020-01-01T00:00:00Z', completed_at: '2020-01-01T00:00:04Z', errors_count: 2, item_errors: ['Invalid MIME header', 'Missing attachment'].map((error_message, index) => ({ error_message, created_at: '2020-01-01T00:00:04Z', error_kind: 'parse_failed', phase: 'parse', source_message_id: `synthetic-${index}` })) }) }, lanes: [], expected: [['failed', 'Invalid MIME header · Missing attachment']] },
  { name: 'completed sync and partial lane warnings', source: { latest_sync: syncRunStatus({ errors_count: 2 }) }, lanes: [lane('document_extraction', { latest: partial })], expected: [] },
  { name: 'disabled lane with a stale failure', lanes: [lane('document_extraction', { configured: false })], expected: [] },
  { name: 'disabled lane still running', lanes: [lane('visual_embedding', { configured: false, active: activeRun })], expected: [['running', 'Running']] },
  { name: 'queued source with completed sync', source: { scheduler_queued: true, sync_unavailable_reason: 'sync_already_running', latest_sync: syncRunStatus() }, lanes: [], expected: [['queued', 'Queued']] },
  { name: 'pending source', source: { scheduler_pending: true, sync_unavailable_reason: 'sync_already_running' }, lanes: [], expected: [['queued', 'Queued']] },
  { name: 'failed source with a queued retry', source: { scheduler_queued: true, latest_sync: syncRunStatus({ status: 'failed', error_message: 'Mailbox unavailable' }) }, lanes: [], expected: [['failed', 'Mailbox unavailable']] },
  { name: 'pending source with scheduler failure', source: { scheduler_pending: true, scheduler_last_error: 'Token expired' }, lanes: [], expected: [['failed', 'Token expired']] },
  { name: 'active sync with queued follow-up and old failure', source: { scheduler_queued: true, active_sync: syncRunStatus({ status: 'running', completed_at: null }), latest_sync: syncRunStatus({ status: 'failed', error_message: 'Old failure' }) }, lanes: [], expected: [['running', 'Syncing']] },
  { name: 'scheduler active with a pending follow-up', source: { scheduler_pending: true, scheduler_started_at: '2026-08-30T11:00:00Z', sync_unavailable_reason: 'sync_already_running' }, lanes: [], expected: [['running', 'Syncing']] },
  { name: 'latest running import without an active snapshot', source: { source_type: 'mbox', latest_sync: syncRunStatus({ status: 'running', completed_at: null }), sync_unavailable_reason: 'source_not_schedulable' }, lanes: [], expected: [['running', 'Syncing']] },
  { name: 'scheduler running before a sync row exists', source: { scheduler_started_at: '2026-08-30T11:00:00Z', sync_unavailable_reason: 'sync_already_running' }, lanes: [], expected: [['running', 'Syncing']] },
])('$name', ({ source, lanes, expected }) => {
  expect(collectJobs(source ? [sourceStatus(source as Partial<SourceStatus>)] : [], lanes).map(({ state, detail }) => [state, detail])).toEqual(expected);
});
