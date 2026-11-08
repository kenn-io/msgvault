import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import FactLedger from './FactLedger.svelte';

const when = '2026-08-01T00:00:00Z';
const person = { id: 7, revision: 3, participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
const raw = { address_kind: 'postal', street_address: '12 Example Street', extended_address: 'Unit 7', country_code: 'CA', original_value: 'Source address text' };
const revision = `sha256:${'a'.repeat(64)}`;

function setup(reason?: string, evidenceResponse?: Promise<Response>) {
  const requests: Request[] = [];
  let adopted = false;
  const server = { reason, absent: false, fail: false, raw, personRevision: 3, readClaims: undefined as (() => Promise<Response>) | undefined };
  let claimReads = 0;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); requests.push(request);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [{ kind: 'postal_address', key: 'system:postal_address', revision, description: 'Postal address', value_type: 'record', cardinality: 'multi', sensitive: true }] });
    if (path.endsWith('/fact-claims')) {
      claimReads++;
      if (server.readClaims) return server.readClaims();
      if (server.fail) return Response.json({ message: 'Synthetic failure' }, { status: 503 });
      if (server.absent) return Response.json({ claims: [] });
      return Response.json({ claims: [{ id: 1, claim_key: 'synthetic-claim', generation_id: 1, target: { kind: 'postal_address', key: 'system:postal_address', revision }, submitted_value: JSON.stringify(raw), relation: 'equals', origin: 'observed', confidence: { reported_score: 99 }, evidence_ids: [5], created_at: when, postal_review: { eligible: !server.reason && !adopted, person_revision: server.personRevision, reason_codes: adopted ? ['already_adopted'] : server.reason ? [server.reason] : [], supporting_evidence_ids: [5], assessment: { original: server.raw, proposed: server.raw, status: 'normalized_unverified', reason_codes: ['not_verified'], fingerprint: 'local' } } }] });
    }
    if (path.endsWith('/fact-evidence')) return evidenceResponse ?? Response.json({ evidence: [] });
    if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
    if (request.method === 'PATCH') { adopted = true; return Response.json(emptyProfile({ ...person, revision: 4 }), { headers: { ETag: '"person-7-r4"' } }); }
    throw new Error(`Unexpected request ${path}`);
  });
  const ledger = new FactLedgerController(client);
  const profileController = new DirectoryProfileController(client, 7, { person, structuredProfile: emptyProfile(person), etags: { person: '"person-7-r3"', structuredProfile: '"person-7-r3"' }, errors: {} });
  ledger.applyContext(true, 7);
  const view = render(FactLedger, { controller: ledger, profileController });
  return { ledger, profileController, requests, server, view, claimReads: () => claimReads };
}

describe('Postal claim review', () => {
  it('keeps an edited claim open when the initial evidence response arrives', async () => {
    let release!: (response: Response) => void;
    const evidence = new Promise<Response>(resolve => { release = resolve; });
    const { ledger } = setup(undefined, evidence);
    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Reveal sensitive fact value' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Review address' }));
    const street = await screen.findByLabelText('Street address');
    await fireEvent.input(street, { target: { value: 'Edited street' } });
    release(Response.json({ evidence: [] }));
    await waitFor(() => expect(ledger.evidence.loading).toBe(false));
    expect(screen.getByLabelText('Street address')).toHaveProperty('value', 'Edited street');
  });

  it('uses server eligibility, reveals the sensitive address deliberately, and adopts once through guarded profile PATCH', async () => {
    const { requests } = setup();
    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Reveal sensitive fact value' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Review address' }));
    expect(await screen.findByLabelText('Unit or apartment')).toHaveProperty('value', 'Unit 7');
    expect(requests.some(request => request.url.endsWith('/addresses/assess'))).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    const target = screen.getByLabelText('Save address as');
    expect(document.activeElement).toBe(target);
    expect(target.getAttribute('aria-invalid')).toBe('true');
    expect(screen.getByText('Choose how to save this address.')).toBeDefined();
    await fireEvent.change(screen.getByLabelText('Save address as'), { target: { value: 'add' } });
    expect(screen.queryByText('Choose how to save this address.')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));
    await waitFor(() => expect(requests.filter(request => request.method === 'PATCH')).toHaveLength(1));
    const patch = requests.find(request => request.method === 'PATCH')!;
    expect(patch.headers.get('If-Match')).toBe('"person-7-r3"');
    const body = await patch.clone().json();
    expect(body.addresses.add[0]).toMatchObject({ original_input: raw, envelope: { source: 'user', source_ref: 'person-fact-claim:synthetic-claim' } });
    expect(body.addresses.add[0].envelope).not.toHaveProperty('confidence');
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Review address' })).toBeNull());
    await fireEvent.click(await screen.findByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(screen.getByText('Already saved')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Review address' })).toBeNull();
  });

  it.each([['stale_target_revision', 'This suggestion is out of date'], ['support_unavailable', 'Evidence unavailable'], ['invalid_claim', 'This suggestion cannot be used'], ['already_adopted', 'Already saved']])('keeps %s claims inspectable without offering supported adoption', async (reason, explanation) => {
    const { requests } = setup(reason);
    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(screen.queryByRole('button', { name: 'Review address' })).toBeNull();
    expect(screen.getByText(explanation)).toBeDefined();
    expect(requests.some(request => request.method === 'PATCH' || request.url.endsWith('/addresses/assess'))).toBe(false);
  });
});

async function revealPostalClaim() {
  await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Reveal sensitive fact value' }));
}

