import { expect, test } from '@playwright/test';
import type { Input, PersonAddress, PersonProfilePatchRequest } from '../../src/lib/api/generated/models';
import { installMixedArchive } from './fixtures/mixed-archive';

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
