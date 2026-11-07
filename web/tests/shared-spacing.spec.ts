import { stubOperationStatus } from './kit-ui';
import { expect, test, type Page } from '@playwright/test';
import { selectWorkspace } from './kit-ui';

import { mockArchiveLayout, row } from './archive-layout';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
import { installOperations } from './e2e/fixtures/operations';

test.beforeEach(async ({ page }) => {
  await mockArchiveLayout(page);
  await page.route('**/api/v1/people**', (route) => route.fulfill({ json: { people: [], total_count: 0 } }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'spacing', identity_revision: 1
  } }));
  await page.goto('/?workspace=relationships');
  await expect(page.getByRole('heading', { name: 'People', exact: true })).toBeVisible();
});

test('page content uses its header gap across workspaces', async ({ page }) => {
  await page.route('**/api/v1/operations/runs**', (route) => route.fulfill({ json: { runs: [], unavailable_kinds: [], membership_revision: 1 } }));
  await stubOperationStatus(page);
  await page.route('**/api/v1/carddav/connections', (route) => route.fulfill({ json: { connections: [] } }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
    id: 1, name: 'Synthetic saved view', schema_version: 2, revision: 1, canonical_state: {},
    created_at: row.occurred_at, updated_at: row.occurred_at
  }] } }));
  await page.route('**/api/v1/deletions', (route) => route.fulfill({ json: { manifests: [{
    id: 'spacing-batch', status: 'pending', message_count: 1, created_at: row.occurred_at, created_by: 'api'
  }] } }));
  await page.route('**/api/v1/deletions/spacing-batch', (route) => route.fulfill({ json: {
    id: 'spacing-batch', status: 'pending', message_count: 1, created_at: row.occurred_at, created_by: 'api', account: 'archive@example.com'
  } }));
  await page.route('**/api/v1/identity/match-candidates**', (route) => route.fulfill({ json: { candidates: [], limit: 100, offset: 0 } }));
  await page.route('**/api/v1/person-relationship-reviews**', (route) => route.fulfill({ json: { reviews: [] } }));
  const gaps: Record<string, number> = {};
  for (const label of ['People', 'Messages', 'Files', 'Sources', 'Deletions', 'Settings']) {
    await selectWorkspace(page, label);
    if (label === 'People') await expect(page.getByText('No people yet', { exact: true })).toBeVisible();
    if (label === 'Messages') await expect(page.getByRole('grid', { name: 'Messages results' }).getByText(row.title, { exact: true })).toBeVisible();
    if (label === 'Files') await expect(page.getByText('SyntheticArchiveAttachmentWithAnUnbrokenDescriptiveFilename.pdf', { exact: true })).toBeVisible();
    if (label === 'Deletions') {
      await page.getByRole('table', { name: 'Staged deletions' }).getByRole('button', { name: /^Open batch from/ }).click();
      const panel = page.getByRole('dialog', { name: /^Staged / });
      await expect(panel.getByText('archive@example.com', { exact: true })).toBeVisible();
      await panel.getByRole('button', { name: 'Close', exact: true }).click();
    }
    if (label === 'Sources') await expect(page.getByRole('table', { name: 'Sources' }).getByText('Archive', { exact: true })).toBeVisible();
    if (label === 'Settings') await expect(page.locator('.kit-settings')).toBeVisible();
    const header = page.locator('.kit-page-header:visible');
    await expect(header).toHaveCount(1);
    gaps[label] = await header.evaluate((element) => {
      const owner = element.parentElement!;
      let next = element.nextElementSibling;
      if (!next) next = owner.nextElementSibling;
      while (next && (next.getBoundingClientRect().height === 0 || next.classList.contains('kit-sr-only'))) next = next.nextElementSibling;
      return next!.getBoundingClientRect().top - element.getBoundingClientRect().bottom;
    });
  }
  expect(gaps).toEqual(Object.fromEntries(Object.keys(gaps).map((label) => [label, ['People', 'Settings'].includes(label) ? 12 : 20])));
});

test('page menus and dropdowns keep the shared minimum width', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [{
    id: 1, name: 'Archive search', schema_version: 2, revision: 1, canonical_state: {},
    created_at: row.occurred_at, updated_at: row.occurred_at
  }] } }));
  await page.goto('/');
  for (const kind of ['dropdown', 'menu']) {
    if (kind === 'dropdown') await page.getByRole('combobox', { name: 'Show as: Table' }).click();
    else {
      const view = page.locator('.saved-view').filter({ hasText: 'Archive search' });
      await view.hover();
      await view.getByRole('button', { name: 'Archive search actions', exact: true }).click();
    }
    const popup = page.getByRole(kind === 'dropdown' ? 'listbox' : 'menu');
    await expect(popup).toBeVisible();
    const bounds = (await popup.boundingBox())!;
    expect.soft(bounds.width).toBeGreaterThanOrEqual(200);
    await page.keyboard.press('Escape');
  }
});

