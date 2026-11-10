// Store fixtures are checked by TestPersonMerge_DetailAddressLabels and TestPersonMergeCountsNewAddresses.
import migratedAddresses from './beeper-migrated-identities.test-fixture.json';
import phoneAddresses from './beeper-phone-identities.test-fixture.json';
import rawPhoneAddresses from './beeper-raw-phone-identities.test-fixture.json';
import mailboxAddresses from './mailbox-case-identities.test-fixture.json';
import { describe, expect, it } from 'vitest';
import type { ParticipantAddress, PersonMergeDetail, PersonMergeLabels } from '../api/generated/models';
import { addressFixture } from './merge-fixtures.test-support';
import { addressText, mergeSummaryRow, mergeAddresses, mergedInAddresses, mergeSentence, scopeText, workspaceLabels } from './merge-labels';

const labelsOf = (addresses: ParticipantAddress[], count = addresses.length): PersonMergeLabels => ({ profiles: [{ id: 7, participant_ids: [] }, { id: 9, participant_ids: [...new Set(addresses.map(a => a.participant_id))] }], participant_ids: [...new Set(addresses.map(a => a.participant_id))], addresses, absorbed_address_count: count, incomplete: false });
const merge = { id: 41, survivor_person_id: 7, absorbed_person_id: 9 } as PersonMergeDetail['merge'];
const absorbed = (participant_id: number) => ({ merge_id: 41, participant_id, origin_side: 'absorbed' });

it('renders the store’s email and formatted phone through History and merged-in rows', () => {
  const labels = labelsOf(phoneAddresses);
  const participants = [absorbed(2)];
  const row = mergeSummaryRow({ merge, participants, labels }, workspaceLabels(labels.addresses));
  expect(row.count).toBe(2);
  for (const addresses of [mergeAddresses({ participants, labels }, 'absorbed', workspaceLabels(labels.addresses)), row.addresses.get(2)!]) {
    expect(addresses.map(addressText)).toEqual(['+1 (202) 555-0123', 'Reader@Example.org']);
  }
  expect(mergedInAddresses([{ ...absorbed(2), merged_at: '2026-01-03T12:00:00Z', addresses: phoneAddresses }], workspaceLabels(phoneAddresses)).map(a => a.value)).toEqual(['+1 (202) 555-0123', 'Reader@Example.org']);
});

it('keeps the legacy Beeper phone readable when its saved copy matches', () => {
  const labels = labelsOf(rawPhoneAddresses, 1);
  labels.profiles = [{ id: 9, participant_ids: [1], contact_points: [{
    identity_key: '["whatsapp","","","+12025550123"]', address_kind: 'phone', original_value: '+1 (202) 555-0123', service_slug: 'whatsapp', service_label: 'WhatsApp',
  }] }];
  const participants = [absorbed(1)];
  const row = mergeSummaryRow({ merge, participants, labels }, workspaceLabels(labels.addresses));
  expect(row.addresses.get(1)?.map(addressText)).toEqual(['WhatsApp · Reader · +12025550123']);
  expect(mergeAddresses({ merge, participants, labels }, 'absorbed', workspaceLabels(labels.addresses)).map(addressText)).toEqual(['WhatsApp · Reader · +12025550123']);
});

describe('mergeSentence', () => {
  it('names both profiles, and a same-name absorbed profile by its first address', () => {
    expect(mergeSentence('Neri Bloom', 'Mira Vale', 'neri@example.org')).toBe('Neri Bloom merged into Mira Vale');
    expect(mergeSentence('Mira Vale · work', 'Mira Vale', 'mira.work@example.org')).toBe('mira.work@example.org merged into Mira Vale');
    expect(mergeSentence('mira', 'Mira Vale', 'mira@example.org')).toBe('mira@example.org merged into Mira Vale');
    expect(mergeSentence('Mira Vale · work', 'Mira Vale')).toBe('Mira Vale · work merged into Mira Vale');
    expect(mergeSentence(undefined, 'Mira Vale', 'mira.work@example.org')).toBe('mira.work@example.org merged into Mira Vale');
    expect(mergeSentence(undefined, undefined)).toBe('Merged-in profile merged into this profile');
    expect(mergeSentence('Ann', 'Joanna Smith', 'ann@example.org')).toBe('Ann merged into Joanna Smith');
    expect(mergeSentence('Mira Vale', 'Mira Vale', 'mira.work@example.org')).toBe('mira.work@example.org merged into Mira Vale');
  });
});


