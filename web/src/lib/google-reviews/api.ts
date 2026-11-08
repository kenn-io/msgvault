import type { APIClient } from '../api/client';
import { controlGoogleReviews, searchGoogleReviewsArchive } from '../api/generated/google-reviews/google-reviews';
import type { GoogleReviewsArchiveSearchResponse, GoogleReviewsRequest, SearchGoogleReviewsArchiveParams } from '../api/generated/models';
export type {
  GoogleReviewsArchiveSearchResponse as ArchiveSearchResponse,
  GoogleReviewsBackfillState as BackfillState,
  GoogleReviewsReview as Review,
  GoogleReviewsPreview as Preview,
  GoogleReviewsReceipt as Receipt,
  GoogleReviewsTargetInfo as Target,
  GoogleReviewsResult as Result,
} from '../api/generated/models';
import type { GoogleReviewsResult as Result } from '../api/generated/models';

export class PreviewConflictError extends Error {
  constructor() {
    super('Review changed or preview expired. Refresh and create a new preview.');
    this.name = 'PreviewConflictError';
  }
}

export class ReceiptUnavailableError extends Error {
  constructor() {
    super('Google Reviews receipt unavailable.');
    this.name = 'ReceiptUnavailableError';
  }
}

export async function control(client: APIClient, body: GoogleReviewsRequest, signal: AbortSignal): Promise<Result> {
  const { data, error, response } = await controlGoogleReviews(body, { ...client, signal, cache: 'no-store' });
  if (data && response.ok) return data;
  if (response.status === 409 && error?.error === 'google_reviews_preview_changed') throw new PreviewConflictError();
  if (body.action === 'receipt' && response.status === 404) throw new ReceiptUnavailableError();
  switch (error?.error) {
    case 'google_reviews_credentials': throw new Error('Reconnect the configured Google account and app with the required permissions.');
    case 'google_reviews_permission': throw new Error('Google denied access. Check project approval, verified location, and account permissions.');
    case 'google_reviews_quota': throw new Error('Google quota reached. Wait before refreshing live reviews.');
    case 'google_reviews_temporary': throw new Error('Google is temporarily unavailable. Retry a live read later.');
  }
  if (response.status === 401) throw new Error('Sign in again, or authorize the configured Google account.');
  if (response.status === 403) {
    if (body.action === 'backfill') throw new Error('Owner authentication is required to start a Google Reviews archive backfill.');
    throw new Error('Access denied. Responses require an owner key, an owner session, and an enabled target.');
  }
  if (response.status === 404) throw new Error('Review or receipt unavailable. Refresh live reviews.');
  if (response.status === 409) throw new Error('Review changed or preview expired. Refresh and create a new preview.');
  throw new Error('Google Reviews request failed. Check project approval, credentials, permissions, and quota.');
}

export async function searchArchive(
  client: APIClient,
  params: SearchGoogleReviewsArchiveParams,
  signal: AbortSignal,
): Promise<GoogleReviewsArchiveSearchResponse> {
  const { data, error, response } = await searchGoogleReviewsArchive(params, { ...client, signal, cache: 'no-store' });
  if (data && response.ok) return data;
  if (response.status === 400) throw new Error('Enter a full-text query of up to 256 UTF-8 bytes.');
  if (response.status === 403) throw new Error('Google Reviews archive search requires owner access.');
  if (error?.error === 'google_reviews_archive_unavailable') throw new Error('Google Reviews archive is unavailable.');
  throw new Error('Google Reviews archive search failed.');
}
