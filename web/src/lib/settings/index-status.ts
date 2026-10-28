import type { EmbeddingStatus, Status } from '../api/generated/models';

export interface IndexStatus {
  tone: 'success' | 'info' | 'warning';
  text: string;
}

/** The one line shown beside the Semantic search switch. */
export function messageIndexStatus(status: EmbeddingStatus | undefined): IndexStatus | undefined {
  if (!status) return undefined;
  if (!status.active_generation) return { tone: 'warning', text: 'Index not built' };
  const [current, eligible, failed, pending] = [status.current, status.eligible, status.failed, status.pending].map(
    (value) => Number(value) || 0,
  );
  const done = `${current.toLocaleString()} of ${eligible.toLocaleString()} messages indexed`;
  if (failed > 0) return { tone: 'warning', text: `${done} · ${failed.toLocaleString()} failed` };
  return { tone: pending > 0 ? 'info' : 'success', text: pending > 0 ? done : 'Ready' };
}

/** The one line shown beside the Visual search switch. */
export function visualIndexStatus(status: Status | undefined): IndexStatus | undefined {
  if (!status) return undefined;
  const [current, eligible] = [status.current, status.eligible].map((value) => Number(value) || 0);
  if (current >= eligible) return { tone: 'success', text: 'Ready' };
  return { tone: 'info', text: `${current.toLocaleString()} of ${eligible.toLocaleString()} files indexed` };
}
