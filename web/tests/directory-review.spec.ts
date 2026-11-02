import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
const peopleURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships' }));
const duplicatesURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', peopleDuplicates: true }));

test('People Filter opens duplicate cards and browser-restored History', async ({ page }) => {
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

test('refreshes cached duplicate tokens and the badge after Unlink', async ({ page }) => {
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
  await page.goto(peopleURL);
  await page.evaluate(async () => { await fetch('/api/v1/people/7/merge', { method: 'POST' }); });
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
  await expect.poll(() => fixture.requests.find(request => request.path.endsWith('/17/review/reject'))?.body).toEqual({ review_token: 'review-17-candidate' });
  await expect(badge).toHaveText('People 1');
});

for (const bulk of [false, true]) {
  test(`deleting a saved person through ${bulk ? 'bulk Delete' : 'More actions'} refreshes cached duplicates`, async ({ page }) => {
    const fixture = await installDirectoryReviewArchive(page);
    Object.assign(fixture.candidates[0]!, { left_kind: 'person', left_id: 7 });
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
