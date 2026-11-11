import { expect, test, type Page } from '@playwright/test';
import { setTemporaryDensity } from './kit-ui';

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
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await grid.evaluate((element) => {
    element.scrollTop = 249 * 36;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-250/);
  await expect(grid.getByText('Synthetic subject 250')).toBeVisible();

  await setTemporaryDensity(page, 'Comfortable');

  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-250/);
  await expect(grid.getByText('Synthetic subject 250').locator('xpath=ancestor::*[@role="row"]')).toHaveCSS('height', '46px');
});

test('Group preserves a deep focused row across density changes', async ({ page }) => {
  await page.route('**/api/v1/explore/groups', (route) => route.fulfill({ json: {
    rows: Array.from({ length: total }, (_, index) => group(index + 1)),
    total_count: total, cache_revision: 'density-groups', search_provenance: {}
  } }));
  const state = exploreState({
    groupingChain: ['source'], activeRow: 'group:source:250',
    scrollAnchor: { key: 'group:source:250', offset: 5 }
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  const grid = page.getByRole('grid', { name: 'Messages grouped by source' });
  await expect(grid).toHaveAttribute('aria-activedescendant', /source-3A250/);
  await expect(grid.getByText('Source 250')).toBeVisible();

  await setTemporaryDensity(page, 'Comfortable');

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

  await setTemporaryDensity(page, 'Comfortable');

  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-250');
  await expect(grid.getByText('file-250.pdf').locator('xpath=ancestor::*[@role="row"]')).toHaveCSS('height', '46px');
});

test('Files restores a deep focused row when row CSS becomes ready late', async ({ page }) => {
  await page.addInitScript(() => {
    const original = CSSStyleDeclaration.prototype.getPropertyValue;
    let rowGeometryReady = false;
    CSSStyleDeclaration.prototype.getPropertyValue = function (property: string): string {
      if (!rowGeometryReady && property === '--table-short-row-height') return '';
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

test.describe('phone width', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  for (const density of ['compact', 'comfortable'] as const) {
    for (const workspace of ['everything', 'files', 'groups'] as const) {
      test(`${workspace} fits phone rows at ${density} density`, async ({ page }) => {
        await page.addInitScript((density) => {
          sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ density }));
        }, density);
        await page.route('**/api/v1/explore', route => route.fulfill({ json: {
          rows: [{ ...entry(1), preview: 'A long excerpt '.repeat(30), has_attachments: true, attachment_count: 1, attachment_size: 2048 }],
          total_count: 1, cache_revision: 'phone', search_provenance: {}
        } }));
        await page.route('**/api/v1/files/search', route => route.fulfill({ json: {
          files: [{ ...file(7), filename: 'Synthetic document with a very long filename.pdf', size_bytes: 2048, content_state: 'unverified', content_available: null }],
          total_count: 1, cache_revision: 'phone', search_provenance: {}
        } }));
        await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: {
          rows: [{ key: '12', label: 'Synthetic source with a very long descriptive name', count: 12, estimated_bytes: 2048, latest_at: '2026-07-18T12:00:00Z' }, group(1), group(2)],
          total_count: 3, cache_revision: 'phone', search_provenance: {}
        } }));
        const state = exploreState(workspace === 'groups' ? { groupingChain: ['source'] } : { workspace });
        await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
        const grid = page.getByRole('grid', { name: workspace === 'everything' ? 'Messages results' : workspace === 'files' ? 'Files results' : 'Messages grouped by source' });
        const title = grid.getByText(workspace === 'everything' ? 'Synthetic subject 1' : workspace === 'files' ? 'Synthetic document with a very long filename.pdf' : 'Synthetic source with a very long descriptive name', { exact: true });
        await expect(title).toBeVisible();
        expect(await grid.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
        const box = (await title.boundingBox())!;
        const bounds = (await grid.boundingBox())!;
        expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width);
        expect(await grid.locator('[aria-rowindex]').evaluateAll(rows => rows.every(row => row.scrollHeight <= row.clientHeight))).toBe(true);
        if (density === 'comfortable') return;
        if (workspace === 'groups') {
          await expect(grid.getByRole('gridcell', { name: '12 items', exact: true })).toBeVisible();
          await expect(grid.getByRole('row').filter({ hasText: 'Source 1' }).getByRole('gridcell', { name: '1 item', exact: true })).toBeVisible();
          await expect(grid.getByRole('row').filter({ hasText: 'Source 2' }).getByRole('gridcell', { name: '2 items', exact: true })).toBeVisible();
          await expect(grid.getByRole('button', { name: 'Show items in Synthetic source with a very long descriptive name' })).toBeVisible();
        }
        if (workspace === 'files') {
          const row = grid.locator('[aria-rowindex="2"]');
          const [icon, filename, date] = await Promise.all([row.locator('.file-kind').boundingBox(), row.locator('.cell--name').boundingBox(), row.locator('.cell--date').boundingBox()]);
          expect(icon!.x + icon!.width).toBeLessThanOrEqual(filename!.x);
          expect(Math.abs(icon!.y - filename!.y)).toBeLessThan(8);
          expect(date!.y).toBeGreaterThan(filename!.y);
          for (const field of ['filename', 'size', 'date']) {
            await grid.getByRole('button', { name: `Sort by ${field}` }).tap();
            await expect(grid.getByRole('columnheader', { name: `Sort by ${field}` })).toHaveAttribute('aria-sort', /ascending|descending/);
          }
        }
        const header = workspace === 'everything' ? 'Subject / title' : workspace === 'groups' ? 'Items' : 'People';
        const column = grid.getByRole('columnheader', { name: header, exact: true });
        await expect(column).toHaveCount(1);
        await expect(column).toHaveClass(/\bkit-sr-only\b/);
        if (workspace === 'groups') {
          expect(await grid.locator('.group-header').evaluate(el => el.getBoundingClientRect().height)).toBe(0);
        }
      });
    }
  }
});

