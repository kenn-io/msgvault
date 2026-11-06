import { expect, test } from '@playwright/test';
import { installOperations } from './e2e/fixtures/operations';
import { embeddingStatus, lane, lanes } from '../src/test/operations';

test('Build index confirmation preserves feature labels at 768px', async ({ page }) => {
  await page.setViewportSize({ width: 768, height: 844 });
  await installOperations(page);
  await page.route('**/api/v1/embeddings/local', route => route.fulfill({ json: {
    host: 'archive.example.test', enabled: true, provider: 'configured',
    phase: 'idle', download_bytes: 0, done_bytes: 0, total_bytes: 0
  } }));
  await page.route('**/api/v1/search/coverage', route => route.fulfill({ json: {
    eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1',
    status: 'unavailable', actions: ['build_index']
  } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  await page.getByRole('button', { name: 'Build index, Search by meaning' }).click();
  await expect(page.getByRole('button', { name: 'Confirm full rebuild' })).toBeVisible();
  const rows = page.getByRole('list', { name: 'Features' }).locator('li');
  for (const row of await rows.all()) {
    const geometry = await row.evaluate(element => {
      const range = document.createRange();
      range.selectNodeContents(element.querySelector('.row__name')!);
      const text = range.getBoundingClientRect();
      const chip = element.querySelector('.row__chip')!.getBoundingClientRect();
      return { name: range.toString().trim(), textRight: text.right, chipLeft: chip.left };
    });
    expect(geometry.textRight, geometry.name).toBeLessThanOrEqual(geometry.chipLeft);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(768);
});

for (const width of [1280, 390]) {
  for (const provider of width === 1280 ? ['builtin', 'ollama'] : ['ollama']) {
    test(`index recovery groups status and error for ${provider} at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1020 });
      await installOperations(page);
      await page.route('**/api/v1/operations/status', route => route.fulfill({ json: {
        lanes: lanes(lane('message_embedding'))
      } }));
      await page.route('**/api/v1/embeddings/status**', route => route.fulfill({ json: {
        ...embeddingStatus, current: 4, pending: 6
      } }));
      await page.route('**/api/v1/embeddings/local', route => route.fulfill({ json: {
        host: 'archive.example.test', enabled: true, provider, download_bytes: 0, phase: 'idle',
        done_bytes: 0, total_bytes: 0, error: 'Message index build interrupted.'
      } }));
      await page.route('**/api/v1/search/coverage', route => route.fulfill({ json: {
        eligible_count: 10, embedded_count: 4, percentage: 40, cache_revision: 'cache-1',
        status: 'initializing', actions: ['build_index']
      } }));
      await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
      const row = page.locator('[data-feature="message_embedding"]');
      await expect(row.locator('.row__coverage')).toBeVisible();
      const coverage = await row.locator('.row__coverage').boundingBox();
      const action = await row.getByRole('button', { name: 'Build index, Search by meaning' }).boundingBox();
      expect(coverage).not.toBeNull();
      expect(action).not.toBeNull();
      if (width === 1280) expect(Math.abs(coverage!.y + coverage!.height / 2 - action!.y - action!.height / 2)).toBeLessThanOrEqual(1);
      else expect(coverage!.y + coverage!.height).toBeLessThanOrEqual(action!.y);
      const spacing = await row.evaluate(element => {
        const name = element.querySelector('.kit-search-by-meaning__name')!.getBoundingClientRect();
        const line = element.querySelector('.kit-search-by-meaning__line')?.getBoundingClientRect();
        const coverage = element.querySelector('.row__coverage')!.getBoundingClientRect();
        const error = element.querySelector('.row__error')!.getBoundingClientRect();
        return { firstGap: coverage.y - (line ?? name).bottom, errorGap: error.y - coverage.bottom, rowPadding: parseFloat(getComputedStyle(element).paddingTop), rowGap: parseFloat(getComputedStyle(element).rowGap) };
      });
      expect(spacing.firstGap).toBeGreaterThan(spacing.errorGap);
      expect(spacing.firstGap).toBeLessThanOrEqual(spacing.rowPadding + (provider === 'ollama' ? spacing.rowGap : 0));
      expect(spacing.errorGap).toBeGreaterThanOrEqual(0);
      if (provider === 'ollama') await expect(row.getByRole('button')).toHaveCount(1);
    });
  }
}

test('local search hint follows results and groups its setup error with matching note type', async ({ page }) => {
  await installOperations(page);
  await page.route('**/api/v1/embeddings/local', route => route.request().method() === 'POST'
    ? route.fulfill({ status: 503, json: { error: 'unavailable', message: 'Local search setup is unavailable' } })
    : route.fulfill({ json: { host: 'archive.example.test', enabled: false, provider: '', phase: 'idle', download_bytes: 290000000, done_bytes: 0, total_bytes: 0 } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', query: 'launch', searchMode: 'full_text' }))}`);
  await page.getByRole('button', { name: 'Turn on', exact: true }).click();
  await expect(page.locator('.local-search-hint').getByRole('alert')).toBeVisible();
  const geometry = await page.locator('.local-search-hint').evaluate(element => {
    const hint = element.querySelector('.kit-search-by-meaning-hint')!;
    const error = element.querySelector('[role="alert"]')!;
    const table = document.querySelector('.results-split')!;
    return {
      hintFont: getComputedStyle(hint).fontSize, errorFont: getComputedStyle(error).fontSize,
      hintLineHeight: getComputedStyle(hint).lineHeight, errorLineHeight: getComputedStyle(error).lineHeight,
      relatedGap: error.getBoundingClientRect().y - hint.getBoundingClientRect().bottom,
      tableGap: element.getBoundingClientRect().y - table.getBoundingClientRect().bottom
    };
  });
  expect(geometry.tableGap).toBeGreaterThan(0);
  expect(geometry.hintFont).toBe(geometry.errorFont);
  expect(geometry.hintLineHeight).toBe(geometry.errorLineHeight);
  expect(geometry.relatedGap).toBeLessThanOrEqual(geometry.tableGap / 2);
});

test('Turn on downloads, indexes, and goes live on a fresh install', async ({ page }) => {
  await installOperations(page);
  let phase: 'idle' | 'downloading' | 'indexing' = 'idle';
  let enabled = false;
  const posts: (string | null)[] = [];
  await page.route('**/api/v1/embeddings/local', route => {
    if (route.request().method() === 'POST') {
      posts.push(route.request().postData());
      phase = 'downloading';
    }
    return route.fulfill({ status: route.request().method() === 'POST' ? 202 : 200, json: {
      host: '', enabled, provider: enabled ? 'builtin' : '', phase, running: phase !== 'idle',
      download_bytes: enabled ? 0 : 290000000, done_bytes: phase === 'downloading' ? 116000000 : 0, total_bytes: 290000000
    } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  const row = page.locator('[data-feature="message_embedding"]');
  await expect(row).toContainText('Downloads 290 MB once, shared by all Kenn apps');
  await row.getByRole('button', { name: 'Turn on' }).click();
  await expect(row.getByRole('progressbar', { name: 'Search by meaning setup' })).toHaveAttribute('aria-valuenow', '116000000');
  await expect(row).toContainText('Downloading 116 MB of 290 MB');
  phase = 'indexing'; enabled = true;
  await expect(row).toContainText('Indexing', { timeout: 3000 });
  phase = 'idle';
  await expect(row.locator('.kit-search-by-meaning > .kit-chip')).toHaveText('On', { timeout: 3000 });
  await expect(row.getByRole('progressbar')).toHaveCount(0);
  expect(posts).toEqual([null]);
});
