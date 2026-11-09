import { expect, test } from '@playwright/test';
import type { PersonContactPoint } from '../src/lib/api/generated/models';
import { installDirectoryReviewArchive, installMixedArchive } from './e2e/fixtures/mixed-archive';
const peopleURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships' }));
const duplicatesURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', peopleDuplicates: true }));

test('inline People fields use the shared field scale on a phone', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await page.getByRole('button', { name: 'Add phone', exact: true }).click();
  const form = page.getByRole('form', { name: 'Edit contact points' });
  for (const field of [form.locator('.kit-text-input').first(), form.locator('select')]) {
    expect(await field.evaluate(element => ({ height: element.getBoundingClientRect().height, font: getComputedStyle(element).fontSize }))).toEqual({ height: 32, font: '16px' });
  }
});

test('People Filter restores History on reload and To review on Back', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.goto(peopleURL);
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Possible duplicates', { exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Possible duplicates' })).toBeVisible();
  await page.getByRole('radio', { name: 'History', exact: true }).click();
  await expect(page.getByText('Nothing decided yet')).toBeVisible();
  await page.reload();
  await expect(page.getByRole('radio', { name: 'History', exact: true })).toBeChecked();
  await page.goBack();
  await expect(page.getByRole('radio', { name: 'To review', exact: true })).toBeChecked();
  await page.getByRole('radio', { name: 'History', exact: true }).click();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Possible duplicates', { exact: true }).click();
  await expect(page.getByRole('radio', { name: 'To review', exact: true })).toBeChecked();
});

test('History fits within the dialog at phone width', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.setViewportSize({ width: 390, height: 900 });
  const profile = { id: 7, name: 'Synthetic One' };
  const earlierName = 'SyntheticEarlierName'.repeat(12);
  await page.route(`**/api/v1/people/${profile.id}/profile/history`, route => route.fulfill({ json: {
    person: { id: profile.id, revision: 4, display_name: profile.name, participant_ids: [], vcard_uid: `synthetic-${profile.id}`, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-07-01T00:00:00Z' },
    names: [{ person_id: profile.id, name_kind: 'formatted', formatted: earlierName, original_value: earlierName, is_derived: false, envelope: {
      id: 71, ordinal: 0, source: 'carddav_import', source_ref: 'synthetic-reference', actor: 'synthetic-importer', confidence: 0.95,
      active_from: '2026-01-01T00:00:00Z', active_until: '2026-06-01T00:00:00Z', superseded_at: '2026-06-02T00:00:00Z',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-06-03T00:00:00Z', vcard: {},
    } }], contact_points: [], addresses: [], dates: [], categories: [], media: [], observations: [],
  } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('button', { name: 'More actions', exact: true }).click();
  await page.getByRole('menuitem', { name: 'History', exact: true }).click();
  const historyDialog = page.getByRole('dialog', { name: 'History', exact: true });
  await historyDialog.getByText('1 earlier value', { exact: true }).click();
  await expect(historyDialog.getByText(earlierName)).toBeVisible();
  expect(await historyDialog.locator('.profile-history').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
});

test('History lays its groups and rows on the Layout scale', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  const profile = { id: 7, name: 'Synthetic One' };
  const envelope = (id: number) => ({ id, ordinal: 0, source: 'user', actor: 'user', confidence: 1, active_from: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} });
  await page.route(`**/api/v1/people/${profile.id}/profile/history`, route => route.fulfill({ json: {
    person: { id: profile.id, revision: 4, display_name: profile.name, participant_ids: [], vcard_uid: `synthetic-${profile.id}`, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-07-01T00:00:00Z' },
    names: [{ person_id: profile.id, name_kind: 'formatted', formatted: profile.name, original_value: profile.name, is_derived: false, envelope: envelope(71) }],
    contact_points: [{ identity_key: '["email","","","one@example.org"]', person_id: profile.id, address_kind: 'email', original_value: 'one@example.org', normalized_value: 'one@example.org', normalization: 'email', normalization_version: 1, envelope: envelope(72) } satisfies PersonContactPoint],
    addresses: [], dates: [], categories: [], media: [], observations: [],
  } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('button', { name: 'More actions', exact: true }).click();
  await page.getByRole('menuitem', { name: 'History', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'History', exact: true });
  const names = dialog.getByRole('heading', { name: 'Names', exact: true });
  const email = dialog.getByRole('heading', { name: 'Email and phone', exact: true });
  await expect(email).toBeVisible();
  const rect = (locator: import('@playwright/test').Locator) => locator.evaluate(el => { const r = el.getBoundingClientRect(); return { top: r.top, bottom: r.bottom, height: r.height }; });
  const nameHeading = await rect(names);
  const nameRow = await rect(dialog.locator('.detail-row', { hasText: profile.name }));
  const emailHeading = await rect(email);
  // A group's heading is 8 above its rows, a value over its provenance line is a 54 row, and groups are 24 apart.
  expect.soft(nameRow.top - nameHeading.bottom, 'heading to rows').toBe(8);
  expect.soft(nameRow.height, 'two-line row').toBe(54);
  expect.soft(emailHeading.top - nameRow.bottom, 'between groups').toBe(24);
});

test('address actions align with the first text line while details opens', async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/participants/12', route => route.fulfill({ json: {
    id: 12, display_label: 'Archive Person', partial_label: false, identifiers: [
      { type: 'email', value: 'linked@example.com', participant_id: 34, is_primary: true, provenance: 'participant_identifiers' }
    ], activity_count: 3, file_count: 1, source_counts: [], cache_revision: 'synthetic',
    cluster: { canonical_id: 12, member_ids: [12, 34], edges: [{ participant_a: 12, participant_b: 34 }] }
  } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipTarget: 'cluster:12' })));
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const identity = page.getByRole('group', { name: 'Identity linked@example.com', exact: true });
  const row = identity.locator('..').locator('..');
  const summary = identity.locator('summary');
  for (const expanded of [false, true]) {
    if (expanded) await summary.click();
    const positions = await row.evaluate(element => {
      const text = document.createRange(); text.selectNodeContents(element.querySelector('summary')!);
      const bounds = text.getBoundingClientRect();
      return { text: bounds.top + bounds.height / 2, actions: [...element.querySelectorAll('button')].map(button => { const box = button.getBoundingClientRect(); return box.top + box.height / 2; }) };
    });
    expect(positions.actions).toHaveLength(2);
    for (const center of positions.actions) expect(Math.abs(center - positions.text)).toBeLessThanOrEqual(2);
  }
});

