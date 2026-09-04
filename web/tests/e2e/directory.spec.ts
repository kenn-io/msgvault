import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

import { installMixedArchive } from './fixtures/mixed-archive';

function directoryURL(personID?: number): string {
  return `/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory', ...(personID === undefined ? {} : { directoryPersonID: personID })
  }))}`;
}

async function expectNoAxeViolations(page: Page, label: string): Promise<void> {
  const result = await new AxeBuilder({ page }).analyze();
  expect(result.violations, `${label}: ${result.violations.map((violation) => violation.id).join(', ')}`)
    .toEqual([]);
}

test('Directory lists durable people, opens split detail, and scopes Media & Files to the durable person', async ({ page }) => {
  const requests: string[] = [];
  page.on('request', (request) => requests.push(new URL(request.url()).pathname));
  await installMixedArchive(page);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL());

  const directory = page.getByRole('main', { name: 'Directory' });
  await expect(directory).toBeVisible();
  const row = page.getByRole('row', { name: /Archive Person/ });
  await expect(row).toBeVisible();
  await expectNoAxeViolations(page, 'Directory list');

  await row.click();
  await expect(page.getByRole('complementary', { name: 'Person detail' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  const overview = page.getByRole('tab', { name: 'Overview' });
  const media = page.getByRole('tab', { name: 'Media & Files' });
  await overview.focus();
  await page.keyboard.press('End');
  await expect(media).toBeFocused();
  await expect(media).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByRole('tabpanel', { name: 'Media & Files' })).toBeVisible();
  await expect(page.getByRole('grid', { name: 'Files results' }).getByText('durable-person.pdf')).toBeVisible();
  expect(requests).toContain('/api/v1/people/42/files/search');
  expect(requests).not.toContain('/api/v1/participants/42/files/search');
  expect(requests).not.toContain('/api/v1/files/search');
  await expectNoAxeViolations(page, 'Directory split media detail');
});

test('Directory opens selected detail in an accessible narrow drawer', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 640, height: 900 });
  await page.goto(directoryURL());

  const row = page.getByRole('row', { name: /Archive Person/ });
  await row.click();
  const drawer = page.getByRole('dialog', { name: 'Person detail' });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole('tabpanel', { name: 'Overview' })).toBeVisible();
  await expectNoAxeViolations(page, 'Directory narrow drawer');
  await drawer.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(drawer).toBeHidden();
  await expect(row).toBeFocused();
});

test('Directory profile maintenance uses exact safe requests and GET-only ambiguity recovery', async ({ page }) => {
  const archive = await installMixedArchive(page);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL(42));

  const maintenance = page.getByRole('region', { name: 'Profile maintenance' });
  await expect(maintenance).toBeVisible();
  await expect(maintenance).toContainText('Time zone');
  await expectNoAxeViolations(page, 'Directory profile maintenance desktop');
  const forbidden = /forbidden-/i;
  expect(await page.locator('body').innerHTML()).not.toMatch(forbidden);

  const toggle = maintenance.getByRole('switch', { name: 'Track this person for profile maintenance' });
  await toggle.focus();
  await page.keyboard.press('Space');
  await expect(toggle).toBeChecked();
  await expect(toggle).toBeFocused();
  await expect(page.getByRole('status', { name: 'Operation status' })).toContainText('Profile maintenance tracking enabled.');

  archive.failNextTrackingMutation();
  archive.failNextTrackingRead();
  await page.keyboard.press('Space');
  const retry = maintenance.getByRole('button', { name: 'Retry profile maintenance state' });
  await expect(retry).toBeVisible();
  await retry.focus();
  await page.keyboard.press('Enter');
  await expect(toggle).not.toBeChecked();
  await expect(toggle).toBeEnabled();
  await expect(toggle).toBeFocused();

  const reveal = maintenance.getByRole('button', { name: 'Show sensitive eligible fields' });
  await reveal.focus();
  await page.keyboard.press('Enter');
  await expect(maintenance.getByText('Private note')).toBeVisible();
  await expect(maintenance.getByText('Sensitive', { exact: true })).toBeVisible();
  expect(await page.locator('body').innerHTML()).not.toMatch(forbidden);

  expect(archive.trackingRequests).toEqual([
    { method: 'GET', path: '/api/v1/people/42/tracking' },
    { method: 'GET', path: '/api/v1/person-fact-targets', includeSensitive: false },
    { method: 'PUT', path: '/api/v1/people/42/tracking', tracked: true },
    { method: 'PUT', path: '/api/v1/people/42/tracking', tracked: false },
    { method: 'GET', path: '/api/v1/people/42/tracking' },
    { method: 'GET', path: '/api/v1/people/42/tracking' },
    { method: 'GET', path: '/api/v1/person-fact-targets', includeSensitive: true }
  ]);

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(maintenance.getByText('Time zone')).toBeVisible();
  await maintenance.getByRole('button', { name: 'Show sensitive eligible fields' }).focus();
  await page.keyboard.press('Enter');
  await expect(maintenance.getByText('Private note')).toBeVisible();
  const targetCards = maintenance.locator('li');
  const first = await targetCards.nth(0).boundingBox();
  const second = await targetCards.nth(1).boundingBox();
  expect(first).not.toBeNull();
  expect(second).not.toBeNull();
  expect(second!.y).toBeGreaterThanOrEqual(first!.y + first!.height - 1);
  await expectNoAxeViolations(page, 'Directory profile maintenance narrow');
});

test('Directory retains rows after an invalid cursor and reloads page one', async ({ page }) => {
  await installMixedArchive(page);
  await page.unroute('**/api/v1/people/directory*');
  let requests = 0;
  await page.route('**/api/v1/people/directory*', (route) => {
    requests += 1;
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    if (cursor) return route.fulfill({ status: 400, json: {
      error: 'invalid_cursor', message: 'Synthetic Directory changed while paging.'
    } });
    return route.fulfill({ json: requests === 1 ? {
      people: [{
        id: 42, revision: 1, display_name: 'Retained Person', primary_channel: 'email',
        contact_state: 'active', categories: [], organizations: []
      }], next_cursor: 'invalidated'
    } : {
      people: [{
        id: 43, revision: 1, display_name: 'Reloaded Person', primary_channel: 'email',
        contact_state: 'active', categories: [], organizations: []
      }]
    } });
  });
  await page.goto(directoryURL());

  await expect(page.getByText('Retained Person')).toBeVisible();
  await page.getByRole('button', { name: 'Load more people' }).click();
  await expect(page.getByRole('alert')).toContainText('Synthetic Directory changed while paging.');
  await expect(page.getByText('Retained Person')).toBeVisible();
  await page.getByRole('button', { name: 'Reload directory' }).click();
  await expect(page.getByText('Reloaded Person')).toBeVisible();
  await expect(page.getByText('Retained Person')).toBeHidden();
});

test('Relationships supplies its selected participant to Directory promotion and commits the returned person', async ({ page }) => {
  await installMixedArchive(page);
  await page.goto('/');

  const relationshipList = page.getByRole('grid', { name: 'Relationship results' });
  await relationshipList.getByText('Archive Person').click();
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('button', { name: 'Open in Directory' }).click();
  await expect(page.getByRole('main', { name: 'Directory' })).toBeVisible();

  const promotionRequest = page.waitForRequest((request) =>
    new URL(request.url()).pathname === '/api/v1/people' && request.method() === 'POST'
  );
  await page.getByRole('button', { name: 'Promote to person' }).click();
  expect((await promotionRequest).postDataJSON()).toEqual({ participant_id: 12 });
  await expect(page).toHaveURL(/directoryPersonID/);
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
});
