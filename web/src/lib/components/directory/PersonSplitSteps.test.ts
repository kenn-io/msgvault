import { addressFixture } from '../../directory/merge-fixtures.test-support';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { mergeResult, person } from '../../directory/entity-fixtures.test-support';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonMergeDetail as GeneratedPersonMergeDetail } from '../../api/generated/models';
import { PersonMergeHistoryController } from '../../directory/person-merge-history-controller.svelte';
import PersonSplitSteps from './PersonSplitSteps.svelte';

type MergeDetail = GeneratedPersonMergeDetail;

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function mergeDetail(
  participants: MergeDetail['participants'] = [{ merge_id: 41, participant_id: 701, origin_side: 'absorbed' }],
): MergeDetail {
  return {
    merge: { ...mergeResult(person(7)).merge, current_person_id: 12 },
    labels: { profiles: [{ id: 7, participant_ids: [] }, { id: 9, participant_ids: [701] }], participant_ids: [], addresses: [], absorbed_address_count: 0, incomplete: true },
    participants,
    rows: [],
    splits: [],
    review_candidates: [],
  };
}

async function preparedController(fetchFn: typeof fetch, participants?: MergeDetail['participants'], labels?: () => MergeDetail['labels']) {
  const value = mergeDetail(participants);
  const routedFetch = vi.fn<typeof fetch>(async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (new URL(request.url).pathname === '/api/v1/person-merges/41') return Response.json({ ...value, labels: { ...(labels?.() ?? value.labels), participants: undefined } });
    return fetchFn(request);
  });
  const controller = new PersonMergeHistoryController(createAPIClient(routedFetch), 7);
  await controller.selectMerge(41);
  await controller.openSplit();
  return { controller };
}

