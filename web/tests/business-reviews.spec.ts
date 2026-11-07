import { expect, test } from '@playwright/test';
import { mockArchiveLayout } from './archive-layout';
import { installCardDAV } from './e2e/fixtures/carddav';

const review = { reviewId: 'synthetic-review', reviewer: { displayName: 'Example Reviewer', profilePhotoUrl: 'https://example.invalid/do-not-fetch' }, starRating: 'FIVE', comment: 'Thoughtful service. Thank you!', reviewReply: { comment: 'We appreciate your visit.', reviewReplyState: 'PENDING' } };

test('live business reviews require exact preview and distinct confirmation', async ({ page }, testInfo) => {
  const browserErrors: string[] = [];
  page.on('pageerror', error => browserErrors.push(error.message));
  // Fixture routes only: no daemon, user archive, OAuth or provider requests.
  await mockArchiveLayout(page);
  await installCardDAV(page);
  let confirmations = 0;
  let receiptLookups = 0;
  let mediaRequests = 0;
  await page.route('https://example.invalid/**', route => { mediaRequests++; return route.abort(); });
  await page.route('**/api/v1/business-reviews/control', async route => {
    const body = route.request().postDataJSON();
    expect(new URL(route.request().url()).search).toBe('');
    if (body.action === 'capabilities') return route.fulfill({ json: {
      capabilities: [{ provider: 'google', available: true, read: true, reply: true }, { provider: 'facebook', available: false, read: false, reply: false, reason: 'Official review-specific access and reply contract unverified.' }],
      targets: [{ name: 'example-shop', provider: 'google', enabled: true, replies_enabled: true }]
    } });
    if (body.action === 'list') return route.fulfill({ json: { reviews: [review] } });
    if (body.action === 'get') return route.fulfill({ json: { review } });
    if (body.action === 'preview_reply') {
      expect(body.comment).toBe('  Thank you!\nWe hope to see you again.  ');
      return route.fulfill({ json: { preview: { review, comment: body.comment, expires_at: '2099-01-01T00:00:00Z', confirmation_token: 'synthetic-token', receipt_id: 'synthetic-receipt' } } });
    }
    if (body.action === 'receipt') {
      expect(body).toEqual({ action: 'receipt', receipt_id: 'synthetic-receipt' });
      receiptLookups++;
    }
    if (body.action === 'confirm_reply') {
      expect(body).toEqual({ action: 'confirm_reply', confirmation_token: 'synthetic-token', confirmed: true });
      confirmations++;
    }
    return route.fulfill({ json: { receipt: { id: 'synthetic-receipt', status: 'submitted', created_at: '2026-11-07T00:00:00Z', updated_at: '2026-11-07T00:00:00Z' } } });
  });
  await page.goto('/?workspace=business-reviews');
  await expect(page.getByRole('heading', { name: 'Business reviews' })).toBeVisible();
  await expect(page.getByText(/Facebook unavailable/)).toBeVisible();
  await page.getByLabel('Target', { exact: true }).selectOption('example-shop');
  await page.getByRole('button', { name: /Example Reviewer/ }).click();
  await expect(page.getByText('Thoughtful service. Thank you!')).toBeVisible();
  await page.getByLabel('Public response').fill('  Thank you!\nWe hope to see you again.  ');
  await page.getByRole('button', { name: 'Preview response' }).click();
  const dialog = page.getByRole('dialog', { name: 'Confirm public response' });
  await expect(dialog).toBeVisible();
  expect(await dialog.getByTestId('exact-response').textContent()).toBe('  Thank you!\nWe hope to see you again.  ');
  expect(confirmations).toBe(0);
  await page.screenshot({ path: testInfo.outputPath('web-preview-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: testInfo.outputPath('web-preview-mobile.png'), fullPage: true });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await dialog.getByRole('button', { name: 'Confirm and submit' }).click();
  await expect(page.getByText(/Submitted to Google/)).toBeVisible();
  await page.getByRole('button', { name: 'Check receipt' }).click();
  await expect.poll(() => receiptLookups).toBe(1);
  await expect(page.getByRole('button', { name: 'Check receipt' })).toBeEnabled();
  expect(confirmations).toBe(1);
  expect(mediaRequests).toBe(0);
  expect(await page.evaluate(() => [localStorage, sessionStorage].some(storage => Object.values(storage).some(value => /Thoughtful service|synthetic-review|synthetic-token|Thank you!/.test(value))))).toBe(false);
  await page.screenshot({ path: testInfo.outputPath('business-review-receipt.png'), fullPage: true });
  expect(browserErrors).toEqual([]);
  await page.getByRole('button', { name: 'Open navigation', exact: true }).click();
  const navigation = page.getByRole('dialog', { name: 'Navigation', exact: true });
  await expect(navigation).toBeVisible();
  await navigation.getByRole('button', { name: 'Messages', exact: true }).click();
  await expect(page.getByText('Thoughtful service. Thank you!')).toHaveCount(0);
});

test('shared mobile navigation control from existing settings', async ({ page }) => {
  await mockArchiveLayout(page);
  await installCardDAV(page);
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto('/?workspace=settings');
  await expect(page.getByRole('button', { name: 'Help', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Help', exact: true }).click();
  const help = page.getByRole('dialog');
  await expect(help).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.keyboard.press('Escape');
  await expect(help).toHaveCount(0);
  await page.getByRole('button', { name: 'Open navigation', exact: true }).click();
  expect(errors).toEqual([]);
  await expect(page.getByRole('dialog', { name: 'Navigation', exact: true })).toBeVisible();
});
