import { expect, test } from '@playwright/test';

function syntheticRow(index: number) {
  return {
    key: `message:${index}`,
    kind: 'message',
    message_type: 'email',
    conversation_type: 'email',
    title: `Synthetic subject ${index}`,
    preview: `Synthetic excerpt ${index}`,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_identifier: 'archive@example.com',
    source_type: 'synthetic',
    participant_labels: ['Example Person'],
    participant_ids: [1],
    attachment_count: 0,
    attachment_size: 0,
    has_attachments: false,
    deleted_from_source: false,
    message_count: 1,
    match: {}
  };
}

test('50,000 rows keep a bounded keyed DOM and stable grid focus', async ({ page }) => {
  const total = 50_000;
  const pageSize = 500;
  const requests: Array<{ cursor?: string; limit?: number }> = [];

  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', async (route) => {
    const body = route.request().postDataJSON() as { cursor?: string; limit?: number };
    requests.push(body);
    const offset = body.cursor ? Number(body.cursor.slice('page:'.length)) : 0;
    const count = Math.min(pageSize, total - offset);
    const rows = Array.from({ length: count }, (_, pageIndex) => {
      const index = offset + pageIndex + 1;
      return {
        key: `message:${index}`,
        kind: index % 2 === 0 ? 'message' : 'future_item',
        message_type: index % 2 === 0 ? 'email' : 'future_type',
        conversation_type: 'email',
        title: `Synthetic subject ${index}`,
        preview: `Synthetic excerpt ${index}`,
        occurred_at: '2026-07-18T12:00:00Z',
        source_id: 1,
        source_identifier: 'archive@example.com',
        source_type: 'synthetic',
        participant_labels: ['Example Person'],
        participant_ids: [1],
        attachment_count: 0,
        attachment_size: 0,
        has_attachments: false,
        deleted_from_source: false,
        message_count: 1,
        match: {}
      };
    });
    expect(rows.length).toBeLessThanOrEqual(pageSize);
    await new Promise((resolve) => setTimeout(resolve, 5));
    await route.fulfill({
      json: {
        rows,
        total_count: total,
        cache_revision: 'cache-e2e',
        search_provenance: {},
        ...(offset + count < total ? { next_cursor: `page:${offset + count}` } : {})
      }
    });
  });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  expect(
    await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--accent-blue').trim())
  ).toBe('#006b61');
  const mutedContrast = await page.evaluate(() => {
    const style = getComputedStyle(document.documentElement);
    const luminance = (color: string) => {
      const channels = color.match(/[0-9a-f]{2}/gi)!.map((part) => Number.parseInt(part, 16) / 255);
      const linear = channels.map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
      return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
    };
    const ratio = (left: string, right: string) => {
      const values = [luminance(left), luminance(right)].sort((a, b) => b - a);
      return (values[0]! + 0.05) / (values[1]! + 0.05);
    };
    const muted = style.getPropertyValue('--text-muted').trim();
    return ['--bg-primary', '--bg-surface', '--bg-surface-hover', '--bg-inset'].map((token) => ({
      token,
      ratio: ratio(muted, style.getPropertyValue(token).trim())
    }));
  });
  for (const { token, ratio } of mutedContrast) expect(ratio, token).toBeGreaterThanOrEqual(4.5);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid).toBeVisible();
  await expect.poll(async () => grid.locator('[role="row"]').count()).toBeGreaterThan(1);
  expect(await grid.locator('[role="row"]').count()).toBeLessThan(80);
  expect(requests).toHaveLength(1);
  expect(requests[0]?.limit).toBeLessThanOrEqual(pageSize);

  await grid.evaluate((element) => {
    element.scrollTop = (500 - 10) * 36;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(() => requests.length).toBe(2);
  await expect(grid.getByText('Synthetic subject 501')).toBeVisible();
  await grid.focus();
  await page.keyboard.press('PageDown');
  await page.keyboard.press('PageDown');
  await expect.poll(async () => {
    const active = await grid.getAttribute('aria-activedescendant');
    return Number(active?.match(/message-3a-(\d+)/)?.[1] ?? 0);
  }).toBeGreaterThan(500);

  await page.keyboard.press('End');
  await expect(grid).toHaveAttribute('aria-busy', 'true');
  await expect(page.getByText(/^Loading more…/)).toBeVisible();
  await expect(grid).toBeFocused();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-50000/, { timeout: 15_000 });
  await expect(grid.getByText('Synthetic subject 50000')).toBeVisible();
  expect(requests).toHaveLength(100);
  expect(requests.every(({ limit }) => (limit ?? 0) <= pageSize)).toBe(true);
  expect(await grid.locator('[role="row"]').count()).toBeLessThan(80);
  const activeId = await grid.getAttribute('aria-activedescendant');
  expect(await page.locator(`#${activeId}`).count()).toBe(1);
});