function sourceResponse() {
  return Response.json(
    { ...person(12, 4, [701], 'Synthetic Source'), created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-02T00:00:00Z' },
    { headers: { ETag: '"person-12-r4"' } },
  );
}

function result(exactReversal: boolean) {
  return {
    exact_reversal: exactReversal,
    cache_state: 'ready' as const,
    identity_revision: 8,
    source_person: { ...person(12, 5, [], 'Synthetic Source'), created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-04T00:00:00Z' },
    new_person: { ...person(19, 1, [701], 'Synthetic Restored'), created_at: '2026-08-04T00:00:00Z', updated_at: '2026-08-04T00:00:00Z' },
    split: {
      id: 55,
      merge_id: 41,
      source_person_id: 12,
      new_person_id: 19,
      new_person_uid: 'synthetic-19',
      source_revision_before: 4,
      source_revision_after: 5,
      exact_reversal: exactReversal,
      actor: 'user',
      created_at: '2026-08-04T00:00:00Z',
    },
    ambiguous_rows: [],
    // Moving the merge's only absorbed address retargets its links; a row edited since the merge stays behind.
    unrestored_rows: exactReversal ? [] : [{ table_name: 'person_names', original_row_key: 'concealed-key', action: 'moved' }],
    uid_alias_disposition: 'retired_uid_alias_retargeted',
  };
}

describe('PersonSplitSteps', () => {
  it('keeps an unknown address selectable when labels fail and preserves it on retry', async () => {
    const participants = [701, 702].map(participant_id => ({ merge_id: 41, participant_id, origin_side: 'absorbed' }));
    let loads = 0;
    const { controller } = await preparedController(async () => Response.json(person(12, 4, [701, 702]), { headers: { ETag: '"person-12-r4"' } }),
      participants, () => ++loads <= 2 ? { ...mergeDetail().labels, participant_ids: [], addresses: [], incomplete: true } : { ...mergeDetail().labels, participant_ids: [701, 702], addresses: [addressFixture(701, 'email', 'person701@example.org'), addressFixture(702, 'email', 'person702@example.org')], incomplete: false });
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByText("Some addresses couldn't be loaded.")).toBeDefined();
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Unknown address 2' }));
    expect(controller.selectedParticipantIDs).toEqual([702]);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(controller.selectedParticipantIDs).toEqual([702]));
    expect(screen.getByRole('checkbox', { name: 'person702@example.org' })).toHaveProperty('checked', true);
    expect(screen.getByRole('checkbox', { name: 'person701@example.org' })).toHaveProperty('disabled', false);
    expect(controller.confirmSplit()).toBe(true);
    controller.destroy();
  });

  it('keeps provider ordinals distinct through selection, preview, and receipt', async () => {
    const choices = ['WhatsApp · Garden member (2)', 'WhatsApp · Garden member (1)'];
    // Reverse ID order so participant collision ordinals follow identity, rather than list position.
    const ids = [702, 701];
    const addresses = ids.map(id => addressFixture(id, 'beeper', `beeper:5:acctA:3:${id}`, { participant_display_name: 'Garden member', service_slug: 'whatsapp', service_label: 'WhatsApp' }));
    const participants = ids.map(participant_id => ({ merge_id: 41, participant_id, origin_side: 'absorbed' }));
    const response = { ...result(false), source_person: person(12, 5, [702], 'Synthetic Source') };
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (path === '/api/v1/people/12/split' && request.method === 'POST') return Response.json(response);
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, ids, 'Synthetic Source'), { headers: { ETag: '\"person-12-r4\"' } });
      if (path === '/api/v1/people/12/profile') return Response.json(emptyProfile(response.source_person));
      if (path === '/api/v1/people/19/profile') return Response.json({ ...emptyProfile(response.new_person), contact_points: [] });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const { controller } = await preparedController(fetchFn, participants, () => ({ ...mergeDetail().labels, participant_ids: ids, addresses, absorbed_address_count: 2, incomplete: false }));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getAllByRole('checkbox').map(choice => choice.getAttribute('aria-label') || choice.closest('label')?.textContent?.trim()).sort()).toEqual([...choices].sort());
    await fireEvent.click(screen.getByRole('checkbox', { name: choices[1] }));
    const preview = screen.getByRole('group', { name: 'After the split' });
    expect(within(preview).getByRole('region', { name: 'New person' }).textContent).toContain(choices[1]);
    expect(within(preview).getByRole('region', { name: 'Stays with Synthetic Source' }).textContent).toContain(choices[0]);
    // Selecting both and then only the second must preserve its original label.
    await fireEvent.click(screen.getByRole('checkbox', { name: choices[0] }));
    await fireEvent.click(screen.getByRole('checkbox', { name: choices[0] }));
    expect(controller.confirmSplit()).toBe(true);
    await act(() => controller.submitSplit());
    const receipt = screen.getByRole('button', { name: 'Open Synthetic Restored' }).closest('.detail-row')!;
    expect(receipt.textContent).toContain(choices[1]);
    expect(document.body.textContent).not.toContain('beeper:5:');
    expect(screen.getByRole('button', { name: 'Open Synthetic Source' }).closest('.detail-row')?.textContent).toContain(choices[0]);
    controller.destroy();
  });

  it('waits for the source profile before showing choices and accepts the first click', async () => {
    let release!: (value: Response) => void;
    const late = new Promise<Response>(resolve => { release = resolve; });
    const controller = new PersonMergeHistoryController(createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/person-merges/41')) return Response.json({ ...mergeDetail(), labels: { ...mergeDetail().labels, participants: undefined, profiles: [{ id: 7, participant_ids: [] }, { id: 9, participant_ids: [701] }], incomplete: false, participant_ids: [701, 999], addresses: [addressFixture(701, 'email', 'neri@example.org'), addressFixture(999, 'email', 'mira@example.org')] } });
      if (path.endsWith('/people/12')) return late;
      throw new Error(`Unexpected request: ${path}`);
    }), 7);
    await controller.selectMerge(41);
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    const opened = controller.openSplit();
    await waitFor(() => expect(controller.sourceLoading).toBe(true));
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.getByText('Loading this profile…')).toBeDefined();
    release(Response.json(person(12, 4, [701, 999]), { headers: { ETag: '"person-12-r4"' } }));
    await opened;
    await fireEvent.click(await screen.findByRole('checkbox', { name: 'neri@example.org' }));
    expect(within(screen.getByRole('group', { name: 'After the split' })).getByText('mira@example.org')).toBeDefined();
    controller.destroy();
  });

  const checkExact = () => {
    const sentence = screen.getByText(/fully reversed/);
    expect(sentence.compareDocumentPosition(screen.getByLabelText('Split result people')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(sentence.textContent).toMatch(/edits and deletions made since .* kept/);
    expect(sentence.textContent).not.toMatch(/Synthetic Source|Synthetic Restored|put back/);
    expect(screen.queryByText('Partly restored')).toBeNull();
  };
  it.each([
    { name: 'partial', exact: false, subset: false, addressName: false, receipt: 'New person · alex@example.org, +12025550101, neri@example.org', check: () => {
      expect(screen.getAllByRole('listitem').map(item => item.textContent)).toEqual(['Partly restored', "1 record couldn't be restored", 'Links to the merged-in profile now open the new person']);
      expect(screen.getByText('Partly restored').closest('details')).toBeNull();
      expect(screen.queryByText(/fully reversed/)).toBeNull();
      expect(screen.getByLabelText('Split result people').compareDocumentPosition(screen.getByRole('list')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    } },
    { name: 'exact', exact: true, subset: false, addressName: false, receipt: 'New person · alex@example.org, +12025550101, neri@example.org', check: checkExact },
    { name: 'subset', exact: false, subset: true, addressName: false, receipt: 'New person · alex@example.org, +12025550101, neri@example.org', check: () => {
      expect(screen.queryByText('Partly restored')).toBeNull();
      expect(screen.queryByText(/fully reversed/)).toBeNull();
      expect(screen.getByText('Links to the merged-in profile still open Synthetic Source')).toBeDefined();
    } },
    { name: 'address fallback', exact: true, subset: false, addressName: true, receipt: 'New person', check: checkExact },
  ])('names both people and places the restore result beside them: $name', async ({ exact, subset, addressName, receipt, check }) => {
    const label = addressName ? 'alex@example.org' : 'alex@example.org, +12025550101, neri@example.org';
    const restoredName = addressName ? label : 'Synthetic Restored';
    const response = { ...result(exact), source_person: person(12, 5, subset ? [702, 999] : [999], 'Synthetic Source'),
      new_person: person(19, 1, [701], restoredName),
      ...(subset ? { unrestored_rows: [], uid_alias_disposition: 'retired_uid_alias_unchanged' } : {}) };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/7') return Response.json(person(7));
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, subset ? [701, 702, 999] : [701, 999], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/12/profile') return Response.json(emptyProfile(response.source_person));
      if (path === '/api/v1/people/19/profile') return Response.json(emptyProfile(response.new_person));
      if (path === '/api/v1/people/12/split' && request.method === 'POST') return Response.json(response);
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const { controller } = await preparedController(fetchFn, subset ? [701, 702].map(participant_id => ({ merge_id: 41, participant_id, origin_side: 'absorbed' })) : undefined, () => ({ ...mergeDetail().labels, incomplete: false,
      participant_ids: subset ? [701, 702, 999] : [701, 999], addresses: [
      { participant_id: 701, type: 'email', value: 'alex@example.org', display_value: '', service_slug: 'email', service_label: '', scope_kind: '', scope_value: '' },
      { participant_id: 701, type: 'phone', value: '+12025550101', display_value: '', service_slug: 'phone', service_label: '', scope_kind: '', scope_value: '' },
    ].filter(a => !addressName || a.type === 'email').map(a => addressFixture(a.participant_id, a.type, a.value, a)).concat([...(addressName ? [] : [addressFixture(701, 'email', 'neri@example.org')]), ...(subset ? [addressFixture(702, 'email', 'person702@example.org')] : []), addressFixture(999, 'email', 'later@example.org')]) }));
    const onOpenPerson = vi.fn();
    render(PersonSplitSteps, { controller, onOpenPerson });

    expect(screen.getByText('None chosen yet')).toBeDefined();
    await fireEvent.click(screen.getByRole('checkbox', { name: label }));
    expect(screen.getByRole('group', { name: 'Addresses to move' })).toBeDefined();
    const [stays, moves] = within(screen.getByRole('group', { name: 'After the split' })).getAllByRole('region');
    expect(within(stays).getByRole('heading').textContent).toBe('Stays with Synthetic Source');
    expect(within(moves).getByRole('heading').textContent).toBe('New person');
    expect(within(moves).getByText(label)).toBeDefined();
    expect(within(stays).queryByText(label)).toBeNull();
    await act(() => { controller.confirmSplit(); return controller.submitSplit(); });

    expect(await screen.findByRole('button', { name: `Open ${restoredName}` })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open Synthetic Source' }).closest('.detail-row')?.textContent).toContain('later@example.org');
    expect(document.body.textContent?.toLowerCase()).not.toContain('undo');
    check();
    const row = screen.getByRole('button', { name: `Open ${restoredName}` }).closest('.detail-row') as HTMLElement;
    expect(within(row).getByText(receipt)).toBeDefined();
    expect(document.body.textContent).not.toMatch(/concealed-key|uid_alias/i);
    await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic Source' }));
    await fireEvent.click(screen.getByRole('button', { name: `Open ${restoredName}` }));
    expect(onOpenPerson.mock.calls).toEqual([[12], [19]]);
    controller.destroy();
  });

  it('shows a known participant with no stored address without offering Retry', async () => {
    const { controller } = await preparedController(async input => new URL((input as Request).url).pathname.endsWith('/profile')
      ? Response.json({ ...emptyProfile(person(12)), contact_points: [] }) : sourceResponse(), undefined, () => ({ ...mergeDetail().labels, incomplete: false, participant_ids: [701] }));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByRole('checkbox', { name: 'No addresses' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });

  it('splits a profile with no participant lineage', async () => {
    const response = { ...result(true), source_person: person(12, 5, [], 'Synthetic Source'), new_person: person(19, 1, [], 'Synthetic Restored') };
    const { controller } = await preparedController(async input => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/12/split' && request.method === 'POST') return Response.json(response);
      throw new Error(`Unexpected ${request.method} ${path}`);
    }, [], () => ({ ...mergeDetail().labels, profiles: [{ id: 7, participant_ids: [] }, { id: 9, participant_ids: [] }], incomplete: false }));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByText('Splitting restores its profile history.')).toBeDefined();
    await act(() => { controller.confirmSplit(); return controller.submitSplit(); });
    expect(screen.getByRole('button', { name: 'Open Synthetic Restored' })).toBeDefined();
    controller.destroy();
  });

  it('disables the address selection while the split is submitted', async () => {
    let resolve!: (response: Response) => void;
    const { controller } = await preparedController(async input => {
      const request = input as Request;
      if (request.method !== 'GET') return new Promise<Response>(done => resolve = done);
      return new URL(request.url).pathname.endsWith('/profile') ? Response.json(emptyProfile(person(12))) : sourceResponse();
    }, undefined, () => ({ ...mergeDetail().labels, incomplete: false, participant_ids: [701], addresses: [addressFixture(701, 'email', 'person701@example.org')] }));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'person701@example.org' }));
    let submitted!: Promise<void>;
    await act(() => { controller.confirmSplit(); submitted = controller.submitSplit(); });
    expect(screen.getByRole('checkbox', { name: 'person701@example.org' })).toHaveProperty('disabled', true);
    resolve(Response.json(result(true)));
    await act(() => submitted);
    expect(await screen.findByRole('button', { name: 'Open Synthetic Restored' })).toBeDefined();
  });

  it('keeps stale recovery visibly blocked and focuses Retry after failure and the checkbox after success', async () => {
    let sourceReads = 0;
    const { controller } = await preparedController(async input => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (request.method === 'POST') return Response.json({ error: 'person_merge_revision_conflict' }, { status: 409 });
      if (path === '/api/v1/people/12') return ++sourceReads > 1 && sourceReads < 4 ? Response.json({}, { status: 503 }) : sourceResponse();
      throw new Error(`Unexpected request: ${path}`);
    }, undefined, () => ({ ...mergeDetail().labels, incomplete: false, participant_ids: [701], addresses: [addressFixture(701, 'email', 'person701@example.org')] }));
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();
    await controller.submitSplit();
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByText(/^This profile changed since you opened it\.$/)).toBeDefined();
    const retry = screen.getByRole('button', { name: 'Retry' });
    await fireEvent.click(retry);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Retry' })));
    expect(screen.getByText(/^This profile changed since you opened it\.$/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    const participant = await screen.findByRole('checkbox', { name: 'person701@example.org' });
    await waitFor(() => expect(document.activeElement).toBe(participant));
    expect(screen.queryByText(/^This profile changed since you opened it\.$/)).toBeNull();
    controller.destroy();
  });
});