it.each(['support_unavailable', 'outside_validity', 'absent', 'failure'])('refreshes reopened supported review and fails closed for %s', async failure => {
  const { server, claimReads } = setup();
  await revealPostalClaim();
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  await screen.findByLabelText('Street address');
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  const before = claimReads();
  if (failure === 'absent') server.absent = true;
  else if (failure === 'failure') server.fail = true;
  else server.reason = failure;
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  expect(screen.queryByLabelText('Street address')).toBeNull();
  await screen.findByRole('alert');
  expect(claimReads()).toBe(before + 1);
  expect(screen.queryByLabelText('Street address')).toBeNull();
});

it('dismisses a failed opening so another address review can be attempted', async () => {
  const { server, ledger } = setup();
  await revealPostalClaim();
  server.fail = true;
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  await screen.findByRole('alert');
  await fireEvent.click(screen.getByRole('button', { name: 'Dismiss address review error' }));
  expect(ledger.postalReview).toBeUndefined();
  server.fail = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  expect(await screen.findByLabelText('Street address')).toBeDefined();
});

it('labels a cleared replacement target without presenting its original text as current', async () => {
  const { profileController } = setup();
  profileController.structuredProfile!.addresses.push({
    person_id: 7, address_kind: 'postal', original_value: 'Old source address', free_text: '',
    envelope: { id: 26, ordinal: 1, source: 'user', created_at: when, updated_at: when, vcard: {} },
  });
  await revealPostalClaim();
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  expect(await screen.findByRole('option', { name: 'Replace: No current address' })).toBeDefined();
  expect(screen.queryByRole('option', { name: 'Replace: Old source address' })).toBeNull();
});

it('opens the exact freshly returned assessment and refuses a mismatched profile revision', async () => {
  const { server } = setup();
  await revealPostalClaim();
  server.raw = { ...raw, street_address: 'Freshly reviewed street' };
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  expect(await screen.findByLabelText('Street address')).toHaveProperty('value', 'Freshly reviewed street');
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  server.personRevision = 4;
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  await screen.findByRole('alert');
  expect(screen.queryByLabelText('Street address')).toBeNull();
});

it('cancels an opening read, suppresses duplicate clicks, and ignores its response after a newer open', async () => {
  const { server, claimReads } = setup();
  await revealPostalClaim();
  let release!: (response: Response) => void;
  server.readClaims = () => new Promise<Response>(resolve => { release = resolve; });
  const before = claimReads();
  const button = screen.getByRole('button', { name: 'Review address' });
  await fireEvent.click(button); await fireEvent.click(button);
  await screen.findByRole('button', { name: 'Cancel opening address review' });
  expect(claimReads()).toBe(before + 1);
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel opening address review' }));
  server.readClaims = undefined;
  server.raw = { ...raw, street_address: 'New open street' };
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  expect(await screen.findByLabelText('Street address')).toHaveProperty('value', 'New open street');
  release(Response.json({ claims: [] }));
  await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  expect(screen.getByLabelText('Street address')).toHaveProperty('value', 'New open street');
});

it('drops an opening response when ledger context changes to another person', async () => {
  const { server, ledger } = setup();
  await revealPostalClaim();
  let release!: (response: Response) => void;
  server.readClaims = () => new Promise<Response>(resolve => { release = resolve; });
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  await screen.findByRole('button', { name: 'Cancel opening address review' });
  ledger.applyContext(false, null);
  release(Response.json({ claims: [] }));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Cancel opening address review' })).toBeNull());
  expect(screen.queryByLabelText('Street address')).toBeNull();
});


it.each(['history restoration', 'section change', 'profile replacement'])('cancels an opening review on %s without surfacing a late error', async change => {
  const { server, ledger, profileController, view } = setup();
  await revealPostalClaim();
  let release!: (response: Response) => void;
  server.readClaims = () => new Promise<Response>(resolve => { release = resolve; });
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  await screen.findByRole('button', { name: 'Cancel opening address review' });
  server.readClaims = undefined;
  if (change === 'history restoration') ledger.applyContext(true, 7, true);
  else if (change === 'section change') await fireEvent.click(screen.getByRole('radio', { name: 'Evidence' }));
  else await view.rerender({ controller: ledger, profileController: new DirectoryProfileController(createAPIClient(async () => Response.json({})), 7, { person, structuredProfile: emptyProfile(person), etags: { structuredProfile: '\"person-7-r3\"' }, errors: {} }) });
  release(Response.json({ message: 'Late failure' }, { status: 503 }));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Cancel opening address review' })).toBeNull());
  expect(screen.queryByLabelText('Street address')).toBeNull();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('refuses an opening result if the viewed profile revision changes while the read is pending', async () => {
  const { server, profileController } = setup();
  await revealPostalClaim();
  let release!: (response: Response) => void;
  server.readClaims = () => new Promise<Response>(resolve => { release = resolve; });
  await fireEvent.click(screen.getByRole('button', { name: 'Review address' }));
  await screen.findByRole('button', { name: 'Cancel opening address review' });
  server.readClaims = undefined;
  profileController.person = { ...person, revision: 4 };
  // The old response still describes revision 3, which cannot open against revision 4.
  release(Response.json({ claims: [{ claim_key: 'synthetic-claim', target: { kind: 'postal_address', key: 'system:postal_address', revision }, postal_review: { eligible: true, person_revision: 3, assessment: { original: raw } } }] }));
  await screen.findByRole('alert');
  expect(screen.queryByLabelText('Street address')).toBeNull();
});
