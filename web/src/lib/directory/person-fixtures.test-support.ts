import type { Person, PersonMerge, PersonMergeResult } from '../api/generated/models';

export function person(id: number, revision = id === 7 ? 4 : 2, participant_ids: number[] = id === 7 ? [] : [2], display_name = id === 7 ? 'Synthetic One' : 'Synthetic Two') {
  return { id, revision, participant_ids, display_name, created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z', vcard_uid: `synthetic-${id}` };
}

export function merge(survivor: Person): PersonMerge {
  return {
    id: 41,
    survivor_person_id: survivor.id,
    absorbed_person_id: survivor.id === 7 ? 9 : 7,
    current_person_id: survivor.id,
    survivor_vcard_uid: survivor.vcard_uid,
    absorbed_vcard_uid: survivor.id === 7 ? 'synthetic-9' : 'synthetic-7',
    survivor_revision_before: survivor.revision - 1,
    absorbed_revision_before: 2,
    survivor_revision_after: survivor.revision,
    actor: 'web',
    snapshot_version: 1,
    snapshot_sha256: 'synthetic-digest',
    created_at: '2026-08-03T00:00:00Z',
  };
}

export function mergeResult(survivor: Person, cacheState: 'ready' | 'stale' = 'ready'): PersonMergeResult {
  return { cache_state: cacheState, identity_revision: 8, merge: merge(survivor), person: survivor, review_candidates: [] };
}
