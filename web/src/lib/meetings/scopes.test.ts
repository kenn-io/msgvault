import { describe, expect, it } from 'vitest';
import { exploreMeetingScope, relationshipMeetingScope } from './scopes';

describe('meeting scopes', () => {
  it('retains repeated person/domain intersections and the corresponding loaded search authority', () => {
    const predicate = { query: 'planning', search_mode: 'hybrid' as const, filters: [
      { dimension: 'participant' as const, values: ['3'] }, { dimension: 'participant' as const, values: ['7'] },
      { dimension: 'domain' as const, values: ['other.example'] }, { dimension: 'domain' as const, values: ['exact.example'] }
    ], cursor: 'old-page', candidate_snapshot_id: 'old-candidate' };
    const scope = exploreMeetingScope(predicate, { cacheRevision: 'detail-cache', searchProvenance: { lexical_index_revision: 'detail-lex', vector_generation: 2 }, candidateSnapshotId: 'detail-candidate' });
    expect(scope).toEqual({ kind: 'explore', explore: { predicate: { ...predicate, cursor: undefined, candidate_snapshot_id: 'detail-candidate' },
      cache_revision: 'detail-cache', search_provenance: { lexical_index_revision: 'detail-lex', vector_generation: 2 }, candidate_snapshot_id: 'detail-candidate' } });
  });

  it('uses the singular participant reference with active source/date filters and excludes stale URL text search', () => {
    expect(relationshipMeetingScope({ participant_id: 7 }, { query: 'stale', search_mode: 'hybrid', filters: [
      { dimension: 'source', values: ['2', '3'] }, { dimension: 'after', values: ['2026-01-01T00:00:00Z'] },
      { dimension: 'before', values: ['2026-03-01T00:00:00Z'] }
    ] })).toEqual({ kind: 'direct', scope: { participant_id: 7, source_ids: [2, 3], after: '2026-01-01T00:00:00Z', before: '2026-03-01T00:00:00Z' } });
  });

  it('intersects repeated source and date filters for an exact domain', () => {
    expect(relationshipMeetingScope({ domains: ['exact.example'] }, { filters: [
      { dimension: 'source', values: ['2', '3'] }, { dimension: 'source', values: ['3', '4'] },
      { dimension: 'after', values: ['2026-01-01T00:00:00Z'] }, { dimension: 'after', values: ['2026-02-01T00:00:00Z'] }
    ] })).toEqual({ kind: 'direct', scope: { domains: ['exact.example'], source_ids: [3], after: '2026-02-01T00:00:00Z' } });
  });

  it('keeps the selected domain when an active OR group already contains it', () => {
    expect(relationshipMeetingScope({ domains: ['exact.example'] }, { filters: [
      { dimension: 'domain', values: ['other.example', 'EXACT.example'] }
    ] })).toEqual({ kind: 'direct', scope: { domains: ['exact.example'] } });
    expect(() => relationshipMeetingScope({ domains: ['exact.example'] }, { filters: [
      { dimension: 'domain', values: ['other.example', 'independent.example'] }
    ] })).toThrow(/filters/);
  });

  it('represents disjoint source constraints as match-none and rejects unrepresentable predicates', () => {
    expect(relationshipMeetingScope({ participant_id: 7 }, { filters: [
      { dimension: 'source', values: ['2'] }, { dimension: 'source', values: ['3'] }
    ] })).toEqual({ kind: 'direct', scope: { participant_id: 7, message_ids: [] } });
    expect(() => relationshipMeetingScope({ participant_id: 7 }, { filters: [{ dimension: 'mailing_list', values: ['important'] }] })).toThrow(/filters/);
  });
});

 it('recognizes guaranteed meeting type and preserves contradictory date bounds as match-none', () => {
   expect(relationshipMeetingScope({ participant_id: 7 }, { filters: [{ dimension: 'message_type', values: ['meeting_transcript', 'email'] }] })).toEqual({ kind: 'direct', scope: { participant_id: 7 } });
   expect(relationshipMeetingScope({ participant_id: 7 }, { filters: [{ dimension: 'message_type', values: ['email'] }] })).toEqual({ kind: 'direct', scope: { participant_id: 7, message_ids: [] } });
   expect(relationshipMeetingScope({ domains: ['exact.example'] }, { filters: [
     { dimension: 'after', values: ['2026-02-01T00:00:00Z'] }, { dimension: 'before', values: ['2026-01-01T00:00:00Z'] }
   ] })).toEqual({ kind: 'direct', scope: { domains: ['exact.example'], message_ids: [] } });
 });
 it('never treats multi-valued participants or domains as scalar membership', () => {
   expect(() => relationshipMeetingScope({ participant_id: 7 }, { filters: [{ dimension: 'participant', values: ['9'] }] })).toThrow(/filters/);
   expect(() => relationshipMeetingScope({ domains: ['exact.example'] }, { filters: [{ dimension: 'domain', values: ['co.example'] }] })).toThrow(/filters/);
 });
