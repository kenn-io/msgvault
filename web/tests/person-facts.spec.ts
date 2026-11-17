import { expect, test, type Page } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
import { installPersonFacts, openPersonFacts } from './e2e/fixtures/person-facts';
import { stubOperationStatus } from './kit-ui';

async function startFacts(page: Page) {
  await installDirectoryReviewArchive(page);
  await stubOperationStatus(page, ['person_sweep']);
  await installPersonFacts(page);
  return openPersonFacts(page);
}

async function chooseLocation(facts: Awaited<ReturnType<typeof openPersonFacts>>) {
  await facts.getByRole('button', { name: 'Search fields: All fields' }).click();
  await facts.getByRole('option', { name: 'Location', exact: true }).click();
}

test('Location shows the saved value above its competing claim', async ({ page }) => {
  const facts = await startFacts(page);
  await chooseLocation(facts);
  const saved = facts.getByRole('region', { name: 'Selected saved value' });
  const claim = facts.getByLabel('Fact claim');
  await expect(saved).toContainText('Boston');
  await expect(claim).toContainText('New York');
  expect(await saved.evaluate((element, following) => Boolean(element.compareDocumentPosition(following) & Node.DOCUMENT_POSITION_FOLLOWING), await claim.elementHandle())).toBe(true);
});

test('Manage fields opens the Fields page', async ({ page }) => {
  const facts = await startFacts(page);
  await facts.getByRole('button', { name: 'Search fields: All fields' }).click();
  await page.getByRole('option', { name: 'Manage fields…' }).click();
  await expect(page.getByRole('heading', { name: 'Fields', exact: true })).toBeVisible();
});

test('a claim links to its decision', async ({ page }) => {
  const facts = await startFacts(page);
  await chooseLocation(facts);
  const claim = facts.getByLabel('Fact claim');
  await claim.getByRole('button', { name: 'Details' }).click();
  await claim.getByRole('button', { name: 'Show decision' }).click();
  await expect(facts.getByLabel('Historical fact decision')).toBeVisible();
});
