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

test('refreshes the duplicate queue and badge after Unlink', async ({ page }) => {
  const fixture = await installDirectoryReviewArchive(page);
  let unlinked = false;
  await page.route(/\/api\/v1\/participants\/701$/, route => route.fulfill({ json: {
    id: 701, display_label: 'Synthetic One', profile: { id: 7, display_name: 'Synthetic One' }, activity_count: 2,
    identifiers: [701, ...(unlinked ? [] : [702])].map(id => ({ participant_id: id, type: 'email', value: `person-${id}@example.com`, is_primary: true })),
    cluster: { canonical_id: 701, member_ids: unlinked ? [701] : [701, 702], edges: unlinked ? [] : [{ participant_a: 701, participant_b: 702 }] },
  } }));
  await page.route('**/api/v1/identity/unlinks', route => {
    unlinked = true;
    return route.fulfill({ json: { identity_revision: 10, cache_state: 'ready' } });
  });
  const badge = page.getByRole('navigation', { name: 'Primary' }).getByRole('button', { name: 'People', exact: true });
  const reads = () => fixture.requests.filter(request => request.query.limit === '11').length;
  const badgeReads = () => fixture.requests.filter(request => request.query.limit === '500').length;
  async function showDuplicates() {
    const before = reads();
    await page.getByRole('button', { name: 'Filter', exact: true }).click();
    await page.getByText('Possible duplicates', { exact: true }).click();
    await expect.poll(reads).toBeGreaterThan(before);
    await expect(page.locator('#duplicate-17')).toBeVisible();
  }
  await page.goto(duplicatesURL);
  await expect(page.locator('#duplicate-17')).toBeVisible();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Saved', { exact: true }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByRole('grid', { name: 'People results' }).getByText('Synthetic One', { exact: true }).click();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const beforeUnlink = badgeReads();
  await page.getByRole('button', { name: 'Unlink person-702@example.com', exact: true }).click();
  await page.getByRole('button', { name: 'Unlink', exact: true }).click();
  await expect.poll(() => unlinked).toBe(true);
  await expect.poll(badgeReads).toBeGreaterThan(beforeUnlink);
  await showDuplicates();
  await page.locator('#duplicate-17').getByRole('button', { name: 'Keep separate' }).click();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await expect(badge).toHaveText('People 2');
});

for (const bulk of [false, true]) {
  test(`deleting a saved person through ${bulk ? 'bulk Delete' : 'More actions'} refreshes cached duplicates`, async ({ page }) => {
    const fixture = await installDirectoryReviewArchive(page);
    Object.assign(fixture.candidates[0]!, { left_kind: 'person', left_id: 7, actionable: false, blocker: 'endpoint_unsupported' });
    let deleted = false;
    await page.route('**/api/v1/people/directory*', route => route.fulfill({ json: { people: [7, 9].filter(id => !deleted || id !== 7).map(id => ({ id, revision: id === 7 ? 4 : 2, display_name: id === 7 ? 'Synthetic One' : 'Synthetic Two', categories: [], organizations: [], primary_channel: 'email', contact_state: 'active' })) } }));
    await page.route(/\/api\/v1\/people\/7$/, route => {
      if (route.request().method() !== 'DELETE') return route.fallback();
      deleted = true;
      fixture.candidates.splice(0, 1);
      return route.fulfill({ status: 204 });
    });
    await page.goto(duplicatesURL);
    await expect(page.locator('#duplicate-17')).toBeVisible();
    await page.getByRole('button', { name: 'Back to People' }).click();
    await page.getByRole('button', { name: 'Filter', exact: true }).click();
    await page.getByText('Saved', { exact: true }).click();
    await page.getByRole('button', { name: 'Filter', exact: true }).click();
    if (bulk) {
      await page.getByRole('checkbox', { name: 'Select Synthetic One', exact: true }).check();
      await page.getByRole('button', { name: 'Delete', exact: true }).click();
      await page.getByRole('button', { name: 'Confirm delete', exact: true }).click();
      await expect(page.getByText('1 deleted.', { exact: true })).toBeVisible();
    } else {
      await page.getByRole('grid', { name: 'People results' }).getByText('Synthetic One', { exact: true }).click();
      await page.getByRole('button', { name: 'More actions', exact: true }).click();
      await page.getByRole('menuitem', { name: 'Delete', exact: true }).click();
      await page.getByRole('button', { name: 'Confirm delete person', exact: true }).click();
      await expect(page.getByRole('dialog', { name: 'Delete person' })).toBeHidden();
    }
    await expect.poll(() => deleted).toBe(true);
    await page.getByRole('button', { name: 'Filter', exact: true }).click();
    await page.getByText('Possible duplicates', { exact: true }).click();
    await expect(page.locator('#duplicate-17')).toHaveCount(0);
    await expect(page.locator('#duplicate-18')).toBeVisible();
  });
}

for (const profile of [{ id: 7, name: 'Synthetic One' }, { id: 9, name: 'Synthetic Two' }]) {
  test(`merge handoff inspects ${profile.name} separately without mutating`, async ({ page }) => {
    const fixture = await installDirectoryReviewArchive(page);
    await page.goto(duplicatesURL);
    await page.locator('#duplicate-19').getByRole('button', { name: 'Merge', exact: true }).click();
    await page.getByRole('button', { name: 'Back to People' }).click();
    const merge = page.getByRole('dialog', { name: 'Resolve person merge' });
    await expect(merge).toBeVisible();

    await merge.getByRole('button', { name: `Open ${profile.name} profile` }).focus();
    await page.keyboard.press('Enter');

    await expect(page).toHaveURL(new RegExp(`directoryPersonID%22%3A${profile.id}`));
    await expect(page.getByRole('heading', { name: profile.name })).toBeVisible();
    await page.getByRole('button', { name: 'More actions', exact: true }).click();
    await page.getByRole('menuitem', { name: 'History', exact: true }).click();
    await expect(page.getByText('No merges', { exact: true })).toBeVisible();
    await expect(page.getByRole('list', { name: 'Person merge history' })).toHaveCount(0);
    expect(fixture.requests.filter((request) => request.path.endsWith('/merge'))).toHaveLength(0);
    expect(fixture.requests.filter((request) => request.path.endsWith('/19/review/accept'))).toHaveLength(1);
  });
}

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
  await expect(metadata).toContainText('Valid from:');
  await expect(metadata).toContainText('Valid until:');
  await expect(metadata).toContainText('Superseded:');
  await expect(metadata).toContainText('Updated:');
  await expect.poll(() => metadata.evaluate(element => {
    const row = element.closest('li')!;
    const bounds = row.getBoundingClientRect();
    const range = document.createRange(); range.selectNodeContents(element);
    return [...range.getClientRects()].every(rect => rect.left >= bounds.left - 1 && rect.right <= bounds.right + 1);
  })).toBe(true);
  expect(await historyDialog.locator('.profile-history').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
});
