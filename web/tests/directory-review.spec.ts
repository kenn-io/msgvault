import { expect, test } from '@playwright/test';
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

test('History wraps long provenance at phone width', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.setViewportSize({ width: 390, height: 900 });
  const profile = { id: 7, name: 'Synthetic One' };
  const reference = 'synthetic-provenance-reference-'.repeat(8);
  await page.route(`**/api/v1/people/${profile.id}/profile/history`, route => route.fulfill({ json: {
    person: { id: profile.id, revision: 4, display_name: profile.name, participant_ids: [], vcard_uid: `synthetic-${profile.id}`, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-07-01T00:00:00Z' },
    names: [{ person_id: profile.id, name_kind: 'formatted', formatted: 'Earlier name', original_value: 'Earlier name', is_derived: false, envelope: {
      id: 71, ordinal: 0, source: 'carddav_import', source_ref: reference, actor: 'synthetic-importer', confidence: 0.95,
      active_from: '2026-01-01T00:00:00Z', active_until: '2026-06-01T00:00:00Z', superseded_at: '2026-06-02T00:00:00Z',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-06-03T00:00:00Z', vcard: {},
    } }], contact_points: [], addresses: [], dates: [], categories: [], media: [], observations: [],
  } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('button', { name: 'More actions', exact: true }).click();
  await page.getByRole('menuitem', { name: 'History', exact: true }).click();
  const historyDialog = page.getByRole('dialog', { name: 'History', exact: true });
  await historyDialog.getByText('1 earlier value', { exact: true }).click();
  const metadata = historyDialog.locator('.meta').filter({ hasText: reference });
  await expect.poll(() => metadata.evaluate(element => {
    const row = element.closest('li')!;
    const bounds = row.getBoundingClientRect();
    const range = document.createRange(); range.selectNodeContents(element);
    return [...range.getClientRects()].every(rect => rect.left >= bounds.left - 1 && rect.right <= bounds.right + 1);
  })).toBe(true);
  expect(await historyDialog.locator('.profile-history').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
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
