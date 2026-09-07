import { expect, type Page } from '@playwright/test';

export async function selectKitOption(
  page: Page,
  title: string,
  option: string
): Promise<void> {
  await page.getByRole('combobox', { name: new RegExp(`^${title}:`) }).click();
  await page.getByRole('option', { name: option, exact: true }).click();
}

export async function selectKitTopBarTab(page: Page, tab: string): Promise<void> {
  const navigation = page.getByRole('navigation', { name: 'Primary' });
  const button = navigation.getByRole('button', { name: tab, exact: true });
  const collapsed = navigation.getByRole('combobox', { name: /^Primary:/ });
  await expect(button.or(collapsed)).toBeVisible();
  if (await button.isVisible()) {
    await button.click();
    return;
  }
  await selectKitOption(page, 'Primary', tab);
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
