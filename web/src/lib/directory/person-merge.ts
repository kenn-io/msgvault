import type { components } from '../api/generated/schema';

type PersonMergeProfile = components['schemas']['PersonMergeProfile'];

export type ValidatedPersonMergeRequired = Omit<components['schemas']['PersonMergeRequiredError'], 'error' | 'message' | 'profiles'> & {
  error: 'person_merge_required';
  message: string;
  profiles: [PersonMergeProfile, PersonMergeProfile];
};

export type PersonMergeSuccess = {
  result: components['schemas']['PersonMergeResult'];
  survivor: components['schemas']['Person'];
  responseETag: string | null;
};

export function isMatchingPersonETag(value: unknown, personID: number): value is string {
  return parsePersonETag(value)?.personID === personID;
}

export function isMatchingPersonRevisionETag(
  value: unknown,
  person: Pick<components['schemas']['Person'], 'id' | 'revision'>
): value is string {
  const parsed = parsePersonETag(value);
  return parsed?.personID === person.id && parsed.revision === person.revision;
}

export function validatePersonMergeRequired(value: unknown): ValidatedPersonMergeRequired | null {
  if (!isRecord(value) || value.error !== 'person_merge_required' || typeof value.message !== 'string' ||
    !Array.isArray(value.profiles) || value.profiles.length !== 2) {
    return null;
  }
  const profiles = value.profiles;
  const ids = new Set<number>();
  for (const profile of profiles) {
    if (!isRecord(profile) || !isRecord(profile.person)) return null;
    const personID = profile.person.id;
    const revision = profile.person.revision;
    if (!isPositiveSafeInteger(personID) || !isPositiveSafeInteger(revision) || ids.has(personID)) return null;
    const tag = parsePersonETag(profile.etag);
    if (!tag || tag.personID !== personID || tag.revision !== revision) return null;
    ids.add(personID);
  }
  return value as ValidatedPersonMergeRequired;
}

export function isPersonMergeRevisionConflict(value: unknown): boolean {
  return isRecord(value) && value.error === 'person_merge_revision_conflict';
}

function parsePersonETag(value: unknown): { personID: number; revision: number } | null {
  if (typeof value !== 'string') return null;
  const match = /^"person-([1-9]\d*)-r([1-9]\d*)"$/.exec(value);
  if (!match) return null;
  const personID = Number(match[1]);
  const revision = Number(match[2]);
  return isPositiveSafeInteger(personID) && isPositiveSafeInteger(revision)
    ? { personID, revision }
    : null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function isPositiveSafeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
}
