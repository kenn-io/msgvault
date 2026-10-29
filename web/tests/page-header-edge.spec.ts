import { expect, test, type Page } from '@playwright/test';

import { selectWorkspace } from './kit-ui';

test.beforeEach(async ({ page }) => {
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/settings', (route) => route.fulfill({ json: {
    settings: [{ key: 'web.theme', value: { string: 'light' } }], pending_restart: false
  } }));
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [{
    id: 1, source_type: 'mbox', identifier: 'import@example.com', display_name: 'Synthetic import',
    last_sync_at: null, updated_at: '2026-07-19T10:00:00Z', active_sync: null, last_successful_sync: null,
    can_sync: false, sync_unavailable_reason: 'source_not_schedulable', scheduled: false, next_sync_at: null,
    latest_sync: null
  }] } }));
});

// The header band's rule and the top of the first block below it, in page pixels.
async function edges(page: Page) {
  return page.locator('header.page-header').evaluate((header) => {
    const band = getComputedStyle(header, '::before');
    const next = header.nextElementSibling;
    return {
      rule: band.borderBottomWidth,
      bandBottom: header.getBoundingClientRect().bottom - parseFloat(band.bottom),
      nextTop: next ? next.getBoundingClientRect().top : null
    };
  });
}

for (const width of [1440, 390]) {
  test(`a bordered table under the page header draws its only edge at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/?workspace=sources');
    await expect(page.getByRole('table', { name: 'Source status' })).toBeVisible();
    const sources = await edges(page);
    expect(sources.rule).toBe('0px');
    expect(sources.bandBottom).toBe(sources.nextTop);

    await selectWorkspace(page, 'Directory');
    await expect(page.getByRole('heading', { level: 1, name: 'Directory' })).toBeVisible();
    expect((await edges(page)).rule).toBe('1px');
  });
}