test('committed searches survive transient typing across real back and forward traversal', async ({ page }) => {
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', (route) =>
    route.fulfill({
      json: {
        rows: [],
        total_count: 0,
        cache_revision: 'cache-history',
        search_provenance: route.request().postDataJSON().query
          ? { lexical_index_revision: 'fts-history' }
          : {}
      }
    })
  );

  const explore = encodeURIComponent(JSON.stringify({ workspace: 'everything' }));
  await page.goto(`/?feature=preview&explore=${explore}`);
  const search = page.getByRole('searchbox', { name: 'Search everything' });
  await search.fill('alpha');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(search).toHaveValue('alpha');

  await search.fill('beta draft');
  await search.fill('beta');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(search).toHaveValue('beta');

  await page.goBack();
  await expect(search).toHaveValue('alpha');
  expect(new URL(page.url()).searchParams.get('feature')).toBe('preview');
  await page.goBack();
  await expect(search).toHaveValue('');
  await page.goForward();
  await expect(search).toHaveValue('alpha');
  await page.goForward();
  await expect(search).toHaveValue('beta');
});

test('End stops safely when a cursor repeats without progress', async ({ page }) => {
  const requests: Array<{ cursor?: string }> = [];
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', async (route) => {
    const body = route.request().postDataJSON() as { cursor?: string };
    requests.push(body);
    await route.fulfill({
      json: {
        rows: [syntheticRow(body.cursor ? 2 : 1)],
        total_count: 3,
        cache_revision: 'cache-repeat',
        search_provenance: {},
        next_cursor: 'page:2'
      }
    });
  });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid.getByText('Synthetic subject 1')).toBeVisible();
  await grid.focus();
  await page.keyboard.press('End');

  await expect(page.getByRole('alert')).toContainText(/repeated a cursor|no row progress/i);
  expect(requests).toHaveLength(2);
  await expect(grid).toHaveAttribute('aria-busy', 'false');
});

test('deep durable focus and scroll restore through real back and reload with 500-row pages', async ({ page }) => {
  const total = 6_000;
  const requests: Array<{ cursor?: string; limit?: number }> = [];
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'loopback', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', async (route) => {
    const body = route.request().postDataJSON() as { cursor?: string; limit?: number };
    requests.push(body);
    const offset = body.cursor ? Number(body.cursor.slice('page:'.length)) : 0;
    const count = Math.min(500, total - offset);
    await route.fulfill({
      json: {
        rows: Array.from({ length: count }, (_, index) => syntheticRow(offset + index + 1)),
        total_count: total,
        cache_revision: 'cache-deep',
        search_provenance: {},
        ...(offset + count < total ? { next_cursor: `page:${offset + count}` } : {})
      }
    });
  });
  const state = {
    schemaVersion: 1,
    workspace: 'everything',
    query: '',
    searchMode: 'full_text',
    filters: [],
    groupingChain: [],
    presentation: 'table',
    sort: [{ field: 'occurred_at', direction: 'desc' }],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'],
    columnWidths: { title: 340 },
    activeRow: 'message:5500',
    selectedRow: null,
    inspectorPinned: false,
    conversationAnchor: null,
    scrollAnchor: { key: 'message:5490', offset: 7 }
  };
  const url = `/?feature=preview&explore=${encodeURIComponent(JSON.stringify(state))}`;

  await page.goto(url);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-5500/);
  await expect(grid.getByText('Synthetic subject 5500')).toBeVisible();
  expect(requests).toHaveLength(11);
  expect(requests.every(({ limit }) => (limit ?? 0) <= 500)).toBe(true);
  expect(await grid.locator('[role="row"]').count()).toBeLessThan(80);

  await page.getByRole('button', { name: 'Settings' }).click();
  await page.goBack();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-5500/);
  await expect(grid.getByText('Synthetic subject 5500')).toBeVisible();

  await page.reload();
  await expect(grid).toHaveAttribute('aria-activedescendant', /message-3a-5500/);
  await expect(grid.getByText('Synthetic subject 5500')).toBeVisible();
  expect(requests.every(({ limit }) => (limit ?? 0) <= 500)).toBe(true);
  expect(await grid.locator('[role="row"]').count()).toBeLessThan(80);
});
