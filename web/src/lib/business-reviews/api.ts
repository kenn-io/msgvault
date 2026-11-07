import type { APIClient } from '../api/client';
import { controlBusinessReviews } from '../api/generated/business-reviews/business-reviews';
import type { BusinessReviewsRequest } from '../api/generated/models';
export type {
  BusinessReviewsReview as Review,
  BusinessReviewsPreview as Preview,
  BusinessReviewsReceipt as Receipt,
  BusinessReviewsTargetInfo as Target,
  BusinessReviewsResult as Result,
} from '../api/generated/models';
import type { BusinessReviewsResult as Result } from '../api/generated/models';

export class PreviewConflictError extends Error {
  constructor() {
    super('Review changed or preview expired. Refresh and create a new preview.');
    this.name = 'PreviewConflictError';
  }
}

export async function control(client: APIClient, body: BusinessReviewsRequest, signal: AbortSignal): Promise<Result> {
  const { data, error, response } = await controlBusinessReviews(body, { ...client, signal, cache: 'no-store' });
  if (data && response.ok) return data;
  if (response.status === 409 && error?.error === 'business_reviews_preview_changed') throw new PreviewConflictError();
  switch (error?.error) {
    case 'business_reviews_credentials': throw new Error('Authorize the configured Google account again. Check its named app and required scopes.');
    case 'business_reviews_permission': throw new Error('Google denied access. Check project approval, verified location, and account permissions.');
    case 'business_reviews_quota': throw new Error('Google quota reached. Wait before refreshing live reviews.');
    case 'business_reviews_temporary': throw new Error('Google is temporarily unavailable. Retry a live read later.');
  }
  if (response.status === 401) throw new Error('Sign in again, or authorize the configured Google account.');
  if (response.status === 403) throw new Error('Access denied. Responses require an owner key, an owner session, and an enabled target.');
  if (response.status === 404) throw new Error('Review or receipt unavailable. Refresh live reviews.');
  if (response.status === 409) throw new Error('Review changed or preview expired. Refresh and create a new preview.');
  throw new Error('Business review request failed. Check project approval, credentials, permissions, and quota.');
}
