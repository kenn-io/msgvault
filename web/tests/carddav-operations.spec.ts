import { expect, test, type Page } from '@playwright/test';

import {
  assertCardDAVForbiddenMarkersAbsent,
  CARD_DAV_FORBIDDEN_MARKERS,
  installCardDAV
} from './e2e/fixtures/carddav';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

// Browser boundary: Playwright proves the built Svelte application's request
// construction, state transitions, keyboard/focus behavior, responsive UI,
// and accessibility through intercepted HTTP transport. It is not live
// CardDAV-server or live Go-backend integration. Task 4 generated-client/API
// tests and the controller request tests own exact wire-contract proof.

test('fixture never records credentials from an unmatched CardDAV account route', async ({ page }) => {
  const fixture = await installCardDAV(page, { configured: true });
  await page.goto('/');

  const status = await page.evaluate(async (password) => {
    const response = await fetch('/api/v1/carddav/account/unmodeled', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        base_url: 'https://carddav.example.test/',
        username: 'synthetic-user',
        password,
        enabled: true,
        schedule: '0 2 * * *',
        unexpected: 'not recorded'
      })
    });
    return response.status;
  }, fixture.password);

  expect(status).toBe(404);
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/account/unmodeled')).toEqual([{
    method: 'POST',
    path: '/api/v1/carddav/account/unmodeled',
    query: {},
    body: {
      base_url: 'https://carddav.example.test/',
      username: 'synthetic-user',
      enabled: true,
      schedule: '0 2 * * *'
    }
  }]);
  expect(JSON.stringify(fixture.requests)).not.toContain(fixture.password);
});

