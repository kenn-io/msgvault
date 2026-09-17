import type { MeetingScopeRequest } from '../api/generated/models';
import type { GroupDetailAuthority } from '../explore/group-detail';
import type { ExplorePredicate } from '../explore/models';
import type { MeetingPanelScope } from './controller.svelte';

export function exploreMeetingScope(predicate: ExplorePredicate, authority: GroupDetailAuthority): Extract<MeetingPanelScope, { kind: 'explore' }> {
  return { kind: 'explore', explore: {
    predicate: { ...predicate, cursor: undefined, candidate_snapshot_id: authority.candidateSnapshotId },
    cache_revision: authority.cacheRevision, search_provenance: authority.searchProvenance,
    candidate_snapshot_id: authority.candidateSnapshotId
  } };
}

const unsupportedFilters = () => new Error('Meeting activity cannot represent these relationship filters exactly. Adjust the filters to load meeting activity.');

/** Relationships intentionally ignores URL text search. Convert only constraints
 * that its direct meeting scope can preserve; never broaden a contextual filter. */
export function relationshipMeetingScope(identity: Pick<MeetingScopeRequest, 'participant_id' | 'domains'>, predicate: ExplorePredicate): MeetingPanelScope {
  const scope: MeetingScopeRequest = { ...identity };
  for (const filter of predicate.filters ?? []) {
    const values = filter.values;
    if (values.length === 0) throw unsupportedFilters();
    if (filter.dimension === 'source') {
      const ids = values.map(Number);
      if (ids.some((id) => !Number.isSafeInteger(id) || id < 1)) throw unsupportedFilters();
      scope.source_ids = scope.source_ids ? scope.source_ids.filter((id) => ids.includes(id)) : [...new Set(ids)];
      if (scope.source_ids.length === 0) { delete scope.source_ids; scope.message_ids = []; }
    } else if (filter.dimension === 'after' || filter.dimension === 'before') {
      if (values.length !== 1 || !Number.isFinite(Date.parse(values[0]!))) throw unsupportedFilters();
      const key = filter.dimension;
      const previous = scope[key];
      if (!previous || (key === 'after' ? Date.parse(values[0]!) > Date.parse(previous) : Date.parse(values[0]!) < Date.parse(previous))) scope[key] = values[0];
    } else if (filter.dimension === 'message_type') {
      if (!values.includes('meeting_transcript')) scope.message_ids = [];
    } else if (filter.dimension === 'deletion') {
      if (values.length !== 1 || !['any', 'active', 'deleted'].includes(values[0]!)) throw unsupportedFilters();
      const deletion = values[0] as MeetingScopeRequest['deletion'];
      if (deletion !== 'any') {
        if (scope.deletion && scope.deletion !== deletion) scope.message_ids = [];
        scope.deletion = deletion;
      }
    } else if (filter.dimension === 'domain') {
      const domains = [...new Set(values.map((value) => value.toLowerCase()))].sort();
      if (scope.domains) {
        // Matching the selected domain already guarantees an OR group that
        // contains it; independent membership intersections remain unsupported.
        if (!scope.domains.every((domain) => domains.includes(domain.toLowerCase()))) throw unsupportedFilters();
      } else scope.domains = domains;
    } else {
      // Participant/domain membership is multi-valued. Set intersection would
      // erase legitimate co-participant matches, and a singular participant
      // reference cannot be combined with a plural identity filter on the wire.
      throw unsupportedFilters();
    }
  }
  if (scope.after && scope.before && Date.parse(scope.after) >= Date.parse(scope.before)) {
    scope.message_ids = [];
    delete scope.after;
    delete scope.before;
  }
  return { kind: 'direct', scope };
}
