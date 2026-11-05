import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

import { parseExploreURLState } from '../../src/lib/explore/state.svelte';
import { installMixedArchive } from './fixtures/mixed-archive';

function directoryURL(personID?: number): string {
  return `/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'relationships', relationshipSaved: true, ...(personID === undefined ? {} : { directoryPersonID: personID })
  }))}`;
}

async function expectNoAxeViolations(page: Page, label: string): Promise<void> {
  const result = await new AxeBuilder({ page }).analyze();
  expect(result.violations, `${label}: ${result.violations.map((violation) => violation.id).join(', ')}`)
    .toEqual([]);
}

test('Directory lists durable people, opens People Files, and scopes files to the durable person', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL());

  const directory = page.getByRole('main', { name: 'People' });
  await expect(directory).toBeVisible();
  const row = page.getByRole('row', { name: /Archive Person/ });
  await expect(row).toBeVisible();
  await expectNoAxeViolations(page, 'Directory list');

  await row.click();
  await expect(page.getByRole('group', { name: 'Person page' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  const personTabs = page.getByRole('radiogroup', { name: 'Person tabs' });
  await personTabs.getByRole('radio', { name: 'Files', exact: true }).click();
  await expect(page.getByRole('grid', { name: 'Files results' }).getByText('durable-person.pdf')).toBeVisible();
});

test('Directory keeps a one-sided last contacted date as a chip across reload until it is removed', async ({ page }) => {
  await installMixedArchive(page);
  const afterBoundaries: Array<string | null> = [];
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname === '/api/v1/people/directory') afterBoundaries.push(url.searchParams.get('last_contact_after'));
  });
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL());

  const directory = page.getByRole('main', { name: 'People' });
  await expect(directory.getByRole('row', { name: /Archive Person/ })).toBeVisible();
  await directory.getByRole('button', { name: 'Filter' }).click();
  await page.getByText('Profile filters', { exact: true }).click();
  const panel = directory.getByRole('group', { name: 'Profile filters' });
  const chip = directory.getByRole('button', { name: 'Remove Last contacted after Jan 1, 2026' });
  await expect(chip).toHaveCount(0);

  await panel.getByLabel('Last contacted after', { exact: true }).fill('2026-01-01');
  await expect(chip).toBeVisible();
  await expect(directory.getByRole('button', { name: /^Remove Last contacted before/ })).toHaveCount(0);
  await expect.poll(() => afterBoundaries.at(-1)).toBe('2026-01-01T00:00:00Z');
  expect(parseExploreURLState(new URL(page.url()).search).directoryLastContactAfter).toBe('2026-01-01');

  await page.reload();
  await expect(chip).toBeVisible();
  await expect.poll(() => afterBoundaries.at(-1)).toBe('2026-01-01T00:00:00Z');
  await directory.getByRole('button', { name: 'Filter' }).click();
  await page.getByText('Profile filters', { exact: true }).click();
  await expect(panel.getByLabel('Last contacted after', { exact: true })).toHaveValue('2026-01-01');

  await page.keyboard.press('Escape');
  await chip.click();
  await expect(chip).toHaveCount(0);
  await expect(panel.getByLabel('Last contacted after', { exact: true })).toHaveValue('');
  await expect.poll(() => afterBoundaries.at(-1)).toBeNull();
  expect(parseExploreURLState(new URL(page.url()).search).directoryLastContactAfter).toBe('');
});

test('Person tabs stay on one line at phone width', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 420, height: 860 });
  await page.goto(directoryURL(42));

  const drawer = page.getByRole('group', { name: 'Person page' });
  const tabs = drawer.getByRole('radiogroup', { name: 'Person tabs' }).getByRole('radio');
  await expect(tabs).toHaveCount(3);
  const offsets = await tabs.evaluateAll((elements) => elements.map((element) => (element as HTMLElement).offsetTop));
  expect(new Set(offsets).size).toBe(1);
});

test('People Details scrolls independently at desktop width', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 1280, height: 600 });
  await page.goto(directoryURL(42));
  const pane = page.getByRole('group', { name: 'Person page' });
  await expect(pane.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await pane.getByRole('radio', { name: 'Details', exact: true }).click();
  const metrics = await pane.evaluate((el) => ({
    scroll: el.scrollHeight,
    client: el.clientHeight,
    overflow: getComputedStyle(el).overflowY
  }));
  expect(metrics.scroll).toBeGreaterThan(metrics.client);
  expect(metrics.overflow).toBe('auto');
  await pane.evaluate((el) => el.scrollTo(0, el.scrollHeight));
  expect(await pane.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  await expect(pane.getByRole('heading', { name: 'Last time we talked' })).toBeInViewport();
});

test('Directory opens an accessible narrow People page and returns to its list', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 640, height: 900 });
  await page.goto(directoryURL());

  const row = page.getByRole('row', { name: /Archive Person/ });
  await row.click();
  const drawer = page.getByRole('group', { name: 'Person page' });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole('radio', { name: 'Messages', exact: true })).toBeChecked();
  await expectNoAxeViolations(page, 'Directory narrow drawer');
  await drawer.getByRole('button', { name: 'Back to People', exact: true }).click();
  await expect(drawer).toBeHidden();
  await expect(page.getByRole('grid', { name: 'People results' })).toBeFocused();
});

