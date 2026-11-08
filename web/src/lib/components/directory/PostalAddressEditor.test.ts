import { webcrypto } from 'node:crypto';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { Assessment, Input, PersonAddress } from '../../api/generated/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { postalFingerprint } from '../../directory/postal-review.svelte';
import StructuredProfileEditor from './StructuredProfileEditor.svelte';

vi.stubGlobal('crypto', webcrypto);
const when = '2026-08-01T00:00:00Z';
const person = { id: 7, revision: 3, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
const raw: Input = { address_kind: 'postal', street_address: ' 12 Example St ', extended_address: ' Unit 7 ', post_office_box: ' Box 9 ', country_code: ' ca ', original_value: ' raw\n address ' };

function setup(options: { unavailable?: boolean; localRejection?: boolean; review?: boolean; conflict?: boolean; current?: PersonAddress } = {}) {
  const requests: Request[] = [];
  const profile = { ...emptyProfile(person), addresses: options.current ? [options.current] : [] };
  const check = vi.fn(async (input: Input): Promise<Assessment> => ({
    original: input, proposed: options.localRejection ? input : { ...input, street_address: '12 Example Street', country_code: 'CA' },
    status: options.localRejection || options.review ? 'needs_review' : options.unavailable ? 'unavailable' : 'validated', provider: 'google',
    checked_at: options.localRejection ? undefined : when,
    reason_codes: options.localRejection ? ['external_country_unsuitable'] : options.unavailable ? ['provider_unavailable'] : [], fingerprint: await postalFingerprint(input),
  }));
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); requests.push(request);
    if (request.url.endsWith('/addresses/assess')) {
      const body = await request.clone().json() as { address: Input; allow_external: boolean };
      return Response.json(await check(body.address));
    }
    if (options.conflict) return Response.json({ error: 'person_revision_conflict', message: 'Profile changed' }, { status: 409 });
    return Response.json({ ...profile, person: { ...person, revision: 4 } }, { headers: { ETag: '"person-7-r4"' } });
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: profile, etags: { person: '"person-7-r3"', structuredProfile: '"person-7-r3"' }, errors: {} });
  return { controller, requests, check };
}

async function enterRaw() {
  for (const [label, value] of [['Street address', raw.street_address], ['Unit or apartment', raw.extended_address], ['PO box', raw.post_office_box], ['Country code', raw.country_code], ['Original text', raw.original_value]]) {
    await fireEvent.input(screen.getByLabelText(label!), { target: { value } });
  }
}

