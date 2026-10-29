import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import { OPERATION_KIND_LABELS, OPERATION_SETUP, type OperationSettingsTarget, type RelatedStatus } from '../operations/labels';

export type JobState = 'running' | 'failed';
export type JobsHealth = 'ok' | 'running' | 'failed' | 'unavailable';
export type JobTarget =
  | { kind: 'source'; id: number }
  | { kind: 'contacts'; connection: string }
  | { kind: 'setting'; target: OperationSettingsTarget }
  | { kind: 'status'; authority: RelatedStatus }
  | { kind: 'none' };
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
    const base = { id: `source:${source.id}`, name: source.display_name || source.identifier, target: { kind: 'source' as const, id: source.id } };
    if (source.active_sync || source.scheduler_pending || source.scheduler_queued) {
      const processed = source.active_sync?.messages_processed;
      items.push({ ...base, state: 'running', detail: processed ? `${processed.toLocaleString()} messages so far` : 'Syncing', at: source.active_sync?.started_at ?? source.scheduler_started_at });
    } else {
      const run = source.latest_sync;
      const failedAt = run?.completed_at ?? run?.started_at;
      const runFailed = run && (run.status === 'failed' || run.status === 'partial' || run.errors_count > 0);
      if (runFailed || source.scheduler_last_error) {
        const diagnostics = runFailed
          ? [run.error_message, ...(run.item_errors ?? []).map(item => item.error_message)].map(message => firstLine(message, '')).filter(Boolean).join(' · ')
          : '';
        const fallback = runFailed && (run.status === 'completed' || run.status === 'partial') ? 'Finished with errors' : 'Sync failed';
        items.push({ ...base, state: 'failed', detail: diagnostics || firstLine(source.scheduler_last_error, fallback), at: runFailed ? failedAt ?? undefined : undefined });
      }
    }
  }
  for (const lane of lanes) {
    if (lane.kind === 'source_sync') continue;
    const setup = OPERATION_SETUP[lane.kind];
    const target: JobTarget = lane.related_status ? { kind: 'status', authority: lane.related_status }
      : setup?.kind === 'settings' ? { kind: 'setting', target: setup.target } : { kind: 'none' };
    const base = { id: lane.kind, name: OPERATION_KIND_LABELS[lane.kind], target };
    if (lane.active) {
      items.push({ ...base, state: 'running', detail: 'Running', at: lane.active.started_at });
    } else if (lane.latest && (lane.latest.state === 'failed' || lane.latest.state === 'partial')) {
      items.push({ ...base, state: 'failed', detail: firstLine(lane.latest.error?.message, lane.latest.state === 'partial' ? 'Finished with errors' : 'Failed'), at: lane.latest.finished_at ?? lane.latest.started_at });
    }
  }
  return items.sort((a, b) => a.state === b.state ? 0 : a.state === 'failed' ? -1 : 1);
}
export function jobsHealth(items: readonly JobItem[]): JobsHealth {
  if (items.some(item => item.state === 'failed')) return 'failed';
  return items.some(item => item.state === 'running') ? 'running' : 'ok';
}