test('dialog dropdown choices keep the shared option inset', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installOperations(page);
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'everything', operationHistory: true })));
  await page.getByRole('dialog', { name: 'Past runs' }).getByRole('combobox', { name: 'State: All states' }).click();
  const option = page.getByRole('option', { name: 'All states', exact: true });
  await expect(option).toHaveCSS('padding-left', '12px');
  await expect(option).toHaveCSS('padding-right', '12px');
});

test('selection bar uses the shared inset and gap', async ({ page }) => {
  await selectWorkspace(page, 'Messages');
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await grid.focus();
  await page.keyboard.press('Shift+A');
  const bar = page.locator('.selection-bar');
  await expect(bar).toBeVisible();
  const selectionInset = await bar.evaluate((element) => {
    const box = element.getBoundingClientRect();
    const count = element.querySelector('.selection-count')!.getBoundingClientRect();
    const style = getComputedStyle(element);
    return { left: count.left - box.left, top: style.paddingTop, gap: style.columnGap };
  });
  expect(selectionInset).toEqual({ left: 12, top: '8px', gap: '8px' });
});

for (const width of [1280, 420]) {
  test.describe(`saved queries at ${width}px`, () => {
    test.use({ viewport: { width, height: 720 } });

    test(`long saved queries keep row actions visible at ${width}px`, async ({ page }) => {
      const query = 'project planning meeting notes and follow up tasks '.repeat(7).trim();
      await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
        id: 1, name: 'Long archive search', schema_version: 2, revision: 1,
        canonical_state: { query, search_mode: 'full_text' },
        created_at: row.occurred_at, updated_at: row.occurred_at
      }] } }));
      await page.reload();
      const opener = page.getByRole('button', { name: 'Open navigation' });
      if (await opener.isVisible()) await opener.click();
      const view = page.locator('.saved-view').filter({ hasText: 'Long archive search' });
      await expect(view).toBeVisible();
      const open = view.getByRole('button', { name: 'Long archive search', exact: true });
      await expect(open).toBeInViewport({ ratio: 1 });
      await open.click({ trial: true });
      await view.hover();
      await view.getByRole('button', { name: 'Long archive search actions', exact: true }).click();
      for (const name of ['Rename', 'Edit description…', 'Delete']) {
        const action = page.getByRole('menuitem', { name, exact: true });
        await expect(action).toBeInViewport({ ratio: 1 });
        await action.click({ trial: true });
      }
      await page.keyboard.press('Escape');
      if (await opener.isVisible()) await opener.click();
      await open.click();
      const label = page.locator('.context-chips .kit-chip__label').filter({ hasText: query });
      await expect(label).toBeVisible();
      expect(await label.evaluate((element) => element.scrollWidth <= element.clientWidth && element.scrollHeight <= element.clientHeight)).toBe(true);
    });
  });
}

test('a long sidebar saved view list keeps every row at its full height and scrolls', async ({ page }) => {
  const description = 'Synthetic description that wraps across several lines of the saved view row. '.repeat(4).trim();
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: Array.from({ length: 40 }, (_, index) => ({
    id: index + 1, name: `Saved view ${index + 1}`, description, schema_version: 1, revision: 1,
    canonical_state: { filters: [{ field: 'source', operator: 'eq', values: ['1'] }], grouping: ['year'], presentation: index < 20 ? 'table' : 'files' },
    created_at: row.occurred_at, updated_at: row.occurred_at
  })) } }));
  await page.reload();
  const sidebar = page.getByRole('complementary', { name: 'Sidebar', exact: true });
  const views = sidebar.locator('.saved-view');
  await expect(views).toHaveCount(40);
  const rows = await views.evaluateAll((elements) => elements.map((element) => {
    const box = element.getBoundingClientRect();
    return { top: box.top, bottom: box.bottom, overflow: element.scrollHeight - element.clientHeight };
  }));
  for (const [index, current] of rows.entries()) {
    expect(current.overflow, `row ${index + 1} content overflows its row`).toBeLessThanOrEqual(0);
    expect(current.bottom - current.top, `row ${index + 1} is squashed`).toBeGreaterThanOrEqual(28);
    if (index > 0) expect(current.top, `row ${index + 1} overlaps the row above`).toBeGreaterThanOrEqual(rows[index - 1]!.bottom - 1);
  }
  const last = sidebar.getByRole('button', { name: 'Saved view 40', exact: true });
  await last.scrollIntoViewIfNeeded();
  await expect(last).toBeInViewport({ ratio: 1 });
  expect(await sidebar.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
});

