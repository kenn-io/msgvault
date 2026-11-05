import { expect, test } from '@playwright/test';
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
      test(`${workspace} fits stacked content at ${density} density`, async ({ page }) => {
        await page.addInitScript((density) => {
          sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ density }));
        }, density);
        await page.route('**/api/v1/explore', route => route.fulfill({ json: {
          rows: [{ ...entry(1), preview: 'A long excerpt '.repeat(30) }],
          total_count: 1, cache_revision: 'phone', search_provenance: {}
        } }));
        await page.route('**/api/v1/files/search', route => route.fulfill({ json: {
          files: [{ ...file(7), filename: 'Synthetic document with a very long filename.pdf', size_bytes: 2048, participant_labels: ['Example Person'], content_state: density === 'compact' ? 'local_content' : 'missing_blob' }],
          total_count: 1, cache_revision: 'phone', search_provenance: {}
        } }));
        await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: {
          rows: [{ key: '1', label: 'Synthetic source with a very long descriptive name', count: 12, estimated_bytes: 2048, latest_at: '2026-07-18T12:00:00Z' }],
          total_count: 1, cache_revision: 'phone', search_provenance: {}
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
        if (workspace !== 'groups') await expect(grid.locator('.phone-sender')).toHaveText('Zoe Sender');
        if (workspace === 'groups') {
          await expect(grid.getByRole('gridcell', { name: '12 items', exact: true })).toBeVisible();
          await expect(grid.getByRole('button', { name: 'Drill into Synthetic source with a very long descriptive name' })).toBeVisible();
        }
      });
    }
  }
});