test('Directory profile maintenance uses exact safe requests and GET-only ambiguity recovery', async ({ page }) => {
  const archive = await installMixedArchive(page);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL(42));
  await page.getByRole('button', { name: 'More actions' }).click();
  await page.getByRole('menuitem', { name: 'Keep this profile updated' }).click();

  const maintenance = page.getByRole('region', { name: 'Profile maintenance' });
  await expect(maintenance).toBeVisible();
  await expect(maintenance).toContainText('Timezone');
  await expectNoAxeViolations(page, 'Directory profile maintenance desktop');
  const forbidden = /forbidden-/i;
  expect(await page.locator('body').innerHTML()).not.toMatch(forbidden);

  const toggle = maintenance.getByRole('switch', { name: 'Update automatically' });
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

  const reveal = maintenance.getByRole('button', { name: 'Show sensitive fields' });
  await expect(reveal).toBeEnabled();
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

  await page.getByRole('dialog', { name: "Keep Archive Person's profile updated" }).getByRole('button', { name: 'Close', exact: true }).click();
  await expect(maintenance).toBeHidden();
  await page.route('**/api/v1/person-fact-targets*', (route) => route.fulfill({ json: {
    targets: ['employment', 'favorite_foods', 'hometown', 'contact_every_n_days', 'timezone', 'languages', 'interests', 'education', 'skills', 'birthday', 'pronouns', 'preferred_name', 'private_note'].map((slug) => ({
      kind: slug === 'employment' ? 'employment' : 'attribute', slug, description: 'Catalog description', value_type: 'text', cardinality: 'multi', sensitive: slug === 'private_note'
    })).filter((target) => !target.sensitive || new URL(route.request().url()).searchParams.get('include_sensitive') === 'true')
  } }));
  await page.setViewportSize({ width: 390, height: 600 });
  const drawer = page.getByRole('group', { name: 'Person page' });
  await drawer.getByRole('button', { name: 'More actions' }).click();
  await page.getByRole('menuitem', { name: 'Keep this profile updated' }).click();
  const drawerMaintenance = page.getByRole('region', { name: 'Profile maintenance' });
  await expect(drawerMaintenance.getByText('Timezone')).toBeVisible();
  const drawerReveal = drawerMaintenance.getByRole('button', { name: 'Show sensitive fields' });
  await expect(drawerReveal).toBeEnabled();
  await drawerReveal.focus();
  await expect(drawerReveal).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(maintenance.getByText('Private note')).toBeVisible();
  const targetCards = maintenance.locator('li');
  const first = await targetCards.nth(0).boundingBox();
  const second = await targetCards.nth(1).boundingBox();
  expect(first).not.toBeNull();
  expect(second).not.toBeNull();
  expect(second!.y).toBeGreaterThanOrEqual(first!.y + first!.height - 1);
  const geometry = await page.getByRole('dialog').evaluate((dialog) => {
    const body = dialog.querySelector('.kit-modal-body')!;
    const rows = [...dialog.querySelectorAll('li')].map((row) => row.getBoundingClientRect());
    return { height: dialog.getBoundingClientRect().height, overflow: body.scrollWidth > body.clientWidth, scrolls: body.scrollHeight > body.clientHeight, lefts: rows.map((row) => row.left), heights: rows.map((row) => row.height) };
  });
  expect(geometry.height).toBeLessThanOrEqual(720);
  expect(geometry.overflow).toBe(false);
  expect(geometry.scrolls).toBe(true);
  expect(new Set(geometry.lefts).size).toBe(1);
  expect(Math.max(...geometry.heights)).toBeLessThanOrEqual(60);
  await expect(page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true })).toBeInViewport();
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
      people: Array.from({ length: 100 }, (_, index) => ({
        id: 42 + index, revision: 1, display_name: index === 0 ? 'Retained Person' : `Saved Person ${index}`, primary_channel: 'email',
        contact_state: 'active', categories: [], organizations: []
      })), next_cursor: 'invalidated'
    } : {
      people: [{
        id: 43, revision: 1, display_name: 'Reloaded Person', primary_channel: 'email',
        contact_state: 'active', categories: [], organizations: []
      }]
    } });
  });
  await page.goto(directoryURL());

  await expect(page.getByText('Retained Person')).toBeVisible();
  const grid = page.getByRole('grid', { name: 'People results' });
  await grid.evaluate(element => element.scrollTo(0, element.scrollHeight));
  await expect(page.getByRole('alert')).toContainText('Synthetic Directory changed while paging.');
  await expect(page.getByText('Retained Person')).toBeVisible();
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(page.getByText('Reloaded Person')).toBeVisible();
  await expect(page.getByText('Retained Person')).toBeHidden();
});