test('keyboard journey configures CardDAV, reconciles roles, syncs history, and resolves a conflict', async ({ page }) => {
  const fixture = await installCardDAV(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);

  const cardDAVCategory = page.getByRole('button', { name: /^CardDAV account/ });
  await cardDAVCategory.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'CardDAV account' })).toBeVisible();

  await page.getByLabel('Base URL').fill('https://carddav.example.test/');
  await page.getByLabel('Username').fill('synthetic-user');
  await page.getByLabel('Password').fill(fixture.password);
  const enabled = page.getByRole('switch', { name: 'Enabled' });
  await enabled.focus();
  await page.keyboard.press('Space');
  await page.getByLabel('Schedule').fill('0 2 * * *');

  const testConnection = page.getByRole('button', { name: 'Test CardDAV connection' });
  await testConnection.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Connection successful. Found 2 address books.')).toBeVisible();

  const save = page.getByRole('button', { name: 'Save CardDAV account' });
  await save.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('CardDAV account saved. Found 2 address books.')).toBeVisible();

  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/account/test')).toEqual([{
    method: 'POST',
    path: '/api/v1/carddav/account/test',
    query: {},
    body: {
      base_url: 'https://carddav.example.test/',
      username: 'synthetic-user',
      enabled: true,
      schedule: '0 2 * * *'
    },
    passwordMatched: true
  }]);
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/account')).toEqual([{
    method: 'PUT',
    path: '/api/v1/carddav/account',
    query: {},
    body: {
      base_url: 'https://carddav.example.test/',
      username: 'synthetic-user',
      enabled: true,
      schedule: '0 2 * * *'
    },
    passwordMatched: true
  }]);
  expect(JSON.stringify(fixture.requests)).not.toContain(fixture.password);

  await expect(page.getByLabel('CardDAV status summary')).toContainText('Configured');
  await expect(page.getByLabel('CardDAV status summary')).toContainText('Runtime available');
  await expect(page.getByText('Scheduled sync enabled')).toBeVisible();

  const publishHere = page.getByRole('checkbox', { name: 'Publish here for Synthetic publishing' });
  await publishHere.focus();
  await page.keyboard.press('Space');
  await expect(publishHere).toBeChecked();
  await expect(page.getByRole('checkbox', { name: 'Sync contacts for Synthetic publishing' })).toBeChecked();

  const applyRoles = page.getByRole('button', { name: 'Apply roles for Synthetic publishing' });
  await applyRoles.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('alert')).toContainText('Current books were refreshed; review and apply again.');
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/books/6')).toEqual([{
    method: 'PATCH',
    path: '/api/v1/carddav/books/6',
    query: {},
    body: { subscribed: true, lookup_source: false, write_target: true }
  }]);

  await applyRoles.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Address-book roles saved. All books were refreshed.')).toBeVisible();
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/books/6')).toHaveLength(2);
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/books/6')[1]?.body).toEqual({
    subscribed: true,
    lookup_source: false,
    write_target: true
  });

  const sync = page.getByRole('button', { name: 'Sync now' });
  await sync.focus();
  await page.keyboard.press('Enter');
  const activeSync = page.getByLabel('Active CardDAV sync');
  await expect(activeSync).toContainText('1 updated');
  await expect(activeSync).toContainText('2 updated');
  await expect(page.getByLabel('Latest CardDAV sync')).toContainText('Succeeded');
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/sync')).toEqual([{
    method: 'POST', path: '/api/v1/carddav/sync', query: {}, body: { full: false }
  }]);

  const loadMore = page.getByRole('button', { name: 'Load more history' });
  await loadMore.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('table', { name: 'CardDAV sync history' }).getByRole('row')).toHaveCount(4);
  expect(fixture.requests.filter(({ path, query }) => path === '/api/v1/carddav/runs' && query.before_id === '90'))
    .toEqual([{ method: 'GET', path: '/api/v1/carddav/runs', query: { limit: '25', before_id: '90' } }]);

  const conflictRow = page.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
  await conflictRow.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('region', { name: 'CardDAV conflict 41 comparison' })).toContainText('Synthetic Local');
  await expect(page.getByRole('region', { name: 'CardDAV conflict 41 comparison' })).toContainText('Deleted. This side is a deletion tombstone.');
  await expect(page.getByRole('region', { name: 'CardDAV conflict 41 comparison' })).toContainText('Additional name, email, or phone values are not shown.');

  await submitConflictChoice(page, 'Keep local card');
  await expect(page.getByRole('alert')).toContainText('Choose again to resolve it.');
  const refreshedChoice = page.getByRole('button', { name: 'Keep local card' }).first();
  await expect(refreshedChoice).toBeFocused();
  await expect(refreshedChoice).toBeEnabled();
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/conflicts/41/resolve')).toEqual([{
    method: 'POST',
    path: '/api/v1/carddav/conflicts/41/resolve',
    query: {},
    body: { choice: 'keep_local' }
  }]);

  await submitConflictChoice(page, 'Keep local card');
  await expect(conflictRow).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Unresolved conflicts' })).toBeFocused();
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/conflicts/41/resolve')).toHaveLength(2);
  const requestLedger = JSON.stringify(fixture.requests);
  for (const marker of CARD_DAV_FORBIDDEN_MARKERS) expect(requestLedger).not.toContain(marker);
  await assertCardDAVForbiddenMarkersAbsent(page);
});

test('pending conflict resolution blocks dismissal, duplicates, and global shortcuts', async ({ page }) => {
  const fixture = await installCardDAV(page, { configured: true, staleConflictOnce: false });
  const release = fixture.holdNextConflictResolution();
  await openCardDAVSettings(page);

  const conflictRow = page.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
  await conflictRow.focus();
  await page.keyboard.press('Enter');
  const choice = page.getByRole('button', { name: 'Keep remote card' }).first();
  await choice.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: 'Keep remote CardDAV card' });
  const submit = dialog.getByRole('button', { name: 'Keep remote card' });
  await submit.focus();
  await page.keyboard.press('Enter');
  await expect.poll(() => mutationCount(fixture.requests, '/api/v1/carddav/conflicts/41/resolve')).toBe(1);

  await expect(dialog.locator('[aria-busy="true"]')).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeDisabled();
  await expect(dialog.getByRole('button', { name: 'Close CardDAV conflict decision' })).toHaveCount(0);
  await page.keyboard.press('Escape');
  await page.keyboard.press('Shift+/');
  await page.keyboard.press('Enter');
  await page.mouse.click(2, 2);
  await expect(dialog).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Keyboard shortcuts' })).toHaveCount(0);
  expect(mutationCount(fixture.requests, '/api/v1/carddav/conflicts/41/resolve')).toBe(1);

  release();
  await expect(dialog).toHaveCount(0);
  await expect(conflictRow).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Unresolved conflicts' })).toBeFocused();
});

