import { expect, test } from '@playwright/test';

import { mockArchiveLayout } from './archive-layout';
import { setKitTheme } from './kit-ui';

test.use({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, colorScheme: 'dark' });

test('palette shows ordered sections, initial selection, and command icons', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await mockArchiveLayout(page);
  await page.goto('/');
  await expect(page.getByText('Synthetic archive subject')).toBeVisible();
  await setKitTheme(page, 'dark');
  await page.keyboard.press('Control+k');
  const palette = page.getByRole('dialog', { name: 'Messages commands' });
  const input = palette.getByRole('combobox');
  await expect(input).toHaveAttribute('placeholder', 'Type a command or search…');
  await expect(palette.getByRole('option').first()).toHaveAccessibleName('Messages');
  await expect(palette.getByRole('option').first()).toHaveAttribute('aria-selected', 'true');
  expect(await palette.locator('.kit-command-palette__section').allTextContents()).toEqual(['Go to', 'Actions', 'Appearance']);
  expect(await palette.getByRole('option').evaluateAll(options => options.every(option => {
    const icon = option.querySelector('svg')?.getBoundingClientRect();
    return icon?.width === 16 && icon.height === 16;
  }))).toBe(true);
  expect(errors).toEqual([]);
});

test('palette keeps fallback rows in Search with and without command matches', async ({ page }) => {
  await mockArchiveLayout(page);
  await page.goto('/');
  await expect(page.getByText('Synthetic archive subject')).toBeVisible();
  await page.keyboard.press('Control+k');
  const palette = page.getByRole('dialog', { name: 'Messages commands' });
  for (const query of ['source', 'zzzxxyy']) {
    await palette.getByRole('combobox').fill(query);
    const search = palette.locator('.kit-command-palette__section', { hasText: /^Search$/ });
    await expect(search).toBeVisible();
    await expect(search.locator('xpath=following-sibling::*[1]').locator('.kit-command-palette__label')).toHaveText(`Search archive for "${query}"`);
    await expect(search.locator('xpath=following-sibling::*[2]')).toHaveAccessibleName('Find person…');
  }
});
