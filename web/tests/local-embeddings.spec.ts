import { expect, test } from '@playwright/test';
import { installOperations } from './e2e/fixtures/operations';

for (const width of [1280, 390]) {
  test(`local search setup and results hint at ${width}px`, async ({ page }, info) => {
    await page.setViewportSize({ width, height: 850 });
    await page.emulateMedia({ colorScheme: 'dark' });
    await installOperations(page);
    await page.route('**/api/v1/settings', route => route.fulfill({ headers: { ETag: '"local-settings"' }, json: {
      groups: [{ id: 'browser', label: 'Appearance', description: '' }],
      settings: [{ key: 'web.theme', group: 'browser', label: 'Theme', kind: 'string', value: { string: 'dark' }, options: ['system', 'light', 'dark'], restart_required: false }], pending_restart: false,
    } }));
    let phase = 'idle';
    let posts = 0;
    await page.route('**/api/v1/embeddings/local', async route => {
      if (route.request().method() === 'POST') { posts++; phase = 'indexing'; }
      await route.fulfill({ status: route.request().method() === 'POST' ? 202 : 200, json: {
        enabled: phase !== 'idle', provider: phase !== 'idle' ? 'builtin' : '', compatible: true,
        ollama_available: false, phase, download_bytes: 290000000, done_bytes: 0, total_bytes: 0,
      } });
    });
    await page.route('**/api/v1/embeddings/status*', route => route.fulfill({ json: { current: 2, eligible: 10, pending: 8, eta_seconds: 60 } }));
    const consoleErrors: string[] = [];
    page.on('pageerror', error => consoleErrors.push(error.message));
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
    const row = page.locator('[data-feature="message_embedding"]');
    await expect(row.getByText('Search by meaning', { exact: true })).toBeVisible();
    await expect(row.getByText('Downloads 290 MB once, shared by all Kenn apps')).toBeVisible();
    await page.screenshot({ path: info.outputPath(`settings-off-${width}.png`), fullPage: true });
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    await row.getByRole('button', { name: 'Turn on', exact: true }).click();
    await expect(row.getByRole('progressbar')).toBeVisible();
    await expect(row.getByText('Indexing 2 of 10 messages')).toBeVisible();
    expect(posts).toBe(1);
    await page.screenshot({ path: info.outputPath(`settings-indexing-${width}.png`), fullPage: true });
    phase = 'idle';
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', query: 'orchard', searchMode: 'full_text' }))}`);
    await expect(page.getByText('Search by meaning is off')).toBeVisible();
    await page.screenshot({ path: info.outputPath(`results-hint-${width}.png`), fullPage: true });
    await page.getByRole('button', { name: 'Turn on', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
    await expect(row.getByRole('progressbar')).toBeVisible();
    expect(posts).toBe(2);
    expect(consoleErrors).toEqual([]);
  });
}
