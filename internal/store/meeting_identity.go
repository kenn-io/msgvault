package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
)

// MeetingIdentityRequest bounds identity resolution to occurrences and notes.
type MeetingIdentityRequest struct {
	OccurrenceRefs     []string
	TranscriptIDs      []int64
	IncludeSuggestions bool
}

// MeetingIdentityInfo describes exact meeting evidence. Suggestions never
// contribute to the count key or attendance.
type MeetingIdentityInfo struct {
	Key                     string
	OccurrenceRef           string
	LinkedTranscriptIDs     []int64
	ExactOccurrenceRefs     []string
	SuggestedOccurrenceRefs []string
}

type MeetingIdentityResult struct {
	ByOccurrence map[string]MeetingIdentityInfo
	ByTranscript map[int64]MeetingIdentityInfo
}

type timelineMeetingGroup struct {
	occurrence linkOccurrence
	linked     []int64
}

// ResolveMeetingIdentitiesContext resolves references in one read snapshot.
// A shared read budget limits calendar candidate reads and recurrence expansion.
// Out-of-scope and inactive references are absent. A scoped request never returns
// suggestions, even when IncludeSuggestions is set.
func (s *Store) ResolveMeetingIdentitiesContext(ctx context.Context, req MeetingIdentityRequest) (MeetingIdentityResult, error) {
	result := MeetingIdentityResult{ByOccurrence: map[string]MeetingIdentityInfo{}, ByTranscript: map[int64]MeetingIdentityInfo{}}
	if len(req.OccurrenceRefs) == 0 && len(req.TranscriptIDs) == 0 {
		return result, ctx.Err()
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	r := s.meetingReader()
	r.batch = &meetingIdentityBatch{events: map[string][]CalendarEventRow{}, keys: map[int64]meetingLinkKey{}, decisions: map[int64][]meetingLinkDecision{}, visible: map[int64]bool{}}
	budget := calrecur.RequestReadBudget
	refs := make([]OccurrenceRef, 0, len(req.OccurrenceRefs))
	eventQueries := make([]meetingEventQuery, 0, len(req.OccurrenceRefs))
	for _, value := range req.OccurrenceRefs {
		ref, err := ParseMeetingIdentityRef(value)
		if err != nil {
			return result, err
		}
		refs = append(refs, ref)
		eventQueries = append(eventQueries, meetingEventQuery{"e.message_id = ?", []any{ref.MessageID}})
	}
	if err := s.preloadMeetingQueries(ctx, r, eventQueries, &budget); err != nil {
		return result, err
	}
	if err := s.preloadMeetingOccurrences(ctx, r, refs, &budget); err != nil {
		return result, err
	}
	groups := map[meetingIdentity]*timelineMeetingGroup{}
	aliases := map[string]*timelineMeetingGroup{}
	// Resolve copies through the same representative selection as meeting links.
	groupFor := func(o linkOccurrence) (*timelineMeetingGroup, error) {
		if g := groups[o.Identity]; g != nil {
			return g, nil
		}
		copies, err := s.resolveMeetingIdentity(ctx, r, o.Identity, &budget)
		if err != nil {
			return nil, err
		}
		if len(copies) > 0 {
			o = copies[0]
		}
		g := &timelineMeetingGroup{occurrence: o}
		groups[o.Identity] = g
		aliases[formatMeetingOccurrence(o.Ref)] = g
		aliases[publicMeetingOccurrenceRef(o)] = g
		return g, nil
	}
	members := map[int64]bool{}
	explicit := map[int64]bool{}
	for _, id := range req.TranscriptIDs {
		members[id] = true
		explicit[id] = true
	}
	requested := map[string]linkOccurrence{}
	copyQueries := []meetingEventQuery{}
	for i, ref := range refs {
		o, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("resolve meeting occurrence: %w", err)
		}
		requested[req.OccurrenceRefs[i]] = o
		predicate, args := s.meetingIdentityPredicate(o.Identity)
		copyQueries = append(copyQueries, meetingEventQuery{predicate, args})
	}
	if err := s.preloadMeetingQueries(ctx, r, copyQueries, &budget); err != nil {
		return result, err
	}
	copyRefs := []OccurrenceRef{}
	for _, o := range requested {
		predicate, args := s.meetingIdentityPredicate(o.Identity)
		copyRefs = append(copyRefs, meetingIdentityCopyRefs(o.Identity, r.batch.events[meetingQueryKey(predicate, args)])...)
	}
	if err := s.preloadMeetingOccurrences(ctx, r, copyRefs, &budget); err != nil {
		return result, err
	}
	for value, o := range requested {
		g, err := groupFor(o)
		if err != nil {
			return result, fmt.Errorf("resolve meeting copies: %w", err)
		}
		aliases[value] = g
	}
	// Complete every returned group, including groups first reached through a
	// transcript or a suggestion. Discover each group once.
	includeCandidate := func(candidate meetingMemberCandidate) bool {
		return candidate.Exact || req.IncludeSuggestions && !sourcescope.Restricted(ctx)
	}
	discovered := map[*timelineMeetingGroup]bool{}
	processed := map[int64]bool{}
	for {
		groupCount := len(groups)
		requestedGroups := map[*timelineMeetingGroup]bool{}
		for _, g := range groups {
			requestedGroups[g] = true
		}
		aliasQueries := []meetingEventQuery{}
		for g := range requestedGroups {
			if discovered[g] {
				continue
			}
			predicate, args := s.meetingIdentityPredicate(g.occurrence.Identity)
			predicate, args = meetingIdentityAliasPredicate(g.occurrence, r.batch.events[meetingQueryKey(predicate, args)])
			if predicate != "" {
				aliasQueries = append(aliasQueries, meetingEventQuery{predicate, args})
			}
		}
		if err := s.preloadMeetingQueries(ctx, r, aliasQueries, &budget); err != nil {
			return result, err
		}
		memberQueries := []meetingEventQuery{}
		for g := range requestedGroups {
			if discovered[g] {
				continue
			}
			discovered[g] = true
			query, args, err := s.meetingIdentityMemberQuery(ctx, r, g.occurrence, &budget)
			if err != nil {
				return result, fmt.Errorf("discover meeting transcripts: %w", err)
			}
			memberQueries = append(memberQueries, meetingEventQuery{query, args})
		}
		for chunk := range slices.Chunk(memberQueries, 20) {
			terms := []string{}
			args := []any{}
			for _, q := range chunk {
				terms = append(terms, q.predicate)
				args = append(args, q.args...)
			}
			err := func() error {
				rows, err := r.QueryContext(ctx, strings.Join(terms, " UNION ALL "), args...)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						return err
					}
					members[id] = true
				}
				return errors.Join(rows.Err(), rows.Close())
			}()
			if err != nil {
				return result, err
			}
		}
		ids := make([]int64, 0, len(members))
		for id := range members {
			if !processed[id] {
				ids = append(ids, id)
				processed[id] = true
			}
		}
		if len(ids) == 0 {
			break
		}
		slices.Sort(ids)
		if err := s.preloadMeetingMembers(ctx, r, ids); err != nil {
			return result, err
		}
		candidateQueries := []meetingEventQuery{}
		for _, id := range ids {
			if !r.batch.visible[id] {
				continue
			}
			for _, d := range r.batch.decisions[id] {
				candidateQueries = append(candidateQueries, meetingEventQuery{"e.message_id = ?", []any{d.Ref.MessageID}})
			}
			predicate, args, err := s.automaticMeetingPredicate(ctx, r, r.batch.keys[id])
			if err != nil {
				return result, err
			}
			candidateQueries = append(candidateQueries, meetingEventQuery{predicate, args})
		}
		if err := s.preloadMeetingQueries(ctx, r, candidateQueries, &budget); err != nil {
			return result, err
		}
		candidateRefs := []OccurrenceRef{}
		for _, id := range ids {
			if !r.batch.visible[id] {
				continue
			}
			for _, d := range r.batch.decisions[id] {
				candidateRefs = append(candidateRefs, d.Ref)
			}
			key := r.batch.keys[id]
			predicate, args, err := s.automaticMeetingPredicate(ctx, r, key)
			if err != nil {
				return result, err
			}
			_, instance, _ := calrecur.ParseInstanceID(key.Value)
			for _, row := range r.batch.events[meetingQueryKey(predicate, args)] {
				ref := OccurrenceRef{MessageID: row.MessageID}
				if row.Event.Kind == meetingSeriesKind {
					if key.Kind == "google_event_id" {
						ref.Key = instance.Key()
					} else if key.ScheduledAt != nil {
						predicate, args := meetingSeriesExceptionsPredicate(r.dialect, row, *key.ScheduledAt, key.ScheduledAt.Add(time.Second))
						candidateQueries = append(candidateQueries, meetingEventQuery{predicate, args})
					}
				}
				candidateRefs = append(candidateRefs, ref)
			}
		}
		if err := s.preloadMeetingQueries(ctx, r, candidateQueries, &budget); err != nil {
			return result, err
		}
		if err := s.preloadMeetingOccurrences(ctx, r, candidateRefs, &budget); err != nil {
			return result, err
		}
		resolvedMembers := map[int64][]meetingMemberCandidate{}
		memberCopies := []meetingEventQuery{}
		for _, id := range ids {
			if !r.batch.visible[id] {
				continue
			}
			candidates, err := s.meetingMemberCandidates(ctx, r, id, &budget)
			if err != nil {
				return result, fmt.Errorf("resolve meeting transcript %d: %w", id, err)
			}
			if !explicit[id] {
				relevant := false
				if len(candidates) > 0 {
					for _, candidate := range candidates {
						if !includeCandidate(candidate) {
							continue
						}
						if g := groups[candidate.Identity]; g != nil && requestedGroups[g] {
							relevant = true
						}
					}
				}
				if !relevant {
					delete(processed, id)
					continue
				}
			}
			resolvedMembers[id] = candidates
			if len(candidates) > 0 {
				for _, candidate := range candidates {
					if !includeCandidate(candidate) {
						continue
					}
					predicate, args := s.meetingIdentityPredicate(candidate.Identity)
					memberCopies = append(memberCopies, meetingEventQuery{predicate, args})
				}
			}
		}
		if err := s.preloadMeetingQueries(ctx, r, memberCopies, &budget); err != nil {
			return result, err
		}
		copyRefs = nil
		for _, candidates := range resolvedMembers {
			for _, candidate := range candidates {
				if !includeCandidate(candidate) {
					continue
				}
				predicate, args := s.meetingIdentityPredicate(candidate.Identity)
				copyRefs = append(copyRefs, meetingIdentityCopyRefs(candidate.Identity, r.batch.events[meetingQueryKey(predicate, args)])...)
			}
		}
		if err := s.preloadMeetingOccurrences(ctx, r, copyRefs, &budget); err != nil {
			return result, err
		}
		for _, id := range ids {
			candidates, ok := resolvedMembers[id]
			if !ok {
				continue
			}
			info := MeetingIdentityInfo{Key: fmt.Sprintf("msg:%d", id)}
			for _, candidate := range candidates {
				if !includeCandidate(candidate) {
					continue
				}
				g, err := groupFor(candidate.linkOccurrence)
				if err != nil {
					return result, fmt.Errorf("resolve transcript occurrence: %w", err)
				}
				value := publicMeetingOccurrenceRef(g.occurrence)
				if candidate.Exact {
					info.ExactOccurrenceRefs = append(info.ExactOccurrenceRefs, value)
					g.linked = append(g.linked, id)
				} else {
					info.SuggestedOccurrenceRefs = append(info.SuggestedOccurrenceRefs, value)
				}
			}
			info.ExactOccurrenceRefs = sortedUniqueMeetingRefs(info.ExactOccurrenceRefs)
			info.SuggestedOccurrenceRefs = sortedUniqueMeetingRefs(info.SuggestedOccurrenceRefs)
			if len(info.ExactOccurrenceRefs) == 1 {
				info.OccurrenceRef = info.ExactOccurrenceRefs[0]
				info.Key = "occ:" + formatMeetingOccurrence(aliases[info.OccurrenceRef].occurrence.Ref)
			}
			result.ByTranscript[id] = info
		}
		if len(groups) == groupCount {
			break
		}
	}
	for value, g := range aliases {
		ref := publicMeetingOccurrenceRef(g.occurrence)
		slices.Sort(g.linked)
		g.linked = slices.Compact(g.linked)
		result.ByOccurrence[value] = MeetingIdentityInfo{Key: "occ:" + formatMeetingOccurrence(g.occurrence.Ref), OccurrenceRef: ref, LinkedTranscriptIDs: slices.Clone(g.linked)}
	}
	for id, info := range result.ByTranscript {
		if info.OccurrenceRef != "" {
			info.LinkedTranscriptIDs = slices.Clone(result.ByOccurrence[info.OccurrenceRef].LinkedTranscriptIDs)
			result.ByTranscript[id] = info
		}
	}
	return result, nil
}

// Public calendar readers address an exception by its message; meeting links
// retain the original series occurrence for counting and manual decisions.
func publicMeetingOccurrenceRef(o linkOccurrence) string {
	if o.Row.Event.Kind == meetingExceptionKind {
		return formatMeetingOccurrence(OccurrenceRef{MessageID: o.Row.MessageID})
	}
	return formatMeetingOccurrence(o.Ref)
}

func sortedUniqueMeetingRefs(refs []string) []string { slices.Sort(refs); return slices.Compact(refs) }

// ParseMeetingIdentityRef validates a public calendar occurrence reference.
func ParseMeetingIdentityRef(value string) (OccurrenceRef, error) {
	text, ok := strings.CutPrefix(value, "evt:")
	if !ok {
		return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence reference %q", value)
	}
	idText, key, _ := strings.Cut(text, "@")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 || id > 9007199254740991 {
		return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence reference %q", value)
	}
	if strings.Contains(text, "@") {
		if _, identity, ok := calrecur.ParseInstanceID("event_" + key); !ok || identity.Key() != key {
			return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence key %q", key)
		}
	}
	return OccurrenceRef{MessageID: id, Key: key}, nil
}
