import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import type { PersonMergeDetail } from '../api/generated/models';
import { loadMergeLabels } from './merge-labels';
import { mergeResult, person } from './person-fixtures.test-support';

const detail: PersonMergeDetail = {
  rows: [], review_candidates: [],
  merge: mergeResult(person(7)).merge,
  participants: [701, 702, 703, 704, 705, 706].map(participant_id => ({ merge_id: 41, participant_id, origin_side: 'absorbed' })),
  splits: [{
    id: 55, merge_id: 41, source_person_id: 7, new_person_id: 19, new_person_uid: 'synthetic-19',
    source_revision_before: 4, source_revision_after: 5, exact_reversal: false,
    actor: 'web', created_at: '2026-08-04T00:00:00Z',
  }],
};

describe('loadMergeLabels', () => {
  it.each([
    {
      source: 'cluster member',
      participant: () => ({ identifiers: [], cluster: { members: [701, 702].map(participant_id => ({ participant_id, display_name: 'Alex Example' })) } }),
      expected: [
        { title: 'Alex Example', subtitle: 'linked', detail: 'Participant 701' },
        { title: 'Alex Example', subtitle: 'linked', detail: 'Participant 702' },
      ],
    },
    {
      source: 'display name',
      participant: () => ({ display_name: 'Alex Example', identifiers: [] }),
      expected: [
        { title: 'Alex Example', subtitle: '', detail: 'Participant 701' },
        { title: 'Alex Example', subtitle: '', detail: 'Participant 702' },
      ],
    },
    {
      source: 'identifier',
      participant: (id: number) => ({ identifiers: [{ participant_id: id, type: 'handle', value: 'alex', scope_kind: 'workspace', scope_value: 'example.org' }] }),
      expected: [
        { title: 'handle', subtitle: '', detail: 'alex · workspace: example.org · Participant 701' },
        { title: 'handle', subtitle: '', detail: 'alex · workspace: example.org · Participant 702' },
      ],
    },
  ])('disambiguates identical $source labels while preserving their detail', async ({ participant, expected }) => {
    const labels = await loadMergeLabels(createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      return Response.json(path.includes('/participants/') ? participant(Number(path.split('/').pop())) : {});
    }), { ...detail, participants: detail.participants!.slice(0, 2) }, new AbortController().signal);
    expect(labels.addresses.get(701)).toEqual([expected[0]]);
    expect(labels.addresses.get(702)).toEqual([expected[1]]);
  });

  it('keeps each participant’s own identifiers, all addresses, and member fallbacks', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.includes('/people/')) return Response.json({ display_name: path.endsWith('/7') ? ' Alex Example ' : 'Alex Restored' });
      if (path.endsWith('/704')) return Response.json({ display_name: ' Singleton Example ', identifiers: [] });
      return Response.json({
        identifiers: [
          { participant_id: 701, type: 'email', value: 'alex@example.com' },
          { participant_id: 701, type: 'phone', value: '+12025550123' },
          { participant_id: 702, type: 'handle', value: 'alex', scope_kind: 'workspace', scope_value: 'example.org' },
        ],
        cluster: { members: [{ participant_id: 703, display_name: 'Member Example', email: 'member@example.org' }, { participant_id: 705, display_name: 'Member Example', phone: '+12025550124' }] },
      });
    });
    const labels = await loadMergeLabels(createAPIClient(fetchFn), detail, new AbortController().signal);
    expect(labels.names).toEqual(new Map([[7, 'Alex Example'], [19, 'Alex Restored']]));
    expect(labels.addresses.get(701)?.map(row => row.title)).toEqual(['alex@example.com', '+12025550123']);
    expect(labels.addresses.get(702)).toEqual([{ title: 'handle', subtitle: '', detail: 'alex · workspace: example.org' }]);
    expect(labels.addresses.get(703)).toEqual([{ title: 'Member Example', subtitle: 'member@example.org', detail: '' }]);
    expect(labels.addresses.get(704)).toEqual([{ title: 'Singleton Example', subtitle: '', detail: '' }]);
    expect(labels.addresses.get(705)).toEqual([{ title: 'Member Example', subtitle: '+12025550124', detail: '' }]);
    expect(labels.addresses.get(706)).toEqual([]);
    expect(labels.incomplete).toBe(false);
  });

  it.each([
    { failure: 'participant 503', destinationStatus: 200, incomplete: true, participants: detail.participants },
    { failure: 'participant throw', destinationStatus: 200, incomplete: true, participants: detail.participants },
    { failure: 'destination 404', destinationStatus: 404, incomplete: false, participants: [] },
    { failure: 'destination 503', destinationStatus: 503, incomplete: true, participants: [] },
  ])('keeps successful results and marks only non-404 failures incomplete: $failure', async ({ failure, destinationStatus, incomplete, participants }) => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/7')) return Response.json({}, { status: 404 });
      if (path.endsWith('/19')) return Response.json(destinationStatus === 200 ? { display_name: 'Alex Restored' } : {}, { status: destinationStatus });
      if (path.endsWith('/701')) {
        if (failure === 'participant throw') throw new Error('offline');
        return Response.json({}, { status: 503 });
      }
      return Response.json({ identifiers: [{ participant_id: 702, type: 'email', value: 'other@example.org' }] });
    });
    const labels = await loadMergeLabels(createAPIClient(fetchFn), { ...detail, participants }, new AbortController().signal);
    expect(labels.names.has(7)).toBe(false);
    if (destinationStatus === 200) {
      expect(labels.names.get(19)).toBe('Alex Restored');
      expect(labels.addresses.get(702)?.[0]?.title).toBe('other@example.org');
    } else {
      expect(labels.names.size).toBe(0);
    }
    expect(labels.incomplete).toBe(incomplete);
  });
});
