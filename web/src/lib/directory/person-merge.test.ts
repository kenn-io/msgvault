import { describe, expect, it } from 'vitest';

import {
  isMatchingPersonETag,
  isMatchingPersonRevisionETag,
  isPersonMergeRevisionConflict,
  validatePersonMergeRequired
} from './person-merge';

function person(id: number, revision: number, displayName: string) {
  return {
    id,
    revision,
    display_name: displayName,
    participant_ids: [id * 10],
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-02T00:00:00Z',
    vcard_uid: `synthetic-${id}`
  };
}

function conflict() {
  return {
    error: 'person_merge_required',
    message: 'Choose a survivor',
    profiles: [
      { person: person(7, 4, 'Synthetic One'), etag: '"person-7-r4"' },
      { person: person(9, 2, 'Synthetic Two'), etag: '"person-9-r2"' }
    ]
  };
}

describe('validatePersonMergeRequired', () => {
  it('normalizes an exact two-profile conflict into an actionable tuple', () => {
    const validated = validatePersonMergeRequired(conflict());

    expect(validated).not.toBeNull();
    expect(validated?.error).toBe('person_merge_required');
    expect(validated?.profiles.map((profile) => profile.person.id)).toEqual([7, 9]);
  });

  it.each([
    { name: 'null profiles', mutate: (value: ReturnType<typeof conflict>) => { value.profiles = null as never; } },
    { name: 'one profile', mutate: (value: ReturnType<typeof conflict>) => { value.profiles = value.profiles.slice(0, 1); } },
    { name: 'duplicate person', mutate: (value: ReturnType<typeof conflict>) => { value.profiles[1] = value.profiles[0]!; } },
    { name: 'zero person', mutate: (value: ReturnType<typeof conflict>) => { value.profiles[0]!.person.id = 0; } },
    { name: 'weak etag', mutate: (value: ReturnType<typeof conflict>) => { value.profiles[0]!.etag = 'W/"person-7-r4"'; } },
    { name: 'mismatched etag', mutate: (value: ReturnType<typeof conflict>) => { value.profiles[0]!.etag = '"person-8-r4"'; } },
    { name: 'zero revision etag', mutate: (value: ReturnType<typeof conflict>) => { value.profiles[0]!.etag = '"person-7-r0"'; } },
    { name: 'generic conflict', mutate: (value: ReturnType<typeof conflict>) => { value.error = 'person_binding_conflict'; } }
  ])('rejects $name instead of inventing merge metadata', ({ mutate }) => {
    const value = conflict();
    mutate(value);

    expect(validatePersonMergeRequired(value)).toBeNull();
  });
});

describe('person merge error and ETag guards', () => {
  it('accepts only a strong tag whose embedded ID and revision are positive', () => {
    expect(isMatchingPersonETag('"person-7-r4"', 7)).toBe(true);
    expect(isMatchingPersonETag('"person-9-r2"', 7)).toBe(false);
    expect(isMatchingPersonETag('W/"person-7-r4"', 7)).toBe(false);
    expect(isMatchingPersonETag('"person-7-r0"', 7)).toBe(false);
  });

  it('requires the strong tag revision to match the returned person snapshot', () => {
    const snapshot = person(7, 4, 'Synthetic One');
    expect(isMatchingPersonRevisionETag('"person-7-r4"', snapshot)).toBe(true);
    expect(isMatchingPersonRevisionETag('"person-7-r5"', snapshot)).toBe(false);
    expect(isMatchingPersonRevisionETag('"person-9-r4"', snapshot)).toBe(false);
  });

  it('recognizes stale state only from the exact merge revision error code', () => {
    expect(isPersonMergeRevisionConflict({ error: 'person_merge_revision_conflict' })).toBe(true);
    expect(isPersonMergeRevisionConflict({ error: 'person_merge_idempotency_conflict' })).toBe(false);
    expect(isPersonMergeRevisionConflict({ error: 'person_carddav_published' })).toBe(false);
    expect(isPersonMergeRevisionConflict({ message: 'changed' })).toBe(false);
  });
});
