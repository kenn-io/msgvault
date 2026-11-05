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
      { merge_id: 41, participant_id: 701, origin_side: 'absorbed', split_id: 55 },
      { merge_id: 41, participant_id: 702, origin_side: 'survivor' },
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
      { id: 62, merge_id: 41, person_id: 12, definition_id: 3, survivor_value_id: 86, absorbed_value_id: 87, state: 'pending', created_at: '2026-08-03T00:00:00Z' },
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
    participant_count: mergeDetail.participants?.length ?? 0,
    pending_candidate_count: mergeDetail.review_candidates?.filter(candidate => candidate.state === 'pending').length ?? 0,
    row_action_counts: { restored: 1 },
    row_count: 1,
    split_count: mergeDetail.splits?.length ?? 0,
  };
}

function renderHistory(mergeDetail = detail(), options: { gone?: boolean; missingDestination?: boolean; unnamedDestinations?: boolean; fail?: boolean } = {}) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/merges') {
      return Response.json({ merges: [summary(mergeDetail)], limit: 100, offset: 0 });
    }
    if (path === '/api/v1/person-merges/41') return Response.json(mergeDetail);
    if (path === '/api/v1/person-merges/41/snapshot') {
      return Response.json({
        version: 1,
        sha256: 'synthetic-digest',
        snapshot: { explicitly_revealed: 'synthetic content' },
      });
    }
    if (path === '/api/v1/people/7' && options.gone) return Response.json({}, { status: 404 });
    if (path === '/api/v1/people/19' && options.missingDestination) return Response.json({}, { status: 404 });
    if (path.startsWith('/api/v1/people/')) {
      const id = Number(path.split('/').at(-1));
      return Response.json(person(id, 4, [701], id === 20 && options.unnamedDestinations ? '' : id === 19 ? 'Alex Restored' : 'Alex Example'), { headers: { ETag: `"person-${id}-r4"` } });
    }
    if (path.startsWith('/api/v1/participants/')) {
      if (options.fail) { options.fail = false; return Response.json({}, { status: 503 }); }
      const id = Number(path.split('/').at(-1));
      return Response.json({ identifiers: [
        { participant_id: id, type: 'email', value: `address${id}@example.org` },
        ...(id === 701 ? [{ participant_id: id, type: 'phone', value: '+12025550101' }] : []),
      ] });
    }
    throw new Error(`Unexpected request: ${path}`);
  });
  render(PersonMergeHistory, { client: createAPIClient(fetchFn), personID: 7, definitions: [{ id: 3, label: 'Role' }] });
  return { requests };
}

describe('PersonMergeHistory', () => {
  it('distinguishes unnamed split destinations by their dates', async () => {
    const mergeDetail = detail();
    mergeDetail.participants = [701, 702].map((participant_id, index) => ({ merge_id: 41, participant_id, origin_side: 'absorbed', split_id: 55 + index }));
    mergeDetail.splits!.push({ ...mergeDetail.splits![0]!, id: 56, new_person_id: 20, created_at: '2026-09-05T12:00:00Z' });
    renderHistory(mergeDetail, { missingDestination: true, unnamedDestinations: true });

    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    const heading = await screen.findByRole('heading', { name: 'Message addresses from the merged-in profile' });
    const addresses = within(heading.parentElement!);
    expect(within(addresses.getByText('address701@example.org').closest('li')!).getByText(/^Split out on Aug [34], 2026,/)).toBeDefined();
    expect(within(addresses.getByText('address702@example.org').closest('li')!).getByText(/^Split out on Sep 5, 2026,/)).toBeDefined();
    const splits = within(screen.getByRole('heading', { name: 'Splits' }).parentElement!);
    expect(splits.getByText(/^Aug [34], 2026,/)).toBeDefined();
    expect(splits.getByText(/^Sep 5, 2026,/)).toBeDefined();
  });

  it.each([false, true])('shows a zero-address split with its destination missing %s', async missing => {
    const mergeDetail = detail();
    mergeDetail.participants = [];
    renderHistory(mergeDetail, { missingDestination: missing });
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    const heading = await screen.findByRole('heading', { name: 'Splits' });
    const splits = within(heading.parentElement!);
    expect(splits.getAllByRole('listitem')).toHaveLength(1);
    expect(splits.getByText(missing ? /^Aug [34], 2026, \d{1,2}:\d{2} [AP]M$/ : 'Alex Restored')).toBeDefined();
    expect(splits.getByText('Partial restore')).toBeDefined();
    expect(screen.getByText('No message addresses were linked to this merge.')).toBeDefined();
    expect(splits.getByText(missing ? 'You' : /^You · Aug [34], 2026, \d{1,2}:\d{2} [AP]M$/)).toBeDefined();
  });

  it('groups addresses and pending reviews like profile history', async () => {
    renderHistory();
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    await screen.findByRole('heading', { name: 'Another profile merged into Alex Example' });
    expect(screen.getByText(/2 addresses · split 1×/)).toBeDefined();
    expect(within(screen.getByRole('list', { name: 'Person merge history' })).getByText('Person 9 merged into person 7')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Next merge history page' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Previous merge history page' })).toBeNull();
    expect(screen.getByRole('heading', { name: 'Needs review' })).toBeDefined();
    expect(screen.getByText(/^You · Aug [23], 2026, \d{1,2}:\d{2} [AP]M$/, { selector: 'p' })).toBeDefined();
    const absorbed = screen.getByRole('heading', { name: 'Message addresses from the merged-in profile' }).parentElement!;
    expect(within(absorbed).getByText('address701@example.org')).toBeDefined();
    expect(within(absorbed).getByText('+12025550101')).toBeDefined();
    const survivor = screen.getByRole('heading', { name: 'Message addresses from Alex Example' }).parentElement!;
    expect(within(survivor).getByText('address702@example.org')).toBeDefined();
    expect(within(absorbed).getAllByText('Split out to Alex Restored')).toHaveLength(2);
    const reviews = within(screen.getByRole('heading', { name: 'Needs review' }).parentElement!);
    expect(reviews.getAllByRole('listitem')).toHaveLength(2);
    expect(reviews.getByText('Role')).toBeDefined();
    expect(reviews.getByText('Field')).toBeDefined();
    expect(document.body.textContent).not.toContain('opaque-row-key-must-not-appear');
    expect(document.body.textContent).not.toContain('private/snapshot/path-must-not-appear');
  });

  it('uses both surviving-profile fallbacks after a 404', async () => {
    renderHistory(detail(), { gone: true });
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    await screen.findByRole('heading', { name: 'Two profiles merged' });
    expect(screen.getByRole('heading', { name: 'Message addresses from the surviving profile' })).toBeDefined();
  });

  it('keeps available labels and retries a failed address lookup', async () => {
    renderHistory(detail(), { fail: true });
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
    renderHistory(detail(null));
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
      if (path === '/api/v1/person-merges/41') return Response.json(mergeDetail);
      if (path.startsWith('/api/v1/people/')) return Response.json(person(Number(path.split('/').at(-1)), 1, [], ''));
      if (path.startsWith('/api/v1/participants/')) return Response.json({ identifiers: [] });
      throw new Error(`Unexpected request: ${path}`);
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