test.describe('phone interactions', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test('groups keep a fully visible row still when keyboard focus reaches it near the bottom', async ({ page }) => {
    await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: {
      rows: Array.from({ length: total }, (_, i) => group(i + 1)), total_count: total, cache_revision: 'visible-bottom'
    } }));
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreState({ groupingChain: ['source'] })))}`);
    const grid = page.getByRole('grid', { name: 'Messages grouped by source' });
    await expect(grid.locator('[aria-rowindex="2"]')).toBeVisible();
    await grid.evaluate(el => { el.scrollTop = 100 * 58 - el.clientHeight + 15; el.dispatchEvent(new Event('scroll')); });
    await grid.focus();
    for (let i = 0; i < 20 && !(await grid.getAttribute('aria-activedescendant'))?.endsWith('99'); i++) {
      await page.keyboard.press('ArrowDown');
    }
    await expect(grid).toHaveAttribute('aria-activedescendant', /source-3A99$/);
    const target = grid.locator('[aria-rowindex="101"]');
    const [rowBounds, gridBounds] = await Promise.all([target.boundingBox(), grid.boundingBox()]);
    expect(rowBounds!.y).toBeGreaterThanOrEqual(gridBounds!.y);
    const bottomGap = gridBounds!.y + gridBounds!.height - rowBounds!.y - rowBounds!.height;
    expect(bottomGap).toBeGreaterThanOrEqual(0);
    expect(bottomGap).toBeLessThan(30);
    const before = await grid.evaluate(el => el.scrollTop);
    await page.keyboard.press('ArrowDown');
    await expect(grid).toHaveAttribute('aria-activedescendant', /source-3A100$/);
    expect(await grid.evaluate(el => el.scrollTop)).toBe(before);
  });
});

async function stubPageRows(page: Page) {
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: Array.from({ length: total }, (_, i) => entry(i + 1)), total_count: total, cache_revision: 'rotation', search_provenance: {}
  } }));
  await page.route('**/api/v1/files/search', route => route.fulfill({ json: {
    files: Array.from({ length: total }, (_, i) => file(i + 1)), total_count: total, cache_revision: 'rotation', search_provenance: {}
  } }));
  await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: {
    rows: Array.from({ length: total }, (_, i) => group(i + 1)),
    total_count: total, cache_revision: 'rotation', search_provenance: {}
  } }));
}

for (const workspace of ['everything', 'files', 'groups'] as const) {
  test(`${workspace} keeps the same row visible across the compact breakpoint with and without focus`, async ({ page }) => {
    await stubPageRows(page);
    const state = exploreState(workspace === 'groups' ? { groupingChain: ['source'] } : { workspace });
    for (const focused of [false, true]) {
      await page.setViewportSize({ width: 1280, height: 720 });
      await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
      const grid = page.getByRole('grid', { name: workspace === 'everything' ? 'Messages results' : workspace === 'files' ? 'Files results' : 'Messages grouped by source' });
      let row = grid.locator('[aria-rowindex="251"]');
      await expect(grid.locator('[aria-rowindex]').first()).toHaveCSS('height', '36px');
      await grid.evaluate(el => { el.scrollTop = 249 * 36; el.dispatchEvent(new Event('scroll')); });
      await expect(row).toBeInViewport();
      if (focused) {
        if (workspace === 'files') await row.dispatchEvent('pointerdown');
        else await grid.focus();
      }
      await page.setViewportSize({ width: 390, height: 844 });
      const phoneHeight = workspace === 'everything' ? 72 : 58;
      await expect(row).toHaveCSS('height', `${phoneHeight}px`);
      await expect(row).toBeInViewport();
      if (focused && workspace !== 'files') {
        await grid.focus();
        for (let i = 0; i < 3; i++) await page.keyboard.press('ArrowDown');
        row = grid.locator('[aria-rowindex="254"]');
      }
      await page.setViewportSize({ width: 1280, height: 720 });
      await expect(row).toHaveCSS('height', '36px');
      await expect(row).toBeInViewport();
      const header = workspace === 'everything' ? 'Subject / title' : workspace === 'groups' ? 'Items' : 'People';
      await expect(grid.getByRole('columnheader', { name: header, exact: true })).not.toHaveClass(/\bkit-sr-only\b/);
      if (focused && workspace !== 'files') {
        const rebased = await grid.evaluate(el => el.scrollTop);
        await page.reload();
        await expect.poll(() => grid.evaluate(el => el.scrollTop)).toBe(rebased);
        await expect(row).toBeInViewport();
      }
    }
  });
}

for (const workspace of ['everything', 'files'] as const) {
  test(`${workspace} keeps a focused row visible when rotating to landscape`, async ({ page }) => {
    await stubPageRows(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreState({ workspace, activeRow: workspace === 'files' ? 'file:250' : 'message:250' })))}`);
    const grid = page.getByRole('grid', { name: workspace === 'everything' ? 'Messages results' : 'Files results' });
    const height = workspace === 'everything' ? 72 : 58;
    await expect(grid.locator('[aria-rowindex]').first()).toHaveCSS('height', `${height}px`);
    await grid.evaluate((el, height) => {
      el.scrollTop = 249 * height - (el.clientHeight - height - 30);
      el.dispatchEvent(new Event('scroll'));
    }, height);
    const row = grid.locator('[aria-rowindex="251"]');
    await expect(row).toBeInViewport();
    await grid.focus();
    await page.setViewportSize({ width: 844, height: 390 });
    await expect(row).toBeInViewport();
  });
}

