import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import PersonMergeHistory from './PersonMergeHistory.svelte';

type MergeDetail = components['schemas']['PersonMergeDetail'];

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function detail(currentPersonID: number | null = 12): MergeDetail {
  return {
    merge: {
      id: 41,
      survivor_person_id: 7,
      absorbed_person_id: 9,
      current_person_id: currentPersonID ?? undefined,
      survivor_vcard_uid: 'synthetic-7',
      absorbed_vcard_uid: 'synthetic-9',
      survivor_revision_before: 3,
      absorbed_revision_before: 2,
      survivor_revision_after: 4,
      actor: 'web',
      snapshot_version: 1,
      snapshot_sha256: 'synthetic-digest',
      created_at: '2026-08-03T00:00:00Z'
    },
    participants: [
      { merge_id: 41, participant_id: 701, origin_side: 'absorbed' },
      { merge_id: 41, participant_id: 702, origin_side: 'survivor', split_id: 55 }
    ],
    rows: [{
      merge_id: 41,
      table_name: 'person_names',
      original_row_key: 'opaque-row-key-must-not-appear',
      snapshot_path: 'private/snapshot/path-must-not-appear',
      action: 'restored',
      origin_side: 'absorbed',
      provenance_kind: 'copied',
      participant_id: 701
    }],
    splits: [{
      id: 55, merge_id: 41, source_person_id: 12, new_person_id: 19,
      new_person_uid: 'synthetic-19', source_revision_before: 4, source_revision_after: 5,
      exact_reversal: false, actor: 'web', created_at: '2026-08-04T00:00:00Z'
    }],
    review_candidates: [{
      id: 61, merge_id: 41, person_id: 12, definition_id: 3,
      survivor_value_id: 81, absorbed_value_id: 82, resolution_value_id: 83,
      state: 'resolved', reviewed_at: '2026-08-05T00:00:00Z', reviewed_by: 'reviewer',
      created_at: '2026-08-03T00:00:00Z'
    }]
  };
}

function summary(mergeDetail: MergeDetail) {
  return {
    merge: mergeDetail.merge,
    participant_count: 2,
    pending_candidate_count: 0,
    row_action_counts: { restored: 1 },
    row_count: 1,
    split_count: 1
  };
}

function renderHistory(currentPersonID: number | null = 12) {
  const mergeDetail = detail(currentPersonID);
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/merges') {
      return Response.json({ merges: [summary(mergeDetail)], limit: 100, offset: 0 });
    }
    if (path === '/api/v1/person-merges/41/snapshot') {
      return Response.json({ version: 1, sha256: 'synthetic-digest', snapshot: { explicitly_revealed: 'synthetic content' } });
    }
    return Response.json(mergeDetail);
  });
  render(PersonMergeHistory, { client: createAPIClient(fetchFn), personID: 7 });
  return { requests };
}

describe('PersonMergeHistory', () => {
  it('renders semantic safe history/detail tables without eagerly fetching or exposing provenance internals', async () => {
    const { requests } = renderHistory();

    const history = await screen.findByRole('table', { name: 'Person merge history' });
    expect(within(history).getByRole('columnheader', { name: 'Merge' })).toBeDefined();
    await fireEvent.click(within(history).getByRole('button', { name: 'Inspect merge 41' }));

    const participants = await screen.findByRole('table', { name: 'Merge participants' });
    expect(within(participants).getByText('absorbed')).toBeDefined();
    expect(within(participants).getByText('Not split')).toBeDefined();
    const rows = screen.getByRole('table', { name: 'Merge row dispositions' });
    for (const heading of ['Table', 'Action', 'Origin', 'Provenance', 'Participant', 'Disposition']) {
      expect(within(rows).getByRole('columnheader', { name: heading })).toBeDefined();
    }
    expect(screen.getByRole('table', { name: 'Prior splits' })).toBeDefined();
    expect(screen.getByRole('table', { name: 'Merge review candidates' })).toBeDefined();
    expect(document.body.textContent).not.toContain('opaque-row-key-must-not-appear');
    expect(document.body.textContent).not.toContain('private/snapshot/path-must-not-appear');
    expect(requests.map((request) => new URL(request.url).pathname)).not.toContain('/api/v1/person-merges/41/snapshot');
  });

  it('reveals the verified opaque snapshot only after the explicit action', async () => {
    const { requests } = renderHistory();
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));

    expect(screen.queryByText(/explicitly_revealed/)).toBeNull();
    await fireEvent.click(await screen.findByRole('button', { name: 'View verified snapshot' }));

    const region = await screen.findByRole('region', { name: 'Verified merge snapshot content' });
    expect(region.textContent).toContain('explicitly_revealed');
    expect(screen.getByText(/SHA-256 synthetic-digest/)).toBeDefined();
    expect(requests.filter((request) => new URL(request.url).pathname.endsWith('/snapshot'))).toHaveLength(1);
  });

  it('does not offer split when detail has no current person', async () => {
    renderHistory(null);
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));

    await screen.findByRole('table', { name: 'Merge participants' });
    expect(screen.queryByRole('button', { name: 'Split merged profile' })).toBeNull();
    expect(screen.getByText(/No current source profile is recorded/)).toBeDefined();
  });

  it('retains a connected focus target when an idle split dialog closes', async () => {
    const mergeDetail = detail();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/merges')) return Response.json({ merges: [summary(mergeDetail)], limit: 100, offset: 0 });
      if (path === '/api/v1/people/12') {
        return Response.json({
          id: 12, revision: 4, display_name: 'Synthetic Source', participant_ids: [701],
          created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-02T00:00:00Z', vcard_uid: 'synthetic-12'
        }, { headers: { ETag: '"person-12-r4"' } });
      }
      return Response.json(mergeDetail);
    });
    render(PersonMergeHistory, { client: createAPIClient(fetchFn), personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    const trigger = await screen.findByRole('button', { name: 'Split merged profile' });
    await fireEvent.click(trigger);
    await screen.findByRole('dialog', { name: 'Split merged profile' });
    const cancel = screen.getByRole('button', { name: 'Cancel' });
    await waitFor(() => expect(cancel).toHaveProperty('disabled', false));

    await fireEvent.click(cancel);

    await waitFor(() => expect(document.activeElement).toBe(trigger));
    expect(trigger.isConnected).toBe(true);
  });
});