test('every sidebar item, footer included, is one height', async ({ page }) => {
  const items = page.getByRole('complementary', { name: 'Sidebar', exact: true }).locator('.sidebar__item');
  const heights = await items.evaluateAll((elements) => elements.map((element) => element.getBoundingClientRect().height));
  expect(heights.length).toBeGreaterThan(3);
  expect(new Set(heights)).toEqual(new Set([36]));
});

for (const width of [1280, 390]) {
  for (const workspace of ['Messages', 'Files']) {
    test(`${workspace} Filter ${workspace === 'Messages' ? 'fields use' : 'filename uses'} the shared scale at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 844 });
      await page.route('**/api/v1/cli/accounts', route => route.fulfill({ json: { accounts: [{
        email: 'archive@example.com', virtual_accounts: [{ key: 'account:1', source_id: 1, account_address: 'archive@example.com', message_count: 1, source_deleted_count: 0 }]
      }] } }));
      await page.route('**/api/v1/sources/1/identities', route => route.fulfill({ json: { identities: [] } }));
      await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({
        workspace: workspace === 'Messages' ? 'everything' : 'files', filters: [{ dimension: 'source', values: ['1'] }]
      })));
      await page.getByRole('button', { name: 'Filter', exact: true }).click();
      const panel = page.getByRole('dialog', { name: 'Filter', exact: true });
      const font = width === 390 ? '16px' : '14px';
      if (workspace === 'Messages') {
        for (const label of ['From', 'To']) {
          await expect(panel.getByLabel(label, { exact: true })).toHaveCSS('height', '32px');
          await expect(panel.getByLabel(label, { exact: true })).toHaveCSS('font-size', font);
        }
        for (const name of ['Account:', 'Identity:']) {
          const picker = panel.getByRole('combobox', { name: new RegExp(`^${name}`) });
          await expect(picker).toHaveCSS('height', '32px');
          await expect(picker).toHaveCSS('font-size', font);
        }
      } else {
        await expect(panel.locator('.kit-text-input')).toHaveCSS('height', '32px');
        await expect(panel.getByRole('searchbox', { name: 'Filter filename' })).toHaveCSS('font-size', font);
      }
    });
  }

  test(`save-view dialog shares header and field insets at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    await page.goto('/?workspace=everything');
    await page.getByRole('button', { name: 'Save view…' }).click();
    const dialog = page.getByRole('dialog', { name: 'Save view' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole('button', { name: 'Close', exact: true })).toHaveCSS('width', '28px');
    await expect(dialog.getByRole('button', { name: 'Close', exact: true })).toHaveCSS('height', '28px');
    const edges = await dialog.evaluate((element) => {
      const headerElement = element.querySelector<HTMLElement>('.kit-modal-header')!;
      const header = headerElement.getBoundingClientRect();
      const body = element.querySelector('.kit-modal-body')!.getBoundingClientRect();
      const heading = element.querySelector('.kit-modal-title')!.getBoundingClientRect();
      const input = element.querySelector('.kit-text-input')!.getBoundingClientRect();
      return { heading: heading.left - header.left - headerElement.clientLeft, field: input.left - body.left,
        fieldHeight: input.height };
    });
    expect(edges).toEqual({ heading: 12, field: 12, fieldHeight: 32 });
    await expect(dialog.getByRole('textbox', { name: 'Name', exact: true })).toHaveCSS('font-size', width === 390 ? '16px' : '14px');
    await dialog.getByRole('button', { name: 'Close', exact: true }).click();
  });
}

for (const width of [1280, 390]) {
  test(`shared dialog keeps its 44px header and footer at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 700 });
    await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [{
      id: 1, name: 'Archive search', schema_version: 2, revision: 1, canonical_state: {},
      created_at: row.occurred_at, updated_at: row.occurred_at
    }] } }));
    await page.reload();
    const opener = page.getByRole('button', { name: 'Open navigation' });
    if (await opener.isVisible()) await opener.click();
    const view = page.locator('.saved-view').filter({ hasText: 'Archive search' });
    await view.hover();
    await view.getByRole('button', { name: 'Archive search actions', exact: true }).click();
    await page.getByRole('menuitem', { name: 'Edit description…' }).click();
    const dialog = page.getByRole('dialog', { name: 'Describe Archive search' });
    await expect(dialog.locator('.kit-modal-header')).toHaveCSS('height', '44px');
    await expect(dialog.locator('.kit-modal-footer')).toHaveCSS('height', '44px');
    const topInset = await dialog.locator('.kit-modal-body').evaluate(body => body.firstElementChild!.getBoundingClientRect().top - body.getBoundingClientRect().top);
    expect(topInset).toBe(12);
    const bounds = (await dialog.boundingBox())!;
    expect(bounds.x).toBeGreaterThanOrEqual(16);
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(width - 16);
    await page.setViewportSize({ width, height: 160 });
    const fixed = dialog.locator('.kit-modal-header, .kit-modal-footer');
    const positions = await fixed.evaluateAll(elements => elements.map(element => element.getBoundingClientRect().top));
    const body = dialog.locator('.kit-modal-body');
    expect(await body.evaluate(element => element.scrollHeight > element.clientHeight)).toBe(true);
    await body.evaluate(element => { element.scrollTop = element.scrollHeight; });
    expect(await body.evaluate(element => element.scrollTop)).toBeGreaterThan(0);
    expect(await fixed.evaluateAll(elements => elements.map(element => element.getBoundingClientRect().top))).toEqual(positions);
    await dialog.getByRole('button', { name: 'Close', exact: true }).click();
    await expect(dialog).toHaveCount(0);
  });

  test(`shared controls and toolbar groups at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    if (width === 390) await expect(page.getByRole('button', { name: 'Open navigation' })).toBeVisible();
    for (const workspace of ['Messages', 'Files']) {
      await selectWorkspace(page, workspace);
      if (workspace === 'Messages') {
        const search = page.getByRole('search');
        await expect(search.locator('.kit-text-input')).toHaveCSS('height', '28px');
        await expect(search.getByRole('searchbox')).toHaveCSS('font-size', '13px');
      }
      const toolbar = page.getByRole('region', { name: 'Active analytical context' });
      await expect(toolbar).toBeVisible();
      const geometry = await toolbar.evaluate(element => {
        const box = element.getBoundingClientRect();
        const groups = [...element.querySelectorAll('.context-group')];
        return {
          left: groups[0]!.getBoundingClientRect().left - box.left - element.clientLeft,
          heights: [...element.querySelectorAll('button')].map(button => button.getBoundingClientRect().height),
          groupsIntact: groups.every(group => new Set([...group.querySelectorAll('button')].map(button => button.getBoundingClientRect().top)).size <= 1),
          withinViewport: groups.every(group => { const bounds = group.getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= window.innerWidth; })
        };
      });
      expect(geometry.left).toBe(12);
      expect(geometry.heights.length).toBeGreaterThan(0);
      expect(geometry.heights.every(height => height === 28)).toBe(true);
      expect(geometry.groupsIntact).toBe(true);
      expect(geometry.withinViewport).toBe(true);
      if (width === 390) {
        const view = (await toolbar.getByRole('button', { name: 'View', exact: true }).boundingBox())!;
        const count = (await toolbar.locator('.context-count').boundingBox())!;
        expect(count.x - view.x - view.width).toBe(16);
      }
    }
  });
}

