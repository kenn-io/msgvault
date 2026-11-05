package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// ResolveMeetingIdentitiesContext resolves a bounded batch in one read snapshot.
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
	eventIDs := make([]int64, 0, len(req.OccurrenceRefs))
	for _, value := range req.OccurrenceRefs {
		ref, err := ParseMeetingIdentityRef(value)
		if err != nil {
			return result, err
		}
		refs = append(refs, ref)
		eventIDs = append(eventIDs, ref.MessageID)
	}
	if err := s.preloadMeetingEvents(ctx, r, eventIDs, &budget); err != nil {
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
	for value, o := range requested {
		g, err := groupFor(o)
		if err != nil {
			return result, fmt.Errorf("resolve meeting copies: %w", err)
		}
		aliases[value] = g
	}
	// Complete every returned group, including groups first reached through a
	// transcript or a suggestion. Discover each group once.
	discovered := map[*timelineMeetingGroup]bool{}
	processed := map[int64]bool{}
	for {
		groupCount := len(groups)
		requestedGroups := map[*timelineMeetingGroup]bool{}
		for _, g := range groups {
			requestedGroups[g] = true
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
			rows, err := r.QueryContext(ctx, strings.Join(terms, " UNION ALL "), args...)
			if err != nil {
				return result, err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return result, err
				}
				members[id] = true
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return result, err
			}
			if closeErr != nil {
				return result, closeErr
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
			// Manual decisions replace automatic evidence, including absent targets.
			manual := false
			for _, d := range r.batch.decisions[id] {
				predicate, args := s.meetingIdentityPredicate(d.Identity)
				candidateQueries = append(candidateQueries, meetingEventQuery{predicate, args})
				manual = manual || d.Decision == "link"
			}
			if !manual {
				predicate, args, err := s.automaticMeetingPredicate(ctx, r, r.batch.keys[id])
				if err != nil {
					return result, err
				}
				candidateQueries = append(candidateQueries, meetingEventQuery{predicate, args})
			}
		}
		if err := s.preloadMeetingQueries(ctx, r, candidateQueries, &budget); err != nil {
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
			exact := len(candidates) == 1 || len(candidates) > 0 && candidates[0].Provenance == "manual"
			if !explicit[id] {
				relevant := false
				if exact || req.IncludeSuggestions && !sourcescope.Restricted(ctx) {
					for _, candidate := range candidates {
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
			if exact || req.IncludeSuggestions && !sourcescope.Restricted(ctx) {
				for _, candidate := range candidates {
					predicate, args := s.meetingIdentityPredicate(candidate.Identity)
					memberCopies = append(memberCopies, meetingEventQuery{predicate, args})
				}
			}
		}
		if err := s.preloadMeetingQueries(ctx, r, memberCopies, &budget); err != nil {
			return result, err
		}
		for _, id := range ids {
			candidates, ok := resolvedMembers[id]
			if !ok {
				continue
			}
			info := MeetingIdentityInfo{Key: fmt.Sprintf("msg:%d", id)}
			exact := len(candidates) == 1 || len(candidates) > 0 && candidates[0].Provenance == "manual"
			for _, candidate := range candidates {
				if !exact && (!req.IncludeSuggestions || sourcescope.Restricted(ctx)) {
					continue
				}
				g, err := groupFor(candidate.linkOccurrence)
				if err != nil {
					return result, fmt.Errorf("resolve transcript occurrence: %w", err)
				}
				value := publicMeetingOccurrenceRef(g.occurrence)
				if exact {
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
		if _, identity, ok := calrecur.ParseInstanceID("event_" + key); !ok || key == "" || identity.Key() != key {
			return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence key %q", key)
		}
	}
	return OccurrenceRef{MessageID: id, Key: key}, nil
}

// Discover only keys or decisions that could resolve to this occurrence. The
// forward resolver applies splits, ambiguity and provider ordering afterwards.
func (s *Store) meetingIdentityMemberQuery(ctx context.Context, r reboundReader, o linkOccurrence, budget *int) (string, []any, error) {
	copies, err := s.meetingIdentityRows(ctx, r, o.Identity, budget)
	if err != nil {
		return "", nil, err
	}
	// A manual decision may retain the ID of a UID-less orphan saved before
	// its master arrived. Include source-local exception targets, then let the
	// forward resolver validate their current identity, splits and visibility.
	if o.Identity.OccurrenceKey != "" {
		_, original, ok := calrecur.ParseInstanceID("event_" + o.Identity.OccurrenceKey)
		if o.Row.Event.Original != nil {
			original, ok = *o.Row.Event.Original, true
		}
		if ok {
			column, value := "e.original_start_at", any(original.Instant)
			if original.AllDay {
				column, value = "e.original_start_date", original.Date
			}
			var terms []string
			var aliasArgs []any
			for _, copy := range copies {
				provider := copy.Event.ProviderEventID
				if copy.Event.Kind == meetingExceptionKind {
					provider = copy.Event.SeriesProviderID
				}
				if provider == "" {
					continue
				}
				terms = append(terms, `(m.source_id=? AND e.series_provider_id=?)`)
				aliasArgs = append(aliasArgs, copy.SourceID, provider)
			}
			if len(terms) > 0 {
				aliasArgs = append(aliasArgs, value)
				aliases, err := meetingEventRows(ctx, r, `e.kind='exception' AND (`+strings.Join(terms, " OR ")+`) AND `+column+`=?`, aliasArgs, budget)
				if err != nil {
					return "", nil, err
				}
				copies = append(copies, aliases...)
			}
		}
	}
	clauses := []string{`(d.target_ical_uid=? AND d.target_ical_uid<>'' AND d.occurrence_key=?)`}
	args := []any{o.Identity.ICalUID, o.Identity.OccurrenceKey}
	keyClauses, keyArgs, err := meetingIdentityUIDMemberClauses(ctx, r, o, copies, budget)
	if err != nil {
		return "", nil, err
	}
	for _, copy := range copies {
		clauses = append(clauses, `(d.target_message_id=? AND (d.occurrence_key=? OR d.occurrence_key=''))`)
		args = append(args, copy.MessageID, o.Identity.OccurrenceKey)
		if provider := copy.Event.ProviderEventID; provider != "" {
			keyClauses = append(keyClauses, `(k.kind='google_event_id' AND k.value IN (?,?))`)
			instance := provider
			if o.Identity.OccurrenceKey != "" {
				instance += "_" + o.Identity.OccurrenceKey
			}
			keyArgs = append(keyArgs, provider, instance)
		}
	}
	query := `SELECT m.id FROM messages m WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL
 AND (EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id AND (` + strings.Join(clauses, " OR ") + `))
 OR EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND (` + strings.Join(keyClauses, " OR ") + `)))`
	args = append(args, keyArgs...)
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(args, scopeArgs...)
	return query + scope, args, nil
}

// Each calendar copy can move the same occurrence to a different start.
// Discovery admits all of those starts; forward resolution decides the link.
func meetingIdentityUIDMemberClauses(ctx context.Context, r reboundReader, o linkOccurrence, rows []CalendarEventRow, budget *int) ([]string, []any, error) {
	clause := `(k.kind='ical_uid' AND k.value=? AND k.value<>'')`
	if o.Identity.OccurrenceKey == "" {
		return []string{clause}, []any{o.Identity.ICalUID}, nil
	}
	copies, err := meetingIdentityCopyOccurrences(ctx, r, o.Identity, rows, budget)
	if err != nil {
		return nil, nil, err
	}
	starts := []any{o.Start}
	for _, copy := range copies {
		starts = append(starts, copy.Start)
	}
	clause = `(k.kind='ical_uid' AND k.value=? AND k.value<>'' AND k.scheduled_at IN (` + placeholders(len(starts)) + `))`
	return []string{clause}, append([]any{o.Identity.ICalUID}, starts...), nil
}