it('keeps duplicate participants and their separate split destinations', () => {
  const participants = mailboxAddresses.map((a, i) => ({ ...absorbed(a.participant_id), split_id: 55 + i }));
  const value = { merge, participants, labels: labelsOf(mailboxAddresses, 1) };
  expect(mergeSummaryRow(value, new Map()).count).toBe(1);
  expect(mergeAddresses(value, 'absorbed', new Map())).toMatchObject([
    { participantID: 1, value: 'Reader@Example.org', splitID: 55 },
    { participantID: 2, value: 'reader@example.org', splitID: 56 },
  ]);
});

it('distinguishes a known empty participant from unavailable addresses', () => {
  const labels = { ...labelsOf([]), participant_ids: [701] };
  const value = { merge, participants: [absorbed(701)], labels };
  expect(mergeSummaryRow(value, new Map()).addresses.get(701)).toEqual([]);
  expect(mergeSummaryRow(value, new Map()).incomplete).toBe(false);
  expect(mergeSummaryRow({ ...value, labels: labelsOf([]) }, new Map()).incomplete).toBe(true);
});

it('groups saved addresses by the keys supplied by the server', () => {
  const points = [
    { address_kind: 'email', original_value: 'Reader@Example.org', identity_key: 'reader-key' },
    { address_kind: 'email', original_value: 'curated@example.org', identity_key: 'curated-key' },
    { address_kind: 'email', original_value: 'Curated@example.org', identity_key: 'curated-key' },
    { address_kind: 'language', original_value: 'en', identity_key: 'language-key' },
    { address_kind: 'username', original_value: 'Neri', identity_key: 'slack-key', service_slug: 'slack', service_label: 'Slack', scope_kind: 'workspace', scope_value: 'T_TWO' },
  ];
  const value = { merge, participants: [absorbed(701)], labels: { ...labelsOf([addressFixture(701, 'email', 'reader@example.org', { identity_key: 'reader-key' })]), profiles: [{ id: 9, participant_ids: [701], contact_points: points }] } };
  expect(mergeAddresses(value, 'absorbed', workspaceLabels([{ scope_kind: 'workspace', scope_value: 'T_ONE' }, points[4]])).map(addressText)).toEqual(['reader@example.org', 'curated@example.org', 'Neri · Slack · workspace 2']);
});

it.each([
  { type: 'beeper', display: '', name: '', expected: 'WhatsApp' },
  { type: 'beeper', display: '@garden-member', name: '', expected: 'WhatsApp · @garden-member' },
  { type: 'handle', key: '@garden-member', display: '', name: '', expected: 'Custom chat · @garden-member' },
  { type: 'beeper', display: '@garden-member', name: 'Garden member', scope: true, expected: 'WhatsApp · Garden member · @garden-member · account 1' },
].map(row => ({ key: 'beeper:8:whatsapp:9:@user:x.y', scope: false, ...row })))('preserves readable $type identity evidence with display "$display"', ({ type, key, display, name, scope, expected }) => {
  const identifiers = [{ participant_id: 701, type, value: key, display_value: display, service_slug: type === 'beeper' ? 'whatsapp' : 'custom-chat', service_label: type === 'beeper' ? 'WhatsApp' : 'Custom chat', scope_kind: scope ? 'account' : '', scope_value: scope ? 'garden-account' : '' }];
  const participant = { merge_id: 41, participant_id: 701, origin_side: 'absorbed' };
  const listed = mergeAddresses({ participants: [participant], labels: labelsOf(identifiers.map(a => addressFixture(a.participant_id, a.type, a.value, { ...a, participant_display_name: name }))) }, 'absorbed', workspaceLabels(identifiers))[0];
  expect(addressText({ title: listed.value, detail: listed.detail })).toBe(expected);
});


it('renders migrated opaque Beeper IDs by their readable name', () => {
  expect(mergeSummaryRow({ merge, labels: labelsOf(migratedAddresses) }, workspaceLabels(migratedAddresses)).addresses.get(2)?.map(addressText)).toEqual(['WhatsApp · Garden member · account 1']);
});

it.each([
  { kind: 'workspace', values: ['T_TWO', 'T_ONE'], expected: ['workspace 1', 'workspace 2'] },
  { kind: 'account', values: ['account-b', 'account-a'], expected: ['account 1', 'account 2'] },
])('keeps $kind numbers when an earlier scope arrives', ({ kind, values, expected }) => {
  const addresses = values.map(scope_value => ({ scope_kind: kind, scope_value }));
  const scopes = workspaceLabels([addresses[1]], workspaceLabels([addresses[0]]));
  expect(addresses.map(address => scopeText(address, scopes))).toEqual(expected);
});
