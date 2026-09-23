import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

test('Files view switch settles its colors immediately with reduced motion', async ({ page }) => {
  await installMixedArchive(page);
  await page.goto('/');
  await page.getByRole('grid', { name: 'Relationship results' }).getByText('Archive Person').click();
  const files = page.getByRole('radio', { name: 'Files 1' });
  await files.click();
  await expect(files).toHaveAttribute('aria-checked', 'true');

  // Switching back to Messages is the segmented-control analogue of the old
  // toggle-off: measure the newly selected segment's colors in the same
  // frame to prove reduced motion settles them immediately.
  const messages = page.getByRole('radio', { name: 'Messages' });
  const colors = await messages.evaluate(async (segment) => {
    const snapshot = () => ({
      foreground: getComputedStyle(segment).color,
      background: getComputedStyle(segment).backgroundColor,
    });
    await Promise.all(segment.getAnimations().map((animation) => animation.finished));
    snapshot();
    segment.click();
    // Flush Svelte's state update within the same frame, before transitions advance.
    await Promise.resolve();
    await Promise.resolve();
    const immediate = snapshot();
    await Promise.all(segment.getAnimations().map((animation) => animation.finished));
    await new Promise(requestAnimationFrame);
    return { immediate, settled: snapshot() };
  });
  expect(colors.immediate).toEqual(colors.settled);
  await expect(messages).toHaveAttribute('aria-checked', 'true');
});
