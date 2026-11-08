import { addressFixture } from '../../directory/merge-fixtures.test-support';
import { mergeResult } from '../../directory/entity-fixtures.test-support';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { formatDateTime } from '../../util/format';
import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import type { AttributeDefinition } from '../../api/generated/models';

const when = '2026-08-01T00:00:00Z';
const currentEnvelope = { ordinal: 0, source: 'user', active_from: when, created_at: when, updated_at: when, vcard: {} };

function person() {
  return { id: 7, revision: 3, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
}

describe('ProfileHistoryDialog', () => {
  it.each(['attributes', 'definitions'])('reloads custom history on open and retries failed %s without hiding profile history', async (failedSection) => {
    let failing = true;
    let note = 'Earlier note';
    const requests: URL[] = [];
    const definition: AttributeDefinition = {
      id: 8, universal_id: '00000000-0000-4000-8000-000000000008', object_type: 'person', slug: 'note', label: 'Note',
      value_type: 'text', field_type: 'textarea', cardinality: 'single', display_order: 0, is_required: false,
      ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_searchable: false,
      is_sensitive: false, is_audited: true, is_deletable: true, history_exempt: false, is_active: true,
      revision: 1, created_at: when, updated_at: when,
    };
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const url = new URL((input instanceof Request ? input : new Request(input)).url);
      requests.push(url);
      if (url.pathname.endsWith('/merges')) return Response.json({ merges: [] });
      if (url.pathname === '/api/v1/people/7') return Response.json(person(), { headers: { ETag: '"person-7-r3"' } });
      if (url.pathname.endsWith('/profile')) return Response.json(emptyProfile(person()), { headers: { ETag: '"profile-7-r3"' } });
      if (url.pathname.endsWith('/contact-state')) return Response.json({ person_id: 7, interaction_count: 0, cadence_status: 'unknown', computed_at: when, stale: false });
      if (url.pathname.endsWith('/profile/history')) return Response.json({ ...emptyProfile(person()), observations: [], names: [{
        person_id: 7, name_kind: 'formatted', original_value: 'Current Name', is_derived: false, envelope: { ...currentEnvelope, id: 2 },
      }] });
      if (url.pathname.endsWith('/attributes')) {
        if (failing && failedSection === 'attributes') return Response.json({ message: 'attributes unavailable' }, { status: 503 });
        return Response.json({ person_id: 7, attributes: [{ definition, current: [{ id: 10, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text', text: 'Imported note' }, source: 'carddav_import', source_ref: 'source-card-7', actor: 'synthetic-importer', confidence: 0.8, active_from: when, created_at: when }], history: [{
          id: 9, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text', text: note },
          source: 'user', active_from: when, active_until: '2026-08-02T00:00:00Z', created_at: when,
        }] }] });
      }
      if (url.pathname.endsWith('/attribute-definitions')) {
        if (failing && failedSection === 'definitions') return Response.json({ message: 'definitions unavailable' }, { status: 503 });
        return Response.json({ definitions: [definition] });
      }
      throw new Error(`unexpected ${url.pathname}`);
    }));
    const controller = new DirectoryProfileController(client, 7, { person: person(), etags: {}, errors: {} });
    const props = { client, controller, personID: 7, onClose: vi.fn() };
    const dialog = render(ProfileHistoryDialog, props);
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain("Couldn't load field history."));
    await fireEvent.click(screen.getByRole('alert').querySelector('summary')!);
    expect(screen.getByRole('alert').textContent).toContain(`${failedSection} unavailable`);
    expect(await screen.findByText('Current Name')).toBeDefined();
    failing = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('Earlier note')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    await dialog.unmount();
    note = 'Latest note';
    render(ProfileHistoryDialog, props);
    expect(await screen.findByText('Latest note')).toBeDefined();
    expect(screen.queryByText('Earlier note')).toBeNull();
    expect(requests.filter(url => url.pathname.endsWith('/attributes')).every(url => url.searchParams.get('history') === 'true')).toBe(true);
  });

  it('preserves structured provenance and classifies every closed value as earlier', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (new URL(request.url).pathname.endsWith('/merges')) return Response.json({ merges: [] });
      return Response.json({
        person: person(),
        names: [
          { person_id: 7, name_kind: 'formatted', original_value: 'Current Name', is_derived: false, envelope: { id: 2, ordinal: 0, source: 'user', active_from: when, created_at: when, updated_at: when, vcard: {} } },
          { person_id: 7, name_kind: 'formatted', original_value: 'Previous Name', is_derived: false, envelope: { id: 1, ordinal: 0, source: 'carddav_import', source_ref: 'synthetic-card', active_from: when, active_until: '2026-08-02T00:00:00Z', created_at: when, updated_at: when, superseded_at: '2026-08-02T00:00:00Z', vcard: {} } }
        ],
        contact_points: [...['T_ALPHA', 'T_BETA'].map((scope, index) => ({
          person_id: 7, address_kind: 'username', original_value: 'alice', identity_key: `slack-${index}`, normalized_value: 'alice', normalization: 'casefold', normalization_version: 1,
          service_slug: 'slack', service_label: 'Slack', scope_kind: 'workspace', scope_value: scope,
          envelope: { id: 20 + index, ordinal: index, source: 'user', created_at: when, updated_at: when, superseded_at: '2026-08-02T00:00:00Z', vcard: {} }
        })), { person_id: 7, address_kind: 'email', original_value: 'current@example.com', normalized_value: 'current@example.com', normalization: 'email', normalization_version: 1, envelope: { ...currentEnvelope, id: 30 } },
        { person_id: 7, address_kind: 'username', original_value: 'Desk 4455', normalized_value: 'Desk 4455', normalization: 'none', normalization_version: 1, service_slug: 'pager', service_label: 'Team hotline', envelope: { ...currentEnvelope, id: 35 } },
        ...['beeper-a', 'beeper-b'].map((scope, index) => ({ person_id: 7, address_kind: 'username', original_value: `reader${index}`, identity_key: `whatsapp-${index}`, normalized_value: `reader${index}`, normalization: 'none', normalization_version: 1, service_slug: 'whatsapp', service_label: 'WhatsApp', scope_kind: 'account', scope_value: scope, envelope: { ...currentEnvelope, id: 40 + index } }))],
        addresses: [{ person_id: 7, address_kind: 'postal', original_value: 'Current address', envelope: { ...currentEnvelope, id: 31 } }],
        dates: [{ person_id: 7, date_kind: 'birthday', date: { month: 2, day: 14 }, date_text: 'February 14', original_value: '--02-14', envelope: { ...currentEnvelope, id: 32 } }],
        categories: [{ person_id: 7, normalized_value: 'scheduled end category', original_value: 'Scheduled end category', envelope: { ...currentEnvelope, id: 33, active_until: '2999-01-01T00:00:00Z' } }, {
          person_id: 7, original_value: 'Past category', normalized_value: 'past category',
          envelope: { id: 8, ordinal: 0, source: 'user', active_until: '2026-07-31T00:00:00Z', created_at: when, updated_at: when, vcard: {} }
        }],
        media: [{ person_id: 7, media_kind: 'photo', has_data: false, original_value: 'Current photo', envelope: { ...currentEnvelope, id: 34 } }],
        observations: []
      });
    }));

    render(ProfileHistoryDialog, { client, personID: 7, onClose: vi.fn() });

    await screen.findByText('Previous Name');
    for (const title of ['Names', 'Email and phone', 'Addresses', 'Dates', 'Categories', 'Media']) {
      expect(screen.getByRole('heading', { name: title })).toBeDefined();
    }
    expect.soft(screen.getByText('Scheduled end category').closest('details')).not.toBeNull();
    const currentRow = screen.getByText('Current Name').closest('.detail-row')!;
    const line = currentRow.querySelector<HTMLElement>('.description')!;
    expect(line.title).toContain('Updated');
    for (const value of ['Current Name', 'current@example.com', 'Current address', 'Birthday: February 14', 'Current photo']) {
      expect(screen.getByText(value).closest('details')).toBeNull();
    }
    const summaries = screen.getAllByText(/earlier values?$/).map((node) => node.textContent);
    expect(summaries).toEqual(['1 earlier value', '2 earlier values', '2 earlier values']);
    for (const value of ['Previous Name', 'Past category']) {
      const row = screen.getByText(value).closest('.detail-row')!;
      expect(row.closest('details')).not.toBeNull();
    }
    expect(screen.getByText('reader0').closest('.detail-row')?.textContent).toContain('WhatsApp');
    expect(document.body.textContent).not.toMatch(/T_ALPHA|T_BETA|beeper-a|beeper-b/);
    // A registered service reads by its catalog name, not its slug.
    expect(screen.getByText('Desk 4455').closest('.detail-row')?.textContent).toMatch(/Team hotline · You · /);
    expect(document.body.textContent).not.toMatch(/Service:|Scope:|Observed:/);
    expect(requests.map((request) => new URL(request.url).pathname)).toContain('/api/v1/people/7/profile/history');
  });

  it('shows readable observation provenance without the numeric source ID', async () => {
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/merges')) return Response.json({ merges: [] });
      return Response.json({ ...emptyProfile(person()), observations: [{
        participant_id: 70, address_kind: 'email', original_value: 'observed@example.test',
        normalized_value: 'observed@example.test', identity_key: 'observed-test-key', normalization: 'email', normalization_version: 1,
        service_slug: 'email', source_id: 987654321, observed_at: '2026-07-31T12:00:00',
        envelope: { ...currentEnvelope, id: 9, source: 'archive_observation', source_ref: 'synthetic-archive' },
      }, { participant_id: 71, address_kind: 'username', original_value: 'observer', normalized_value: 'observer', identity_key: 'observer-key', normalization: 'none', normalization_version: 1, service_slug: 'slack', scope_kind: 'workspace', scope_value: 'T_BETA', source_id: 12, observed_at: when, envelope: { ...currentEnvelope, id: 36, source: 'archive_observation' } }] });
    });
    render(ProfileHistoryDialog, { client, personID: 7, onClose: vi.fn() });

    expect(await screen.findByRole('heading', { name: 'Seen in services' })).toBeDefined();
    const row = (await screen.findByText('observed@example.test')).closest('.detail-row')!;
    const line = row.querySelector<HTMLElement>('.description')!;
    expect(line.textContent).toMatch(/^Email · From your messages · /);
    expect(line.title).toContain('Seen');
    expect(line.title).toContain('Reference: synthetic-archive');
    expect(row.textContent).not.toMatch(/987654321|synthetic-archive/);
    expect(screen.getByText('observer').closest('.detail-row')?.textContent).not.toContain('T_BETA');
  });

  it.each(['available', 'failed'] as const)('waits for history and ends a split on its receipt: initially %s', async initial => {
    const initialFailure = initial === 'failed';
    let releaseReconciliation!: () => void;
    const pendingReconciliation = new Promise<void>(resolve => { releaseReconciliation = resolve; });
    let historyReads = 0;
    let initialResponse: Response | undefined;
    let releaseInitial!: (response: Response) => void;
    const pendingInitial = new Promise<Response>(resolve => { releaseInitial = resolve; });
    const source = { ...person(), participant_ids: [1, 2] };
    const remaining = { ...source, revision: 4, participant_ids: [2] };
    const restored = { ...source, id: 9, revision: 1, display_name: 'Restored Person', participant_ids: [1], vcard_uid: 'person-9' };
    const merge = { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'person-7', absorbed_vcard_uid: 'person-9', survivor_revision_before: 2, absorbed_revision_before: 1, survivor_revision_after: 3, actor: 'user', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when };
    const split = { id: 55, merge_id: 41, source_person_id: 7, new_person_id: 9, new_person_uid: 'person-9', source_revision_before: 3, source_revision_after: 4, exact_reversal: true, actor: 'user', created_at: when };
    const observed = { participant_id: 1, type: 'email', value: 'observed@example.com', normalized_value: 'observed@example.com', display_value: '', service_slug: 'email', service_label: 'Email', scope_kind: '', scope_value: '' };
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const labels = {
        profiles: [{ id: 9, display_name: 'Restored Person', participant_ids: [1] }, { id: 7, display_name: 'Retained name', participant_ids: [] }],
        participants: [{ merge_id: 41, participant_id: 1, origin_side: 'absorbed' }], participant_ids: [1],
        addresses: [addressFixture(1, 'email', observed.value, { identity_key: 'observed-key' })], absorbed_address_count: 1, incomplete: false,
      };
      if (path.endsWith('/profile/history')) {
        historyReads += 1;
        if (initialFailure && historyReads === 1) return Response.json({ error: 'unavailable', message: 'history unavailable' }, { status: 503 });
        const response = Response.json({
          person: source,
          names: [{ person_id: 7, name_kind: 'formatted', formatted: 'Retained name', original_value: 'Retained name', is_derived: false, envelope: { ...currentEnvelope, id: 1 } }, { person_id: 7, name_kind: 'nickname', formatted: 'Moved alias', original_value: 'Moved alias', is_derived: false, envelope: { ...currentEnvelope, id: 2 } }],
          contact_points: [{ person_id: 7, address_kind: 'email', original_value: 'moved@example.com', normalized_value: 'moved@example.com', normalization: 'email', normalization_version: 1, envelope: { ...currentEnvelope, id: 3 } }],
          observations: [{ participant_id: 1, address_kind: 'email', original_value: 'observed@example.com', normalized_value: 'observed@example.com', identity_key: 'observed-key', service_slug: 'email', normalization: 'email', normalization_version: 1, observed_at: when, envelope: { ...currentEnvelope, id: 4, source: 'archive_observation' } }],
          addresses: [], dates: [], categories: [], media: [],
        });
        if (historyReads === (initialFailure ? 2 : 1)) {
          initialResponse = response;
          return pendingInitial;
        }
        return response;
      }
      if (path.endsWith('/merges')) return Response.json({ merges: [{ merge, labels, participant_count: 1, row_count: 0, pending_candidate_count: 0, split_count: 0, row_action_counts: {} }], limit: 100, offset: 0 });
      if (path === '/api/v1/person-merges/41') return Response.json({ merge, labels: { ...labels, participants: undefined }, participants: labels.participants, rows: [], splits: [], review_candidates: [] });
      if (path === '/api/v1/people/7') return Response.json(source, { headers: { ETag: '"person-7-r3"' } });
      if (path === '/api/v1/people/7/split') return Response.json({ source_person: remaining, new_person: restored, split, exact_reversal: true, ambiguous_rows: [], unrestored_rows: [], uid_alias_disposition: 'restored', identity_revision: 2, cache_state: 'ready' }, { headers: { ETag: '"person-7-r4"', 'X-New-Person-ETag': '"person-9-r1"' } });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    const onSplitCommitted = vi.fn(async () => { await pendingReconciliation; });
    const view = render(ProfileHistoryDialog, { client, personID: 7, onClose: vi.fn(), onSplitCommitted });
    try {
      if (initialFailure) {
        expect((await screen.findByRole('alert')).textContent).toContain("Couldn't load names, email, phone, and other details.");
        await screen.findByRole('button', { name: /^Open .+ merged into .+ · / });
        await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      }
      await act(async () => { await vi.waitFor(() => expect(initialResponse).toBeDefined()); });
      expect(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / })).toBeDefined();
      expect(screen.getByText('Loading profile history…')).toBeDefined();
      await act(async () => { releaseInitial(initialResponse!); });
      for (const value of ['Moved alias', 'moved@example.com']) expect(await screen.findByText(value)).toBeDefined();
      await fireEvent.click(await screen.findByRole('button', { name: /^Open .+ merged into .+ · / }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Split…' }));
      await fireEvent.click(await screen.findByRole('checkbox', { name: 'observed@example.com' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Split into new person' }));
      await waitFor(() => expect(onSplitCommitted).toHaveBeenCalledWith({ sourcePersonID: 7, newPersonID: 9 }));
      expect(await screen.findByRole('dialog', { name: 'Split complete' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Done' })).toHaveProperty('disabled', false);
    } finally { releaseReconciliation(); releaseInitial(initialResponse ?? Response.json({})); view.unmount(); }
  });
});

const mergedInCases = [
  { name: 'removed before the merge', rows: 2, original: 'Mira@Example.org' },
  { name: 'only seen in services', rows: 1, original: 'mira@example.org' },
];

it.each(['history', 'merges'])('keeps account numbers when %s arrives first', async first => {
  let releaseSecond!: () => void;
  const pendingSecond = new Promise<void>(resolve => { releaseSecond = resolve; });
  const merge = { ...mergeResult(person()).merge, id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7 };
  const address = (id: number, name: string) => addressFixture(id, 'beeper', `beeper:whatsapp:${name}`, { participant_display_name: name, service_slug: 'whatsapp', service_label: 'WhatsApp', scope_kind: 'account', scope_value: 'account-z' });
  const participants = [{ merge_id: 41, participant_id: 1, origin_side: 'absorbed' }, { merge_id: 41, participant_id: 2, origin_side: 'survivor' }];
  const labels = { profiles: [{ id: 7, display_name: 'Mira', participant_ids: [2] }, { id: 9, display_name: 'Neri', participant_ids: [1] }], participants, participant_ids: [1, 2], addresses: [address(1, 'Neri'), address(2, 'Mira')], absorbed_address_count: 1, incomplete: false };
  const saved = { person_id: 7, address_kind: 'username', original_value: 'reader', normalized_value: 'reader', identity_key: 'saved-whatsapp-key', service_slug: 'whatsapp', service_label: 'WhatsApp', scope_kind: 'account', scope_value: 'account-a', normalization: 'none', normalization_version: 1, envelope: { ...currentEnvelope, id: 10 } };
  const source = { ...person(), display_name: 'Mira', participant_ids: [1, 2] };
  const detail = { merge, labels: { ...labels, participants: undefined }, participants, rows: [], splits: [], review_candidates: [] };
  render(ProfileHistoryDialog, { personID: 7, onClose: vi.fn(), client: createAPIClient(async input => {
    const request = input as Request, path = new URL(request.url).pathname;
    if (path.endsWith('/profile/history')) { if (first === 'merges') await pendingSecond; return Response.json({ ...emptyProfile(source), observations: [], contact_points: [saved] }); }
    if (path.endsWith('/merges')) { if (first === 'history') await pendingSecond; return Response.json({ merges: [{ merge, labels, participant_count: 1, row_count: 0, split_count: 0, pending_candidate_count: 0, row_action_counts: {} }], limit: 100, offset: 0 }); }
    if (path === '/api/v1/person-merges/41') return Response.json(detail);
    throw new Error(`unexpected ${request.method} ${path}`);
  }) });
  if (first === 'history') expect((await screen.findByText('reader')).closest('.detail-row')?.textContent).toContain('WhatsApp · account 1');
  else await screen.findByRole('button', { name: /^Open Neri merged into Mira/ });
  releaseSecond();
  expect((await screen.findByText('reader')).closest('.detail-row')?.textContent).toContain(`WhatsApp · account ${first === 'history' ? 1 : 2}`);
  expect((await screen.findByRole('button', { name: /Open the merge that brought in WhatsApp/ })).closest('.detail-row')?.textContent).toContain(`account ${first === 'history' ? 2 : 1}`);
  expect(document.body.textContent).not.toMatch(/account-a|account-z/);
  await fireEvent.click(await screen.findByRole('button', { name: /^Open Neri merged into Mira/ }));
  const cards = await screen.findByRole('group', { name: 'Merged profiles' });
  const survivor = within(cards).getByRole('region', { name: 'Mira' });
  expect(within(survivor).getByText(`account ${first === 'history' ? 2 : 1}`)).toBeDefined();

});

async function renderMergedInAddress(entry: typeof mergedInCases[number]) {
  const first = { ...mergeResult(person()).merge, id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7 };
  const identifiers = [{ participant_id: 1, type: 'email', value: 'mira@example.org', display_value: '', normalized_value: 'mira@example.org', service_slug: 'email', service_label: '', scope_kind: '', scope_value: '' }];
  const addresses = identifiers.map(a => addressFixture(a.participant_id, a.type, a.value, { ...a, identity_key: 'mira-key' }));
  const labels = (merge: typeof first) => ({
    profiles: [{ id: merge.survivor_person_id, display_name: 'Current person', participant_ids: [20] }, { id: merge.absorbed_person_id, display_name: 'First person', participant_ids: identifiers.map(item => item.participant_id) }],
    participants: [{ merge_id: merge.id, participant_id: 20, origin_side: 'survivor' }, ...identifiers.map(item => ({ merge_id: merge.id, participant_id: item.participant_id, origin_side: 'absorbed' }))],
    participant_ids: [20, ...identifiers.map(item => item.participant_id)], addresses, absorbed_address_count: addresses.length, incomplete: false,
  });
  render(ProfileHistoryDialog, { personID: 7, onClose: vi.fn(), client: createAPIClient(async input => {
    const path = new URL((input as Request).url).pathname;
    if (path.endsWith('/merges')) return Response.json({ merges: [{ merge: first, labels: labels(first), participant_count: identifiers.length + 1, row_count: 0, split_count: 0, pending_candidate_count: 0, row_action_counts: {} }], limit: 100, offset: 0 });
    if (path === '/api/v1/person-merges/41') return Response.json({ merge: first, labels: { ...labels(first), participants: undefined }, participants: labels(first).participants, rows: [], splits: [], review_candidates: [] });
    return Response.json({ ...emptyProfile(person()), observations: entry.name === 'only seen in services' ? [{ participant_id: 1, address_kind: 'email', original_value: entry.original, normalized_value: 'mira@example.org', identity_key: 'mira-key', service_slug: 'email', scope_kind: '', scope_value: '', normalization: 'email', normalization_version: 1, source_id: 1, observed_at: when, envelope: { ...currentEnvelope, id: 9, source: 'archive_observation' } }, { participant_id: 1, address_kind: 'email', original_value: entry.original, normalized_value: 'mira@example.org', identity_key: 'mira-key', service_slug: 'email', service_label: 'Email', scope_kind: '', scope_value: '', normalization: 'email', normalization_version: 1, source_id: 2, observed_at: '2026-08-03T12:00:00Z', envelope: { ...currentEnvelope, id: 10, source: 'archive_observation', source_ref: 'example-source', actor: 'importer' } }] : [], contact_points: entry.name === 'removed before the merge' ? [{ person_id: 7, address_kind: 'email', original_value: entry.original, identity_key: 'mira-key', normalized_value: 'mira@example.org', service_slug: 'email', service_label: '', scope_kind: '', scope_value: '', normalization: 'none', normalization_version: 1, envelope: { ...currentEnvelope, id: 3, superseded_at: '2026-07-31T00:00:00Z' } }] : [] });
  }) });
  await screen.findAllByRole('button', { name: /^Open .+ merged into .+ · / });
  const section = screen.getByRole('heading', { name: 'Email and phone' }).closest('section')!;
  return section;
}

it.each(mergedInCases)('lists each merged-in address once: $name', async entry => {
  const section = await renderMergedInAddress(entry);
  expect(section.querySelectorAll('.detail-row')).toHaveLength(entry.rows);
  expect(screen.getByRole('button', { name: 'Open the merge that brought in mira@example.org' })).toBeDefined();
  if (entry.name === 'removed before the merge') expect(screen.getByText(entry.original).closest('details')).not.toBeNull();
  else {
    expect(section.querySelector('.detail-row')!.textContent).toContain('Email · Merged in · ');
    const provenance = section.querySelector('.detail-row [title]')?.getAttribute('title');
    expect(provenance).toContain(`Seen ${formatDateTime(when)}`);
    expect(provenance).toContain(`Seen ${formatDateTime('2026-08-03T12:00:00Z')}`);
    expect(provenance).toContain('Reference: example-source');
    expect(provenance).toContain('Actor: msgvault');
    expect(screen.queryByRole('heading', { name: 'Seen in services' })).toBeNull();
  }
});