test('Sources notice keeps Retry usable on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route('**/api/v1/sources/status', route => route.fulfill({ status: 500, json: { message: 'Synthetic upstream unavailable' } }));
  await page.route('**/api/v1/carddav/connections', route => route.fulfill({ json: { connections: [] } }));
  await page.goto('/?workspace=sources');
  const notice = page.getByRole('alert');
  const retry = notice.getByRole('button', { name: 'Retry', exact: true });
  await expect(retry).toHaveCSS('height', '28px');
  await expect(retry).toBeInViewport({ ratio: 1 });
});

test('loaded message sender, body, and attachment edges align with the reader', async ({ page }) => {
  await page.route('**/api/v1/messages/1/recordings', (route) => route.fulfill({ json: { message_id: 1, recordings: [] } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [{ ...row, message_type: 'meeting_transcript', conversation_id: 11, anchor_message_id: 1 }], total_count: 1, cache_revision: 'spacing', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
    id: 11, anchor_id: 1, has_before: false, has_after: false, total: 1,
    messages: [{ id: 1, conversation_id: 11, subject: row.title, message_type: 'meeting_transcript',
      from: 'Example Person', to: [], sent_at: row.occurred_at, snippet: row.preview,
      body: 'A plain text archive message.', body_html: '',
      attachments: [{ id: 1, filename: 'Meeting-notes.txt', stored: false }] }]
  } }));
  await selectWorkspace(page, 'Messages');
  await page.getByText(row.title, { exact: true }).click();
  const reader = page.getByRole('complementary', { name: `Reading pane: ${row.title}` });
  await expect(reader.getByText('A plain text archive message.')).toBeVisible();
  await expect(reader.getByText('Meeting-notes.txt (not downloaded)')).toBeVisible();
  const inset = await reader.locator('.pane-header').evaluate((element) => {
    const box = element.getBoundingClientRect();
    const heading = element.querySelector('.pane-heading')!.getBoundingClientRect();
    const style = getComputedStyle(element);
    return { left: heading.left - box.left, top: style.paddingTop, gap: style.columnGap };
  });
  expect(inset).toEqual({ left: 12, top: '8px', gap: '16px' });
  const insets = await reader.evaluate((element) => [
    '.card-header', '.card-body', '.attachments'
  ].map((selector) => {
    const section = element.querySelector(selector)!;
    return section.firstElementChild!.getBoundingClientRect().left - section.getBoundingClientRect().left;
  }));
  expect(insets).toEqual([12, 12, 12]);
});

