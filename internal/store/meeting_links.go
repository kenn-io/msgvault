package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

const meetingLinkSeriesKind = "series"
const meetingLinkCalendarType = "calendar_event"
const meetingLinkCancelledStatus = "cancelled"

// EffectiveLink is computed from visible exact evidence or a user decision.
type EffectiveLink struct {
	MemberMessageID int64
	Target          OccurrenceRef
	Basis, Origin   string
}

type linkOccurrence struct {
	Ref         OccurrenceRef
	Row         CalendarEventRow
	Original    calrecur.Identity
	Start, End  time.Time
	BoundsKnown bool
}

func linkMemberVisible(ctx context.Context, r rowReader, scope SourceScope, id int64) (bool, error) {
	clause, args := linkScopeSQL(scope)
	var found int64
	err := r.QueryRowContext(ctx, `SELECT m.id FROM messages m WHERE m.id = ? AND m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL`+clause, append([]any{id}, args...)...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func memberLinkKeys(ctx context.Context, r rowReader, id int64) ([]meetingLinkKey, error) {
	rows, err := r.QueryContext(ctx, `SELECT kind,value,base,instance_key,scheduled_at FROM meeting_link_keys WHERE message_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []meetingLinkKey
	for rows.Next() {
		var k meetingLinkKey
		var at sql.NullTime
		if err := rows.Scan(&k.Kind, &k.Value, &k.Base, &k.InstanceKey, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			k.ScheduledAt = &at.Time
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Scope precedes LIMIT. A 201st candidate proves ambiguity; it is never
// silently omitted and cannot yield a false unique automatic match.
func boundedLinkEventRows(ctx context.Context, r rowReader, scope SourceScope, predicate string, args []any) ([]CalendarEventRow, bool, error) {
	ids, broad, err := boundedLinkEventIDs(ctx, r, scope, predicate, args)
	if err != nil || broad {
		return nil, broad, err
	}
	result, err := calendarEventRowsByMessageIDs(ctx, r, scope, ids)
	return result, false, err
}

func linkKeyCandidates(ctx context.Context, r rowReader, scope SourceScope, key meetingLinkKey) ([]CalendarEventRow, bool, error) {
	predicate, args := linkKeyPredicate(key)
	rows, broad, err := boundedLinkEventRows(ctx, r, scope, predicate, args)
	if err != nil || broad {
		return nil, broad, err
	}
	// Sparse exceptions may have no UID/join key. Include them and the master
	// of a directly matched exception, but preserve the per-key total cap.
	seen := map[int64]bool{}
	for _, row := range rows {
		seen[row.MessageID] = true
	}
	original := slices.Clone(rows)
	type seriesKey struct {
		source                    int64
		provider, namespace, base string
	}
	fetched := map[seriesKey]bool{}
	for _, row := range original {
		if row.Event.Kind == "single" {
			continue
		}
		base := row.Event.SeriesProviderID
		if row.Event.Kind == meetingLinkSeriesKind {
			base = row.Event.ProviderEventID
		}
		group := seriesKey{row.SourceID, row.Event.Provider, row.Event.Namespace, base}
		if fetched[group] {
			continue
		}
		fetched[group] = true
		var related []CalendarEventRow
		var tooMany bool
		if key.Kind == "google_event_id" && (key.Base != "" || row.Event.Original != nil) {
			var id calrecur.Identity
			if key.Base != "" {
				var parseErr error
				id, parseErr = calrecur.ParseKey(key.InstanceKey)
				if parseErr != nil {
					return nil, false, parseErr
				}
			} else {
				id = *row.Event.Original
			}
			master := row
			master.Event.ProviderEventID = base
			related, tooMany, err = linkOccurrenceExceptionRows(ctx, r, scope, master, id)
			if err == nil && !tooMany && row.Event.Kind == "exception" {
				var parents []CalendarEventRow
				parents, tooMany, err = boundedLinkEventRows(ctx, r, scope, `m.source_id = ? AND e.provider = ? AND e.namespace = ? AND e.kind = 'series' AND e.provider_event_id = ?`, []any{row.SourceID, row.Event.Provider, row.Event.Namespace, base})
				related = append(related, parents...)
			}
		} else {
			related, tooMany, err = boundedLinkEventRows(ctx, r, scope, `m.source_id = ? AND e.provider = ? AND e.namespace = ? AND (e.provider_event_id = ? OR e.series_provider_id = ?)`, []any{row.SourceID, row.Event.Provider, row.Event.Namespace, base, base})
		}
		if err != nil || tooMany {
			return nil, tooMany, err
		}
		for _, extra := range related {
			if !seen[extra.MessageID] {
				seen[extra.MessageID] = true
				rows = append(rows, extra)
			}
		}
		if len(rows) > meetingLinkCandidateLimit {
			return nil, true, nil
		}
	}
	return rows, false, nil
}

func linkSeriesSet(ctx context.Context, r rowReader, row CalendarEventRow) (calrecur.Set, time.Duration, int, error) {
	zone, err := currentCalendarZone(ctx, r, row.SourceID)
	if err != nil {
		return calrecur.Set{}, 0, 0, err
	}
	e := row.Event
	if !e.AllDay && e.EventTimeZone != "" {
		if loc, err := time.LoadLocation(e.EventTimeZone); err == nil {
			zone = loc
		}
	}
	var start time.Time
	if e.Start != nil {
		start = *e.Start
	}
	set, err := calrecur.ParseSet(start, e.AllDay, e.StartDate, zone, e.Recurrence)
	if err != nil {
		return set, 0, 0, err
	}
	var duration time.Duration
	days := 0
	if e.Start != nil && e.End != nil {
		duration = e.End.Sub(*e.Start)
	}
	if e.AllDay {
		from, err := time.Parse("2006-01-02", e.StartDate)
		if err != nil {
			return set, 0, 0, fmt.Errorf("parse all-day meeting bounds: %w", err)
		}
		to, err := time.Parse("2006-01-02", e.EndDate)
		if err != nil {
			return set, 0, 0, fmt.Errorf("parse all-day meeting bounds: %w", err)
		}
		days = int(to.Sub(from) / (24 * time.Hour))
	}
	return set, duration, days, nil
}

func linkExceptions(master CalendarEventRow, rows []CalendarEventRow) []calrecur.Exception {
	var out []calrecur.Exception
	for _, row := range rows {
		e := row.Event
		if e.Kind == "exception" && e.Original != nil && row.SourceID == master.SourceID && e.Provider == master.Event.Provider && e.Namespace == master.Event.Namespace && e.SeriesProviderID == master.Event.ProviderEventID {
			out = append(out, calrecur.Exception{Original: *e.Original, Start: e.Start, End: e.End, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled", Ref: row.MessageID})
		}
	}
	return out
}

func standaloneLinkOccurrence(ctx context.Context, r rowReader, row CalendarEventRow) (linkOccurrence, error) {
	e := row.Event
	o := linkOccurrence{Ref: OccurrenceRef{MessageID: row.MessageID}, Row: row, BoundsKnown: e.HasBounds}
	if e.Start != nil {
		o.Start = *e.Start
	}
	if e.End != nil {
		o.End = *e.End
	}
	if e.Original != nil {
		o.Original = *e.Original
		if e.Start == nil {
			o.Start = e.Original.Instant
			o.End = o.Start
		}
	}
	if e.AllDay {
		zone, err := currentCalendarZone(ctx, r, row.SourceID)
		if err != nil {
			return o, err
		}
		date := e.StartDate
		if date == "" && e.Original != nil {
			date = e.Original.Date
		}
		var errStart error
		o.Start, _, errStart = calrecur.ResolveCivilDateBoundary(date, zone)
		if errStart != nil {
			return o, fmt.Errorf("parse all-day meeting bounds: %w", errStart)
		}
		if e.EndDate != "" {
			o.End, _, errStart = calrecur.ResolveCivilDateBoundary(e.EndDate, zone)
		} else {
			o.End = o.Start
		}
		if errStart != nil {
			return o, fmt.Errorf("parse all-day meeting bounds: %w", errStart)
		}
	}
	return o, nil
}

func occurrenceLogical(o linkOccurrence) string {
	if o.Row.Event.ICalUID != "" {
		key := "ical:" + o.Row.Event.ICalUID
		identity := o.Ref.Key
		if identity == "" && o.Row.Event.Original != nil {
			identity = o.Row.Event.Original.Key()
		}
		if identity != "" {
			key += "@" + identity
		}
		return key
	}
	return FormatOccurrenceRef(o.Ref)
}

func resolveLinkOccurrence(ctx context.Context, r rowReader, scope SourceScope, ref OccurrenceRef, budget *int) (linkOccurrence, error) {
	return readLinkOccurrence(ctx, r, scope, ref, budget, false)
}

// Suggestions exclude cancellation before expansion. Exact links and user
// decisions can still refer to cancelled historical occurrences.
func readLinkOccurrence(ctx context.Context, r rowReader, scope SourceScope, ref OccurrenceRef, budget *int, hideCancelled bool) (linkOccurrence, error) {
	if err := ctx.Err(); err != nil {
		return linkOccurrence{}, err
	}
	resolved, err := resolveOccurrenceRefAlias(ctx, r, scope, ref)
	if err != nil {
		return linkOccurrence{}, err
	}
	rows, err := calendarEventRowsByMessageIDs(ctx, r, scope, []int64{resolved.MessageID})
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, sql.ErrNoRows
	}
	if err := spendLinkBudget(ctx, budget, 1); err != nil {
		return linkOccurrence{}, err
	}
	row := rows[0]
	if resolved.Key == "" {
		if row.Event.Kind == meetingLinkSeriesKind {
			return linkOccurrence{}, fmt.Errorf("series target requires an occurrence key: %w", sql.ErrNoRows)
		}
		return standaloneLinkOccurrence(ctx, r, row)
	}
	if row.Event.Kind != meetingLinkSeriesKind {
		return linkOccurrence{}, sql.ErrNoRows
	}
	id, err := calrecur.ParseKey(resolved.Key)
	if err != nil {
		return linkOccurrence{}, err
	}
	if hideCancelled && row.Event.Status == meetingLinkCancelledStatus {
		return linkOccurrence{Ref: resolved, Row: row, Original: id}, nil
	}
	related, broad, err := linkOccurrenceExceptionRows(ctx, r, scope, row, id)
	if err != nil {
		return linkOccurrence{}, err
	}
	if broad {
		return linkOccurrence{}, ErrQueryTooBroad
	}
	if hideCancelled {
		effective := effectiveLinkRow(row, id, related)
		if effective.Event.Status == meetingLinkCancelledStatus {
			return linkOccurrence{Ref: resolved, Row: effective, Original: id}, nil
		}
	}
	set, duration, days, err := linkSeriesSet(ctx, r, row)
	if err != nil {
		return linkOccurrence{}, err
	}
	exc, err := linkReadExceptions(ctx, r, row, related)
	if err != nil {
		return linkOccurrence{}, err
	}
	originalAt := id.Instant
	if id.AllDay {
		originalAt, _, err = calrecur.ResolveCivilDateBoundary(id.Date, set.Zone)
		if err != nil {
			return linkOccurrence{}, fmt.Errorf("parse occurrence civil date: %w", err)
		}
	}
	allowance, err := reserveLinkRecurrence(ctx, set, originalAt, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	exists, err := calrecur.Contains(ctx, set, exc, id, allowance)
	if err != nil {
		return linkOccurrence{}, linkRecurrenceError(err)
	}
	if !exists {
		return linkOccurrence{}, sql.ErrNoRows
	}
	// Expand around the effective time of this particular identity, including a move.
	at := id.Instant
	if id.AllDay {
		at, _, err = calrecur.ResolveCivilDateBoundary(id.Date, set.Zone)
		if err != nil {
			return linkOccurrence{}, fmt.Errorf("parse occurrence civil date: %w", err)
		}
	}
	for _, e := range exc {
		if e.Original.Key() != id.Key() {
			continue
		}
		if e.Start != nil {
			at = *e.Start
		}
		if e.StartDate != "" || e.EndDate != "" {
			date := e.StartDate
			if date == "" {
				date = at.In(e.Zone).Format(time.DateOnly)
			}
			at, _, err = calrecur.ResolveCivilDateBoundary(date, e.Zone)
			if err != nil {
				return linkOccurrence{}, fmt.Errorf("parse occurrence civil date: %w", err)
			}
		}
	}

	allowance, err = reserveLinkRecurrence(ctx, set, at.Add(24*time.Hour), budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	occurrences, err := calrecur.Expand(ctx, set, duration, days, exc, at.Add(-24*time.Hour), at.Add(24*time.Hour), allowance)
	if err != nil {
		return linkOccurrence{}, linkRecurrenceError(err)
	}
	for _, o := range occurrences {
		if o.Original.Key() == id.Key() {
			return linkOccurrence{Ref: resolved, Row: effectiveLinkRow(row, id, related), Original: id, Start: o.Start, End: o.End, BoundsKnown: o.BoundsKnown}, nil
		}
	}
	return linkOccurrence{}, sql.ErrNoRows
}

func linkRecurrenceError(err error) error {
	if errors.Is(err, calrecur.ErrBudgetExceeded) {
		return fmt.Errorf("%w: %w", ErrQueryTooBroad, err)
	}
	return err
}

func autoLinkOccurrences(ctx context.Context, r rowReader, scope SourceScope, key meetingLinkKey, rows []CalendarEventRow, budget *int) ([]linkOccurrence, string, error) {
	var out []linkOccurrence
	basis := key.Kind
	seen := map[string]bool{}
	add := func(o linkOccurrence) {
		ref := FormatOccurrenceRef(o.Ref)
		if !seen[ref] {
			seen[ref] = true
			out = append(out, o)
		}
	}
	for _, row := range rows {
		if err := spendLinkBudget(ctx, budget, 1); err != nil {
			return nil, basis, err
		}
		e := row.Event
		direct := key.Kind == "google_event_id" && e.Provider == "google" && e.ProviderEventID == key.Value
		if key.Kind == "google_event_id" {
			if direct && e.Kind != meetingLinkSeriesKind {
				o, err := resolveLinkOccurrence(ctx, r, scope, OccurrenceRef{MessageID: row.MessageID}, budget)
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
					continue
				}
				if err != nil {
					return nil, basis, err
				}
				basis = "provider_event_id"
				add(o)
			} else if e.Kind == "exception" && e.Provider == "google" && key.Base != "" && e.SeriesProviderID == key.Base && e.Original != nil && e.Original.Key() == key.InstanceKey {
				o, err := resolveLinkOccurrence(ctx, r, scope, OccurrenceRef{MessageID: row.MessageID}, budget)
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
					continue
				}
				if err != nil {
					return nil, basis, err
				}
				basis = "instance_id"
				add(o)
			} else if e.Kind == meetingLinkSeriesKind && e.Provider == "google" && key.Base != "" && e.ProviderEventID == key.Base {
				o, err := resolveLinkOccurrence(ctx, r, scope, OccurrenceRef{MessageID: row.MessageID, Key: key.InstanceKey}, budget)
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
					continue
				}
				if err != nil {
					return nil, basis, err
				}
				basis = "instance_id"
				add(o)
			}
			continue
		}
		if key.Kind == "ical_uid" && e.ICalUID != key.Value || key.Kind == "join_key" && e.JoinKey != key.Value {
			continue
		}
		var candidates []linkOccurrence
		if e.Kind == meetingLinkSeriesKind {
			if key.ScheduledAt == nil {
				continue
			}
			set, duration, days, err := linkSeriesSet(ctx, r, row)
			if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, basis, err
			}
			allowance, err := reserveLinkRecurrence(ctx, set, key.ScheduledAt.Add(24*time.Hour), budget)
			if err != nil {
				return nil, basis, err
			}
			exceptions, err := linkReadExceptions(ctx, r, row, rows)
			if err != nil {
				return nil, basis, err
			}
			expanded, err := calrecur.Expand(ctx, set, duration, days, exceptions, key.ScheduledAt.Add(-24*time.Hour), key.ScheduledAt.Add(24*time.Hour), allowance)
			if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, basis, linkRecurrenceError(err)
			}
			for _, o := range expanded {
				candidates = append(candidates, linkOccurrence{Ref: OccurrenceRef{MessageID: row.MessageID, Key: o.Original.Key()}, Row: effectiveLinkRow(row, o.Original, rows), Original: o.Original, Start: o.Start, End: o.End, BoundsKnown: o.BoundsKnown})
			}
		} else {
			o, err := resolveLinkOccurrence(ctx, r, scope, OccurrenceRef{MessageID: row.MessageID}, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, basis, err
			}
			candidates = append(candidates, o)
		}
		for _, o := range candidates {
			if key.Kind == "join_key" && o.Row.Event.JoinKey != key.Value {
				continue
			}
			if key.Kind == "ical_uid" && e.Kind == "single" {
				add(o)
				continue
			}
			if key.ScheduledAt == nil {
				continue
			}
			delta := key.ScheduledAt.Sub(o.Start)
			if key.Kind == "ical_uid" && delta >= -time.Minute && delta <= time.Minute || key.Kind == "join_key" && delta >= -10*time.Minute && delta < 10*time.Minute {
				add(o)
			}
		}
	}
	return out, basis, nil
}

type meetingLinkDecision struct {
	Ref, Logical, Decision string
	CreatedAt              time.Time
}

type linkDecisionIndex struct {
	newestSplitByLogical map[string]time.Time
	splitByRef           map[OccurrenceRef]struct{}
	splitLogicals        map[string]struct{}
}

func visibleLinkDecisions(ctx context.Context, r rowReader, scope SourceScope, memberID int64, budget *int) ([]meetingLinkDecision, []linkOccurrence, error) {
	rows, err := r.QueryContext(ctx, `SELECT target_ref,target_logical,decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? ORDER BY target_ref`, memberID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var decisions []meetingLinkDecision
	for rows.Next() {
		var d meetingLinkDecision
		if err := rows.Scan(&d.Ref, &d.Logical, &d.Decision, &d.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		decisions = append(decisions, d)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	var visible []meetingLinkDecision
	var targets []linkOccurrence
	for _, d := range decisions {
		kind, ref, _, err := ParseMeetingRef(d.Ref)
		if err != nil || kind != "evt" {
			continue
		}
		target, err := resolveLinkOccurrence(ctx, r, scope, ref, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		visible = append(visible, d)
		targets = append(targets, target)
	}
	return visible, targets, nil
}

func indexLinkDecisions(ctx context.Context, decisions []meetingLinkDecision, targets []linkOccurrence, budget *int) (linkDecisionIndex, error) {
	if err := spendLinkBudget(ctx, budget, len(decisions)); err != nil {
		return linkDecisionIndex{}, err
	}
	index := linkDecisionIndex{
		newestSplitByLogical: make(map[string]time.Time),
		splitByRef:           make(map[OccurrenceRef]struct{}),
		splitLogicals:        make(map[string]struct{}),
	}
	for i, decision := range decisions {
		if decision.Decision != "split" {
			continue
		}
		target := targets[i]
		logical := occurrenceLogical(target)
		if created, ok := index.newestSplitByLogical[logical]; !ok || decision.CreatedAt.After(created) {
			index.newestSplitByLogical[logical] = decision.CreatedAt
		}
		index.splitByRef[target.Ref] = struct{}{}
		index.splitLogicals[logical] = struct{}{}
		if decision.Logical != "" {
			index.splitLogicals[decision.Logical] = struct{}{}
		}
	}
	return index, nil
}

func linksForMember(ctx context.Context, r rowReader, scope SourceScope, memberID int64, budget *int) ([]EffectiveLink, error) {
	if err := spendLinkBudget(ctx, budget, 1); err != nil {
		return nil, err
	}
	visible, err := linkMemberVisible(ctx, r, scope, memberID)
	if err != nil || !visible {
		return nil, err
	}
	decisions, targets, err := visibleLinkDecisions(ctx, r, scope, memberID, budget)
	if err != nil {
		return nil, err
	}
	decisionIndex, err := indexLinkDecisions(ctx, decisions, targets, budget)
	if err != nil {
		return nil, err
	}
	var out []EffectiveLink
	seen := map[string]bool{}
	add := func(o linkOccurrence, basis, origin string) {
		ref := FormatOccurrenceRef(o.Ref)
		if !seen[ref] {
			seen[ref] = true
			out = append(out, EffectiveLink{MemberMessageID: memberID, Target: o.Ref, Basis: basis, Origin: origin})
		}
	}
	for i, d := range decisions {
		if d.Decision != "link" {
			continue
		}
		logical := occurrenceLogical(targets[i])
		if splitAt, hasSplit := decisionIndex.newestSplitByLogical[logical]; !hasSplit || !splitAt.After(d.CreatedAt) {
			add(targets[i], "user", "user")
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	keys, err := memberLinkKeys(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		rows, broad, err := linkKeyCandidates(ctx, r, scope, key)
		if err != nil {
			return nil, err
		}
		if broad {
			continue
		}
		matches, basis, err := autoLinkOccurrences(ctx, r, scope, key, rows, budget)
		if err != nil {
			return nil, err
		}
		if len(matches) > 1 {
			logical := occurrenceLogical(matches[0])
			// A ref without a UID can never prove that two rows are copies.
			ambiguous := matches[0].Row.Event.ICalUID == ""
			for _, o := range matches[1:] {
				if o.Row.Event.ICalUID == "" || occurrenceLogical(o) != logical {
					ambiguous = true
				}
			}
			if ambiguous {
				continue
			}
		}
		for _, o := range matches {
			logical := occurrenceLogical(o)
			_, exactSplit := decisionIndex.splitByRef[o.Ref]
			_, logicalSplit := decisionIndex.splitLogicals[logical]
			split := exactSplit || logicalSplit
			if !split {
				add(o, basis, "auto")
			}
		}
	}
	slices.SortFunc(out, func(a, b EffectiveLink) int {
		return strings.Compare(FormatOccurrenceRef(a.Target), FormatOccurrenceRef(b.Target))
	})
	return out, nil
}

func logicalMeetingKey(ctx context.Context, r rowReader, scope SourceScope, memberID int64, budget *int) (string, error) {
	fallback := fmt.Sprintf("msg:%d", memberID)
	links, err := linksForMember(ctx, r, scope, memberID, budget)
	if err != nil {
		return "", err
	}
	var key string
	for _, link := range links {
		o, err := resolveLinkOccurrence(ctx, r, scope, link.Target, budget)
		if err != nil {
			return "", err
		}
		logical := occurrenceLogical(o)
		if key != "" && key != logical {
			return fallback, nil
		}
		key = logical
	}
	if key == "" {
		return fallback, nil
	}
	return key, nil
}

// Only indexed key/decision candidates enter this lookup. Verification uses
// the forward function so user overrides and splits agree in both directions.
func membersForOccurrence(ctx context.Context, r rowReader, scope SourceScope, occ OccurrenceRef, budget *int) ([]EffectiveLink, error) {
	target, err := resolveLinkOccurrence(ctx, r, scope, occ, budget)
	if err != nil {
		return nil, err
	}
	e := target.Row.Event
	ids := map[int64]bool{}
	clause, scopeArgs := linkScopeSQL(scope)
	// Direct exception IDs are candidates alongside master IDs and parsed bases.
	var related []CalendarEventRow
	if e.Kind == meetingLinkSeriesKind {
		var broad bool
		related, broad, err = linkOccurrenceExceptionRows(ctx, r, scope, target.Row, target.Original)
		if err != nil {
			return nil, err
		}
		if broad {
			return nil, ErrQueryTooBroad
		}
	}
	providerValues := make([]any, 0, len(related)+1)
	providerMarks := make([]string, 0, len(related)+1)
	if e.Kind != meetingLinkSeriesKind || target.Ref.Key == "" {
		providerValues = append(providerValues, e.ProviderEventID)
		providerMarks = append(providerMarks, "?")
	}
	for _, row := range related {
		if row.Event.Original != nil && row.Event.Original.Key() == target.Ref.Key {
			providerValues = append(providerValues, row.Event.ProviderEventID)
			providerMarks = append(providerMarks, "?")
		}
	}
	googleBase, instanceKey := e.ProviderEventID, target.Ref.Key
	if e.Kind == "exception" && e.Original != nil {
		googleBase = e.SeriesProviderID
		if instanceKey == "" {
			instanceKey = e.Original.Key()
		}
	}
	googleValueMatch := "1=0"
	if len(providerMarks) > 0 {
		googleValueMatch = "k.value IN (" + strings.Join(providerMarks, ",") + ")"
	}
	searchStart := target.Start.UTC()
	keyArgs := append([]any{e.Provider}, providerValues...)
	keyArgs = append(keyArgs, googleBase, instanceKey, e.ICalUID, e.Kind, searchStart.Add(-time.Minute), searchStart.Add(time.Minute), e.JoinKey, searchStart.Add(-10*time.Minute), searchStart.Add(10*time.Minute))
	keyRows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.deleted_at IS NULL AND m.message_type = 'meeting_transcript' AND m.id IN (SELECT k.message_id FROM meeting_link_keys k WHERE ((k.kind = 'google_event_id' AND ? = 'google' AND (`+googleValueMatch+` OR (k.base = ? AND k.instance_key = ?))) OR (k.kind = 'ical_uid' AND k.value = ? AND k.value <> '' AND (? = 'single' OR (k.scheduled_at >= ? AND k.scheduled_at <= ?))) OR (k.kind = 'join_key' AND k.value = ? AND k.value <> '' AND k.scheduled_at >= ? AND k.scheduled_at < ?)))`+clause+` ORDER BY m.id LIMIT 201`, append(keyArgs, scopeArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = keyRows.Close() }()
	for keyRows.Next() {
		var id int64
		if err := keyRows.Scan(&id); err != nil {
			_ = keyRows.Close()
			return nil, err
		}
		ids[id] = true
	}
	err = keyRows.Err()
	_ = keyRows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) > meetingLinkCandidateLimit {
		return nil, ErrQueryTooBroad
	}
	refs := []any{FormatOccurrenceRef(target.Ref)}
	marks := []string{"?"}
	for _, row := range related {
		if row.Event.Original != nil && row.Event.Original.Key() == target.Ref.Key {
			refs = append(refs, FormatOccurrenceRef(OccurrenceRef{MessageID: row.MessageID}))
			marks = append(marks, "?")
		}
	}
	decisionRows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.deleted_at IS NULL AND m.message_type = 'meeting_transcript' AND m.id IN (SELECT d.member_message_id FROM meeting_link_decisions d WHERE d.decision = 'link' AND d.target_ref IN (`+strings.Join(marks, ",")+`))`+clause+` ORDER BY m.id LIMIT 201`, append(refs, scopeArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = decisionRows.Close() }()
	for decisionRows.Next() {
		var id int64
		if err := decisionRows.Scan(&id); err != nil {
			_ = decisionRows.Close()
			return nil, err
		}
		ids[id] = true
	}
	err = decisionRows.Err()
	_ = decisionRows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) > meetingLinkCandidateLimit {
		return nil, ErrQueryTooBroad
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	var out []EffectiveLink
	for _, id := range ordered {
		links, err := linksForMember(ctx, r, scope, id, budget)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			if link.Target == target.Ref {
				out = append(out, link)
			}
		}
	}
	return out, nil
}

// An exception changes effective evidence while retaining its master's identity.
func effectiveLinkRow(master CalendarEventRow, id calrecur.Identity, rows []CalendarEventRow) CalendarEventRow {
	for _, row := range rows {
		e := row.Event
		if e.Kind == "exception" && e.Original != nil && e.Original.Key() == id.Key() && row.SourceID == master.SourceID && e.Provider == master.Event.Provider && e.Namespace == master.Event.Namespace && e.SeriesProviderID == master.Event.ProviderEventID {
			if e.StartDate != "" || e.EndDate != "" {
				master.Event.AllDay = true
			} else if e.Start != nil || e.End != nil {
				master.Event.AllDay = false
			}
			if e.Status == meetingLinkCancelledStatus {
				master.Event.Status = meetingLinkCancelledStatus
			}
			if e.HasBounds || e.JoinURL != "" || e.JoinKey != "" {
				master.Event.JoinURL = e.JoinURL
				master.Event.JoinKey = e.JoinKey
			}
			if row.Subject != "" {
				master.Subject = row.Subject
			}
			if e.HasBounds || len(e.Attendees) > 0 {
				master.Event.Attendees = e.Attendees
			}
		}
	}
	return master
}

// Qualified targets need only the exceptions of their original identity. Other
// occurrences cannot change this target's existence, move, or alias refs.
func linkOccurrenceExceptionRows(ctx context.Context, r rowReader, scope SourceScope, master CalendarEventRow, id calrecur.Identity) ([]CalendarEventRow, bool, error) {
	predicate := `e.kind = 'exception' AND m.source_id = ? AND e.provider = ? AND e.namespace = ? AND e.series_provider_id = ?`
	args := []any{master.SourceID, master.Event.Provider, master.Event.Namespace, master.Event.ProviderEventID}
	original, identityArgs := linkOriginalIdentityPredicate(id)
	predicate += ` AND ` + original
	args = append(args, identityArgs...)
	return boundedLinkEventRows(ctx, r, scope, predicate, args)
}
