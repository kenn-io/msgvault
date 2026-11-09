import { addressFixture, identityKeyFixture } from '../../directory/merge-fixtures.test-support';
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
    labels: { profiles: [], participant_ids: [], addresses: [], absorbed_address_count: 0, incomplete: true },
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
  // Saved addresses load on their own after Split opens; these tests read them settled.
  if (await controller.openSplit()) await vi.waitFor(() => expect(controller.sourceSaved ?? controller.sourceSavedError).not.toBeNull());
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
  it('keeps provider ordinals distinct through selection, preview, and receipt', async () => {
    const choices = ['WhatsApp · Garden member (2)', 'WhatsApp · Garden member (1)'];
    // Reverse ID order so participant collision ordinals follow identity, rather than list position.
    const ids = [702, 701];
    const addresses = ids.map(id => addressFixture(id, 'beeper', `beeper:8:whatsapp:3:${id}`, { participant_display_name: 'Garden member', service_slug: 'whatsapp', service_label: 'WhatsApp' }));
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
    const { controller } = await preparedController(fetchFn, participants, () => ({ profiles: [], participants, participant_ids: ids, addresses, absorbed_address_count: 2, incomplete: false }));
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
    expect(document.body.textContent).not.toContain('beeper:8:');
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
    expect(controller.selectedParticipantIDs).toEqual([701]);
    expect(within(screen.getByRole('group', { name: 'After the split' })).getByText('mira@example.org')).toBeDefined();
    expect(controller.confirmSplit()).toBe(true);
    controller.destroy();
  });

  it.each([false, true])('names both people and places the restore result beside them: exact=%s', async exact => {
    const response = { ...result(exact), source_person: person(12, 5, [999], 'Synthetic Source') };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/7') return Response.json(person(7));
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701, 999], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/12/profile') return Response.json(emptyProfile(response.source_person));
      if (path === '/api/v1/people/19/profile') return Response.json(emptyProfile(response.new_person));
      if (path === '/api/v1/people/12/split' && request.method === 'POST') return Response.json(response);
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const { controller } = await preparedController(fetchFn, undefined, () => ({ ...mergeDetail().labels, incomplete: false,
      participant_ids: [701, 999], addresses: [
      { participant_id: 701, type: 'email', value: 'alex@example.org', normalized_value: 'alex@example.org', display_value: '', service_slug: 'email', service_label: 'Email', scope_kind: '', scope_value: '' },
      { participant_id: 701, type: 'phone', value: '+12025550101', normalized_value: '+12025550101', display_value: '', service_slug: 'phone', service_label: 'Phone', scope_kind: '', scope_value: '' },
      { participant_id: 701, type: 'slack', value: 'T_EXAMPLE:U_NERI', normalized_value: 'T_EXAMPLE:U_NERI', display_value: '', service_slug: 'slack', service_label: 'Slack', scope_kind: 'workspace', scope_value: 'T_EXAMPLE' },
    ].map(a => addressFixture(a.participant_id, a.type, a.value, a)).concat([addressFixture(701, 'email', 'neri@example.org'), addressFixture(999, 'email', 'later@example.org')]) }));
    const onOpenPerson = vi.fn();
    render(PersonSplitSteps, { controller, onOpenPerson });

    const label = 'alex@example.org, +12025550101, Slack · workspace 1, neri@example.org';
    expect(screen.getByText('None chosen yet')).toBeDefined();
    await fireEvent.click(screen.getByRole('checkbox', { name: label }));
    expect(screen.getByRole('group', { name: 'Addresses to move' })).toBeDefined();
    const [stays, moves] = within(screen.getByRole('group', { name: 'After the split' })).getAllByRole('region');
    expect(within(stays).getByRole('heading').textContent).toBe('Stays with Synthetic Source');
    expect(within(moves).getByRole('heading').textContent).toBe('New person');
    expect(within(moves).getByText(label)).toBeDefined();
    expect(within(stays).queryByText(label)).toBeNull();
    await act(() => { controller.confirmSplit(); return controller.submitSplit(); });

    expect(await screen.findByRole('button', { name: 'Open Synthetic Restored' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open Synthetic Source' }).closest('.detail-row')?.textContent).toContain('later@example.org');
    expect(document.body.textContent?.toLowerCase()).not.toContain('undo');
    if (exact) {
      // An exact split keeps later edits on their records, wherever those now are, and leaves deleted records deleted,
      // so the one explanation above the people names neither profile as their owner.
      const sentence = screen.getByText(/fully reversed/);
      expect(sentence.compareDocumentPosition(screen.getByLabelText('Split result people')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(sentence.textContent).toMatch(/edits and deletions made since .* kept/);
      expect(sentence.textContent).not.toMatch(/Synthetic Source|Synthetic Restored|put back/);
      expect(screen.queryByText('Partly restored')).toBeNull();
    } else {
      // The partial-restore lines are shown under the two people, not folded away.
      const details = screen.getAllByRole('listitem').map(item => item.textContent);
      expect(details).toEqual(['Partly restored', "1 record couldn't be restored", 'Links to the merged-in profile now open the new person']);
      expect(screen.getByText('Partly restored').closest('details')).toBeNull();
      expect(screen.queryByText(/fully reversed/)).toBeNull();
      expect(screen.getByLabelText('Split result people').compareDocumentPosition(screen.getByRole('list')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
    expect(screen.getByText(`New person · ${label}`)).toBeDefined();
    expect(document.body.textContent).not.toMatch(/concealed-key|uid_alias/i);
    await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic Source' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic Restored' }));
    expect(onOpenPerson.mock.calls).toEqual([[12], [19]]);
  });

  it('shows a known participant with no stored address without offering Retry', async () => {
    const { controller } = await preparedController(async input => new URL((input as Request).url).pathname.endsWith('/profile')
      ? Response.json({ ...emptyProfile(person(12)), contact_points: [] }) : sourceResponse(), undefined, () => ({ ...mergeDetail().labels, incomplete: false, participant_ids: [701] }));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByRole('checkbox', { name: 'No addresses' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });

  it('splits a saved-only profile without calling it addressless', async () => {
    let posted: Request | undefined;
    const saved = { person_id: 19, identity_key: identityKeyFixture('email', 'saved@example.org'), address_kind: 'email', original_value: 'saved@example.org', normalized_value: 'saved@example.org', normalization: 'email', normalization_version: 1, envelope: {} };
    const response = { ...result(true), source_person: person(12, 5, [], 'Synthetic Source'), new_person: person(19, 1, [], 'Synthetic Restored') };
    const { controller } = await preparedController(async input => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/12/profile') return Response.json({ ...emptyProfile(response.source_person), contact_points: posted ? [] : [{ ...saved, person_id: 12 }] });
      if (path === '/api/v1/people/19/profile') return Response.json({ ...emptyProfile(response.new_person), contact_points: [saved] });
      if (path === '/api/v1/people/12/split' && request.method === 'POST') { posted = request; return Response.json(response); }
      throw new Error(`Unexpected ${request.method} ${path}`);
    }, [], () => ({ profiles: [], participant_ids: [], addresses: [], absorbed_address_count: 0, incomplete: false }));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByText('saved@example.org')).toBeDefined();
    expect(screen.getByText('Splitting restores its profile history.')).toBeDefined();
    await act(() => { expect(controller.confirmSplit()).toBe(true); return controller.submitSplit(); });
    expect(await posted!.json()).toMatchObject({ participant_ids: [] });
    expect(screen.getByRole('button', { name: 'Open Synthetic Restored' }).closest('.detail-row')?.textContent).toContain('Saved: saved@example.org');
    controller.destroy();
  });

  it('disables the address selection while the split is submitted', async () => {
    let resolve!: (response: Response) => void;
    const { controller } = await preparedController(async input => (input as Request).method === 'GET' ? sourceResponse() : new Promise<Response>(done => resolve = done));
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Unknown address 1' }));
    let submitted!: Promise<void>;
    await act(() => { controller.confirmSplit(); submitted = controller.submitSplit(); });
    expect(screen.getByRole('checkbox', { name: 'Unknown address 1' })).toHaveProperty('disabled', true);
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
    expect(controller.confirmSplit()).toBe(true);
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
    expect(sourceReads).toBe(4);
    expect(screen.queryByText(/^This profile changed since you opened it\.$/)).toBeNull();
    controller.destroy();
  });
});

describe('saved contacts in Split', () => {
  const contact = (id: number, address_kind: string, original_value: string) => ({
    person_id: id, address_kind, original_value, identity_key: identityKeyFixture(address_kind, original_value), normalized_value: original_value, normalization: address_kind, normalization_version: 1,
    envelope: { id: id * 10, source: 'user', ordinal: 0, actor: 'user', confidence: 1, active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} },
  });
  const structured = (profile: ReturnType<typeof person>, contact_points: ReturnType<typeof contact>[]) => ({
    person: profile, contact_points, names: [], addresses: [], dates: [], categories: [], media: [],
  });
  const catalogContacts = (id: number) => [
    { ...contact(id, 'username', 'Desk 4455'), service_slug: 'pager', service_label: 'Team hotline', identity_key: identityKeyFixture('pager', 'Desk 4455'), normalization: 'none' },
    contact(id, 'email', 'plain@example.org'),
  ];
  const savedLabels = () => ({ ...mergeDetail().labels, incomplete: false,
    profiles: [{ id: 7, participant_ids: [], contact_points: [contact(7, 'email', 'kept@example.org')] }, { id: 9, participant_ids: [701], contact_points: [contact(9, 'phone', '+12025550144')] }],
    participant_ids: [701], addresses: [addressFixture(701, 'email', 'guest@example.org')],
  });

  it('shows the saved contacts actually present on both profiles in the exact receipt', async () => {
    let committed = false;
    const source = person(12, 4, [701], 'Synthetic Source');
    const splitResult = result(true);
    const { controller } = await preparedController(async (input) => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/12/split' && request.method === 'POST') { committed = true; return Response.json(splitResult); }
      if (path === '/api/v1/people/12/profile') return Response.json(structured(committed ? splitResult.source_person : source, [contact(12, 'email', 'kept@example.org'), ...(!committed ? [contact(12, 'phone', '+12025550144')] : [])]));
      if (path === '/api/v1/people/19/profile') return Response.json(structured(splitResult.new_person, [contact(19, 'phone', '+12025550144')]));
      if (path === '/api/v1/people/12') return Response.json(source, { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/19') return Response.json(splitResult.new_person);
      throw new Error(`Unexpected request: ${path}`);
    }, mergeDetail().participants, savedLabels);
    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    const stayed = screen.getByRole('button', { name: 'Open Synthetic Source' }).closest('.detail-row')!;
    const moved = screen.getByRole('button', { name: 'Open Synthetic Restored' }).closest('.detail-row')!;
    expect.soft(stayed.textContent).toContain('kept@example.org');
    expect.soft(moved.textContent).toContain('+12025550144');
    expect.soft(stayed.textContent).not.toContain('+12025550144');
    controller.destroy();
  });

  it.each(['every', 'some', 'also saved'] as const)('lists unmatched saved addresses beside the choices: %s', async choice => {
    const participants = [701, 703].map(participant_id => ({ merge_id: 41, participant_id, origin_side: 'absorbed' }));
    const alsoSaved = choice === 'also saved';
    const { controller } = await preparedController(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path === '/api/v1/people/12/profile') return Response.json(structured(person(12, 4, [701, 703], 'Synthetic Source'), [contact(12, 'email', alsoSaved ? 'guest@example.org' : 'kept@example.org'), contact(12, 'phone', '+12025550144'), ...(choice === 'some' ? catalogContacts(12) : [])]));
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701, 703], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      throw new Error(`Unexpected request: ${path}`);
    }, participants, () => alsoSaved ? savedLabels() : { ...savedLabels(), participant_ids: [701, 703], addresses: [...savedLabels().addresses, addressFixture(703, 'email', 'other@example.org')] });
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    const guest = screen.getByRole('checkbox', { name: 'guest@example.org' });
    if (alsoSaved) {
      expect(document.getElementById(guest.getAttribute('aria-describedby')!)?.textContent).toBe('Also saved');
    }
    await fireEvent.click(guest);
    if (choice === 'every') await fireEvent.click(screen.getByRole('checkbox', { name: 'other@example.org' }));
    const [stays, moves] = within(screen.getByRole('group', { name: 'After the split' })).getAllByRole('region');
    if (alsoSaved) expect(moves.textContent).toContain('guest@example.org');
    else {
      expect(within(stays).queryAllByRole('listitem').map(item => item.textContent)).toEqual(choice === 'every' ? [] : ['other@example.org']);
      expect(within(moves).queryByText('+12025550144')).toBeNull();
    }
    const saved = screen.getByRole('region', { name: 'Saved on Synthetic Source' });
    expect(within(saved).getByText('Saved on Synthetic Source now. The result shows where each one ends up.')).toBeDefined();
    expect(within(saved).getAllByRole('listitem').map(item => item.textContent)).toEqual(alsoSaved ? ['+12025550144'] : ['kept@example.org', '+12025550144', ...(choice === 'some' ? ['Desk 4455 · Team hotline', 'plain@example.org'] : [])]);
    controller.destroy();
  });

  it('marks a matching receipt address as saved once alongside catalog labels', async () => {
    const number = '+12025550144';
    const scope = { service_slug: 'phone', service_label: 'Phone', scope_kind: '', scope_value: '' };
    const points = [
      { ...contact(19, 'phone', number), ...scope, original_value: '+1 (202) 555-0144' },
      ...catalogContacts(19),
    ];
    let committed = false;
    const { controller } = await preparedController(async input => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (path === '/api/v1/people/12/split' && request.method === 'POST') { committed = true; return Response.json(result(true)); }
      if (path === '/api/v1/people/12/profile') return Response.json(structured(committed ? result(true).source_person : person(12, 4, [701], 'Synthetic Source'), committed ? [] : points.map(point => ({ ...point, person_id: 12 }))));
      if (path === '/api/v1/people/19/profile') return Response.json(structured(result(true).new_person, points));
      if (path === '/api/v1/people/12') return sourceResponse();
      throw new Error(`Unexpected request: ${path}`);
    }, undefined, () => ({ ...savedLabels(), participant_ids: [701], addresses: [addressFixture(701, 'phone', number, scope)] }));
    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    const receipt = screen.getByRole('button', { name: 'Open Synthetic Restored' }).closest('.detail-row')!;
    expect.soft(receipt.textContent?.match(/\+1(?:[ ()-]*\d){10}/g)).toHaveLength(1);
    expect.soft(receipt.textContent).toContain('Saved:');
    expect.soft(receipt.textContent).toContain('Desk 4455 · Team hotline');
    expect.soft(receipt.textContent).toContain('plain@example.org');
    controller.destroy();
  });

  it('shows the committed receipt and connects Retry to saved-address recovery', async () => {
    const controller = new PersonMergeHistoryController(createAPIClient(vi.fn()), 7);
    controller.committedResult = { result: result(true), receiptETags: { source: null, created: null } };
    controller.receiptSavedError = 'Profile store busy';
    const retry = vi.spyOn(controller, 'retryReceiptSaved').mockResolvedValue();
    render(PersonSplitSteps, { controller, onOpenPerson: vi.fn() });
    expect(screen.getByText("The split is done, but saved addresses couldn't be loaded.")).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open Synthetic Restored' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(retry).toHaveBeenCalledOnce();
    controller.destroy();
  });
});
