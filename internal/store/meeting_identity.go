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
	r := s.meetingReader(ctx)
	groups := map[meetingIdentity]*timelineMeetingGroup{}
	aliases := map[string]*timelineMeetingGroup{}
	// Resolve copies through the same representative selection as meeting links.
	groupFor := func(o linkOccurrence) (*timelineMeetingGroup, error) {
		if g := groups[o.Identity]; g != nil {
			return g, nil
		}
		budget := calrecur.RequestReadBudget
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
	for _, value := range req.OccurrenceRefs {
		ref, err := parseMeetingIdentityRef(value)
		if err != nil {
			return result, err
		}
		budget := calrecur.RequestReadBudget
		o, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("resolve meeting occurrence: %w", err)
		}
		g, err := groupFor(o)
		if err != nil {
			return result, fmt.Errorf("resolve meeting copies: %w", err)
		}
		aliases[value] = g
	}
	// Reverse discovery is deliberately limited to requested occurrences, with no
	// activity-window predicate: a recording may arrive long after its meeting.
	requestedGroups := map[*timelineMeetingGroup]bool{}
	for _, value := range req.OccurrenceRefs {
		if g := aliases[value]; g != nil {
			requestedGroups[g] = true
		}
	}
	for g := range requestedGroups {
		ids, err := s.meetingIdentityMembers(ctx, r, g.occurrence)
		if err != nil {
			return result, fmt.Errorf("discover meeting transcripts: %w", err)
		}
		for _, id := range ids {
			members[id] = true
		}
	}
	ids := make([]int64, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		visible, err := visibleMeetingTranscript(ctx, r, id)
		if err != nil {
			return result, err
		}
		if !visible {
			continue
		}
		info := MeetingIdentityInfo{Key: fmt.Sprintf("msg:%d", id)}
		budget := calrecur.RequestReadBudget
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
				continue
			}
		}
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

func parseMeetingIdentityRef(value string) (OccurrenceRef, error) {
	text, ok := strings.CutPrefix(value, "evt:")
	if !ok {
		return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence reference %q", value)
	}
	idText, key, _ := strings.Cut(text, "@")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence reference %q", value)
	}
	if strings.Contains(text, "@") {
		if _, _, ok := calrecur.ParseInstanceID("event_" + key); !ok {
			return OccurrenceRef{}, fmt.Errorf("invalid meeting occurrence key %q", key)
		}
	}
	return OccurrenceRef{MessageID: id, Key: key}, nil
}

func visibleMeetingTranscript(ctx context.Context, r reboundReader, id int64) (bool, error) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{id}, args...)
	var found int
	err := r.QueryRowContext(ctx, `SELECT 1 FROM messages m WHERE m.id=? AND m.message_type='meeting_transcript' AND m.deleted_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id=m.id)`+scope, args...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Discover only keys or decisions that could resolve to this occurrence. The
// forward resolver applies splits, ambiguity and provider ordering afterwards.
func (s *Store) meetingIdentityMembers(ctx context.Context, r reboundReader, o linkOccurrence) ([]int64, error) {
	budget := calrecur.RequestReadBudget
	copies, err := s.meetingIdentityRows(ctx, r, o.Identity, &budget)
	if err != nil {
		return nil, err
	}
	// A manual decision may retain the ID of a UID-less orphan saved before
	// its master arrived. Include source-local exception targets, then let the
	// forward resolver validate their current identity, splits and visibility.
	if o.Identity.OccurrenceKey != "" {
		_, original, ok := calrecur.ParseInstanceID("event_" + o.Identity.OccurrenceKey)
		if ok {
			column, value := "e.original_start_at", any(original.Instant)
			if original.AllDay {
				column, value = "e.original_start_date", original.Date
			}
			var terms []string
			var aliasArgs []any
			for _, copy := range copies {
				if copy.Event.Kind != meetingSeriesKind && copy.Event.Kind != meetingSingleKind {
					continue
				}
				terms = append(terms, `(m.source_id=? AND e.series_provider_id=?)`)
				aliasArgs = append(aliasArgs, copy.SourceID, copy.Event.ProviderEventID)
			}
			if len(terms) > 0 {
				aliasArgs = append(aliasArgs, value)
				aliases, err := meetingEventRows(ctx, r, `e.kind='exception' AND (`+strings.Join(terms, " OR ")+`) AND `+column+`=?`, aliasArgs, &budget)
				if err != nil {
					return nil, err
				}
				copies = append(copies, aliases...)
			}
		}
	}
	clauses := []string{`(d.target_ical_uid=? AND d.target_ical_uid<>'' AND d.occurrence_key=?)`}
	args := []any{o.Identity.ICalUID, o.Identity.OccurrenceKey}
	keyClauses := []string{`(k.kind='ical_uid' AND k.value=? AND k.value<>'')`}
	keyArgs := []any{o.Identity.ICalUID}
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
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id=m.id)
 AND (EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id AND (` + strings.Join(clauses, " OR ") + `))
 OR EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND (` + strings.Join(keyClauses, " OR ") + `)))`
	args = append(args, keyArgs...)
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(args, scopeArgs...)
	rows, err := r.QueryContext(ctx, query+scope+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