test('chips keep pill corners and removable chips keep visible insets', async ({ page }) => {
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [{
    id: 1, name: 'Archive search', schema_version: 2, revision: 1,
    canonical_state: { query: 'archive', search_mode: 'full_text' },
    created_at: row.occurred_at, updated_at: row.occurred_at
  }] } }));
  await selectWorkspace(page, 'Messages');
  await page.getByRole('searchbox', { name: 'Search messages' }).fill('archive');
  await page.getByRole('combobox', { name: 'Group by', exact: true }).click();
  await page.getByRole('option', { name: 'Source', exact: true }).click();
  const chips = page.locator('.context-chips .chip');
  await expect(chips).toHaveCount(2);
  const geometry = await chips.evaluateAll((elements) => elements.map((element) => {
    const range = document.createRange();
    range.selectNodeContents(element.querySelector('.kit-chip__label')!);
    const box = element.getBoundingClientRect();
    const icon = element.querySelector('svg')!.getBoundingClientRect();
    const border = Number.parseFloat(getComputedStyle(element).borderLeftWidth);
    return { height: box.height, left: range.getBoundingClientRect().left - box.left - border,
      right: box.right - border - icon.right };
  }));
  expect(geometry).toEqual([{ height: 26, left: 8, right: 8 }, { height: 26, left: 8, right: 8 }]);
  for (const chip of await chips.all()) {
    const shape = await chip.evaluate((element) => ({ radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height }));
    expect.soft(shape.radius).toBeGreaterThanOrEqual(shape.height / 2);
  }
  const longQuery = 'archive '.repeat(100).trim();
  await page.getByRole('searchbox', { name: 'Search messages' }).fill(longQuery);
  const queryChip = chips.filter({ hasText: longQuery });
  const wrapping = await queryChip.locator('.kit-chip__label').evaluate((element) => ({
    clipped: element.scrollWidth > element.clientWidth,
    wrapped: element.getBoundingClientRect().height > Number.parseFloat(getComputedStyle(element).lineHeight)
  }));
  expect(wrapping).toEqual({ clipped: false, wrapped: true });
  await expect(queryChip.getByRole('button', { name: 'Remove search', exact: true })).toBeVisible();
  await selectWorkspace(page, 'People');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Profile filters', { exact: true }).click();
  await page.getByRole('textbox', { name: 'Category filter' }).fill('Archive');
  const filter = page.locator('.chips .kit-chip').filter({ hasText: 'Archive' });
  await expect(filter).toBeVisible();
  const directoryGeometry = await filter.evaluate((element) => {
    const range = document.createRange(); range.selectNodeContents(element.querySelector('.kit-chip__label')!);
    const box = element.getBoundingClientRect();
    const icon = element.querySelector('svg')!.getBoundingClientRect();
    const border = Number.parseFloat(getComputedStyle(element).borderLeftWidth);
    return { height: box.height, left: range.getBoundingClientRect().left - box.left - border,
      right: box.right - border - icon.right };
  });
  expect(directoryGeometry).toEqual({ height: 26, left: 8, right: 8 });
  const directoryShape = await filter.evaluate((element) => ({ radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height }));
  expect.soft(directoryShape.radius).toBeGreaterThanOrEqual(directoryShape.height / 2);
  await selectWorkspace(page, 'Sources');
  const status = page.locator('.kit-chip').filter({ hasText: 'Never synced' });
  await expect(status).toBeVisible();
  const statusShape = await status.evaluate((element) => ({ radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius), height: element.getBoundingClientRect().height }));
  expect.soft(statusShape.radius).toBeGreaterThanOrEqual(statusShape.height / 2);
});