test.describe('phone interactions', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  for (const workspace of ['everything', 'files'] as const) {
    test(`${workspace} shows the roster when the sender is unavailable`, async ({ page }) => {
      await page.route(`**/api/v1/${workspace === 'files' ? 'files/search' : 'explore'}`, route => route.fulfill({ json:
        workspace === 'files'
          ? { files: [{ ...file(1), sender_label: '', participant_labels: ['Alice Recipient', 'Zoe Sender'] }], total_count: 1, cache_revision: 'fallback' }
          : { rows: [{ ...entry(1), sender_label: '' }], total_count: 1, cache_revision: 'fallback' }
      }));
      await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreState({ workspace })))}`);
      const grid = page.getByRole('grid', { name: workspace === 'files' ? 'Files results' : 'Messages results' });
      await expect(grid.locator('.phone-sender')).toHaveText('Alice Recipient, Zoe Sender');
    });
  }

  for (const workspace of ['everything', 'files'] as const) {
    test(`${workspace} keeps a focused row visible when rotating to landscape`, async ({ page }) => {
      await page.route(`**/api/v1/${workspace === 'files' ? 'files/search' : 'explore'}`, route => route.fulfill({ json:
        workspace === 'files'
          ? { files: Array.from({ length: total }, (_, i) => file(i + 1)), total_count: total, cache_revision: 'landscape' }
          : { rows: Array.from({ length: total }, (_, i) => entry(i + 1)), total_count: total, cache_revision: 'landscape' }
      }));
      await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreState({ workspace, activeRow: workspace === 'files' ? 'file:250' : 'message:250' })))}`);
      const grid = page.getByRole('grid', { name: workspace === 'files' ? 'Files results' : 'Messages results' });
      await expect(grid).toHaveAttribute('aria-activedescendant', /250/);
      await grid.evaluate((el, height) => {
        el.scrollTop = 249 * height - (el.clientHeight - height - 30);
        el.dispatchEvent(new Event('scroll'));
      }, workspace === 'files' ? 58 : 72);
      const current = grid.locator('[aria-rowindex="251"]');
      await expect(current).toBeInViewport();
      await page.setViewportSize({ width: 844, height: 390 });
      await expect(current).toBeInViewport();
    });
  }

  test('labels a group with one item in the singular', async ({ page }) => {
    await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: {
      rows: [group(1), group(2)], total_count: 2, cache_revision: 'singular', search_provenance: {}
    } }));
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreState({ groupingChain: ['source'] })))}`);
    const grid = page.getByRole('grid', { name: 'Messages grouped by source' });
    await expect(grid.getByRole('gridcell', { name: '1 item', exact: true })).toBeVisible();
    await expect(grid.getByRole('gridcell', { name: '2 items', exact: true })).toBeVisible();
  });

  for (const workspace of ['everything', 'groups'] as const) {
    test(`${workspace} restores the rebased scroll anchor after rotation and reload`, async ({ page }) => {
      await page.route('**/api/v1/explore', route => route.fulfill({ json: {
        rows: Array.from({ length: total }, (_, i) => entry(i + 1)), total_count: total, cache_revision: 'anchor', search_provenance: {}
      } }));
      await page.route('**/api/v1/explore/groups', route => route.fulfill({ json: {
        rows: Array.from({ length: total }, (_, i) => group(i + 1)), total_count: total, cache_revision: 'anchor', search_provenance: {}
      } }));
      const state = exploreState(workspace === 'groups' ? { groupingChain: ['source'] } : {});
      await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
      const grid = page.getByRole('grid', { name: workspace === 'groups' ? 'Messages grouped by source' : 'Messages results' });
      await expect(grid.locator('[aria-rowindex]').first()).toBeVisible();
      const phoneHeight = workspace === 'groups' ? 58 : 72;
      const key = workspace === 'groups' ? 'group:source:101' : 'message:101';
      await grid.evaluate((el, top) => { el.scrollTop = top; el.dispatchEvent(new Event('scroll')); }, 100 * phoneHeight + 50);
      await expect.poll(() => page.evaluate(() => history.state?.exploreState?.scrollAnchor)).toEqual({ key, offset: 50 });
      await page.setViewportSize({ width: 1280, height: 720 });
      await expect.poll(() => grid.evaluate(el => el.scrollTop)).toBe(3635);
      await page.reload();
      await expect.poll(() => grid.evaluate(el => el.scrollTop)).toBe(3635);
    });
  }
});

for (const workspace of ['everything', 'files', 'groups'] as const) {
  test(`${workspace} keeps the top row after plain scrolling across the compact breakpoint`, async ({ page }) => {
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
    const state = exploreState(workspace === 'groups'
      ? { groupingChain: ['source'] }
      : { workspace });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
    const grid = page.getByRole('grid', { name: workspace === 'everything' ? 'Messages results' : workspace === 'files' ? 'Files results' : 'Messages grouped by source' });
    const title = workspace === 'everything' ? 'Synthetic subject 250' : workspace === 'files' ? 'file-250.pdf' : 'Source 250';
    const row = grid.getByRole('row').filter({ hasText: title });
    await expect(grid.locator('[aria-rowindex]').first()).toHaveCSS('height', '36px');
    await grid.evaluate(el => { el.scrollTop = 249 * 36; el.dispatchEvent(new Event('scroll')); });
    await expect(row).toBeInViewport();
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(row).toHaveCSS('height', workspace === 'everything' ? '72px' : '58px');
    await expect(row).toBeInViewport();
    await page.setViewportSize({ width: 1280, height: 720 });
    await expect(row).toHaveCSS('height', '36px');
    await expect(row).toBeInViewport();
    await setTemporaryDensity(page, 'Comfortable');
    await expect(row).toHaveCSS('height', '46px');
    await expect.poll(() => grid.evaluate(el => el.scrollTop)).toBe(249 * 46);
  });
}

for (const workspace of ['everything', 'files'] as const) {
  test(`${workspace} keeps a focused row near the viewport bottom on narrowing`, async ({ page }) => {
    await page.route(`**/api/v1/${workspace === 'files' ? 'files/search' : 'explore'}`, route => route.fulfill({ json:
      workspace === 'files'
        ? { files: Array.from({ length: total }, (_, i) => file(i + 1)), total_count: total, cache_revision: 'focus' }
        : { rows: Array.from({ length: total }, (_, i) => entry(i + 1)), total_count: total, cache_revision: 'focus' }
    }));
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(exploreState({workspace, activeRow: workspace === 'files' ? 'file:250' : 'message:250'})))}`);
    const grid = page.getByRole('grid', { name: workspace === 'files' ? 'Files results' : 'Messages results' });
    await expect(grid).toHaveAttribute('aria-activedescendant', /250/);
    await grid.evaluate(el => {
      const header = el.querySelector('[role="row"]') as HTMLElement;
      el.scrollTop = 249 * 36 - (el.clientHeight - header.offsetHeight - 36);
      el.dispatchEvent(new Event('scroll'));
    });
    const current = grid.locator('[aria-rowindex="251"]');
    await expect(current).toBeInViewport();
    const offset = await grid.evaluate(el => 249 * 36 - el.scrollTop);
    await page.setViewportSize({ width: 390, height: 844 });
    const height = workspace === 'files' ? 58 : 72;
    await expect(current).toHaveCSS('height', `${height}px`);
    await expect.poll(() => grid.evaluate(el => el.scrollTop)).toBe(249 * height - offset);
    await expect(current).toBeInViewport();
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
    source_type: 'synthetic', sender_label: 'Zoe Sender', participant_labels: ['Alice Recipient', 'Zoe Sender'], participant_ids: [1],
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
    mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: index, sender_label: 'Zoe Sender',
    content_state: 'missing_blob', content_available: false
  };
}
