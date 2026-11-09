// Store fixtures are checked by TestPersonMerge_DetailAddressLabels and TestWebMergeIdentityFixturesMatchStore.
import migratedAddresses from './beeper-migrated-identities.test-fixture.json';
import phoneAddresses from './beeper-phone-identities.test-fixture.json';
import rawPhoneAddresses from './beeper-raw-phone-identities.test-fixture.json';
import mailboxAddresses from './mailbox-case-identities.test-fixture.json';
import { describe, expect, it } from 'vitest';
import type { ParticipantAddress, PersonMergeDetail, PersonMergeLabels } from '../api/generated/models';
import { addressFixture, identityKeyFixture } from './merge-fixtures.test-support';
import { addressText, mergeSummaryRow, mergeAddresses, mergedInAddresses, mergeSentence, savedAddresses } from './merge-labels';

const labelsOf = (addresses: ParticipantAddress[], count = addresses.length): PersonMergeLabels => ({ profiles: [], participant_ids: [...new Set(addresses.map(a => a.participant_id))], addresses, absorbed_address_count: count, incomplete: false });
const merge = { id: 41, survivor_person_id: 7, absorbed_person_id: 9 } as PersonMergeDetail['merge'];
const absorbed = (participant_id: number) => ({ merge_id: 41, participant_id, origin_side: 'absorbed' });

it('renders the store’s email and formatted phone through History and merged-in rows', () => {
  const labels = labelsOf(phoneAddresses);
  const participants = [absorbed(2)];
  const row = mergeSummaryRow({ merge, participants, labels });
  expect(row.count).toBe(2);
  for (const addresses of [mergeAddresses({ participants, labels }, 'absorbed'), row.addresses.get(2)!]) {
    expect(addresses.map(addressText)).toEqual(['+1 (202) 555-0123', 'Reader@Example.org']);
  }
  expect(mergedInAddresses([{ ...absorbed(2), merged_at: '2026-01-03T12:00:00Z', addresses: phoneAddresses }]).map(a => a.value)).toEqual(['+1 (202) 555-0123', 'Reader@Example.org']);
});

it('keeps the legacy Beeper phone readable when its saved copy matches', () => {
  const labels = labelsOf(rawPhoneAddresses, 1);
  labels.profiles = [{ id: 9, participant_ids: [1], contact_points: [{
    identity_key: '["whatsapp","","","+12025550123"]', address_kind: 'phone', original_value: '+1 (202) 555-0123', normalized_value: '+12025550123', service_slug: 'whatsapp', service_label: 'WhatsApp',
  }] }];
  const participants = [absorbed(1)];
  const row = mergeSummaryRow({ merge, participants, labels });
  expect(row.addresses.get(1)?.map(addressText)).toEqual(['WhatsApp · Reader · +12025550123']);
  expect(mergeAddresses({ merge, participants, labels }, 'absorbed').map(addressText)).toEqual(['WhatsApp · Reader · +12025550123']);
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
  expect(mergeSummaryRow(value).count).toBe(1);
  expect(mergeAddresses(value, 'absorbed')).toMatchObject([
    { participantID: 1, value: 'Reader@Example.org', splitID: 55 },
    { participantID: 2, value: 'reader@example.org', splitID: 56 },
  ]);
});

it('distinguishes a known empty participant from unavailable addresses', () => {
  const labels = { ...labelsOf([]), participant_ids: [701] };
  const value = { merge, participants: [absorbed(701)], labels };
  expect(mergeSummaryRow(value).addresses.get(701)).toEqual([]);
  expect(mergeSummaryRow(value).incomplete).toBe(false);
  expect(mergeSummaryRow({ ...value, labels: labelsOf([]) }).incomplete).toBe(true);
});

