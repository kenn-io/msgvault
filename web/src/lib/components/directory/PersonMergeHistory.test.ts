import { mergeResult, person } from '../../directory/entity-fixtures.test-support';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonMergeDetail as GeneratedPersonMergeDetail } from '../../api/generated/models';
import PersonMergeHistory from './PersonMergeHistory.svelte';

type MergeDetail = GeneratedPersonMergeDetail;

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function detail(currentPersonID: number | null = 12): MergeDetail {
  return {
    merge: { ...mergeResult(person(7)).merge, actor: 'user', current_person_id: currentPersonID ?? undefined },
    participants: [
      { merge_id: 41, participant_id: 701, origin_side: 'absorbed' },
      { merge_id: 41, participant_id: 702, origin_side: 'survivor', split_id: 55 },
    ],
    rows: [
      {
        merge_id: 41,
        table_name: 'person_names',
        original_row_key: 'opaque-row-key-must-not-appear',
        snapshot_path: 'private/snapshot/path-must-not-appear',
        action: 'restored',
        origin_side: 'absorbed',
        provenance_kind: 'copied',
        participant_id: 701,
      },
    ],
    splits: [
      {
        id: 55,
        merge_id: 41,
        source_person_id: 12,
        new_person_id: 19,
        new_person_uid: 'synthetic-19',
        source_revision_before: 4,
        source_revision_after: 5,
        exact_reversal: false,
        actor: 'user',
        created_at: '2026-08-04T00:00:00Z',
      },
    ],
    review_candidates: [
      { id: 60, merge_id: 41, person_id: 12, definition_id: 4, survivor_value_id: 84, absorbed_value_id: 85, state: 'pending', created_at: '2026-08-03T00:00:00Z' },
      {
        id: 61,
        merge_id: 41,
        person_id: 12,
        definition_id: 3,
        survivor_value_id: 81,
        absorbed_value_id: 82,
        resolution_value_id: 83,
        state: 'accepted',
        reviewed_at: '2026-08-05T00:00:00Z',
        reviewed_by: 'user',
        created_at: '2026-08-03T00:00:00Z',
      },
    ],
  };
}

function summary(mergeDetail: MergeDetail) {
  return {
    merge: mergeDetail.merge,
    participant_count: 2,
    pending_candidate_count: 0,
    row_action_counts: { restored: 1 },
    row_count: 1,
    split_count: 1,
  };
}

function renderHistory(currentPersonID: number | null = 12, options: { gone?: boolean; empty?: boolean; fail?: boolean } = {}) {
  const mergeDetail = detail(currentPersonID);
  if (options.empty) mergeDetail.participants = [];
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/merges') {
      return Response.json({ merges: [summary(mergeDetail)], limit: 100, offset: 0 });
    }
    if (path === '/api/v1/person-merges/41/snapshot') {
      return Response.json({
        version: 1,
        sha256: 'synthetic-digest',
        snapshot: { explicitly_revealed: 'synthetic content' },
      });
    }
    if (path === '/api/v1/people/7' && options.gone) return Response.json({}, { status: 404 });
    if (path.startsWith('/api/v1/people/')) return Response.json(person(Number(path.split('/').at(-1)), 1, [], path.endsWith('/19') ? 'Alex Restored' : 'Alex Example'));
    if (path.startsWith('/api/v1/participants/')) {
      if (options.fail) { options.fail = false; return Response.json({}, { status: 503 }); }
      const id = Number(path.split('/').at(-1));
      return Response.json({ identifiers: [{ participant_id: id, type: 'email', value: `address${id}@example.org` }] });
    }
    return Response.json(mergeDetail);
  });
  render(PersonMergeHistory, { client: createAPIClient(fetchFn), personID: 7, definitions: [{ id: 3, label: 'Role' }] });
  return { requests };
}

function renderRegressionHistory(mergeDetail: MergeDetail, missingAddress = false, missingDestination = false) {
  render(PersonMergeHistory, { client: createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/merges') return Response.json({ merges: [summary(mergeDetail)], limit: 100, offset: 0 });
    if (path === '/api/v1/person-merges/41') return Response.json(mergeDetail);
    if (path === '/api/v1/people/19' && missingDestination) return Response.json({}, { status: 404 });
    if (path.startsWith('/api/v1/people/')) {
      const id = Number(path.split('/').at(-1));
      return Response.json(person(id, 4, [701], id === 19 ? 'Alex Restored' : 'Alex Example'), { headers: { ETag: `"person-${id}-r4"` } });
    }
    if (path === '/api/v1/participants/701') return Response.json({ identifiers: missingAddress ? [] : [
      { participant_id: 701, type: 'email', value: 'alex@example.org' },
      { participant_id: 701, type: 'phone', value: '+12025550101' },
    ] });
    if (path === '/api/v1/participants/702') return Response.json({ identifiers: [{ participant_id: 702, type: 'email', value: 'other@example.org' }] });
    throw new Error(`Unexpected request: ${path}`);
  }), personID: 7 });
}

