import { expect, test } from '@playwright/test';
import { mkdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import { captureScreenshot, type ScreenshotOptions } from './docs-fixture-screenshot';
import { selectWorkspace, setKitTheme, setTemporaryDensity } from './kit-ui';

const outputDir = process.env.MSGVAULT_DOCS_SCREENSHOT_OUTPUT ?? '';
const platform = process.env.MSGVAULT_DOCS_SCREENSHOT_PLATFORM ?? 'darwin';

const exploreURL = (workspace: 'everything') => `/?workspace=${workspace}`;

async function waitForOverview(page: import('@playwright/test').Page) {
  const grid = page.getByRole('grid', { name: 'Messages results' });
  await expect(grid).toBeVisible();
  await expect.poll(async () => await grid.locator('[data-row-key]').count()).toBeGreaterThan(1);
  await expect(grid.locator('[data-row-key]').first()).toBeVisible();
  return grid;
}

async function captureEvidence(page: import('@playwright/test').Page, filename: string) {
  // Leave no hover or focus ring from the setup clicks in published images.
  await page.mouse.move(0, page.viewportSize()!.height / 2);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await captureScreenshot(page, path.join(outputDir, filename));
}

test('writes the final stable screenshot buffer', async ({}, testInfo) => {
  const frames = [Buffer.from('transient screenshot'), Buffer.from('stable screenshot'), Buffer.from('stable screenshot')];
  let screenshotCalls = 0;
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
    outputPath
  );

  expect(screenshotCalls).toBe(3);
  expect(screenshotOptions).toEqual([
    { fullPage: false, animations: 'disabled', caret: 'hide' },
    { fullPage: false, animations: 'disabled', caret: 'hide' },
    { fullPage: false, animations: 'disabled', caret: 'hide' }
  ]);
  expect(await readFile(outputPath)).toEqual(Buffer.from('stable screenshot'));
});

test.describe('documentation fixture capture', () => {
  test.skip(!outputDir, 'documentation screenshot output is configured only for manual capture');

  test('real archive provides Messages and People evidence', async ({ page }) => {
    await mkdir(outputDir, { recursive: true });
    for (const theme of ['dark', 'light'] as const) {
      for (const density of ['comfortable', 'compact'] as const) {
        await page.goto(exploreURL('everything'));
        await setKitTheme(page, theme);
        await setTemporaryDensity(page, density === 'compact' ? 'Compact' : 'Comfortable');
        await expect(page.locator('html')).toHaveAttribute('data-density', density);
        const grid = await waitForOverview(page);
        const firstRow = grid.locator('[data-row-key]').first();
        await expect(firstRow).toContainText(/\S/);
        await captureEvidence(page, `analytical-${theme}-${density}-${platform}.png`);
      }
    }

    // The relationships-* filenames predate the People workspace. They stay
    // because published docs builds hydrate these exact asset names.
    if (platform === 'darwin') {
      for (const [theme, density, filename] of [
        ['dark', 'comfortable', 'relationships-dark-comfortable-darwin.png'],
        ['light', 'compact', 'relationships-light-compact-darwin.png']
      ] as const) {
        await page.goto(exploreURL('everything'));
        await setKitTheme(page, theme);
        await setTemporaryDensity(page, density === 'compact' ? 'Compact' : 'Comfortable');
        await waitForOverview(page);
        await selectWorkspace(page, 'People');
        await expect(page.getByRole('main', { name: 'People' })).toBeVisible();
        const list = page.getByRole('grid', { name: 'People results' });
        await expect.poll(async () => await list.getByRole('row').count()).toBeGreaterThan(1);
        const selectedRow = list.getByRole('row').first();
        await expect(selectedRow).toContainText(/\S/);
        await selectedRow.click();
        const person = page.getByRole('group', { name: 'Person page' });
        await expect(person.getByRole('heading').first()).toContainText(/\S/);
        // The calendar opens on the person's latest active year.
        const calendar = person.getByRole('region', { name: 'Relationship activity calendar' });
        await expect(calendar.locator('.calendar-graphs')).toBeVisible();
        await expect(calendar.getByText(/^No interactions in/)).toHaveCount(0);
        const messages = person.getByRole('grid', { name: 'Messages results' });
        await expect.poll(async () => await messages.locator('[data-row-key]').count()).toBeGreaterThan(0);
        await expect(messages.locator('[data-row-key]').first()).toContainText(/\S/);
        await captureEvidence(page, filename);
      }
    }
  });
});