it('groups saved addresses by the keys supplied by the server', () => {
  const points = [
    { address_kind: 'email', original_value: 'Reader@Example.org', normalized_value: 'reader@example.org', identity_key: identityKeyFixture('email', 'reader@example.org') },
    { address_kind: 'email', original_value: 'curated@example.org', normalized_value: 'curated@example.org', identity_key: identityKeyFixture('email', 'curated@example.org') },
    { address_kind: 'email', original_value: 'Curated@example.org', normalized_value: 'curated@example.org', identity_key: identityKeyFixture('email', 'curated@example.org') },
    { address_kind: 'language', original_value: 'en', normalized_value: 'en', identity_key: identityKeyFixture('language', 'en') },
  ];
  expect(savedAddresses(points, new Set([identityKeyFixture('email', 'reader@example.org')])).map(addressText)).toEqual(['curated@example.org']);
});

it.each([
  { type: 'beeper', display: '', name: '', expected: 'WhatsApp' },
  { type: 'beeper', display: '', name: 'Neri Bloom', expected: 'WhatsApp · Neri Bloom' },
  { type: 'beeper', display: '@garden-member', name: '', expected: 'WhatsApp · @garden-member' },
  { type: 'handle', key: '@garden-member', display: '', name: '', expected: 'Custom chat · @garden-member' },
  { type: 'beeper', display: '@garden-member', name: 'Garden member', scope: true, expected: 'WhatsApp · Garden member · @garden-member · account: garden-account' },
].map(row => ({ key: 'beeper:8:whatsapp:9:@user:x.y', scope: false, ...row })))('preserves readable $type identity evidence with display "$display"', ({ type, key, display, name, scope, expected }) => {
  const identifiers = [{ participant_id: 701, type, value: key, normalized_value: type === 'beeper' ? `beeper:${key}` : 'garden-member', display_value: display, service_slug: type === 'beeper' ? 'whatsapp' : 'custom-chat', service_label: type === 'beeper' ? 'WhatsApp' : 'Custom chat', scope_kind: scope ? 'account' : '', scope_value: scope ? 'garden-account' : '' }];
  const participant = { merge_id: 41, participant_id: 701, origin_side: 'absorbed' };
  const listed = mergeAddresses({ participants: [participant], labels: labelsOf(identifiers.map(a => addressFixture(a.participant_id, a.type, a.value, { ...a, participant_display_name: name }))) }, 'absorbed')[0];
  expect(addressText({ title: listed.value, detail: listed.detail })).toBe(expected);
});


it('numbers workspaces from the complete merge for current, saved, and merged-in rows', () => {
  const addresses = ['T_TWO', 'T_ONE'].map(scope_value => addressFixture(701, 'slack', `${scope_value}:U_NERI`, { display_value: '', participant_display_name: 'Neri Bloom', service_slug: 'slack', service_label: 'Slack', scope_kind: 'workspace', scope_value }));
  const value = { merge, participants: [absorbed(701)], labels: labelsOf(addresses) };
  const expected = ['Slack · Neri Bloom · workspace 2', 'Slack · Neri Bloom · workspace 1'];
  const row = mergeSummaryRow(value);
  expect(row.addresses.get(701)?.map(addressText)).toEqual(expected);
  expect(mergeAddresses(value, 'absorbed').map(addressText)).toEqual(expected);
  expect(mergedInAddresses([{ ...absorbed(701), merged_at: '2026-01-03T12:00:00Z', addresses }]).map(a => a.value)).toEqual(expected);
  const point = { identity_key: identityKeyFixture('slack', 'T_TWO:U_NERI', 'workspace', 'T_TWO'), address_kind: 'username', original_value: 'Neri Bloom', normalized_value: 'T_TWO:U_NERI', service_slug: 'slack', service_label: 'Slack', scope_kind: 'workspace', scope_value: 'T_TWO' };
  expect(savedAddresses([point], new Set(), row.workspaceLabels).map(addressText)).toEqual(['Neri Bloom · Slack · workspace 2']);
});

it('renders migrated opaque Beeper IDs by their readable name', () => {
  expect(mergeSummaryRow({ merge, labels: labelsOf(migratedAddresses) }).addresses.get(2)?.map(addressText)).toEqual(['WhatsApp · Garden member · account: garden-account']);
});
