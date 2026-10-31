import { getTimeline as generatedGetTimeline } from '../api/generated/timeline/timeline';
import type { GetTimelineParams, TimelineCandidate, TimelineResult } from '../api/generated/models';
import type { APIClient } from '../api/client';

export class TimelineRequestError extends Error {
  constructor(message: string, readonly status: number, readonly candidates: TimelineCandidate[] = []) {
    super(message);
  }
}

export async function readTimeline(client: APIClient, request: GetTimelineParams, signal: AbortSignal): Promise<TimelineResult> {
  const { data, error, response } = await generatedGetTimeline({ ...request, format: 'json' }, { ...client, signal });
  if (data && typeof data !== 'string') return data;
  const candidates = response.status === 409 && Array.isArray(error?.candidates)
    ? error.candidates.filter(isCandidate) : [];
  throw new TimelineRequestError(
    typeof error?.message === 'string' ? error.message : `Could not load the timeline (${response.status}).`,
    response.status,
    candidates,
  );
}

function isCandidate(value: unknown): value is TimelineCandidate {
  if (!value || typeof value !== 'object') return false;
  const candidate = value as Partial<TimelineCandidate>;
  return typeof candidate.display_name === 'string' && typeof candidate.address === 'string' &&
    typeof candidate.person_uid === 'string' && Number.isSafeInteger(candidate.person_id) &&
    Array.isArray(candidate.participant_ids) && candidate.participant_ids.every((id) => Number.isSafeInteger(id) && id > 0);
}