test('Settings uses the shared scrollbar and integer field sizes', async ({ page }) => {
  await page.route('**/api/v1/settings', route => route.fulfill({ json: {
    groups: [{ id: 'sources', label: 'Sources' }],
    settings: [{ key: 'slack.media_max_participants', group: 'sources', kind: 'integer', label: 'Participant limit', value: { integer: 25 }, restart_required: true }],
    pending_restart: false
  } }));
  await selectWorkspace(page, 'Settings');
  await page.getByRole('navigation', { name: 'Settings', exact: true }).getByRole('button', { name: 'Sources', exact: true }).click();
  await expect(page.getByRole('spinbutton').locator('..')).toHaveCSS('height', '32px');
  const size = await page.locator('.kit-settings__scroll').evaluate((element) => getComputedStyle(element, '::-webkit-scrollbar').width);
  expect(size).toBe('8px');
});
test.describe('touch reading', () => {
  test.use({ isMobile: true, hasTouch: true, viewport: { width: 620, height: 900 } });

  test('body and recording transcript share the reading size in one card', async ({ page }) => {
    await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
      rows: [{ ...row, message_type: 'beeper', conversation_type: 'direct_chat', conversation_id: 11, anchor_message_id: 1 }],
      total_count: 1, cache_revision: 'reading-size', search_provenance: {}
    } }));
    await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
      id: 11, anchor_id: 1, has_before: false, has_after: false, total: 1,
      messages: [{ id: 1, conversation_id: 11, subject: row.title, message_type: 'beeper',
        from: 'Example Person', to: [], sent_at: row.occurred_at, body: 'A synthetic voice note body.',
        attachments: [{ id: 1, filename: 'Voice-note.ogg', mime_type: 'audio/ogg', size_bytes: 2048, stored: false }] }]
    } }));
    await page.route('**/api/v1/messages/1/recordings', (route) => route.fulfill({ json: {
      message_id: 1, recordings: [{ attachment_id: 1, filename: 'Voice-note.ogg', size_bytes: 2048, state: 'ready',
        transcript: { origin: 'generated', units: [{ text: 'A synthetic voice note transcript.' }] } }]
    } }));
    await selectWorkspace(page, 'Messages');
    await page.getByText(row.title, { exact: true }).click();
    const card = page.locator('.message-card');
    await expect(card.locator('.card-body pre')).toHaveText('A synthetic voice note body.');
    await expect(card.locator('.transcript')).toContainText('A synthetic voice note transcript.');
    await expect(card.locator('.card-body pre')).toHaveCSS('font-size', '14px');
    await expect(card.locator('.transcript')).toHaveCSS('font-size', '14px');
  });
});

test('selected rows keep their fill and accent on stripes and in the reader', async ({ page }) => {
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [1, 2].map(id => ({ ...row, key: `message:${id}`, title: `Example ${id}` })), total_count: 2, cache_revision: 'spacing', search_provenance: {}
  } }));
  await selectWorkspace(page, 'Messages');
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await grid.focus();
  await page.keyboard.press('Space');
  await expect(grid.locator('.data-row--selected')).toHaveCSS('box-shadow', /^rgb\([^)]+\) 3px 0px 0px 0px inset$/);
  await page.keyboard.press('Space');
  await grid.getByRole('checkbox', { name: 'Select all loaded items' }).check();
  const selected = grid.locator('.data-row--selected');
  await expect(selected).toHaveCount(2);
  for (const selectedRow of await selected.all()) {
    await expect(selectedRow).toHaveCSS('box-shadow', /^rgb\([^)]+\) 3px 0px 0px 0px inset$/);
  }
  const colors = () => selected.evaluateAll(rows => rows.map(row => getComputedStyle(row).backgroundColor));
  expect((await colors())[0]).toBe((await colors())[1]);
  await grid.getByText('Example 1', { exact: true }).click();
  await expect(page.getByRole('complementary', { name: /Reading pane/ })).toBeVisible();
  expect((await colors())[0]).toBe((await colors())[1]);
  await grid.focus();
  await page.keyboard.press('Space');
  await page.keyboard.press('Space');
  await expect(grid.locator('.data-row--selected.data-row--inspected')).toHaveCSS('box-shadow', /^rgb\([^)]+\) 3px 0px 0px 0px inset$/);
});


