import { expect, test } from '@playwright/test';

const row = {
  key: 'message:1',
  kind: 'message',
  message_type: 'email',
  conversation_type: 'email',
  title: 'Synthetic archive subject',
  preview: 'Synthetic analytical excerpt for visual verification.',
  occurred_at: '2026-07-18T12:00:00Z',
  source_id: 1,
  source_identifier: 'archive@example.com',
  source_type: 'synthetic',
  participant_labels: ['Example Person'],
  participant_ids: [1],
  attachment_count: 1,
  attachment_size: 2048,
  has_attachments: true,
  deleted_from_source: false,
  message_count: 1,
  match: {}
};

test.beforeEach(async ({ page }) => {
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/settings', (route) => route.fulfill({
    headers: { ETag: '"synthetic-settings"' },
    json: {
      settings: [
        { key: 'web.theme', group: 'browser', kind: 'string', value: { string: 'system' }, options: ['system', 'light', 'dark'], restart_required: true },
        { key: 'web.density', group: 'browser', kind: 'string', value: { string: 'compact' }, options: ['compact', 'comfortable'], restart_required: true }
      ],
      pending_restart: true
    }
  }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({
    json: { rows: [row], total_count: 1, cache_revision: 'cache-theme', search_provenance: {} }
  }));
  await page.route('**/api/v1/explore/groups', (route) => route.fulfill({
    json: {
      rows: [{ key: '1', label: 'Synthetic source', count: 1, estimated_bytes: 2048, latest_at: '2026-07-18T12:00:00Z' }],
      total_count: 1, cache_revision: 'cache-theme', search_provenance: {}
    }
  }));
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({
    json: { saved_views: [] }
  }));
  await page.route('**/api/v1/files/search', (route) => route.fulfill({
    json: {
      files: [{
        id: 1, key: 'file:1', entry_key: 'message:1', message_id: 1, conversation_id: 1,
        occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
        source_identifier: 'archive@example.com', containing_title: 'Synthetic archive subject',
        filename: 'synthetic.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      }],
      total_count: 1, cache_revision: 'cache-theme', search_provenance: {}
    }
  }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await expect(page.getByText('Synthetic archive subject')).toBeVisible();
});

test('one registry drives selection, searchable help, palette, and editable suspension', async ({ page }) => {
  const grid = page.getByRole('grid', { name: 'Everything results' });
  const renderedRow = page.locator('[data-row-key="message:1"]');
  await grid.focus();
  await expect(grid).toHaveCSS('outline-style', 'solid');
  await page.keyboard.press('Shift+A');
  await expect(renderedRow).toHaveAttribute('aria-selected', 'true');
  await expect(renderedRow.getByText('✓')).toBeVisible();
  await page.keyboard.press('x');
  await expect(renderedRow).toHaveAttribute('aria-selected', 'false');

  await page.keyboard.press('Shift+/');
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  await expect(help).toBeVisible();
  await expect(help.getByText('Select all visible rows')).toBeVisible();
  await expect(help.getByText('Clear selection')).toBeVisible();
  await help.getByRole('searchbox', { name: 'Search keyboard shortcuts' }).fill('deletion');
  await expect(help.getByText('Review selected messages for deletion')).toBeVisible();
  await expect(help.getByText('Open filters')).toHaveCount(0);
  await help.getByRole('button', { name: 'Close' }).click();

  await grid.focus();
  await page.keyboard.press('Shift+A');
  await expect(renderedRow).toHaveAttribute('aria-selected', 'true');
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
  const palette = page.getByRole('dialog', { name: 'Everything commands' });
  await expect(palette).toBeVisible();
  await palette.getByRole('combobox').fill('clear selection');
  const clearSelection = palette.getByRole('option', { name: /Clear selection/ });
  await expect(clearSelection).toBeVisible();
  await expect(clearSelection).toHaveAttribute('aria-selected', 'true');
  await page.keyboard.press('Enter');
  await expect(renderedRow).toHaveAttribute('aria-selected', 'false');
  await expect(grid).toBeFocused();

  const search = page.getByRole('searchbox', { name: 'Search everything' });
  await search.focus();
  await page.keyboard.press('Shift+A');
  await expect(renderedRow).toHaveAttribute('aria-selected', 'false');
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
  await expect(palette).toHaveCount(0);
  await page.keyboard.press('Tab');
  await expect(search).not.toBeFocused();

  await page.emulateMedia({ reducedMotion: 'reduce' });
  const reduced = await page.evaluate(() => {
    const probe = document.createElement('div');
    probe.style.transitionDuration = '10s';
    probe.style.animationDuration = '10s';
    document.body.append(probe);
    const style = getComputedStyle(probe);
    const result = { transition: style.transitionDuration, animation: style.animationDuration };
    probe.remove();
    return result;
  });
  expect(Number.parseFloat(reduced.transition)).toBeLessThanOrEqual(0.00001);
  expect(Number.parseFloat(reduced.animation)).toBeLessThanOrEqual(0.00001);
});

test('keyboard palette grouping focuses the replacement grid', async ({ page }) => {
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await grid.focus();
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
  const palette = page.getByRole('dialog', { name: 'Everything commands' });
  await palette.getByRole('combobox').fill('group by source');
  const command = palette.getByRole('option', { name: 'Group by Source' });
  await expect(command).toHaveAttribute('aria-selected', 'true');

  await page.keyboard.press('Enter');

  const grouped = page.getByRole('grid', { name: 'Everything grouped by source' });
  await expect(palette).toHaveCount(0);
  await expect(grouped).toBeVisible();
  await expect(grouped).toBeFocused();
});

for (const theme of ['light', 'dark'] as const) {
  test(`${theme} rendered Everything and Settings pairs meet contrast`, async ({ page }) => {
    await page.getByLabel('Temporary theme').selectOption(theme);
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
    const requiredRoles = await page.locator('html').evaluate((element) => {
      const style = getComputedStyle(element);
      return ['--bg-canvas', '--bg-subtle', '--border-strong', '--surface-raised', '--text-danger']
        .map((token) => [token, style.getPropertyValue(token).trim()]);
    });
    expect(requiredRoles.filter(([, value]) => !value)).toEqual([]);
    await expectRenderedContrast(page.locator('[data-row-key="message:1"] strong'), 4.5);

    const infoButton = page.getByRole('button', { name: 'Search', exact: true });
    await expect(infoButton).toHaveClass(/kit-button--solid/);
    await expect(infoButton).toHaveClass(/kit-button--info/);
    await expectRenderedContrast(infoButton, 4.5);

    const grid = page.getByRole('grid', { name: 'Everything results' });
    await grid.focus();
    await page.keyboard.press('Shift+A');
    const selectedRow = page.locator('[data-row-key="message:1"]');
    await expect(selectedRow).toHaveAttribute('aria-selected', 'true');
    await expectRenderedShadow(selectedRow, 3);

    await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
    const palette = page.getByRole('dialog', { name: 'Everything commands' });
    const activeOption = palette.getByRole('option', { selected: true }).first();
    await expect(activeOption).toBeVisible();
    await expectRenderedContrast(activeOption, 4.5);
    await expectRenderedShadow(activeOption, 3);
    await page.keyboard.press('Escape');

    await page.getByRole('button', { name: 'Saved Views' }).click();
    const workflowButton = page.getByRole('button', { name: 'Save current view' });
    await expect(workflowButton).toHaveClass(/kit-button--solid/);
    await expect(workflowButton).toHaveClass(/kit-button--workflow/);
    await expectRenderedContrast(workflowButton, 4.5);

    await page.getByRole('button', { name: 'Files' }).click();
    const filesGrid = page.getByRole('grid', { name: 'Files results' });
    await filesGrid.focus();
    await page.keyboard.press('ArrowDown');
    await expectRenderedShadow(filesGrid, 3);

    await page.getByRole('button', { name: 'Settings' }).click();
    const settings = page.getByRole('main', { name: 'Settings' });
    await expect(settings).toBeVisible();
    await expectRenderedContrast(settings.locator('.field-copy span').first(), 4.5);
    await expectRenderedContrast(settings.locator('.pending'), 4.5);
    await expectRenderedBoundary(settings.locator('.field').first(), 'borderTopColor', 3);
    await expectRenderedBoundary(settings.locator('.pending'), 'borderLeftColor', 3);
  });
}

for (const theme of ['light', 'dark'] as const) {
  for (const density of ['compact', 'comfortable'] as const) {
    test(`${theme} ${density} analytical shell snapshot and geometry`, async ({ page }) => {
      await page.getByLabel('Temporary theme').selectOption(theme);
      await page.getByLabel('Temporary density').selectOption(density);
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
      await expect(page.locator('html')).toHaveAttribute('data-density', density);
      await expect(page.locator('[data-row-key="message:1"]')).toHaveCSS(
        'height', density === 'compact' ? '36px' : '46px'
      );
      await page.evaluate(() => document.fonts.ready);
      await expect(page).toHaveScreenshot(`analytical-${theme}-${density}.png`, {
        animations: 'disabled',
        fullPage: true
      });
    });
  }
}

async function expectRenderedContrast(locator: import('@playwright/test').Locator, minimum: number) {
  const ratio = await locator.evaluate(measureRenderedContrast, { property: 'color' });
  expect(ratio).toBeGreaterThanOrEqual(minimum);
}

async function expectRenderedBoundary(
  locator: import('@playwright/test').Locator,
  property: 'borderTopColor' | 'borderLeftColor',
  minimum: number
) {
  const ratio = await locator.evaluate(measureRenderedContrast, { property });
  expect(ratio).toBeGreaterThanOrEqual(minimum);
}

async function expectRenderedShadow(locator: import('@playwright/test').Locator, minimum: number) {
  await expect.poll(
    () => locator.evaluate(measureRenderedContrast, { property: 'boxShadow' })
  ).toBeGreaterThanOrEqual(minimum);
}

function measureRenderedContrast(
  element: Element,
  { property }: { property: 'color' | 'borderTopColor' | 'borderLeftColor' | 'boxShadow' }
): number {
  let current: Element | null = element;
  let background = getComputedStyle(document.documentElement).backgroundColor;
  while (current) {
    const candidate = getComputedStyle(current).backgroundColor;
    const channels = candidate.match(/[\d.]+/g)?.map(Number) ?? [];
    if (channels.length === 3 || (channels[3] ?? 0) >= 0.99) {
      background = candidate;
      break;
    }
    current = current.parentElement;
  }
  const rendered = getComputedStyle(element)[property];
  const foreground = property === 'boxShadow'
    ? rendered.match(/rgba?\([^)]+\)|#[0-9a-f]{3,8}/i)?.[0] ?? rendered
    : rendered;
  const luminance = (color: string) => {
    const channels = (color.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []).map((value) => value / 255);
    const linear = channels.map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
    return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
  };
  const values = [luminance(foreground), luminance(background)].sort((a, b) => b - a);
  return (values[0]! + 0.05) / (values[1]! + 0.05);
}
