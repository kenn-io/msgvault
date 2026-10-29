import { expect, type Page } from '@playwright/test';

export async function selectKitOption(
  page: Page,
  title: string,
  option: string
): Promise<void> {
  await page.getByRole('combobox', { name: new RegExp(`^${title}:`) }).click();
  await page.getByRole('option', { name: option, exact: true }).click();
}

export async function selectWorkspace(page: Page, label: string): Promise<void> {
  if (['Directory', 'Reviews', 'Deletions'].includes(label)) {
    await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
    await page.getByRole('dialog', { name: 'Messages commands' }).getByRole('option', { name: label === 'Reviews' ? 'Open Reviews' : label, exact: true }).click();
    return;
  }
  const opener = page.getByRole('button', { name: 'Open navigation' });
  if (await opener.isVisible()) await opener.click();
  await page
    .getByRole('complementary', { name: 'Sidebar' })
    .getByRole('button', { name: label, exact: true })
    .click();
}

export async function setTemporaryDensity(
  page: Page,
  density: 'Compact' | 'Comfortable'
): Promise<void> {
  await page.getByRole('button', { name: 'Display' }).click();
  await page.getByRole('menuitemradio', { name: density }).click();
  await page.keyboard.press('Escape');
}

export async function setKitTheme(page: Page, theme: 'light' | 'dark'): Promise<void> {
  await page.getByRole('button', { name: 'Display', exact: true }).click();
  await page.getByRole('menuitemradio', { name: theme === 'light' ? 'Light' : 'Dark', exact: true }).click();
  await page.keyboard.press('Escape');
  await expectKitTheme(page, theme);
}

export async function expectKitTheme(page: Page, theme: 'light' | 'dark'): Promise<void> {
  const root = page.locator('html');
  if (theme === 'dark') await expect(root).toHaveClass(/\bdark\b/);
  else await expect(root).not.toHaveClass(/\bdark\b/);
}
