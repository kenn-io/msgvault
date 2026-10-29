import { documentStatus } from '../src/test/operations';
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

test('narrow navigation menu keeps keyboard focus inside the dialog', async ({ page }) => {
  await page.setViewportSize({ width: 420, height: 860 });
  await page.goto('/?workspace=everything');
  const opener = page.getByRole('button', { name: 'Open navigation' });

  await opener.click();
  await expect(page.getByRole('button', { name: 'Messages' })).toBeFocused();
  for (let index = 0; index < 20; index += 1) await page.keyboard.press('Tab');
  await expect(page.getByRole('dialog', { name: 'Navigation' }).locator(':focus')).toHaveCount(1);
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

test('desktop saved-view deletion keeps keyboard focus on Undo', async ({ page }) => {
  const saved = { id: 3, name: 'Project notes', revision: 1, schema_version: 2,
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    canonical_state: { filters: [], grouping: [], presentation: 'table', sort: [], columns: [] } };
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
});

test('Settings Overview Details closes back to Overview with a status title', async ({ page }) => {
  await page.route('**/api/v1/operations/status', route => route.fulfill({ json: { lanes: [{
    kind: 'document_extraction', lane: 'documents', configured: true,
    related_status: 'getDocumentIndexStatus', supported_actions: [], history_availability: 'available'
  }] } }));
  await page.route('**/api/v1/documents/status/current', route => route.fulfill({ json: documentStatus }));
  await page.goto('/?workspace=settings');
  const details = page.getByRole('button', { name: 'Details, Document extraction' });
  await details.click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toHaveAccessibleName('Document index status');
  await dialog.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  await expect(details).toBeVisible();
  await expect(page.getByRole('main', { name: 'Settings' })).toBeVisible();
});
