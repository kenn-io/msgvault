import { expect, test } from '@playwright/test';
import type { Input, PersonAddress, PersonProfilePatchRequest } from '../../src/lib/api/generated/models';
import { installDirectoryReviewArchive, installMixedArchive } from './fixtures/mixed-archive';

for (const [width, withoutSubtleCrypto] of [[1280, false], [420, false], [1280, true], [420, true]] as const) {
  test(`postal review preserves raw input and confirms once at ${width}px${withoutSubtleCrypto ? ' without SubtleCrypto' : ''}`, async ({ page }) => {
    if (withoutSubtleCrypto) {
      // Model the browser capability missing on supported remote HTTP origins.
      await page.addInitScript(() => Object.defineProperty(globalThis.crypto, 'subtle', { value: undefined, configurable: true }));
    }
    await installMixedArchive(page);
    const when = '2026-08-01T00:00:00Z';
    let revision = 1;
    let addresses: PersonAddress[] = [];
    let checks = 0;
    const writes: PersonProfilePatchRequest[] = [];
    const person = () => ({ id: 42, revision, participant_ids: [12], display_name: 'Archive Person', vcard_uid: 'urn:uuid:archive-person', created_at: when, updated_at: when });
    const profile = () => ({ person: person(), names: [], contact_points: [], addresses, dates: [], categories: [], media: [] });
    await page.route('**/api/v1/people/42/profile', async route => {
      if (route.request().method() === 'PATCH') {
        expect(route.request().headers()['if-match']).toBe('"person-42-r1"');
        const body = route.request().postDataJSON() as PersonProfilePatchRequest;
        writes.push(body); revision++;
        const saved = body.addresses!.add![0]!;
        addresses = [{ ...saved, person_id: 42, original_value: saved.original_input?.original_value ?? '', envelope: { ...saved.envelope, id: 81, ordinal: 0, created_at: when, updated_at: when, vcard: {} }, assessment: { original: saved.original_input!, proposed: saved, status: 'normalized_unverified', reason_codes: ['not_verified'], original_extended_parts: [], proposed_extended_parts: [] } }];
      }
      await route.fulfill({ json: profile(), headers: { ETag: `"person-42-r${revision}"` } });
    });
    await page.route('**/api/v1/people/42/addresses/assess', async route => {
      checks++;
      const body = route.request().postDataJSON() as { address: Input; allow_external: boolean };
      expect(body.allow_external).toBe(true);
      await route.fulfill({ json: { original: body.address, proposed: { ...body.address, street_address: '12 Example Street', country_code: 'CA' }, status: 'validated', reason_codes: [], provider: 'google', checked_at: when, original_extended_parts: [], proposed_extended_parts: [] }, headers: { 'Cache-Control': 'no-store' } });
    });
    await page.setViewportSize({ width, height: 900 });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 42 }))}`);
    expect(await page.evaluate(() => globalThis.crypto?.subtle === undefined)).toBe(withoutSubtleCrypto);
    const pane = page.getByRole('group', { name: 'Person page' });
    await pane.getByRole('radio', { name: 'Details', exact: true }).click();
    await pane.getByRole('button', { name: 'Add address', exact: true }).click();
    const form = pane.getByRole('form', { name: 'Edit addresses' });
    await form.getByLabel('Street address', { exact: true }).fill(' 12 Example St ');
    await form.getByLabel('Unit or apartment').fill(' Unit 7 ');
    await form.getByLabel('PO box').fill(' Box 9 ');
    await form.getByLabel('Country code').fill(' ca ');
    await form.getByLabel('Original text').fill(' raw source\nsecond line ');
    expect(checks).toBe(0);
    await form.getByRole('button', { name: 'Check with Google' }).click();
    await expect(form.getByRole('link', { name: 'Google Maps' })).toBeVisible();
    await expect(form.getByText('12 Example Street', { exact: false })).toBeVisible();
    await expect(form.getByLabel('Street address', { exact: true })).toHaveValue(' 12 Example St ');
    expect(writes).toHaveLength(0);
    await form.getByRole('button', { name: 'Check with Google' }).click();
    expect(checks).toBe(1);
    const layout = await form.evaluate(element => ({ width: element.getBoundingClientRect().width, scroll: element.scrollWidth, client: element.clientWidth }));
    expect(layout.width).toBeLessThanOrEqual(width);
    expect(layout.scroll).toBeLessThanOrEqual(layout.client + 1);
    await form.getByRole('button', { name: 'Save correction' }).click();
    await expect(form).toHaveCount(0);
    expect(writes).toHaveLength(1);
    expect(writes[0]!.addresses!.add![0]).toMatchObject({ street_address: '12 Example Street', extended_address: ' Unit 7 ', post_office_box: ' Box 9 ', original_input: { street_address: ' 12 Example St ', original_value: ' raw source\nsecond line ' }, envelope: { source: 'user' } });
    expect(writes[0]!.addresses!.add![0]).not.toHaveProperty('provider');
    await expect(pane.getByText(/12 Example Street/)).toBeVisible();
  });
}

test('an existing postal address keeps its source when checked and cleared', async ({ page }) => {
  await installMixedArchive(page);
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 42, revision: 1, participant_ids: [12], display_name: 'Archive Person', vcard_uid: 'urn:uuid:archive-person', created_at: when, updated_at: when };
  const original: Input = { address_kind: 'postal', street_address: 'Old street', free_text: 'Old delivery text', country_code: 'CA', original_value: 'Original source' };
  const addresses: PersonAddress[] = [{ ...original, person_id: 42, envelope: { source: 'user', id: 81, ordinal: 0, created_at: when, updated_at: when, vcard: {} }, assessment: { original, proposed: original, status: 'needs_review', reason_codes: [], original_extended_parts: [], proposed_extended_parts: [] } }];
  const writes: PersonProfilePatchRequest[] = [];
  await page.route('**/api/v1/people/42/profile', async route => {
    if (route.request().method() === 'PATCH') writes.push(route.request().postDataJSON());
    await route.fulfill({ json: { person, names: [], contact_points: [], addresses, dates: [], categories: [], media: [] }, headers: { ETag: '"person-42-r1"' } });
  });
  await page.route('**/api/v1/people/42/addresses/assess', async route => {
    const body = route.request().postDataJSON() as { address: Input };
    await route.fulfill({ json: { original: body.address, proposed: body.address, status: 'needs_review', reason_codes: [], original_extended_parts: [], proposed_extended_parts: [] } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 42 }))}`);
  const pane = page.getByRole('group', { name: 'Person page' });
  await pane.getByRole('radio', { name: 'Details', exact: true }).click();
  await pane.getByRole('button', { name: /Edit Address:.*Old street/ }).click();
  const form = pane.getByRole('form', { name: 'Edit addresses' });
  await form.getByRole('button', { name: 'Check locally' }).click();
  await expect(form.getByText('Original address')).toBeVisible();
  await form.getByLabel('Street address', { exact: true }).fill('');
  await form.getByLabel('Current address text').fill('');
  await form.getByRole('button', { name: 'Save address' }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]!.addresses!.add![0]).toMatchObject({ free_text: '', original_input: original });
  expect(writes[0]!.addresses!.add![0]).not.toHaveProperty('street_address');
  expect(writes[0]!.addresses!.supersede).toEqual([81]);
});