test('keyboard publication ambiguity locks mutation, retries GET only, and repeats a keyed handoff', async ({ page }) => {
  test.slow();
  await installMixedArchive(page);
  const fixture = await installCardDAV(page, { configured: true });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory', directoryPersonID: 42
  }))}`);

  const publish = page.getByRole('switch', { name: 'Publish person to CardDAV' });
  await expect(publish).toBeVisible();
  await expect(page.getByText(/^Attributes:/)).toHaveCount(0);
  await publish.focus();
  await page.keyboard.press('Space');
  const unpublish = page.getByRole('switch', { name: 'Remove person from CardDAV' });
  await expect(unpublish).toBeFocused();
  await expect(page.getByRole('status', { name: 'Operation status' }))
    .toContainText('Published this person to CardDAV in Synthetic contacts.');
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'POST')).toBe(1);

  fixture.failNextPublicationRead();
  await page.keyboard.press('Space');
  const publicationRegion = page.getByRole('region', { name: 'CardDAV publication' });
  await expect(publicationRegion.getByRole('alert'))
    .toContainText('Current CardDAV publication state is unknown.');
  await expect(unpublish).toHaveCount(0);
  await expect(publicationRegion.getByRole('switch')).toHaveCount(0);
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'DELETE')).toBe(1);
  const readsBeforeRetry = mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'GET');

  const retry = page.getByRole('button', { name: 'Retry CardDAV publication state' });
  await retry.focus();
  await page.keyboard.press('Enter');
  const handoff = page.getByRole('button', { name: 'Review CardDAV conflict 42' });
  await expect(handoff).toBeVisible();
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'GET')).toBe(readsBeforeRetry + 1);
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'DELETE')).toBe(1);

  await handoff.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Conflict comparison' })).toBeFocused();
  await expect(page.getByText('Resolved by keeping the local card.')).toBeVisible();
  expect(mutationCount(fixture.requests, '/api/v1/carddav/conflicts/42', 'GET')).toBe(1);
  await expect(page.getByRole('dialog')).toHaveCount(0);

  await page.goBack();
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  const repeatedHandoff = page.getByRole('button', { name: 'Review CardDAV conflict 42' });
  await repeatedHandoff.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Conflict comparison' })).toBeFocused();
  expect(mutationCount(fixture.requests, '/api/v1/carddav/conflicts/42', 'GET')).toBe(2);
  await assertCardDAVForbiddenMarkersAbsent(page);
});

async function openCardDAVSettings(page: Page): Promise<void> {
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  const category = page.getByRole('button', { name: /^CardDAV account/ });
  await category.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'CardDAV account' })).toBeVisible();
}

function mutationCount(
  requests: Array<{ method: string; path: string }>,
  path: string,
  method = 'POST'
): number {
  return requests.filter((request) => request.path === path && request.method === method).length;
}

async function submitConflictChoice(page: Page, label: string): Promise<void> {
  const choice = page.getByRole('button', { name: label }).first();
  await choice.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: /Keep (local|remote) CardDAV card/ });
  await expect(dialog).toBeVisible();
  const submit = dialog.getByRole('button', { name: label });
  await submit.focus();
  await page.keyboard.press('Enter');
  await expect(dialog).toHaveCount(0);
}
