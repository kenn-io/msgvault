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
  const opener = page.getByRole('button', { name: 'Open navigation' });
  if (await opener.isVisible()) await opener.click();
  await page
    .getByRole('navigation', { name: 'Primary' })
    .getByRole('button', { name: label, exact: true })
    .click();
}

export async function setTemporaryDensity(
  page: Page,
  density: 'Auto' | 'Compact' | 'Comfortable'
): Promise<void> {
  await page.getByRole('button', { name: 'Display' }).click();
  await page.getByRole('menuitemradio', { name: density }).click();
  await page.keyboard.press('Escape');
}

export async function setKitTheme(page: Page, theme: 'light' | 'dark'): Promise<void> {
  const toggle = page.getByRole('button', { name: /^Change theme \(current:/ });
  const wanted = theme === 'light' ? 'Light' : 'Dark';
  for (let attempt = 0; attempt < 3; attempt += 1) {
    if ((await toggle.getAttribute('aria-label'))?.includes(`current: ${wanted}`)) break;
    await toggle.click();
  }
  await expectKitTheme(page, theme);
}

export async function expectKitTheme(page: Page, theme: 'light' | 'dark'): Promise<void> {
  const root = page.locator('html');
  if (theme === 'dark') await expect(root).toHaveClass(/\bdark\b/);
  else await expect(root).not.toHaveClass(/\bdark\b/);
}
