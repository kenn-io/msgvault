import { expect, test } from '@playwright/test';

const total = 300;

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
});

test('Everything preserves a deep focused row across density changes', async ({ page }) => {
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: Array.from({ length: total }, (_, index) => entry(index + 1)),
    total_count: total, cache_revision: 'density-everything', search_provenance: {}
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await grid.evaluate((element) => {
    element.scrollTop = 249 * 36;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-250/);
  await expect(grid.getByText('Synthetic subject 250')).toBeVisible();

  await page.getByLabel('Temporary density').selectOption('comfortable');

  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-250/);
  await expect(grid.getByText('Synthetic subject 250').locator('xpath=ancestor::*[@role="row"]')).toHaveCSS('height', '46px');
});

test('Group preserves a deep focused row across density changes', async ({ page }) => {
  await page.route('**/api/v1/explore/groups', (route) => route.fulfill({ json: {
    rows: Array.from({ length: total }, (_, index) => ({
      key: String(index + 1), label: `Source ${index + 1}`, count: index + 1,
      estimated_bytes: 10, latest_at: '2026-07-18T12:00:00Z'
    })),
    total_count: total, cache_revision: 'density-groups', search_provenance: {}
  } }));
  const state = exploreState({
    groupingChain: ['source'], activeRow: 'group:source:250',
    scrollAnchor: { key: 'group:source:250', offset: 5 }
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Everything grouped by source' });
  await expect(grid).toHaveAttribute('aria-activedescendant', /source-3A250/);
  await expect(grid.getByText('Source 250')).toBeVisible();

  await page.getByLabel('Temporary density').selectOption('comfortable');

  await expect(grid).toHaveAttribute('aria-activedescendant', /source-3A250/);
  await expect(grid.getByText('Source 250').locator('xpath=ancestor::*[@role="row"]')).toHaveCSS('height', '46px');
});

test('Files preserves a deep focused row across density changes', async ({ page }) => {
  await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
    files: Array.from({ length: total }, (_, index) => file(index + 1)),
    total_count: total, cache_revision: 'density-files', search_provenance: {}
  } }));
  const state = exploreState({ workspace: 'files', activeRow: 'file:250' });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Files results' });
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-250');
  await expect(grid.getByText('file-250.pdf')).toBeVisible();

  await page.getByLabel('Temporary density').selectOption('comfortable');

  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-250');
  await expect(grid.getByText('file-250.pdf').locator('xpath=ancestor::*[@role="row"]')).toHaveCSS('height', '46px');
});

test('Files restores a deep focused row when row CSS becomes ready late', async ({ page }) => {
  await page.addInitScript(() => {
    const original = CSSStyleDeclaration.prototype.getPropertyValue;
    let rowGeometryReady = false;
    CSSStyleDeclaration.prototype.getPropertyValue = function (property: string): string {
      if (!rowGeometryReady && property === '--row-height') return '';
      return original.call(this, property);
    };
    Object.defineProperty(window, '__releaseRowGeometry', {
      value: () => { rowGeometryReady = true; }
    });
  });
  await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
    files: Array.from({ length: total }, (_, index) => file(index + 1)),
    total_count: total, cache_revision: 'late-density-files', search_provenance: {}
  } }));
  const state = exploreState({ workspace: 'files', activeRow: 'file:250' });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Files results' });
  await expect(grid.getByText('Preparing files layout…')).toBeVisible();
  await expect(grid).not.toHaveAttribute('aria-activedescendant', /.+/);
  const density = await page.locator('html').getAttribute('data-density');

  await page.evaluate(() => {
    (window as typeof window & { __releaseRowGeometry: () => void }).__releaseRowGeometry();
  });

  await expect(page.locator('html')).toHaveAttribute('data-density', density ?? 'compact');
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-250');
  await expect(grid.getByText('file-250.pdf')).toBeVisible();
  await expect(grid.getByText('file-250.pdf').locator('xpath=ancestor::*[@role="row"]')).toHaveCSS('height', '36px');
});

function exploreState(overrides: Record<string, unknown>) {
  return {
    schemaVersion: 2, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
    groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' }, fileFilenameQuery: '', fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
    activeRow: null, selectedRow: null, inspectorPinned: false, inspectorWidth: 380,
    conversationAnchor: null, scrollAnchor: null, ...overrides
  };
}

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

function file(index: number) {
  return {
    id: index, key: `file:${index}`, entry_key: `message:${index}`, message_id: index,
    conversation_id: index, occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
    source_type: 'synthetic', source_identifier: 'archive@example.com',
    containing_title: `Containing item ${index}`, filename: `file-${index}.pdf`,
    mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: index,
    content_state: 'missing_blob', content_available: false
  };
}
