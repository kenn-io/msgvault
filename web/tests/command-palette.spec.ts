import { expect, test } from '@playwright/test';

import { mockArchiveLayout } from './archive-layout';
import { setKitTheme } from './kit-ui';

test.use({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, colorScheme: 'dark' });

test('palette shows commands, grouping, fallbacks, and persisted recents', async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await mockArchiveLayout(page);
  await page.goto('/');
  await expect(page.getByText('Synthetic archive subject')).toBeVisible();
  await setKitTheme(page, 'dark');
  await page.keyboard.press('Control+k');
  let palette = page.getByRole('dialog', { name: 'Messages commands' });
  const input = palette.getByRole('combobox');
  await expect(input).toHaveAttribute('placeholder', 'Type a command or search…');
  await expect(palette.getByRole('option').first()).toHaveAccessibleName('Messages');
  await expect(palette.getByRole('option').first()).toHaveAttribute('aria-selected', 'true');
  expect(await palette.locator('.kit-command-palette__section').allTextContents()).toEqual(['Go to', 'Actions', 'Appearance']);
  expect(await palette.getByRole('option').evaluateAll(options => options.every(option => {
    const icon = option.querySelector('svg')?.getBoundingClientRect();
    return icon?.width === 16 && icon.height === 16 && option.getBoundingClientRect().height >= 36;
  }))).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('artifacts/07-shell-commands.png') });
  await palette.screenshot({ path: testInfo.outputPath('artifacts/07-shell-commands-full.png') });

  await input.fill('source');
  await expect(palette.getByRole('option', { name: 'Sources', exact: true })).toBeVisible();
  await expect(palette.getByRole('option', { name: 'Group by… › Source', exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('artifacts/190-shell-commands-filtered.png') });
  await input.fill('zzzxxyy');
  await expect(palette.getByRole('option')).toHaveCount(2);
  await expect(palette.getByRole('option', { name: /Search archive/ })).toHaveAttribute('aria-selected', 'true');
  await page.screenshot({ path: testInfo.outputPath('artifacts/191-shell-commands-no-results.png') });

  await input.clear();
  await palette.getByRole('option', { name: /Group by…/ }).click();
  await expect(palette.getByRole('listbox')).toHaveAccessibleName('Group by…');
  await palette.getByRole('option', { name: 'Source', exact: true }).click();
  await expect(page.getByLabel('Active analytical context')).toContainText('Source');
  await page.keyboard.press('Control+k');
  await palette.getByRole('option', { name: 'People', exact: true }).click();
  await page.reload();
  await page.keyboard.press('Control+k');
  palette = page.getByRole('dialog', { name: 'Messages commands' });
  await expect(palette.locator('.kit-command-palette__section').first()).toHaveText('Recent');
  await expect(palette.getByRole('option').first()).toHaveAccessibleName('People');
  expect(errors).toEqual([]);
});
