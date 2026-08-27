import { writeFile } from 'node:fs/promises';

export type ScreenshotOptions = {
  fullPage: boolean;
  animations: 'disabled';
  caret: 'hide';
};

export type ScreenshotPage = {
  screenshot: (options: ScreenshotOptions) => Promise<Buffer>;
};

const stabilizationDelays = [0, 100, 250, 500];
const stabilizationTimeoutMs = 5_000;

export async function captureScreenshot(
  page: ScreenshotPage,
  outputPath: string
): Promise<void> {
  const startedAt = Date.now();
  let previous: Buffer | undefined;
  let attempt = 0;
  let screenshot: Buffer;
  while (true) {
    if (Date.now() - startedAt > stabilizationTimeoutMs) {
      throw new Error('screenshot did not stabilize within 5000ms');
    }
    const delay = stabilizationDelays[Math.min(attempt, stabilizationDelays.length - 1)];
    if (delay) await new Promise((resolve) => setTimeout(resolve, delay));
    screenshot = await page.screenshot({ fullPage: false, animations: 'disabled', caret: 'hide' });
    if (previous?.equals(screenshot)) break;
    previous = screenshot;
    attempt += 1;
  }
  await writeFile(outputPath, screenshot);
}
