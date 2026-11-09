import { addressFixture, identityKeyFixture } from '../../directory/merge-fixtures.test-support';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
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
  const value = {
    merge: { ...mergeResult(person(7)).merge, actor: 'user', current_person_id: currentPersonID ?? undefined },
    participants: [
      { merge_id: 41, participant_id: 701, origin_side: 'absorbed' },
      { merge_id: 41, participant_id: 702, origin_side: 'survivor' },
      { merge_id: 41, participant_id: 703, origin_side: 'absorbed', split_id: 55 },
    ] as MergeDetail['participants'],
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
  const { participants: _, ...labels } = summary(value as MergeDetail).labels;
  return { ...value, labels };
}

function summary(mergeDetail: MergeDetail) {
  return {
    merge: mergeDetail.merge,
    labels: {
      profiles: [{ id: 7, display_name: 'Alex Example', participant_ids: [702] }, { id: 9, display_name: 'Alex Example · work', participant_ids: [701, 703] }],
      participants: mergeDetail.participants, participant_ids: mergeDetail.participants.map(({ participant_id }) => participant_id), absorbed_address_count: 3, incomplete: false,
      addresses: [701, 702, 703].flatMap(participant_id => [
        { participant_id, type: 'email', value: `address${participant_id}@example.org` },
        ...(participant_id === 701 ? [{ participant_id, type: 'phone', value: '+12025550101' }] : []),
      ]).map(identifier => addressFixture(identifier.participant_id, identifier.type, identifier.value, identifier)),
    },
    participant_count: mergeDetail.participants.length,
    pending_candidate_count: 0,
    row_action_counts: { restored: 1 },
    row_count: 1,
    split_count: 1,
  };
}

function addressIdentifiers(): MergeDetail['labels']['addresses'] {
  return [
    { participant_id: 701, type: 'email', value: 'alex@example.org' },
    { participant_id: 701, type: 'phone', value: '+12025550101', ...{ display_value: '+1 202 555 0101' } },
    { participant_id: 702, type: 'email', value: 'other@example.org' },
    { participant_id: 703, type: 'email', value: 'split@example.org' },
  ].map(identifier => addressFixture(identifier.participant_id, identifier.type, identifier.value, identifier));
}

function renderHistory(value: number | null | MergeDetail = 12, options: {
  unnamedSurvivor?: boolean; fail?: boolean; count?: number; addresses?: MergeDetail['labels']['addresses'];
  list?: MergeDetail[]; respond?: (path: string, request: Request) => Response | Promise<Response> | undefined;
  onSplitCommitted?: () => Promise<void>;
} = {}) {
  const mergeDetail = typeof value === 'object' && value !== null ? value : detail(value);
  if (options.addresses) mergeDetail.labels.addresses = options.addresses;
  if (options.count !== undefined) mergeDetail.labels.absorbed_address_count = options.count;
  const requests: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const path = new URL(request.url).pathname;
    const custom = options.respond?.(path, request);
    if (custom) return custom;
    const labels = { ...mergeDetail.labels, participants: mergeDetail.participants };
    if (options.unnamedSurvivor) labels.profiles = labels.profiles.map(profile => profile.id === 7 ? { ...profile, display_name: undefined } : profile);
    if (options.fail) { labels.addresses = []; labels.participant_ids = []; labels.incomplete = true; }
    // The list reads its own labels; `fail` breaks only the selected merge's.
    const listed = options.fail ? { ...labels, participant_ids: mergeDetail.labels.participant_ids, addresses: mergeDetail.labels.addresses, incomplete: false } : labels;
    if (path === '/api/v1/people/7/merges') return Response.json({ merges: (options.list ?? [mergeDetail]).map(item => ({ ...summary(item), labels: item === mergeDetail ? listed : { ...item.labels, participants: item.participants } })), limit: 100, offset: 0 });
    if (path.endsWith('/snapshot')) return Response.json({ version: 1, sha256: 'synthetic-digest', snapshot: { explicitly_revealed: 'synthetic content', persons: labels.profiles } });
    if (path === '/api/v1/people/12/profile') return Response.json(emptyProfile(person(12)));
    if (path.startsWith('/api/v1/people/')) {
      const id = Number(path.split('/').at(-1));
      return Response.json(person(id, 4, [701], id === 19 ? 'Alex Restored' : 'Alex Example'), { headers: { ETag: `"person-${id}-r4"` } });
    }
    const item = (options.list ?? [mergeDetail]).find(item => path === `/api/v1/person-merges/${item.merge.id}`);
    if (item) return Response.json({ ...item, labels: { ...(item === mergeDetail ? labels : item.labels), participants: undefined } });
    throw new Error(`Unexpected request: ${path}`);
  });
  const onOpenPerson = vi.fn();
  const onClose = vi.fn();
  render(PersonMergeHistory, { client, personID: 7, definitions: [{ id: 3, label: 'Role' }], onOpenPerson, onClose, onSplitCommitted: options.onSplitCommitted });
  return { requests, options, onOpenPerson, onClose };
}

