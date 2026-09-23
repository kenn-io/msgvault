import { expect, test } from '@playwright/test';

for (const width of [390, 320]) {
  test(`People sweep setup fits a ${width}px viewport with disclosure open`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    await page.goto('/tests/fixtures/people-inference.html');
    await expect(page.getByRole('heading', { name: 'People sweep' })).toBeVisible();
    await page.getByRole('button', { name: 'Check provider' }).click();
    await expect(page.getByRole('region', { name: 'Archive disclosure' })).toBeVisible();

    const overflow = await page.evaluate(() => ({
      viewport: window.innerWidth,
      document: document.documentElement.scrollWidth,
      component: document.querySelector<HTMLElement>('[aria-label="People sweep settings"]')!.scrollWidth,
      componentWidth: document.querySelector<HTMLElement>('[aria-label="People sweep settings"]')!.clientWidth,
    }));
    expect(overflow.document).toBeLessThanOrEqual(overflow.viewport);
    expect(overflow.component).toBeLessThanOrEqual(overflow.componentWidth);
    await expect(page.getByRole('button', { name: 'Select and enable' })).toBeDisabled();

    await page.getByRole('combobox', { name: 'Profile', exact: true }).selectOption('spare-profile');
    await page.getByRole('button', { name: 'Remove profile' }).click();
    await expect(page.getByRole('button', { name: 'Confirm removal' })).toBeVisible();
    const confirmationWidth = await page.evaluate(() => ({
      viewport: window.innerWidth,
      document: document.documentElement.scrollWidth,
    }));
    expect(confirmationWidth.document).toBeLessThanOrEqual(confirmationWidth.viewport);
  });
}