async function mockMeetingSelection(page: Page, {
  meetingCount, selectedCount, totalCount = meetingCount, extraRows = [],
}: { meetingCount: number; selectedCount: number; totalCount?: number; extraRows?: typeof row[] }) {
  const meetings = Array.from({ length: meetingCount }, (_, index) => ({ ...row, key: `message:${index + 1}`, title: `Meeting ${index + 1}`, message_type: 'meeting_transcript' }));
  await page.route('**/api/v1/explore', route => route.fulfill({ json: {
    rows: [...meetings, ...extraRows], total_count: totalCount, cache_revision: 'spacing', search_provenance: {}
  } }));
  await page.route('**/api/v1/explore/preflight', route => route.fulfill({ json: {
    count: selectedCount, deletable_count: 0, estimated_bytes: 128 * 1024 * 1024, cache_revision: 'spacing', search_provenance: {},
    unavailable_actions: [
      { action: 'export', reason: 'browser_export_requires_single_message' },
      { action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' },
      { action: 'open_in_source', reason: 'trusted_source_link_unavailable' }
    ], action_targets: []
  } }));
}

test.describe('all-matching selection', () => {
  // Touch controls leave less room for the selection summary.
  test.use({ hasTouch: true });

  test('filtered all-matching selection shows size and exclusions at 1024px', async ({ page }) => {
    await page.setViewportSize({ width: 1024, height: 900 });
    await mockMeetingSelection(page, { meetingCount: 50, selectedCount: 49 });
    const explore = { filters: [{ dimension: 'message_type', values: ['meeting_transcript'] }], presentation: 'table' };
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(explore))}`);
    const grid = page.getByRole('grid', { name: 'Messages results' });
    await grid.getByRole('checkbox', { name: 'Select all loaded items' }).check();
    await page.getByRole('button', { name: 'Select all 50 matching', exact: true }).click();
    await grid.getByRole('checkbox', { name: 'Unselect Meeting 1', exact: true }).uncheck();
    const bar = page.locator('.selection-bar');
    const count = bar.locator('.selection-count');
    await expect(count).toHaveText(/^All 50 .*except 1 · .+$/);
    await page.evaluate(() => document.fonts.ready);
    const geometry = await count.evaluate(element => ({ content: element.scrollWidth, available: element.clientWidth }));
    expect(geometry.content).toBeLessThanOrEqual(geometry.available);
  });
});

for (const width of [1024, 1440]) {
  test(`selection bar stays on one row and anchors meeting context at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await mockMeetingSelection(page, {
      meetingCount: 500, selectedCount: 500, totalCount: 10035,
      extraRows: [{ ...row, key: 'message:501', title: 'Example email' }],
    });
    await selectWorkspace(page, 'Messages');
    const grid = page.getByRole('grid', { name: 'Messages results' });
    await grid.focus();
    await grid.getByRole('checkbox', { name: 'Select all loaded items' }).check();
    await grid.focus();
    await page.keyboard.press('End');
    await page.keyboard.press('Space');
    const bar = page.locator('.selection-bar');
    await expect(bar.getByRole('button', { name: 'Select all 10,035 matching', exact: true })).toBeVisible();
    const layout = await bar.evaluate(element => ({
      tops: Array.from(element.querySelectorAll('button')).filter(button => button.getBoundingClientRect().height > 0).map(button => button.getBoundingClientRect().top),
      clipped: element.querySelector('.selection-count')!.scrollWidth > element.querySelector('.selection-count')!.clientWidth
    }));
    expect(new Set(layout.tops).size).toBe(1);
    expect(layout.clipped).toBe(false);
    const countText = await bar.locator('.selection-count').evaluate(element => {
      const range = document.createRange();
      range.selectNodeContents(element);
      return range.getBoundingClientRect().right;
    });
    const selectAll = (await bar.getByRole('button', { name: 'Select all 10,035 matching', exact: true }).boundingBox())!;
    expect(selectAll.x - countText).toBeLessThanOrEqual(12);
    const trigger = bar.getByRole('button', { name: 'Meeting context…' });
    for (const remounted of [false, true]) {
      if (remounted) {
        await grid.focus();
        await page.keyboard.press('Space');
        await expect(trigger).toHaveCount(0);
        await page.keyboard.press('Space');
      }
      await trigger.click();
      const panel = page.getByRole('dialog', { name: 'Meeting context', exact: true });
      await expect(panel).toBeVisible();
      const buttonBox = (await trigger.boundingBox())!;
      const panelBox = (await panel.boundingBox())!;
      expect(panelBox.x).toBeGreaterThanOrEqual(0);
      expect(panelBox.y).toBeGreaterThanOrEqual(0);
      expect(panelBox.x + panelBox.width).toBeLessThanOrEqual(width);
      expect(panelBox.y + panelBox.height).toBeLessThanOrEqual(900);
      expect(Math.abs(panelBox.x - buttonBox.x)).toBeLessThanOrEqual(1);
      const gap = panelBox.y < buttonBox.y ? buttonBox.y - panelBox.y - panelBox.height : panelBox.y - buttonBox.y - buttonBox.height;
      expect(gap).toBeGreaterThanOrEqual(0);
      expect(gap).toBeLessThanOrEqual(16);
      await page.keyboard.press('Escape');
    }
  });
}

