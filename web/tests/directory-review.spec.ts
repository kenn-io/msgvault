import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
const peopleURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships' }));
const duplicatesURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', peopleDuplicates: true }));

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