describe('Postal address editor', () => {
  it('saves raw input and unit/PO box/country without an implicit external check', async () => {
    const { controller, requests, check } = setup();
    render(StructuredProfileEditor, { controller, section: 'addresses' });
    await enterRaw();
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    const body = await requests[0]!.clone().json();
    expect(body.addresses.add[0]).toMatchObject({ ...raw, original_input: raw, envelope: { source: 'user' } });
    expect(check).not.toHaveBeenCalled();
  });

  it.each([false, true])('checks explicitly, shows original/proposed and attribution, then confirms a first-entry correction with raw input (needs review=%s)', async review => {
    const { controller, requests } = setup({ review });
    render(StructuredProfileEditor, { controller, section: 'addresses' });
    await enterRaw();
    await fireEvent.click(screen.getByRole('button', { name: 'Check with Google' }));
    const attribution = await screen.findByText('Google Maps');
    expect(attribution.getAttribute('translate')).toBe('no');
    const attributionStyle = getComputedStyle(attribution);
    expect(attributionStyle.whiteSpace).toBe('nowrap');
    expect(attributionStyle.fontFamily).toContain('Arial');
    expect(attributionStyle.fontStyle).toBe('normal');
    expect(attributionStyle.fontWeight).toBe('400');
    expect(attributionStyle.fontSize).toBe('14px');
    expect(attributionStyle.letterSpacing).toBe('normal');
    expect(attributionStyle.color).toBe('rgb(31, 31, 31)');
    expect(attributionStyle.backgroundColor).toBe('rgb(255, 255, 255)');
    const luminance = (color: string) => color.match(/\d+/g)!.slice(0, 3).map(Number).map(value => value / 255).map(value => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4).reduce((sum, value, index) => sum + value * [0.2126, 0.7152, 0.0722][index]!, 0);
    expect((luminance(attributionStyle.backgroundColor) + 0.05) / (luminance(attributionStyle.color) + 0.05)).toBeGreaterThanOrEqual(4.5);
    expect(screen.getByText('Original address')).toBeDefined(); expect(screen.getByText('Suggested address')).toBeDefined();
    expect(requests).toHaveLength(1);
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({ address: raw, allow_external: true });
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm corrected address' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    const body = await requests[1]!.clone().json();
    expect(body.addresses.add[0]).toMatchObject({ street_address: '12 Example Street', extended_address: raw.extended_address, post_office_box: raw.post_office_box, original_input: raw, envelope: { source: 'user' } });
    expect(body.addresses.add[0]).not.toHaveProperty('assessment');
    expect(body.addresses.add[0]).not.toHaveProperty('provider');
  });

  it('shows local rejection without Google attribution when no provider request was made', async () => {
    const { controller, requests } = setup({ localRejection: true });
    render(StructuredProfileEditor, { controller, section: 'addresses' });
    await enterRaw();
    await fireEvent.input(screen.getByLabelText('Country code'), { target: { value: 'ZZ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Check with Google' }));
    expect(await screen.findByText('Needs review')).toBeDefined();
    expect(screen.getByText('Enter a supported country code')).toBeDefined();
    expect(screen.queryByText('Google Maps')).toBeNull();
    expect(requests).toHaveLength(1);
    expect(requests[0]!.method).toBe('POST');
  });

  it('keeps the entered address and allows entry when the provider is unavailable', async () => {
    const { controller, requests } = setup({ unavailable: true });
    render(StructuredProfileEditor, { controller, section: 'addresses' });
    await enterRaw(); await fireEvent.click(screen.getByRole('button', { name: 'Check with Google' }));
    expect(await screen.findByText('The provider is unavailable')).toBeDefined();
    expect(screen.queryByText('Google Maps')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Keep original' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect((await requests[1]!.clone().json()).addresses.add[0]).toMatchObject({ ...raw, original_input: raw });
  });

  it('preserves reviewed revision after a check and a background profile refresh; a conflict requires new review', async () => {
    const { controller, requests } = setup({ conflict: true });
    render(StructuredProfileEditor, { controller, section: 'addresses' });
    await enterRaw(); await fireEvent.click(screen.getByRole('button', { name: 'Check with Google' }));
    await screen.findByText('Google Maps');
    controller.structuredProfileETag = '"person-7-r9"';
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm corrected address' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[1]!.headers.get('If-Match')).toBe('"person-7-r3"');
    expect(await screen.findByRole('alert')).toBeDefined();
    expect(requests.filter(request => request.method === 'PATCH')).toHaveLength(1);
  });

  it('preserves imported raw and untouched fields while declaring the edited row as user input', async () => {
    const current: PersonAddress = { person_id: 7, address_kind: 'postal', street_address: 'Old Street', extended_address: 'Unit 7', post_office_box: 'Box 9', country_code: 'CA', extended_components: '["District"]', geo_uri: 'geo:0,0', timezone: 'Europe/Paris', place_uri: 'urn:example', label: 'Postal label', free_text: 'Free text', original_value: raw.original_value!, assessment: { original: raw, proposed: raw, status: 'needs_review', reason_codes: ['not_verified'], fingerprint: 'original' }, envelope: { id: 11, ordinal: 3, pref: 1, type_label: 'home', type_tokens: ['home'], source: 'vcard_import', source_resource_uid: 'synthetic-resource', source_ref: 'person-fact-claim:synthetic-claim', vcard: { property: 'ADR', pid: ['1.1'] }, created_at: when, updated_at: when } };
    const { controller, requests } = setup({ current });
    render(StructuredProfileEditor, { controller, section: 'addresses', current });
    await fireEvent.input(screen.getByLabelText('Street address'), { target: { value: 'Changed Street' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    const address = (await requests[0]!.clone().json()).addresses.add[0];
    expect(address).toMatchObject({ extended_address: 'Unit 7', post_office_box: 'Box 9', extended_components: '["District"]', geo_uri: 'geo:0,0', timezone: 'Europe/Paris', place_uri: 'urn:example', label: 'Postal label', free_text: 'Free text', original_input: raw });
    expect(address.envelope).toEqual({ source: 'user', ordinal: 3, pref: 1, type_label: 'home', type_tokens: ['home'], source_ref: 'person-fact-claim:synthetic-claim' });
  });

  it('requires an explicit add-or-supersede choice for a sweep draft and retains its origin', async () => {
    const { controller, requests } = setup();
    render(StructuredProfileEditor, { controller, section: 'addresses', postalCandidate: { input: raw, sourceRef: 'person-fact-claim:synthetic-claim', personRevision: 3 } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    expect(requests).toHaveLength(0);
    await fireEvent.change(screen.getByLabelText('Save address as'), { target: { value: 'add' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    const body = await requests[0]!.clone().json();
    expect(body.addresses.supersede).toEqual([]);
    expect(body.addresses.add[0]).toMatchObject({ original_input: raw, envelope: { source: 'user', source_ref: 'person-fact-claim:synthetic-claim' } });
    expect(body.addresses.add[0].envelope).not.toHaveProperty('confidence');
  });

  it('retains presentation metadata when replacing an address with a suggestion', async () => {
    const current: PersonAddress = { person_id: 7, address_kind: 'postal', street_address: 'Old street', original_value: 'Old street', envelope: { id: 11, ordinal: 3, pref: 1, type_label: 'home', type_tokens: ['home'], source: 'user', created_at: when, updated_at: when, vcard: {} } };
    const { controller, requests } = setup({ current });
    render(StructuredProfileEditor, { controller, section: 'addresses', postalCandidate: { input: raw, sourceRef: 'person-fact-claim:synthetic-claim', personRevision: 3 } });
    await fireEvent.change(screen.getByLabelText('Save address as'), { target: { value: 'supersede:11' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    const body = await requests[0]!.clone().json();
    expect(body.addresses.supersede).toEqual([11]);
    expect(body.addresses.add[0].envelope).toMatchObject({ source: 'user', source_ref: 'person-fact-claim:synthetic-claim', ordinal: 3, pref: 1, type_label: 'home', type_tokens: ['home'] });
  });

  it('does not restore original text into a cleared current address', async () => {
    const original: Input = { address_kind: 'postal', original_value: 'Old street', country_code: 'CA' };
    const current: PersonAddress = { person_id: 7, address_kind: 'postal', original_value: 'Old street', country_code: 'CA', assessment: { original, proposed: { address_kind: 'postal', country_code: 'CA' }, status: 'needs_review', reason_codes: [], fingerprint: 'local' }, envelope: { id: 11, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } };
    const { controller, requests } = setup({ current });
    render(StructuredProfileEditor, { controller, section: 'addresses', current });
    expect(screen.getByLabelText('Current address text')).toHaveProperty('value', '');
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect((await requests[0]!.clone().json()).addresses.add[0]).not.toHaveProperty('free_text');
  });

  it('does not use a synthesized display value as original input', async () => {
    const original: Input = { address_kind: 'postal', street_address: 'Old street', country_code: 'CA' };
    const current: PersonAddress = { person_id: 7, address_kind: 'postal', street_address: 'Old street', country_code: 'CA', original_value: ';;Old street;;;;', assessment: { original, proposed: original, status: 'normalized_unverified', reason_codes: ['not_verified'], fingerprint: 'local' }, envelope: { id: 11, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } };
    const { controller, requests } = setup({ current });
    render(StructuredProfileEditor, { controller, section: 'addresses', current });
    expect(screen.getByLabelText('Original text')).toHaveProperty('value', '');
    await fireEvent.input(screen.getByLabelText('Street address'), { target: { value: '' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    const saved = (await requests[0]!.clone().json()).addresses.add[0];
    expect(saved).not.toHaveProperty('original_value');
    expect(saved).not.toHaveProperty('free_text');
  });

  it('sends an explicit empty current text when clearing a suggested address', async () => {
    const candidate: Input = { address_kind: 'postal', free_text: 'Old street', original_value: 'Old street' };
    const { controller, requests } = setup();
    render(StructuredProfileEditor, { controller, section: 'addresses', postalCandidate: { input: candidate, sourceRef: 'person-fact-claim:synthetic-claim', personRevision: 3 } });
    await fireEvent.change(screen.getByLabelText('Save address as'), { target: { value: 'add' } });
    await fireEvent.input(screen.getByLabelText('Current address text'), { target: { value: '' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect((await requests[0]!.clone().json()).addresses.add[0].free_text).toBe('');
  });

  it('does not restore source text after clearing a structured suggestion', async () => {
    const candidate: Input = { address_kind: 'postal', street_address: 'Old street', country_code: 'CA', original_value: 'Old street' };
    const { controller, requests } = setup();
    render(StructuredProfileEditor, { controller, section: 'addresses', postalCandidate: { input: candidate, sourceRef: 'person-fact-claim:synthetic-claim', personRevision: 3 } });
    await fireEvent.change(screen.getByLabelText('Save address as'), { target: { value: 'add' } });
    await fireEvent.input(screen.getByLabelText('Street address'), { target: { value: '' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect((await requests[0]!.clone().json()).addresses.add[0].free_text).toBe('');
  });
});


it('binds DOM edits to request invalidation and suppresses duplicate external clicks', async () => {
 const { controller, requests } = setup();
 let release!: (value: Assessment) => void;
 const pending = new Promise<Assessment>(resolve => release = resolve);
 const check = vi.spyOn(controller, 'assessPostalAddress').mockReturnValue(pending);
 render(StructuredProfileEditor, { controller, section: 'addresses' });
 await enterRaw();
 const button = screen.getByRole('button', { name: 'Check with Google' });
 await fireEvent.click(button);
 await waitFor(() => expect(check).toHaveBeenCalledOnce());
 await fireEvent.click(button);
 expect(check).toHaveBeenCalledOnce();
 await fireEvent.input(screen.getByLabelText('Street address'), { target: { value: 'Changed street' } });
 await fireEvent.input(screen.getByLabelText('Street address'), { target: { value: raw.street_address } });
 release({ original: raw, proposed: { ...raw, street_address: 'Stale correction' }, provider: 'google', status: 'validated', reason_codes: [], fingerprint: await postalFingerprint(raw) });
 await waitFor(() => expect(screen.queryByText('Checking address…')).toBeNull());
 expect(screen.queryByText('Google Maps')).toBeNull();
 expect(screen.queryByText('Stale correction')).toBeNull();
 expect(requests).toHaveLength(0);
});

it('drops a pending result if the component receives a different person controller', async () => {
 const { controller } = setup();
 let release!: (value: Assessment) => void;
 const pending = new Promise<Assessment>(resolve => release = resolve);
 const check = vi.spyOn(controller, 'assessPostalAddress').mockReturnValue(pending);
 const view = render(StructuredProfileEditor, { controller, section: 'addresses' });
 await enterRaw(); await fireEvent.click(screen.getByRole('button', { name: 'Check with Google' }));
 await waitFor(() => expect(check).toHaveBeenCalledOnce());
 const nextPerson = { ...person, id: 8 };
 const next = new DirectoryProfileController(createAPIClient(async () => Response.json(emptyProfile(nextPerson))), 8, { person: nextPerson, structuredProfile: emptyProfile(nextPerson), etags: { person: '"person-8-r3"', structuredProfile: '"person-8-r3"' }, errors: {} });
 await view.rerender({ controller: next, section: 'addresses' });
 release({ original: raw, proposed: raw, provider: 'google', status: 'validated', reason_codes: [], fingerprint: await postalFingerprint(raw) });
 await waitFor(() => expect(screen.queryByText('Checking address…')).toBeNull());
 expect(screen.queryByText('Google Maps')).toBeNull();
});

it('makes an explicit local check without granting external consent', async () => {
 const { controller, requests } = setup();
 render(StructuredProfileEditor, { controller, section: 'addresses' });
 await enterRaw(); await fireEvent.click(screen.getByRole('button', { name: 'Check locally' }));
 await waitFor(() => expect(requests).toHaveLength(1));
 expect((await requests[0]!.clone().json()).allow_external).toBe(false);
 expect(requests.some(request => request.method === 'PATCH')).toBe(false);
});

it('keeps captured provenance read-only and saves current raw-only text edits separately', async () => {
  const original: Input = { address_kind: 'postal', original_value: '  First raw text\nUnit 7  ' };
  const current: PersonAddress = { person_id: 7, address_kind: 'postal', original_value: original.original_value!, assessment: { original, proposed: original, status: 'needs_review', reason_codes: ['not_verified'], fingerprint: 'raw' }, envelope: { id: 11, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } };
  const { controller, requests } = setup({ current });
  render(StructuredProfileEditor, { controller, section: 'addresses', current });
  expect(screen.getByLabelText('Original text')).toHaveProperty('readOnly', true);
  await fireEvent.input(screen.getByLabelText('Current address text'), { target: { value: '  Updated raw text\nUnit 8  ' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
  await waitFor(() => expect(requests).toHaveLength(1));
  expect((await requests[0]!.clone().json()).addresses.add[0]).toMatchObject({ free_text: '  Updated raw text\nUnit 8  ', original_input: original });
});

it('keeps the first snapshot while saving current free-text edits after the first check', async () => {
  const { controller, requests } = setup();
  render(StructuredProfileEditor, { controller, section: 'addresses' });
  await fireEvent.input(screen.getByLabelText('Original text'), { target: { value: '  First raw draft  ' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Check locally' }));
  await screen.findByText('Original address');
  expect(screen.getByLabelText('Original text')).toHaveProperty('readOnly', true);
  await fireEvent.input(screen.getByLabelText('Current address text'), { target: { value: '  Revised text after check  ' } });
  expect(screen.queryByText('Google Maps')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
  await waitFor(() => expect(requests).toHaveLength(2));
  const saved = (await requests[1]!.clone().json()).addresses.add[0];
  expect(saved.free_text).toBe('  Revised text after check  ');
  expect(saved.original_input.original_value).toBe('  First raw draft  ');
});


it.each(['birth_place', 'death_place'])('keeps %s edits outside postal provenance and current-text capture', async kind => {
  const text = 'msgvault:postal-input:v1\n{"schema":1,"input":{"original_value":"literal place"}}';
  const current: PersonAddress = { person_id: 7, address_kind: kind, original_value: text, envelope: { id: 11, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } };
  const { controller, requests } = setup({ current });
  render(StructuredProfileEditor, { controller, section: 'addresses', current });
  expect(screen.queryByLabelText('Current address text')).toBeNull();
  await fireEvent.input(screen.getByLabelText('Country'), { target: { value: 'Canada' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
  await waitFor(() => expect(requests).toHaveLength(1));
  const saved = (await requests[0]!.clone().json()).addresses.add[0];
  expect(saved.original_value).toBe(text);
  expect(saved).not.toHaveProperty('free_text');
  expect(saved).not.toHaveProperty('original_input');
});

it.each(['birth_place', 'death_place'])('saves the entered %s as the place text', async kind => {
  const current: PersonAddress = { person_id: 7, address_kind: kind, original_value: 'Old place', envelope: { id: 11, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } };
  const { controller, requests } = setup({ current });
  render(StructuredProfileEditor, { controller, section: 'addresses', current });
  await fireEvent.input(screen.getByLabelText('Street address'), { target: { value: 'New place' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
  await waitFor(() => expect(requests).toHaveLength(1));
  expect((await requests[0]!.clone().json()).addresses.add[0].original_value).toBe('New place');
});