describe('PersonMergeHistory', () => {
  it.each([
    [false, false, 'Split out to Alex Restored'],
    [true, false, 'Split out to a profile'],
    [false, true, 'Split out'],
  ])('ties split addresses to their destination, missing name %s, missing split %s', async (missingDestination, missingSplit, expected) => {
    const mergeDetail = detail();
    if (missingSplit) mergeDetail.splits = [];
    renderRegressionHistory(mergeDetail, false, missingDestination);
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    const heading = await screen.findByRole('heading', { name: 'Message addresses from Alex Example' });
    expect(within(heading.parentElement!).getByText(expected)).toBeDefined();
  });

  it.each([false, true])('uses Inspect address labels in split choices and confirmation, missing address %s', async missingAddress => {
    renderRegressionHistory(detail(), missingAddress);
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    const heading = await screen.findByRole('heading', { name: 'Message addresses from the merged-in profile' });
    const labels = missingAddress ? ['Participant 701'] : ['alex@example.org', '+12025550101'];
    for (const label of labels) expect(within(heading.parentElement!).getByText(label)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Split merged profile' }));
    const choice = await screen.findByRole('checkbox', { name: labels.join(', ') });
    await fireEvent.click(choice);
    expect(screen.getByRole('checkbox', { name: `I confirm splitting ${labels.join(', ')} from Alex Example (Person 12).` })).toBeDefined();
  });

  it('titles a section containing only decided candidates Review decisions', async () => {
    const mergeDetail = detail();
    mergeDetail.review_candidates = mergeDetail.review_candidates!.filter(candidate => candidate.state !== 'pending');
    renderRegressionHistory(mergeDetail);
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    expect(await screen.findByRole('heading', { name: 'Review decisions' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Needs review' })).toBeNull();
    await fireEvent.click(screen.getByText('1 earlier decision'));
    expect(screen.getByText(/^Accepted · You ·/)).toBeDefined();
  });

  it('groups addresses, splits, and review decisions like profile history', async () => {
    const { requests } = renderHistory();
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    await screen.findByRole('heading', { name: 'Another profile merged into Alex Example' });
    expect(screen.getByText(/^You · Aug [23], 2026, \d{1,2}:00 [AP]M$/, { selector: 'p' })).toBeDefined();
    const absorbed = screen.getByRole('heading', { name: 'Message addresses from the merged-in profile' }).parentElement!;
    expect(within(absorbed).getByText('address701@example.org')).toBeDefined();
    const survivor = screen.getByRole('heading', { name: 'Message addresses from Alex Example' }).parentElement!;
    expect(within(survivor).getByText('address702@example.org')).toBeDefined();
    expect(within(survivor).getByText('Split out to Alex Restored')).toBeDefined();
    expect(screen.getByText('Alex Restored')).toBeDefined();
    expect(screen.getByText('Partial restore')).toBeDefined();
    expect(screen.getByText('Waiting for review')).toBeDefined();
    expect(screen.getByText('Field')).toBeDefined();
    await fireEvent.click(screen.getByText('1 earlier decision'));
    expect(screen.getByText('Role')).toBeDefined();
    expect(screen.getByText(/^Accepted · You ·/)).toBeDefined();
    expect(screen.queryByRole('table')).toBeNull();
    expect(requests.some(request => request.url.endsWith('/snapshot'))).toBe(false);
  });

  it('uses both surviving-profile fallbacks after a 404', async () => {
    renderHistory(12, { gone: true });
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    await screen.findByRole('heading', { name: 'Two profiles merged' });
    expect(screen.getByRole('heading', { name: 'Message addresses from the surviving profile' })).toBeDefined();
    expect(screen.queryByText(/couldn't be loaded/)).toBeNull();
  });

  it('explains a merge with no message addresses', async () => {
    renderHistory(12, { empty: true });
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    expect(await screen.findByText('No message addresses were linked to this merge.')).toBeDefined();
  });

  it('keeps available labels and retries a failed address lookup', async () => {
    renderHistory(12, { fail: true });
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    expect(await screen.findByText('Participant 701')).toBeDefined();
    expect(screen.getByText("Some names and addresses couldn't be loaded.")).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('address701@example.org')).toBeDefined();
    expect(screen.queryByText(/couldn't be loaded/)).toBeNull();
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

    await screen.findByRole('heading', { name: 'Another profile merged into Alex Example' });
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
        return Response.json(
          {
            id: 12,
            revision: 4,
            display_name: 'Synthetic Source',
            participant_ids: [701],
            created_at: '2026-08-01T00:00:00Z',
            updated_at: '2026-08-02T00:00:00Z',
            vcard_uid: 'synthetic-12',
          },
          { headers: { ETag: '"person-12-r4"' } },
        );
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
