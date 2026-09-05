import type { OperationRunSummary } from './models';

export interface OperationFocusAnchor {
  readonly kind: OperationRunSummary['kind'];
  readonly lane: OperationRunSummary['lane'];
  readonly trigger: OperationRunSummary['trigger'];
  readonly startedAt: string;
  readonly collisionOrdinal: number;
  readonly slot: number;
}

export function orderedOperationRows(rows: readonly OperationRunSummary[]): OperationRunSummary[] {
  return [...rows].sort((left, right) => Date.parse(right.started_at) - Date.parse(left.started_at));
}

function matches(anchor: OperationFocusAnchor, row: OperationRunSummary): boolean {
  return row.kind === anchor.kind && row.lane === anchor.lane && row.trigger === anchor.trigger &&
    row.started_at === anchor.startedAt;
}

export function operationFocusAnchor(
  rows: readonly OperationRunSummary[],
  runID: string
): OperationFocusAnchor | undefined {
  const ordered = orderedOperationRows(rows);
  const slot = ordered.findIndex((row) => row.id === runID);
  if (slot < 0) return undefined;
  const selected = ordered[slot]!;
  const base: OperationFocusAnchor = {
    kind: selected.kind,
    lane: selected.lane,
    trigger: selected.trigger,
    startedAt: selected.started_at,
    collisionOrdinal: 0,
    slot
  };
  return {
    ...base,
    collisionOrdinal: ordered.slice(0, slot).filter((row) => matches(base, row)).length
  };
}

export function resolveOperationFocusAnchor(
  rows: readonly OperationRunSummary[],
  anchor: OperationFocusAnchor | undefined
): string | undefined {
  if (!anchor) return undefined;
  const ordered = orderedOperationRows(rows);
  const collisions = ordered.filter((row) => matches(anchor, row));
  return collisions[anchor.collisionOrdinal]?.id ?? ordered[anchor.slot]?.id;
}
