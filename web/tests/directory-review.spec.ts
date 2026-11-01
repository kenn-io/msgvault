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

test('duplicate Undo restores the pair and the next decision settles exact evidence and notes', async ({ page }) => {
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
  expect(fixture.requests.find(request => request.method === 'POST')?.body).toEqual({ review_token: 'review-17-candidate', notes: 'Different people' });
  await page.getByRole('button', { name: 'Undo' }).click();
});

test('duplicate merge chooses a survivor through the existing revision-checked dialog', async ({ page }) => {
  const fixture = await installDirectoryReviewArchive(page);
  await page.goto(duplicatesURL);
  await page.locator('.cards > li').last().getByRole('button', { name: 'Merge', exact: true }).click();
  await page.getByRole('button', { name: 'Back to People' }).click();
  const merge = page.getByRole('dialog', { name: 'Resolve person merge' });
  await expect(merge).toBeVisible();
  await merge.getByRole('radio', { name: 'Synthetic One (Person 7)' }).click();
  await merge.getByRole('checkbox', { name: /I understand/ }).check();
  await merge.getByRole('button', { name: 'Merge into selected survivor' }).click();
  await expect(merge).toBeHidden();
  expect(fixture.requests.find(request => request.path.endsWith('/7/merge'))?.headers['if-match']).toBe('"person-7-r4", "person-9-r2"');
});
