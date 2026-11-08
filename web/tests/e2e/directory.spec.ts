import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

import { installMixedArchive } from './fixtures/mixed-archive';
import { stubOperationStatus } from '../kit-ui';

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
  expect(JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').directoryLastContactAfter).toBe('2026-01-01');

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
  expect(JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').directoryLastContactAfter).toBeUndefined();
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

test('Directory profile updates menu is accessible on a phone', async ({ page }) => {
  await installMixedArchive(page);
  await stubOperationStatus(page, ['person_sweep']);
  await page.setViewportSize({ width: 390, height: 600 });
  await page.goto(directoryURL(42));
  await page.getByRole('button', { name: 'More actions' }).click();
  const item = page.getByRole('menuitemcheckbox', { name: /Keep this profile updated/ });
  await expect(item).toBeInViewport();
  await expectNoAxeViolations(page, 'Directory profile updates menu');
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menu')).toBeHidden();
});

test('Directory profile updates offer Set up… while people lookups are off', async ({ page }) => {
  await installMixedArchive(page);
  await page.goto(directoryURL(42));
  await page.getByRole('button', { name: 'More actions' }).click();
  await page.getByRole('menuitem', { name: /Keep this profile updated/ }).click();
  await expect(page).toHaveURL(/settingsCategory%22%3A%22enrichment/);
});

test('Directory lets an enrolled person stop updates while people lookups are off', async ({ page }) => {
  await installMixedArchive(page);
  let tracked = true;
  await page.route('**/api/v1/people/42/tracking', route => {
    if (route.request().method() === 'PUT') tracked = (route.request().postDataJSON() as { tracked: boolean }).tracked;
    return route.fulfill({ json: { person_id: 42, tracked, tracked_at: tracked ? '2026-08-01T00:00:00Z' : null } });
  });
  await page.goto(directoryURL(42));
  await page.getByRole('button', { name: 'More actions' }).click();
  const item = page.getByRole('menuitemcheckbox', { name: /Keep this profile updated/ });
  await expect(item).toHaveAttribute('aria-checked', 'true');
  await expect(page.getByRole('menuitem', { name: 'Set up people lookups…' })).toBeVisible();
  await item.click();
  await expect(page.getByRole('menuitem', { name: /Keep this profile updated/ })).toContainText('Set up…');
  await expect(page.getByRole('menuitem', { name: 'Set up people lookups…' })).toHaveCount(0);
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