test.describe('form picker focus', () => {
  for (const picker of ['Time zone', 'Organization']) {
    test(`${picker} keeps keyboard focus visible in a form at 1280px`, async ({ page }) => {
      await page.setViewportSize({ width: 1280, height: 844 });
      if (picker === 'Time zone') {
        await page.route('**/api/v1/settings', route => route.fulfill({ json: {
          groups: [{ id: 'sources', label: 'Sources' }],
          settings: [{ key: 'slack.schedule', group: 'sources', kind: 'string', label: 'Slack schedule', value: { string: '1 2 3 4 5' }, validation: { format: 'cron' }, restart_required: true }],
          pending_restart: false
        } }));
        await page.goto('/?workspace=settings');
        await page.getByRole('navigation', { name: 'Settings', exact: true }).getByRole('button', { name: 'Sources', exact: true }).click();
      } else {
        await installDirectoryReviewArchive(page);
        await page.route('**/api/v1/organizations**', route => route.fulfill({ json: { organizations: [], total: 0, limit: 50, offset: 0 } }));
        await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
        await page.getByRole('radio', { name: 'Details', exact: true }).click();
        await page.getByRole('button', { name: 'Add work', exact: true }).click();
      }
      const trigger = page.getByRole('button', { name: new RegExp(`^${picker}:`) });
      const predecessor = picker === 'Time zone'
        ? page.getByRole('textbox', { name: 'Slack schedule', exact: true })
        : page.getByRole('button', { name: 'New organization', exact: true });
      await predecessor.focus();
      const restingBorder = await trigger.evaluate(element => getComputedStyle(element).borderColor);
      await page.keyboard.press(picker === 'Time zone' ? 'Tab' : 'Shift+Tab');
      await expect(trigger).toBeFocused();
      await expect.poll(() => trigger.evaluate(element => getComputedStyle(element).borderColor)).not.toBe(restingBorder);
      const focusedBorder = await trigger.evaluate(element => getComputedStyle(element).borderColor);
      await trigger.click();
      const input = page.getByRole('combobox', { name: picker, exact: true });
      await expect(input).toHaveCSS('border-color', focusedBorder);
    });
  }
});

for (const width of [1280, 390]) {
  test(`custom schedule keeps its caret and painted text aligned at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    await page.route('**/api/v1/settings', route => route.fulfill({ json: {
      groups: [{ id: 'sources', label: 'Sources' }],
      settings: [{ key: 'slack.schedule', group: 'sources', kind: 'string', label: 'Slack schedule', value: { string: '1 2 3 4 5' }, validation: { format: 'cron' }, restart_required: true }],
      pending_restart: false
    } }));
    await page.goto('/?workspace=settings');
    await page.getByRole('navigation', { name: 'Settings', exact: true }).getByRole('button', { name: 'Sources', exact: true }).click();
    const input = page.getByRole('textbox', { name: 'Slack schedule', exact: true });
    await expect(input).toBeVisible();
    const editor = input.locator('..');
    const geometry = await editor.evaluate(element => {
      const input = element.querySelector<HTMLInputElement>('input')!;
      const mirror = element.querySelector<HTMLElement>('.cron__mirror')!;
      const metrics = (node: Element) => { const style = getComputedStyle(node); return [style.fontFamily, style.fontSize, style.fontWeight, style.lineHeight, style.letterSpacing, style.paddingLeft, style.paddingRight, node.getBoundingClientRect().height]; };
      const text = document.createRange(); text.selectNodeContents(mirror.querySelector('span')!);
      return { input: metrics(input), mirror: metrics(mirror), outerHeight: element.getBoundingClientRect().height,
        inputOrigin: input.getBoundingClientRect().left + parseFloat(getComputedStyle(input).paddingLeft), mirrorOrigin: text.getBoundingClientRect().left };
    });
    expect.soft(geometry.input).toEqual(geometry.mirror);
    expect.soft(geometry.outerHeight).toBe(32);
    expect.soft(geometry.inputOrigin).toBe(geometry.mirrorOrigin);
    await input.focus();
    await page.keyboard.press('Home');
    await page.keyboard.press('Shift+ArrowRight');
    expect(await input.evaluate(element => [element.selectionStart, element.selectionEnd])).toEqual([0, 1]);
    await page.keyboard.insertText('0');
    await expect(input).toHaveValue('0 2 3 4 5');
    await expect(editor.locator('.cron__mirror')).toHaveText('0 2 3 4 5');
    const long = Array.from({ length: 60 }, (_, index) => index).join(',') + ' 2 3 4 5';
    await input.fill(long);
    await page.keyboard.press('End');
    await expect.poll(() => input.evaluate(element => element.scrollLeft)).toBeGreaterThan(0);
    await expect.poll(() => editor.evaluate(element => element.querySelector('input')!.scrollLeft - element.querySelector('.cron__mirror')!.scrollLeft)).toBe(0);
  });
}
