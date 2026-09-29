import { expect, test } from '@playwright/test';

import { selectWorkspace } from './kit-ui';

function entry(index: number) {
  return {
    key: `message:${index}`, kind: 'message', message_type: 'email', conversation_type: 'email',
    title: `Synthetic subject ${index}`, preview: `Synthetic excerpt ${index}`,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_identifier: 'archive@example.com',
    source_type: 'synthetic', participant_labels: ['Example Person'], participant_ids: [1],
    attachment_count: 0, attachment_size: 0, has_attachments: false, deleted_from_source: false,
    message_count: 1, match: {}
  };
}

test.beforeEach(async ({ page }) => {
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/settings', (route) => route.fulfill({ json: {
    settings: [
      { key: 'web.theme', value: { string: 'light' } },
      { key: 'web.density', value: { string: 'compact' } }
    ], pending_restart: false
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [1, 2, 3].map(entry), total_count: 3, cache_revision: 'shell-navigation', search_provenance: {}
  } }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [] } }));
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [] } }));
});

test('sidebar rail keeps names and survives reload', async ({ page }) => {
  await page.goto('/?workspace=everything');
  await page.getByRole('button', { name: 'Collapse sidebar' }).click();
  await expect(
    page.getByRole('navigation', { name: 'Primary' }).getByRole('button', { name: 'Saved views' })
  ).toBeVisible();
  await page.reload();
  await expect(page.getByRole('button', { name: 'Expand sidebar' })).toBeVisible();
});

test('narrow navigation menu traps focus and closes three ways', async ({ page }) => {
  await page.setViewportSize({ width: 420, height: 860 });
  await page.goto('/?workspace=everything');
  const opener = page.getByRole('button', { name: 'Open navigation' });

  await opener.click();
  await expect(page.getByRole('button', { name: 'Everything' })).toBeFocused();
  for (let index = 0; index < 20; index += 1) await page.keyboard.press('Tab');
  await expect(page.getByRole('dialog', { name: 'Navigation' }).locator(':focus')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(opener).toBeFocused();

  await opener.click();
  await page.getByRole('button', { name: 'Close navigation' }).click({ position: { x: 400, y: 400 } });
  await expect(opener).toBeFocused();

  await selectWorkspace(page, 'Sources');
  await expect(page.getByRole('heading', { level: 1, name: 'Sources' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Navigation' })).toHaveCount(0);
});

test('Escape in the narrow menu leaves an open reading pane open', async ({ page }) => {
  await page.setViewportSize({ width: 420, height: 860 });
  await page.goto('/?workspace=everything');
  await page.getByRole('grid', { name: 'Everything results' }).getByRole('row').nth(1).click();
  await expect(page.getByRole('complementary', { name: /^Reading pane/ })).toBeVisible();
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('complementary', { name: /^Reading pane/ })).toBeVisible();
});

test('global search from another workspace opens Everything', async ({ page }) => {
  await page.goto('/?workspace=sources');
  await page.getByRole('searchbox', { name: 'Search everything' }).fill('fixture');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('main', { name: 'Sources' })).toBeVisible();
});

test('grid shortcuts pressed outside the grid still move the active row', async ({ page }) => {
  await page.goto('/?workspace=everything');
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect(grid).not.toBeFocused();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1$/);

  await page.keyboard.press('j');

  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-2$/);
});
