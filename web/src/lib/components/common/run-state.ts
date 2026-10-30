import type { ChipTone } from '@kenn-io/kit-ui';

export type RunState = 'running' | 'succeeded' | 'partial' | 'failed' | 'queued' | 'cancelled';

/** The one run-state vocabulary: label and tone for every job, sync, and run. */
export const RUN_STATES: Record<RunState, { label: string; tone: ChipTone }> = {
  queued: { label: 'Queued', tone: 'info' },
  cancelled: { label: 'Cancelled', tone: 'muted' },
  running: { label: 'Running', tone: 'info' },
  succeeded: { label: 'Succeeded', tone: 'success' },
  partial: { label: 'Partial', tone: 'warning' },
  failed: { label: 'Failed', tone: 'danger' },
};
