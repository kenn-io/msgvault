import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import { operationStateChip, OPERATION_KIND_LABELS, OPERATION_SETUP, type OperationSettingsTarget, type RelatedStatus } from '../operations/labels';
import { syncRunning, sourceRunState } from '../sources/labels';

export type JobState = 'queued' | 'running' | 'failed';
export type JobsHealth = 'ok' | 'queued' | 'running' | 'failed' | 'unavailable';
export type JobTarget =
  | { kind: 'source' }
  | { kind: 'history'; operationKind: OperationLaneStatus['kind']; operationRunID: string }
  | { kind: 'setting'; target: OperationSettingsTarget }
  | { kind: 'status'; authority: RelatedStatus };
export interface JobItem {
  id: string;
  name: string;
  state: JobState;
  detail: string;
  at?: string;
  target: JobTarget;
}
const firstLine = (message: string | null | undefined, fallback: string): string => message?.split(/\r?\n/)[0]?.trim() || fallback;

export function collectJobs(sources: readonly SourceStatus[], lanes: readonly OperationLaneStatus[]): JobItem[] {
  const items: JobItem[] = [];
  for (const source of sources) {
    const base = { id: `source:${source.id}`, name: source.display_name || source.identifier, target: { kind: 'source' as const } };
    if (syncRunning(source) || source.scheduler_started_at) {
      const processed = source.active_sync?.messages_processed;
      items.push({ ...base, state: 'running', detail: processed ? `${processed.toLocaleString()} messages so far` : 'Syncing', at: source.active_sync?.started_at ?? source.scheduler_started_at });
    } else {
      const run = source.latest_sync;
      const failedAt = run?.completed_at ?? run?.started_at;
      const runFailed = sourceRunState(source) === 'failed';
      if (runFailed || source.scheduler_last_error) {
        const diagnostics = runFailed
          ? [run?.error_message, ...(run?.item_errors ?? []).map(item => item.error_message)].map(message => firstLine(message, '')).filter(Boolean).join(' · ')
          : '';
        items.push({ ...base, state: 'failed', detail: diagnostics || firstLine(source.scheduler_last_error, 'Sync failed'), at: runFailed ? failedAt ?? undefined : undefined });
      } else if (source.scheduler_queued || source.scheduler_pending) {
        items.push({ ...base, state: 'queued', detail: 'Queued' });
      }
    }
  }
  for (const lane of lanes) {
    if (lane.kind === 'source_sync') continue;
    const setup = OPERATION_SETUP[lane.kind];
    // Registered kinds without a settings setup always supply a related status.
    const target: JobTarget = !lane.related_status && setup?.kind === 'settings'
      ? { kind: 'setting', target: setup.target } : { kind: 'status', authority: lane.related_status! };
    const base = { id: lane.kind, name: OPERATION_KIND_LABELS[lane.kind], target };
    if (lane.active) {
      items.push({ ...base, state: lane.active.state === 'queued' ? 'queued' : 'running', detail: operationStateChip(lane.active.state).label, at: lane.active.started_at });
    } else if (lane.configured && lane.latest && operationStateChip(lane.latest.state).tone === 'danger') {
      items.push({ ...base, target: { kind: 'history', operationKind: lane.kind, operationRunID: lane.latest.id }, state: 'failed', detail: firstLine(lane.latest.error?.message, 'Failed'), at: lane.latest.finished_at ?? lane.latest.started_at });
    }
  }
  return items.sort((a, b) => Number(b.state === 'failed') - Number(a.state === 'failed'));
}
export function jobsHealth(items: readonly JobItem[]): JobsHealth {
  if (items.some(item => item.state === 'failed')) return 'failed';
  if (items.some(item => item.state === 'running')) return 'running';
  return items.some(item => item.state === 'queued') ? 'queued' : 'ok';
}