function deferred<T = Response>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(next => { resolve = next; });
  return { promise, resolve };
}

const posts = (requests: Request[]) => requests.filter(request => request.method === 'POST');

describe('PersonMergeHistory', () => {
  it('lets the user choose addresses and leave while saved addresses and a past destination name load', async () => {
    const saved = deferred(), name = deferred();
    const { requests, onClose } = renderHistory(detail(), { respond: path => {
      if (path === '/api/v1/people/12/profile') return saved.promise;
      if (path === '/api/v1/people/19') return name.promise;
      return undefined;
    } });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Split…' }));
      const choice = await screen.findByRole('checkbox', { name: 'address701@example.org, +12025550101' });
      expect(choice).toHaveProperty('disabled', false);
      expect(screen.getByText('Loading saved addresses…')).toBeDefined();
      expect(screen.queryByRole('checkbox', { name: 'Unknown address 1' })).toBeNull();
      await fireEvent.click(choice);
      expect(choice).toHaveProperty('checked', true);
      expect(screen.getByRole('button', { name: 'Split into new person' })).toHaveProperty('disabled', false);
      expect(screen.getByRole('button', { name: 'Close' })).toHaveProperty('disabled', false);

      await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
      expect(onClose).toHaveBeenCalledOnce();
      expect(posts(requests)).toHaveLength(0);
    } finally {
      saved.resolve(Response.json({ ...emptyProfile(person(12)), contact_points: [] }));
      name.resolve(Response.json(person(19, 4, [703], 'Alex Restored')));
    }
  });

  it('prevents dismissal while a split is pending', async () => {
    const pending = deferred();
    renderHistory(detail(), { respond: (path, request) => {
      if (request.method === 'POST') return pending.promise;
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      return undefined;
    } });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Split…' }));
      await fireEvent.click(await screen.findByRole('checkbox', { name: 'address701@example.org, +12025550101' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Split into new person' }));
      await waitFor(() => expect(screen.getByRole('button', { name: 'Split into new person' }).getAttribute('aria-busy')).toBe('true'));
      expect(screen.getByRole('button', { name: 'Close' })).toHaveProperty('disabled', true);
    } finally { pending.resolve(Response.json({}, { status: 503 })); }
  });

  it('lets the user finish a committed split while the profiles refresh and the receipt loads', async () => {
    const reconciliation = deferred<void>(), receipt = [deferred(), deferred()];
    const onSplitCommitted = vi.fn(() => reconciliation.promise);
    let savedReads = 0;
    const result = { exact_reversal: true, cache_state: 'ready', identity_revision: 8,
      source_person: person(12, 5, [], 'Synthetic Source'), new_person: person(19, 1, [701], 'Synthetic Restored'),
      split: { ...detail().splits![0], id: 56, exact_reversal: true }, ambiguous_rows: [], unrestored_rows: [], uid_alias_disposition: 'restored' };
    const { onOpenPerson, onClose } = renderHistory(detail(), { onSplitCommitted, respond: (path, request) => {
      if (request.method === 'POST') return Response.json(result, { headers: { ETag: '"person-12-r5"', 'X-New-Person-ETag': '"person-19-r1"' } });
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      if (path.endsWith('/profile')) return ++savedReads === 1 ? Response.json({ ...emptyProfile(person(12)), contact_points: [] }) : receipt[savedReads - 2]!.promise;
      return undefined;
    } });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Split…' }));
      await fireEvent.click(await screen.findByRole('checkbox', { name: 'address701@example.org, +12025550101' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Split into new person' }));

      await screen.findByRole('dialog', { name: 'Split complete' });
      expect(screen.getByText('Loading saved addresses…')).toBeDefined();
      const done = screen.getByRole('button', { name: 'Done' });
      await waitFor(() => expect(done).toHaveProperty('disabled', false));
      expect(screen.getByRole('button', { name: 'Close' })).toHaveProperty('disabled', false);
      await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic Source' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic Restored' }));
      expect(onOpenPerson.mock.calls).toEqual([[12], [19]]);
      await fireEvent.click(done);
      await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
      expect(onClose).toHaveBeenCalledTimes(3);
    } finally {
      reconciliation.resolve();
      for (const read of receipt) read.resolve(Response.json({ ...emptyProfile(person(12)), contact_points: [] }));
    }
  });

  it('renders saved email and phone on the History card', async () => {
    const value = detail();
    value.participants = value.participants.filter(participant => participant.origin_side === 'survivor');
    value.labels.profiles[1] = { ...value.labels.profiles[1], display_name: 'Curated Guest', contact_points: [
      { identity_key: identityKeyFixture('email', 'curated@example.org'), address_kind: 'email', original_value: 'curated@example.org', normalized_value: 'curated@example.org' },
      { identity_key: identityKeyFixture('phone', '+12025550144'), address_kind: 'phone', original_value: '+12025550144', normalized_value: '+12025550144' },
    ] };
    renderHistory(value);
    await fireEvent.click(await screen.findByRole('button', { name: /^Open Curated Guest merged into Alex Example/ }));
    const card = await screen.findByRole('region', { name: 'Curated Guest' });
    expect(card.textContent).toContain('curated@example.org');
    expect(card.textContent).toContain('+12025550144');
  });

  it('waits for detail before showing its Split action', async () => {
    let release!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { release = resolve; });
    const merge = detail();
    renderHistory(merge, { respond: path => path === '/api/v1/person-merges/41' ? pending : undefined });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+/ }));
      expect(screen.queryByRole('button', { name: 'Split…' })).toBeNull();
      expect(screen.getByText('Loading merge…', { selector: 'p' })).toBeDefined();
      expect(screen.queryByRole('button', { name: 'another profile' })).toBeNull();
      expect(screen.queryByRole('heading', { name: 'Splits' })).toBeNull();
    } finally {
      release(Response.json({ ...merge, labels: { ...merge.labels, participants: undefined } }));
    }
  });

  it('shows loading until the page with its recorded names arrives', async () => {
    let resolve!: (response: Response) => void;
    const pending = new Promise<Response>(done => { resolve = done; });
    renderHistory(12, { respond: path => path.endsWith('/merges') ? pending : undefined });
    expect(await screen.findByText('Loading merges…')).toBeDefined();
    expect(screen.queryByText('Merged-in profile merged into this profile')).toBeNull();
    const item = summary(detail());
    item.labels.absorbed_address_count = 2;
    resolve(Response.json({ merges: [item], limit: 100, offset: 0 }));
    expect(await screen.findByText('address701@example.org merged into Alex Example')).toBeDefined();
    expect(screen.getByText('2 addresses moved in', { exact: false })).toBeDefined();
  });

  it('keeps each merge’s recorded survivor even after that profile was merged away', async () => {
    const merges = [detail(), detail()];
    merges[1].merge.id = 42;
    merges[1].participants = merges[1].participants.map(participant => ({ ...participant, merge_id: 42 }));
    merges[1].labels.participants = merges[1].participants;
    for (const merge of merges) merge.labels.profiles = [{ id: 9, display_name: 'Neri Bloom', participant_ids: [701] }, { id: 7, display_name: merge.merge.id === 41 ? 'Mira Vale' : 'Mira Vale renamed', participant_ids: [702] }];
    renderHistory(merges[0], { list: merges });
    expect(await screen.findByText('Neri Bloom merged into Mira Vale')).toBeDefined();
    expect(await screen.findByText('Neri Bloom merged into Mira Vale renamed')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: /^Open Neri Bloom merged into Mira Vale · / }));
    const cards = within(await screen.findByRole('group', { name: 'Merged profiles' })).getAllByRole('region');
    expect(within(cards[1]).getByRole('heading').textContent).toBe('Mira Vale');
  });

  it('shows a loaded zero moved-in count', async () => {
    const value = detail();
    value.splits = [];
    for (const participant of value.participants) delete participant.split_id;
    renderHistory(value, {
      addresses: addressIdentifiers().filter(identifier => identifier.participant_id === 702), count: 0,
    });
    const row = await screen.findByRole('button', { name: /^Open Alex Example · work merged into Alex Example · / });
    expect(row.closest('.detail-row')?.querySelector('.description')?.textContent).toContain('0 addresses moved in');
  });

  it.each([[false, false], [true, false], [true, true]])('summarizes a whole-profile split: exact=%s same day=%s', async (exact, sameDay) => {
    const value = detail();
    for (const participant of value.participants) if (participant.origin_side === 'absorbed') participant.split_id = 55;
    value.splits![0].exact_reversal = exact;
    if (sameDay) value.splits![0].created_at = value.merge.created_at;
    renderHistory(value);
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    const line = await screen.findByText((_, element) => element?.tagName === 'P' && (element.textContent ?? '').includes('split back to Alex Restored'));
    expect(line.textContent).toMatch(sameDay ? /split back to Alex Restored · nothing left to split/ : exact ? /split back to Alex Restored on Aug [34] · nothing left to split/ : /split back to Alex Restored on Aug [34] · partly restored · nothing left to split/);
    expect(screen.queryByRole('button', { name: 'Split…' })).toBeNull();
  });

  it.each([false, true])('names the addresses each split took beside its destination in the summary, deleted=%s', async deleted => {
    const value = detail();
    value.labels.profiles = [{ id: 7, display_name: 'Survivor', participant_ids: [702] }, { id: 9, display_name: 'Merged Guest', participant_ids: [701, 703, 704] }];
    value.participants = [
      { merge_id: 41, participant_id: 701, origin_side: 'absorbed', split_id: 55 },
      { merge_id: 41, participant_id: 703, origin_side: 'absorbed', split_id: 56 },
      { merge_id: 41, participant_id: 704, origin_side: 'absorbed' },
      { merge_id: 41, participant_id: 702, origin_side: 'survivor' },
    ] as MergeDetail['participants'];
    value.labels.addresses = [
      { participant_id: 701, type: 'email', value: 'first@example.org', normalized_value: 'first@example.org', display_value: '', service_slug: 'email', service_label: '', scope_kind: '', scope_value: '' },
      { participant_id: 703, type: 'handle', value: '@alex', normalized_value: 'alex', display_value: '', service_slug: 'slack', service_label: 'Slack', scope_kind: 'workspace', scope_value: 'garden' },
      { participant_id: 704, type: 'email', value: 'pending@example.org', normalized_value: 'pending@example.org', display_value: '', service_slug: 'email', service_label: '', scope_kind: '', scope_value: '' },
    ].map(a => addressFixture(a.participant_id, a.type, a.value, a));
    value.labels.participant_ids = [701, 702, 703, 704];
    value.splits = [value.splits![0], { ...value.splits![0], id: 56, new_person_id: 29, new_person_uid: 'synthetic-29' }];
    renderHistory(value, { respond: path => {
      if (path === '/api/v1/people/19') return Response.json(person(19, 1, [701], 'First Restored'));
      if (path === '/api/v1/people/29') return deleted ? Response.json({}, { status: 404 }) : Response.json(person(29, 1, [703], 'Second Restored'));
      return undefined;
    } });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open Merged Guest merged into Survivor/ }));
    const line = await screen.findByText((_, element) => element?.tagName === 'P' && (element.textContent ?? '').startsWith('Merged by you'));
    await waitFor(() => expect(line.textContent).toContain('First Restored (first@example.org)'));
    expect(line.textContent).not.toContain('partly restored');
    expect.soft(line.textContent).toMatch(new RegExp(`split back to ${deleted ? 'another profile' : 'Second Restored'} \\(Slack · @alex · workspace: garden\\)`));
    expect.soft(line.textContent).not.toContain('pending@example.org');
    expect.soft(screen.getByRole('region', { name: 'Merged Guest' }).textContent).not.toMatch(/Restored|another profile|split back/i);
  });

  it.each([
    { destination: 'unnamed', layout: 'card' },
    { destination: 'unnamed', layout: 'row' },
    { destination: 'literal fallback name', layout: 'card' },
    { destination: 'literal fallback name', layout: 'row' },
  ] as const)('opens only an existing $destination split destination in the $layout layout', async ({ destination, layout }) => {
    const value = detail();
    if (layout === 'row') {
      // The split took only a participant with no address on the cards.
      value.labels.addresses = value.labels.addresses.filter(identifier => identifier.participant_id !== 703);
    }
    const { onOpenPerson, requests } = renderHistory(value, { respond: path => path === '/api/v1/people/19'
      ? Response.json(person(19, 1, [703], destination === 'literal fallback name' ? 'another profile' : ''))
      : undefined });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    if (layout === 'row') {
      await screen.findByRole('heading', { name: 'Splits' });
      expect(screen.getByText(/^You · Aug [34]$/)).toBeDefined();
    } else await screen.findByText((_, element) => element?.tagName === 'P' && (element.textContent ?? '').includes('split back to another profile'));
    expect(requests.filter(request => new URL(request.url).pathname === '/api/v1/people/19')).toHaveLength(1);
    const name = layout === 'row' ? destination === 'literal fallback name' ? 'Open another profile' : 'Open restored profile' : 'another profile';
    await fireEvent.click(await screen.findByRole('button', { name }));
    expect(onOpenPerson).toHaveBeenCalledWith(19);
  });

  it.each([false, true])('groups addresses, splits, and review decisions, only decided=%s', async decided => {
    const value = detail();
    if (decided) value.review_candidates = value.review_candidates!.filter(candidate => candidate.state !== 'pending');
    renderHistory(value);
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    await screen.findByRole('heading', { name: 'address701@example.org merged into Alex Example' });
    expect(screen.getByRole('button', { name: 'Back to History' })).toBeDefined();
    expect(await screen.findByRole('heading', { name: decided ? 'Review decisions' : 'Needs review' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: decided ? 'Needs review' : 'Review decisions' })).toBeNull();
    expect(screen.getByText(/^Merged by you · Aug [23]$/, { selector: 'span' })).toBeDefined();
    // Two cards: the merged-in profile, then the one it merged into.
    const [, survivor] = within(await screen.findByRole('group', { name: 'Merged profiles' })).getAllByRole('region');
    expect(within(survivor).getByText('address702@example.org')).toBeDefined();
    // The split shows once, on the merge line: to whom, when, and how; the cards carry no tag for it.
    expect(within(survivor).queryByRole('button')).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Splits' })).toBeNull();
    if (!decided) {
      expect(screen.getByText('Field')).toBeDefined();
      await fireEvent.click(screen.getByText('1 earlier decision'));
    }
    expect(screen.getByText('Role')).toBeDefined();
    expect(screen.getByText(/^Accepted by you · Aug [45]$/)).toBeDefined();
    expect(screen.queryByRole('table')).toBeNull();
    expect(document.body.textContent).not.toContain('opaque-row-key-must-not-appear');
    expect(document.body.textContent).not.toContain('private/snapshot/path-must-not-appear');
  });

  it('labels an unnamed surviving profile on its card', async () => {
    renderHistory(12, { unnamedSurvivor: true });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    expect(await screen.findByRole('heading', { name: 'This profile' })).toBeDefined();
  });

  it('keeps recorded names while addresses fail, and retries them', async () => {
    const { options } = renderHistory(12, { fail: true });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    const cards = within(await screen.findByRole('group', { name: 'Merged profiles' }));
    expect(cards.getByRole('heading', { name: 'Alex Example' })).toBeDefined();
    expect(cards.queryByText('address703@example.org')).toBeNull();
    expect(screen.getByText("Some names and addresses couldn't be loaded.")).toBeDefined();
    options.fail = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(screen.queryByText(/couldn't be loaded/)).toBeNull());
    expect(within(await screen.findByRole('group', { name: 'Merged profiles' })).getByText('address703@example.org')).toBeDefined();
  });

  it('reveals the profile at merge time only after the explicit action', async () => {
    renderHistory();
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));

    expect(screen.queryByText(/explicitly_revealed/)).toBeNull();
    await screen.findByRole('group', { name: 'Merged profiles' });
    await fireEvent.click(await screen.findByText('Profile at merge time'));

    expect((await screen.findByRole('region', { name: 'Raw values at merge time' })).textContent).toContain('explicitly_revealed');
    expect(screen.queryByRole('heading', { name: 'Raw values' })).toBeNull();
    expect(screen.getByText(/^Captured Aug [23], \d{1,2}:\d{2}\s?[AP]M$/)).toBeDefined();
    expect(screen.getByText('SHA-256 syntheti…').title).toBe('SHA-256 synthetic-digest');
  });

  it('explains when the profile it merged into is gone', async () => {
    renderHistory(detail(null), { addresses: addressIdentifiers() });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    expect(await screen.findByText(/the profile it merged into is gone$/)).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Split…' })).toBeNull();
  });

  it('returns from Split to the merge in the same dialog', async () => {
    const mergeDetail = detail();
    renderHistory(mergeDetail, { respond: path => path === '/api/v1/people/12'
      ? Response.json(person(12, 4, [701], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } }) : undefined });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
    const trigger = await screen.findByRole('button', { name: 'Split…' });
    await fireEvent.click(trigger);
    await screen.findByRole('dialog', { name: 'Split Synthetic Source' });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    const back = screen.getByRole('button', { name: /^Back to .+ merged into / });
    await waitFor(() => expect(back).toHaveProperty('disabled', false));
    // Back returns to the merge and × closes History, so the footer holds only the split itself.
    const footer = document.querySelector<HTMLElement>('.kit-modal-footer');
    expect(within(footer!).getAllByRole('button').map(button => button.textContent?.trim())).toEqual(['Split into new person']);
    expect(screen.getByRole('button', { name: 'Close' })).toBeDefined();

    await fireEvent.click(back);

    expect(await screen.findByRole('button', { name: 'Split…' })).toBeDefined();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
  });
});
