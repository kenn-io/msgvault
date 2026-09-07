import { describe, expect, it } from 'vitest';

import type { OperationRunSummary } from './models';
import { operationFocusAnchor, resolveOperationFocusAnchor } from './focus';

function row(id: string, startedAt = '2026-08-30T10:00:00Z'): OperationRunSummary {
  return {
    id, kind: 'source_sync', lane: 'messages', state: 'succeeded', trigger: 'manual',
    started_at: startedAt, finished_at: '2026-08-30T10:01:00Z', counters: []
  };
}

describe('operation focus anchors', () => {
  it('resolves the exact collision ordinal after every opaque reference rotates', () => {
    const before = [row('old-first'), row('old-second'), row('old-third', '2026-08-30T09:00:00Z')];
    const anchor = operationFocusAnchor(before, 'old-second');

    expect(anchor).toMatchObject({ collisionOrdinal: 1, slot: 1 });
    expect(resolveOperationFocusAnchor(
      [row('new-first'), row('new-second'), row('new-third', '2026-08-30T09:00:00Z')],
      anchor
    )).toBe('new-second');
  });

  it('uses the original visible slot only when the immutable tuple disappears', () => {
    const anchor = operationFocusAnchor([row('old-first'), row('old-second')], 'old-second');
    expect(resolveOperationFocusAnchor([
      row('replacement-first', '2026-08-30T11:00:00Z'),
      row('replacement-second', '2026-08-30T09:00:00Z')
    ], anchor)).toBe('replacement-second');
  });
});
