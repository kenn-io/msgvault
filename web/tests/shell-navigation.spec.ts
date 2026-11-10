import { visualStatus } from '../src/test/operations';
import { view } from '../src/lib/components/shell/fixtures.test-support';
import { stubOperationStatus } from './kit-ui';
import { expect, test } from '@playwright/test';

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

const saved = view(3, 'Project notes');

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
  await stubOperationStatus(page);
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [] } }));
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [] } }));
});

test.describe('touch navigation', () => {
  test.use({ hasTouch: true, isMobile: true });
  test('touch drawer sizes controls and keeps focus through close', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
    await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [saved, view(4, 'Second view'), view(5, 'Files view', 'files')] } }));
    await page.goto('/?workspace=everything');
    const opener = page.getByRole('button', { name: 'Open navigation' });
    const search = await page.getByRole('searchbox', { name: 'Search messages' }).boundingBox();
    const mode = await page.getByRole('combobox', { name: /Search mode/ }).boundingBox();
    expect(search!.width).toBeGreaterThan(mode!.width);
    expect(mode!.width).toBeLessThan(120);

    await opener.click();
    await expect(page.getByRole('button', { name: 'Messages' })).toBeFocused();
    for (let index = 0; index < 20; index += 1) await page.keyboard.press('Tab');
    await expect(page.getByRole('dialog', { name: 'Navigation' }).locator(':focus')).toHaveCount(1);
    const drawer = page.getByRole('dialog', { name: 'Navigation' });
    await expect(drawer.getByRole('button', { name: 'Project notes', exact: true })).toBeVisible();
    await expect(drawer.getByRole('button', { name: 'Project notes actions' })).toBeVisible();
    for (const button of await drawer.locator('button:visible').all()) {
      expect((await button.boundingBox())!.height, await button.getAttribute('aria-label') ?? await button.innerText()).toBeGreaterThanOrEqual(44);
    }
    await drawer.getByRole('button', { name: 'Close navigation' }).click();
    await expect(drawer).toBeHidden();
    await expect(opener).toBeFocused();

    let attempts = 0;
    await page.route('**/api/v1/saved-views', route => {
      attempts += 1;
      return attempts === 1
        ? route.fulfill({ status: 500, json: { error: 'Unavailable' } })
        : route.fulfill({ json: { saved_views: [saved] } });
    });
    await page.goto('/?workspace=everything');
    await page.getByRole('button', { name: 'Open navigation' }).tap();
    const retry = drawer.getByRole('button', { name: 'Retry' });
    await expect(retry).toBeVisible();
    for (const button of await drawer.locator('button:visible').all()) {
      expect((await button.boundingBox())!.height, await button.getAttribute('aria-label') ?? await button.innerText()).toBeGreaterThanOrEqual(44);
    }
    await retry.tap();
    await expect(drawer.getByRole('button', { name: 'Project notes', exact: true })).toBeVisible();
  });
});

test('mouse navigation retains compact control sizes', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(false);
  await page.goto('/?workspace=everything');
  await page.getByRole('button', { name: 'Open navigation' }).click();
  const drawer = page.getByRole('dialog', { name: 'Navigation' });
  expect((await drawer.getByRole('button', { name: 'Close navigation' }).boundingBox())!.height).toBeLessThan(44);
});

test('grid shortcuts pressed outside the grid still move the active row', async ({ page }) => {
  await page.goto('/?workspace=everything');
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect(grid).not.toBeFocused();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-1$/);

  await page.keyboard.press('j');

  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-2$/);
});

test('desktop saved-view deletion keeps keyboard focus through Undo', async ({ page }) => {
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [saved] } }));
  await page.goto('/?workspace=everything');
  const row = page.getByRole('button', { name: 'Project notes', exact: true });
  await row.focus();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('button', { name: 'Project notes actions' })).toBeFocused();
  await page.keyboard.press('Enter');
  await page.keyboard.press('End');
  await page.keyboard.press('Enter');
  const undo = page.getByRole('button', { name: 'Undo', exact: true });
  await expect(undo).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(row).toBeVisible();
  await expect(undo).toBeHidden();
  await expect(row).toBeFocused();
});

test('renaming a saved view restores keyboard focus after alphabetical reordering', async ({ page }) => {
  const views = ['Alpha', 'Bravo', 'Charlie'].map((name, index) => ({
    id: index + 1, name, revision: 1, schema_version: 2,
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    canonical_state: { filters: [], grouping: [], presentation: 'table', sort: [], columns: [] }
  }));
  await page.route('**/api/v1/saved-views', route => route.fulfill({
    json: { saved_views: [...views].sort((a, b) => a.name.localeCompare(b.name)) }
  }));
  await page.route('**/api/v1/saved-views/1', async route => {
    expect(route.request().method()).toBe('PATCH');
    expect(route.request().postDataJSON()).toEqual({ name: 'Zulu' });
    views[0] = { ...views[0]!, name: 'Zulu', revision: 2 };
    await route.fulfill({ json: views[0] });
  });
  await page.goto('/?workspace=everything');
  const list = page.getByRole('list', { name: 'Messages saved views' });
  await expect(list.locator('.saved-view__open')).toHaveText(['Alpha', 'Bravo', 'Charlie']);
  await list.getByRole('button', { name: 'Alpha', exact: true }).focus();
  await page.keyboard.press('Tab');
  await page.keyboard.press('Enter');
  await page.getByRole('menuitem', { name: 'Rename', exact: true }).click();
  const input = page.getByRole('textbox', { name: 'Rename Alpha' });
  await input.fill('Zulu');
  await input.press('Enter');
  await expect(list.locator('.saved-view__open')).toHaveText(['Bravo', 'Charlie', 'Zulu']);
  await expect(list.getByRole('button', { name: 'Zulu', exact: true })).toBeFocused();
});

test('Settings Overview Details closes back to Overview with a status title', async ({ page }) => {
  await page.route('**/api/v1/operations/status', route => route.fulfill({ json: { lanes: [{
    kind: 'visual_embedding', lane: 'visual_attachments', configured: true,
    related_status: 'getVisualAttachmentStatus', supported_actions: [], history_availability: 'available'
  }] } }));
  await page.route('**/api/v1/multimodal/status', route => route.fulfill({ json: visualStatus }));
  await page.goto('/?workspace=settings');
  const details = page.getByRole('button', { name: 'Details, Visual search' });
  await details.click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toHaveAccessibleName('Visual attachment status');
  await dialog.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(details).toBeVisible();
  await expect(page.getByRole('main', { name: 'Settings' })).toBeVisible();
});
