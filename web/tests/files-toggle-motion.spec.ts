import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

test('Files toggle settles its colors immediately with reduced motion', async ({ page }) => {
  await installMixedArchive(page);
  await page.goto('/');
  await page.getByRole('grid', { name: 'Relationship results' }).getByText('Archive Person').click();
  const files = page.getByRole('button', { name: 'Files 1' });
  await files.click();
  await expect(files).toHaveAttribute('aria-expanded', 'true');

  const colors = await files.evaluate(async (button) => {
    const label = button.querySelector('.kit-button__label-text')!;
    const snapshot = () => ({
      foreground: getComputedStyle(label).color,
      background: getComputedStyle(button).backgroundColor,
    });
    await Promise.all(button.getAnimations().map((animation) => animation.finished));
    snapshot();
    button.click();
    // Flush Svelte's state update within the same frame, before transitions advance.
    await Promise.resolve();
    await Promise.resolve();
    const immediate = snapshot();
    await Promise.all(button.getAnimations().map((animation) => animation.finished));
    await new Promise(requestAnimationFrame);
    return { immediate, settled: snapshot() };
  });
  expect(colors.immediate).toEqual(colors.settled);
});