for (const width of [1440, 390]) {
  test(`first source choices align with their heading at ${width}`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
    await page.route('**/api/v1/carddav/connections', route => route.fulfill({ json: { connections: [] } }));
    await page.setViewportSize({ width, height: 1000 });
    await page.goto('/?workspace=sources');
    const heading = page.getByRole('heading', { name: 'Add your first source', exact: true });
    await expect(heading).toBeVisible();
    const label = page.getByRole('button', { name: /^Gmail Includes/ }).getByText('Gmail', { exact: true });
    const titleBounds = await heading.boundingBox();
    const labelBounds = await label.boundingBox();
    expect(Math.abs(labelBounds!.x - titleBounds!.x)).toBeLessThanOrEqual(1);
  });
}

test('History and Split fit a 390px dialog', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 });
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await installDirectoryReviewArchive(page);
  await page.goto(peopleURL);
  await page.evaluate(() => fetch('/api/v1/people/7/merge', {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'If-Match': '"person-7-r4", "person-9-r2"', 'Idempotency-Key': 'synthetic-history-phone' },
    body: JSON.stringify({ absorbed_person_id: 9 }),
  }).then(() => undefined));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('button', { name: 'More actions', exact: true }).click();
  await page.getByRole('menuitem', { name: 'History', exact: true }).click();
  const dialog = page.getByRole('dialog');
  const row = dialog.getByRole('button', { name: /^Open Synthetic Two merged into Synthetic One/ });
  await row.click();
  await dialog.getByRole('button', { name: 'Split…', exact: true }).click();
  const choice = dialog.getByRole('checkbox').first();
  await choice.check();
  await dialog.getByRole('button', { name: 'Split into new person', exact: true }).click();
  await dialog.getByRole('button', { name: 'Open Synthetic Restored', exact: true }).waitFor();
  expect(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  expect(errors).toEqual([]);
});
