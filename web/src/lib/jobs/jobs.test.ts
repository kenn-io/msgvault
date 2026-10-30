import { describe, expect, it } from 'vitest';
import type { SourceStatus } from '../api/generated/models';
import { collectJobs, jobsHealth } from './jobs';

import { jobLane as lane, jobRun, sourceStatus, syncRunStatus } from './fixtures.test-support';
import canonical from '../../../tests/e2e/fixtures/operations-public.json';
const activeRun = canonical.runs.find(run => run.kind === 'visual_embedding') as import('../api/generated/models').OperationRunSummary;
const partial = canonical.runs.find(run => run.kind === 'document_extraction') as import('../api/generated/models').OperationRunSummary;

describe('jobs', () => {
  it('includes a failed job with its daemon error and history target', () => {
    const [job] = collectJobs([], [lane('document_extraction')]);
    expect(job?.detail).toBe('Upstream operation failed.');
    expect(job?.state).toBe('failed');
    expect(job?.target).toEqual({ kind: 'history', operationKind: 'document_extraction', operationRunID: jobRun('document_extraction').id });
  });
  it('routes running and queued jobs to related status or settings', () => {
    const jobs = collectJobs([], [
      lane('document_extraction', { active: { ...jobRun('document_extraction'), state: 'running', finished_at: undefined, error: undefined } }),
      lane('message_embedding', { active: { ...jobRun('message_embedding'), state: 'running', finished_at: undefined, error: undefined } }),
      lane('person_enrichment', { active: { ...jobRun('person_enrichment'), state: 'queued', finished_at: undefined, error: undefined } }),
    ]);
    expect(jobs.map(job => job.target)).toEqual([
      { kind: 'status', authority: 'getDocumentIndexStatus' },
      { kind: 'setting', target: { settingsCategory: 'search', settingsAuthority: 'semantic_search' } },
      { kind: 'setting', target: { settingsCategory: 'enrichment', settingsAuthority: '' } },
    ]);
  });
  it('sorts failures first and prioritizes running work over queued work in health', () => {
    const running = collectJobs([], [lane('visual_embedding', { active: activeRun })]);
    const queued = collectJobs([], [lane('person_enrichment', { active: { ...jobRun('person_enrichment'), state: 'queued', finished_at: undefined, error: undefined } })]);
    const mixed = collectJobs([], [
      lane('visual_embedding', { active: activeRun }),
      lane('person_enrichment', { active: { ...jobRun('person_enrichment'), state: 'queued', finished_at: undefined, error: undefined } }),
      lane('message_embedding'),
    ]);
    expect(mixed.map(job => job.id)).toEqual(['message_embedding', 'visual_embedding', 'person_enrichment']);
    expect(queued[0]?.detail).toBe('Queued');
    expect(jobsHealth(queued)).toBe('queued');
    expect(jobsHealth([...queued, ...running])).toBe('running');
    expect(jobsHealth(mixed)).toBe('failed');
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
  { name: 'pending source with scheduler failure skips its source sync lane', source: { scheduler_pending: true, scheduler_last_error: 'Schedule failed' }, lanes: [lane('source_sync')], expected: [['failed', 'Schedule failed']] },
  { name: 'trims a scheduler error to its first line', source: { scheduler_last_error: 'Schedule failed\nmore detail' }, lanes: [], expected: [['failed', 'Schedule failed']] },
  { name: 'active sync with queued follow-up and old failure', source: { scheduler_queued: true, active_sync: syncRunStatus({ status: 'running', completed_at: null }), latest_sync: syncRunStatus({ status: 'failed', error_message: 'Old failure' }) }, lanes: [], expected: [['running', 'Syncing']] },
  { name: 'scheduler active with a pending follow-up', source: { scheduler_pending: true, scheduler_started_at: '2026-08-30T11:00:00Z', sync_unavailable_reason: 'sync_already_running' }, lanes: [], expected: [['running', 'Syncing']] },
  { name: 'latest running import without an active snapshot', source: { source_type: 'mbox', latest_sync: syncRunStatus({ status: 'running', completed_at: null }), sync_unavailable_reason: 'source_not_schedulable' }, lanes: [], expected: [['running', 'Syncing']] },
  { name: 'scheduler running before a sync row exists', source: { scheduler_started_at: '2026-08-30T11:00:00Z', sync_unavailable_reason: 'sync_already_running' }, lanes: [], expected: [['running', 'Syncing']] },
])('$name', ({ source, lanes, expected }) => {
  expect(collectJobs(source ? [sourceStatus(source as Partial<SourceStatus>)] : [], lanes).map(({ state, detail }) => [state, detail])).toEqual(expected);
});