function exploreState(overrides: Record<string, unknown>) {
  return {
    schemaVersion: 2, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
    groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' }, fileFilenameQuery: '', fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
    activeRow: null, selectedRow: null, inspectorPinned: false,
    conversationAnchor: null, scrollAnchor: null, ...overrides
  };
}

function entry(index: number) {
  return {
    key: `message:${index}`, kind: 'message', message_type: 'email', conversation_type: 'email',
    title: `Synthetic subject ${index}`, preview: `Synthetic excerpt ${index}`,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_identifier: 'archive@example.com',
    source_type: 'synthetic', participant_ids: [1],
    attachment_count: 0, attachment_size: 0, has_attachments: false, deleted_from_source: false,
    message_count: 1, match: {}
  };
}

function group(index: number) {
  return {
    key: String(index), label: `Source ${index}`, count: index,
    estimated_bytes: 10, latest_at: '2026-07-18T12:00:00Z'
  };
}

function file(index: number) {
  return {
    id: index, key: `file:${index}`, entry_key: `message:${index}`, message_id: index,
    conversation_id: index, occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
    source_type: 'synthetic', source_identifier: 'archive@example.com',
    containing_title: `Containing item ${index}`, filename: `file-${index}.pdf`,
    mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: index,
    content_state: 'unverified', content_available: null
  };
}
