import { expect, test } from '@playwright/test';
import { mkdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import { captureScreenshot, type ScreenshotOptions } from './docs-fixture-screenshot';
import { selectKitOption, setKitTheme } from './kit-ui';

const outputDir = process.env.MSGVAULT_DOCS_SCREENSHOT_OUTPUT ?? '';
const platform = process.env.MSGVAULT_DOCS_SCREENSHOT_PLATFORM ?? 'darwin';
const compareDir = process.env.MSGVAULT_DOCS_SCREENSHOT_COMPARE_DIR;
const comparePublishedAssets = Boolean(compareDir);

const exploreURL = (workspace: 'everything' | 'relationships') =>
  `/?explore=${encodeURIComponent(JSON.stringify({ workspace }))}`;

async function waitForOverview(page: import('@playwright/test').Page) {
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid).toBeVisible();
  await expect.poll(async () => await grid.locator('[data-row-key]').count()).toBeGreaterThan(1);
  await expect(grid.locator('[data-row-key]').first()).toBeVisible();
  return grid;
}

async function captureEvidence(
  page: import('@playwright/test').Page,
  filename: string,
  compareOptions?: { maxDiffPixelRatio?: number }
) {
  await captureScreenshot(
    page,
    path.join(outputDir, filename),
    comparePublishedAssets
      ? async (screenshot) => {
          const options = compareOptions ?? (platform === 'darwin' ? { maxDiffPixelRatio: 0.005 } : { maxDiffPixels: 100 });
          await expect(screenshot).toMatchSnapshot(filename, options);
        }
      : undefined
  );
}

test('writes the exact screenshot buffer supplied to the comparer', async ({}, testInfo) => {
  const frames = [Buffer.from('transient screenshot'), Buffer.from('stable screenshot'), Buffer.from('stable screenshot')];
  let screenshotCalls = 0;
  let comparedScreenshot: Buffer | undefined;
  const screenshotOptions: ScreenshotOptions[] = [];
  const outputPath = path.join(testInfo.outputDir, 'exact-buffer.png');
  await mkdir(testInfo.outputDir, { recursive: true });

  await captureScreenshot(
    {
      screenshot: async (options) => {
        screenshotCalls += 1;
        screenshotOptions.push(options);
        return frames.shift()!;
      }
    },
    outputPath,
    async (buffer) => {
      comparedScreenshot = buffer;
    }
  );

  expect(screenshotCalls).toBe(3);
  expect(screenshotOptions).toEqual([
    { fullPage: false, animations: 'disabled', caret: 'hide' },
    { fullPage: false, animations: 'disabled', caret: 'hide' },
    { fullPage: false, animations: 'disabled', caret: 'hide' }
  ]);
  expect(comparedScreenshot).toEqual(Buffer.from('stable screenshot'));
  expect(await readFile(outputPath)).toEqual(Buffer.from('stable screenshot'));
});

test.describe('documentation fixture capture', () => {
  test.skip(!outputDir, 'documentation screenshot output is configured only for the dedicated docs workflow');

  test('real archive provides analytical and relationship evidence', async ({ page }) => {
    await mkdir(outputDir, { recursive: true });
    for (const theme of ['dark', 'light'] as const) {
      for (const density of ['comfortable', 'compact'] as const) {
        await page.goto(exploreURL('everything'));
        await setKitTheme(page, theme);
        await selectKitOption(page, 'Temporary density', `Density: ${density === 'compact' ? 'Compact' : 'Comfortable'}`);
        await expect(page.locator('html')).toHaveAttribute('data-density', density);
        const grid = await waitForOverview(page);
        const firstRow = grid.locator('[data-row-key]').first();
        await expect(firstRow).toContainText(/\S/);
        await captureEvidence(page, `analytical-${theme}-${density}-${platform}.png`);
      }
    }

    if (platform === 'darwin') {
      for (const [theme, density, filename, maxDiffPixelRatio] of [
        ['dark', 'comfortable', 'relationships-dark-comfortable-darwin.png', 0.025],
        ['light', 'compact', 'relationships-light-compact-darwin.png', 0.05]
      ] as const) {
        await page.goto(exploreURL('relationships'));
        await setKitTheme(page, theme);
        await selectKitOption(page, 'Temporary density', `Density: ${density === 'compact' ? 'Compact' : 'Comfortable'}`);
        const hub = page.getByRole('main', { name: 'Relationships' });
        await expect(hub).toBeVisible();
        await page.getByRole('button', { name: 'All senders' }).click();
        const list = page.getByRole('grid', { name: 'Relationship results' });
        await expect(list).toBeVisible();
        await expect.poll(async () => await list.locator('[data-row-key]').count()).toBeGreaterThan(1);
        const selectedRow = list.locator('[data-row-key]').first();
        await expect(selectedRow).toContainText(/\S/);
        await selectedRow.click();
        await expect(page.getByRole('heading').first()).toBeVisible();
        const timeline = page.getByRole('grid', { name: 'Relationship activity' });
        await expect(timeline).toBeVisible();
        await expect.poll(async () => await timeline.getByRole('row').count()).toBeGreaterThan(0);
        await expect(timeline.locator('[role="row"]').first()).toContainText(/\S/);
        await expect(page.getByLabel('Relationship activity intensity from less to more')).toBeVisible();
        await captureEvidence(page, filename, { maxDiffPixelRatio });
      }
    }
  });
});
