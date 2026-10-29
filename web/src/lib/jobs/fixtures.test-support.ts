import type { OperationLaneStatus, OperationRunSummary, SourceStatus, SyncRunStatus } from '../api/generated/models';
import { lane, RELATED_STATUS } from '../../test/operations';
import canonical from '../../../tests/e2e/fixtures/operations-public.json';

const failedRuns = [...canonical.job_runs, ...canonical.runs.filter(run => run.state === 'failed')] as OperationRunSummary[];

export function jobRun(kind: OperationRunSummary['kind']): OperationRunSummary {
  return structuredClone(failedRuns.find(run => run.kind === kind)!);
}

export function jobLane(kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus {
  const latest = jobRun(kind);
  return lane(kind, { related_status: RELATED_STATUS[kind], ...(kind === 'carddav_sync' ? { supported_actions: ['carddav_sync'] } : {}), latest, ...extra });
}

export function sourceStatus(extra: Partial<SourceStatus> = {}): SourceStatus {
  const busy = !!(extra.active_sync || extra.scheduler_queued || extra.scheduler_pending || extra.scheduler_started_at);
  return { id: 1, identifier: 'archive@example.com', display_name: 'Example archive', source_type: 'gmail', updated_at: '2026-08-30T11:00:00Z', last_sync_at: null, active_sync: null, latest_sync: null, last_successful_sync: null, can_sync: !busy && !extra.sync_unavailable_reason, sync_unavailable_reason: busy ? 'sync_already_running' : undefined, scheduled: false, next_sync_at: null, ...extra };
}

export function syncRunStatus(extra: Partial<SyncRunStatus> = {}): SyncRunStatus {
  return { id: 1, source_id: 1, status: 'completed', started_at: '2026-08-30T11:00:00Z', completed_at: '2026-08-30T11:00:04Z', error_message: null, errors_count: 0, messages_added: 0, messages_processed: 0, messages_updated: 0, ...extra };
}
