import { expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { control, PreviewConflictError, ReceiptUnavailableError } from './api';

it.each([
  [502, 'google_reviews_credentials', 'Authorize the configured Google account again. Check its named app and required scopes.'],
  [403, 'google_reviews_permission', 'Google denied access. Check project approval, verified location, and account permissions.'],
  [429, 'google_reviews_quota', 'Google quota reached. Wait before refreshing live reviews.'],
  [503, 'google_reviews_temporary', 'Google is temporarily unavailable. Retry a live read later.'],
] as const)('shows a safe actionable category for %s/%s without upstream text', async (status, code, message) => {
  const client = createAPIClient(async () => Response.json({ error: code, message: 'private-upstream-canary' }, { status }));
  await expect(control(client, { action: 'capabilities' }, new AbortController().signal)).rejects.toThrow(message);
});

it('classifies a missing receipt separately from a missing live review', async () => {
  const client = createAPIClient(async () => Response.json({ error: 'google_reviews_receipt_not_found' }, { status: 404 }));
  const receiptError = await control(client, { action: 'receipt', receipt_id: 'synthetic-receipt' }, new AbortController().signal).catch(cause => cause);
  expect(receiptError).toBeInstanceOf(ReceiptUnavailableError);
  expect(receiptError.message).toBe('Google Reviews receipt unavailable.');
  const reviewError = await control(client, { action: 'get', target: 'shop', review_id: 'synthetic-review' }, new AbortController().signal).catch(cause => cause);
  expect(reviewError).toBeInstanceOf(Error);
  expect(reviewError).not.toBeInstanceOf(ReceiptUnavailableError);
});

it('explains that starting an archive backfill requires owner authentication', async () => {
  const client = createAPIClient(async () => Response.json({ error: 'google_reviews_denied' }, { status: 403 }));
  await expect(control(client, { action: 'backfill', target: 'shop' }, new AbortController().signal))
    .rejects.toThrow('Owner authentication is required to start a Google Reviews archive backfill.');
});

it.each([
  [409, 'google_reviews_preview_changed', 'PreviewConflictError'],
  [409, 'proxy_conflict', 'Error'],
  [500, 'google_reviews_preview_changed', 'Error'],
  [403, 'google_reviews_preview_changed', 'Error'],
] as const)('classifies only exact preview rejection %s/%s as %s', async (status, code, name) => {
  const client = createAPIClient(async () => Response.json({ error: code, message: 'private-conflict-canary' }, { status }));
  const failure = await control(client, { action: 'confirm_reply', confirmation_token: 'token1', confirmed: true }, new AbortController().signal).catch(cause => cause);
  expect(failure).toBeInstanceOf(Error);
  expect(failure.name).toBe(name);
  expect(failure.message).not.toContain('private-conflict-canary');
  if (name === 'PreviewConflictError') {
    expect(failure).toBeInstanceOf(PreviewConflictError);
    expect(failure.message).toBe('Review changed or preview expired. Refresh and create a new preview.');
  } else expect(failure).not.toBeInstanceOf(PreviewConflictError);
});