test('a sweep address stays tied to its evidence through check, edit, and save', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  const when = '2026-08-01T00:00:00Z';
  const revision = `sha256:${'a'.repeat(64)}`;
  const original: Input = { address_kind: 'postal', street_address: '12 Example Street', free_text: 'Delivery note', country_code: 'CA', original_value: 'Source address' };
  const assessment = { original, proposed: original, status: 'normalized_unverified', reason_codes: ['not_verified'], original_extended_parts: [], proposed_extended_parts: [] };
  const claim = { id: 71, claim_key: 'synthetic-claim', generation_id: 72, target: { kind: 'postal_address', key: 'system:postal_address', revision }, submitted_value: JSON.stringify(original), relation: 'support', origin: 'observed', confidence: { reported_score: 99 }, evidence_ids: [51], created_at: when, postal_review: { eligible: true, person_revision: 1, reason_codes: [], supporting_evidence_ids: [51], assessment } };
  const person = { id: 42, revision: 1, participant_ids: [12], display_name: 'Archive Person', vcard_uid: 'urn:uuid:archive-person', created_at: when, updated_at: when };
  const writes: PersonProfilePatchRequest[] = [];
  await page.route('**/api/v1/person-fact-targets*', route => route.fulfill({ json: { targets: [{ ...claim.target, description: 'Postal address', value_type: 'record', cardinality: 'multi', sensitive: true }] } }));
  await page.route('**/api/v1/people/42/fact-claims*', route => route.fulfill({ json: { claims: [claim] } }));
  await page.route('**/api/v1/people/42/profile', async route => {
    if (route.request().method() === 'PATCH') writes.push(route.request().postDataJSON());
    await route.fulfill({ json: { person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, headers: { ETag: '"person-42-r1"' } });
  });
  await page.route('**/api/v1/people/42/addresses/assess', async route => {
    const body = route.request().postDataJSON() as { address: Input };
    await route.fulfill({ json: { ...assessment, original: body.address, proposed: body.address } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 42 }))}`);
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const facts = page.getByRole('region', { name: 'Facts', exact: true });
  await facts.getByRole('button', { name: 'Show', exact: true }).click();
  await facts.getByRole('radio', { name: 'Claims' }).click();
  await facts.getByRole('button', { name: 'Reveal sensitive fact value' }).click();
  await facts.getByRole('button', { name: 'Review address' }).click();
  const form = facts.getByRole('form', { name: 'Edit addresses' });
  await expect(form.getByLabel('Street address', { exact: true })).toHaveValue('12 Example Street');
  await form.getByRole('button', { name: 'Check locally' }).click();
  await expect(form.getByText('Original address')).toBeVisible();
  await form.getByLabel('Street address', { exact: true }).fill('14 Example Street');
  await form.getByLabel('Current address text').fill('');
  await form.getByLabel('Save address as').selectOption('add');
  await form.getByRole('button', { name: 'Save address' }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]!.addresses!.add![0]).toMatchObject({ street_address: '14 Example Street', free_text: '', original_input: original, envelope: { source_ref: 'person-fact-claim:synthetic-claim' } });
});

test('a delayed check cannot restore a stale review after editing a manual address', async ({ page }) => {
  await installMixedArchive(page);
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 42, revision: 1, participant_ids: [12], display_name: 'Archive Person', vcard_uid: 'urn:uuid:archive-person', created_at: when, updated_at: when };
  const writes: PersonProfilePatchRequest[] = [];
  let release!: () => void;
  const pending = new Promise<void>(resolve => { release = resolve; });
  let checking = false;
  let completed = false;
  await page.route('**/api/v1/people/42/profile', async route => {
    if (route.request().method() === 'PATCH') writes.push(route.request().postDataJSON());
    await route.fulfill({ json: { person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, headers: { ETag: '"person-42-r1"' } });
  });
  await page.route('**/api/v1/people/42/addresses/assess', async route => {
    const body = route.request().postDataJSON() as { address: Input };
    checking = true;
    await pending;
    await route.fulfill({ json: { original: body.address, proposed: { ...body.address, street_address: 'Stale correction' }, provider: 'google', checked_at: when, status: 'validated', reason_codes: [], original_extended_parts: [], proposed_extended_parts: [] } }).catch(() => {});
    completed = true;
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 42 }))}`);
  const pane = page.getByRole('group', { name: 'Person page' });
  await pane.getByRole('radio', { name: 'Details', exact: true }).click();
  await pane.getByRole('button', { name: 'Add address', exact: true }).click();
  const form = pane.getByRole('form', { name: 'Edit addresses' });
  const street = form.getByLabel('Street address', { exact: true });
  await street.fill('12 Example Street');
  await form.getByLabel('Current address text').fill('Delivery note');
  await form.getByLabel('Original text').fill('Source address');
  await form.getByRole('button', { name: 'Check with Google' }).click();
  await expect.poll(() => checking).toBe(true);
  await street.fill('14 Example Street');
  await street.fill('12 Example Street');
  await form.getByLabel('Current address text').fill('');
  await expect(form.getByLabel('Original text')).toBeEditable();
  await form.getByLabel('Original text').fill('Corrected source');
  release();
  await expect.poll(() => completed).toBe(true);
  await expect(form.getByText('Stale correction')).toHaveCount(0);
  await expect(form.getByText('Google Maps')).toHaveCount(0);
  await form.getByRole('button', { name: 'Save address' }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]!.addresses!.add![0]).toMatchObject({ street_address: '12 Example Street', free_text: '', original_input: { original_value: 'Corrected source' } });
});
