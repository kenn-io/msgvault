import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import type { PersonMergeDetail } from '../api/generated/models';
import { loadMergeLabels } from './merge-labels';

const detail: PersonMergeDetail = {
  rows: [], review_candidates: [],
  merge: { id: 41, survivor_person_id: 7 } as PersonMergeDetail['merge'],
  participants: [701, 702, 703, 704].map(participant_id => ({ merge_id: 41, participant_id, origin_side: 'absorbed' })),
  splits: [{ new_person_id: 19 } as NonNullable<PersonMergeDetail['splits']>[number]],
};

describe('loadMergeLabels', () => {
  it('keeps each participant’s own identifiers, all addresses, and member fallbacks', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.includes('/people/')) return Response.json({ display_name: path.endsWith('/7') ? ' Alex Example ' : 'Alex Restored' });
      return Response.json({
        identifiers: [
          { participant_id: 701, type: 'email', value: 'alex@example.com' },
          { participant_id: 701, type: 'phone', value: '+12025550123' },
          { participant_id: 702, type: 'handle', value: 'alex', scope_kind: 'workspace', scope_value: 'example.org' },
        ],
        cluster: { members: [{ participant_id: 703, display_name: 'Member Example', email: 'member@example.org' }] },
      });
    });
    const labels = await loadMergeLabels(createAPIClient(fetchFn), detail, new AbortController().signal);
    expect(labels.names).toEqual(new Map([[7, 'Alex Example'], [19, 'Alex Restored']]));
    expect(labels.addresses.get(701)?.map(row => row.title)).toEqual(['alex@example.com', '+12025550123']);
    expect(labels.addresses.get(702)).toEqual([{ title: 'handle', subtitle: '', detail: 'alex · workspace: example.org' }]);
    expect(labels.addresses.get(703)?.[0]?.title).toBe('Member Example');
    expect(labels.addresses.get(704)).toEqual([]);
    expect(labels.incomplete).toBe(false);
  });

  it.each(['503', 'throw'])('keeps successful results and treats each profile 404 independently of a %s failure', async failure => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/7')) return Response.json({}, { status: 404 });
      if (path.endsWith('/19')) return Response.json({ display_name: 'Alex Restored' });
      if (path.endsWith('/701')) {
        if (failure === 'throw') throw new Error('offline');
        return Response.json({}, { status: 503 });
      }
      return Response.json({ identifiers: [{ participant_id: 702, type: 'email', value: 'other@example.org' }] });
    });
    const labels = await loadMergeLabels(createAPIClient(fetchFn), detail, new AbortController().signal);
    expect(labels.names.has(7)).toBe(false);
    expect(labels.names.get(19)).toBe('Alex Restored');
    expect(labels.addresses.get(702)?.[0]?.title).toBe('other@example.org');
    expect(labels.incomplete).toBe(true);
  });

  it.each([
    [404, false],
    [503, true],
  ])('marks profile lookup incomplete only for non-404 failures, destination status %s', async (destinationStatus, incomplete) => {
    const labels = await loadMergeLabels(createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      return Response.json({}, { status: path.endsWith('/7') ? 404 : destinationStatus });
    }), { ...detail, participants: [] }, new AbortController().signal);
    expect(labels.names.size).toBe(0);
    expect(labels.incomplete).toBe(incomplete);
  });
});
