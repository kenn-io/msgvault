import { expect, type Page } from '@playwright/test';

export async function openPersonFacts(page: Page) {
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 42 })));
  await expect(page.getByRole('radio', { name: 'Messages', exact: true })).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const facts = page.getByRole('region', { name: 'Facts', exact: true });
  await facts.locator('summary').filter({ has: page.getByRole('heading', { name: 'Facts', exact: true }) }).click();
  return facts;
}
