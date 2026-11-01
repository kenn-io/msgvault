import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
import { parseExploreURLState } from '../src/lib/explore/state.svelte';
const peopleURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships' }));
const duplicatesURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', peopleDuplicates: true }));

test('People Filter opens duplicate cards, evidence, and browser-restored History', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.goto(peopleURL);
  await expect(page.getByRole('navigation', { name: 'Primary' }).getByRole('button', { name: 'People', exact: true })).toContainText('3');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Possible duplicates', { exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Possible duplicates' })).toBeVisible();
  const card = page.locator('.cards > li').first();
  await card.getByText('Evidence and notes').click();
  await expect(card.getByRole('list', { name: 'Evidence for identity match 17' })).toContainText('Both synthetic endpoints');
  await page.getByRole('radio', { name: 'History', exact: true }).click();
  await expect(page.getByText('Nothing decided yet')).toBeVisible();
  await page.reload();
  await expect(page.getByRole('radio', { name: 'History', exact: true })).toBeChecked();
  expect(parseExploreURLState(new URL(page.url()).search).identityState).toBe('accepted');
  await page.goBack();
  await expect(page.getByRole('radio', { name: 'To review', exact: true })).toBeChecked();
});

test('duplicate Undo cancels a write and pagehide submits the staged decision', async ({ page }) => {
  const fixture = await installDirectoryReviewArchive(page);
  await page.goto(duplicatesURL);
  let card = page.locator('.cards > li').first();
  await card.getByRole('button', { name: 'Keep separate' }).click();
  await page.getByRole('button', { name: 'Undo' }).click();
  expect(fixture.requests.filter(request => request.method === 'POST')).toHaveLength(0);
  card = page.locator('.cards > li').first();
  await card.getByText('Evidence and notes').click();
  await card.getByLabel('Decision notes').fill('Different people');
  await card.getByRole('button', { name: 'Keep separate' }).click();
  await page.locator('.cards > li').first().getByRole('button', { name: 'Keep separate' }).click();
  await expect.poll(() => fixture.requests.filter(request => request.method === 'POST').length).toBe(1);
  await expect(page.locator('#duplicate-18')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Undo', exact: true })).toBeVisible();
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide')));
  await expect.poll(() => fixture.requests.filter(request => request.method === 'POST').length).toBe(2);
});


test('refreshes cached duplicate tokens and the badge after Merge with, Unlink, and Split', async ({ page }) => {
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
  await expect(page.locator('#duplicate-19')).toBeVisible();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Saved', { exact: true }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByRole('grid', { name: 'People results' }).getByText('Synthetic One', { exact: true }).click();
  await page.getByRole('button', { name: 'Merge with…', exact: true }).click();
  await page.getByRole('button', { name: /^Search people(: |$)/ }).click();
  await page.getByRole('combobox', { name: 'Search people', exact: true }).fill('Synthetic Two');
  await page.getByRole('option', { name: /Synthetic Two/ }).click();
  await page.getByRole('dialog', { name: 'Merge Synthetic One', exact: true }).getByRole('button', { name: 'Merge', exact: true }).click();
  const merge = page.getByRole('dialog', { name: 'Resolve person merge' });
  await merge.getByRole('radio', { name: 'Synthetic One (Person 7)' }).check();
  await merge.getByRole('checkbox', { name: /I understand/ }).check();
  await merge.getByRole('button', { name: 'Merge into selected survivor' }).click();
  await expect(badge).toContainText('2');
  await showDuplicates();
  await expect(page.locator('#duplicate-19')).toHaveCount(0);
  await page.getByRole('button', { name: 'Back to People' }).click();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const beforeUnlink = badgeReads();
  await page.getByRole('button', { name: 'Unlink person-702@example.com', exact: true }).click();
  await page.getByRole('button', { name: 'Unlink', exact: true }).click();
  await expect.poll(() => unlinked).toBe(true);
  await expect.poll(badgeReads).toBeGreaterThan(beforeUnlink);
  await showDuplicates();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await page.getByRole('button', { name: 'More actions', exact: true }).click();
  await page.getByRole('menuitem', { name: 'History', exact: true }).click();
  await page.getByRole('button', { name: 'Inspect merge 41' }).click();
  await page.getByRole('button', { name: 'Split merged profile', exact: true }).click();
  await page.getByRole('checkbox', { name: 'Participant 901', exact: true }).check();
  await page.getByRole('checkbox', { name: /I confirm splitting/ }).check();
  const beforeSplit = badgeReads();
  await page.getByRole('button', { name: 'Create restored person' }).click();
  await expect(page.getByRole('heading', { name: 'Split completed' })).toBeVisible();
  await expect.poll(badgeReads).toBeGreaterThan(beforeSplit);
  await page.getByRole('button', { name: 'Open source profile Synthetic One (Person 7)', exact: true }).click();
  await showDuplicates();
  await page.locator('#duplicate-17').getByRole('button', { name: 'Keep separate' }).click();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await expect.poll(() => fixture.requests.find(request => request.path.endsWith('/17/review/reject'))?.body).toEqual({ review_token: 'review-17-postsplit' });
  await expect(badge).toContainText('1');
});
