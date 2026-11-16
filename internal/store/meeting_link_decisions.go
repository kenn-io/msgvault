package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingLinkDecisionResult reports the saved choice or restored automatic matching.
type MeetingLinkDecisionResult struct {
	Left  string `json:"left"`
	Right string `json:"right"`
	State string `json:"state"`
}

var ErrMeetingLinkConflict = errors.New("a saved choice conflicts with this request")
var ErrMeetingLinkSameCopy = errors.New("meeting references name the same copy")
var ErrInvalidMeetingLinkDecision = errors.New("invalid meeting decision")

type meetingDecisionEndpoint struct {
	ID        int64
	Kind, Key string
}

func (e meetingDecisionEndpoint) ref() string {
	if e.Kind == meetingRefEvent {
		ref, err := e.meetingRef()
		if err == nil {
			return formatMeetingOccurrence(ref.OccurrenceRef)
		}
	}
	return fmt.Sprintf("%s:%d", e.Kind, e.ID)
}

func endpointFromRef(ref MeetingLinkRef) meetingDecisionEndpoint {
	e := meetingDecisionEndpoint{ID: ref.MessageID, Kind: ref.Kind}
	if ref.Identity != (calrecur.Identity{}) {
		e.Key = ref.Identity.Key()
	}
	return e
}

func (e meetingDecisionEndpoint) meetingRef() (MeetingLinkRef, error) {
	ref := fmt.Sprintf("%s:%d", e.Kind, e.ID)
	if e.Key != "" {
		ref += "@" + e.Key
	}
	return ParseMeetingLinkRef(ref)
}

func compareMeetingEndpoints(a, b meetingDecisionEndpoint) int {
	if a.ID < b.ID {
		return -1
	}
	if a.ID > b.ID {
		return 1
	}
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return strings.Compare(a.Key, b.Key)
}

func validateMeetingLinkDecision(decision string) error {
	if decision != "link" && decision != "unlink" && decision != "clear" {
		return fmt.Errorf("%w: decision must be link, unlink, or clear", ErrInvalidMeetingLinkDecision)
	}
	return nil
}

// resolveMeetingMessage follows the survivor of each hidden message.
func (s *Store) resolveMeetingMessage(ctx context.Context, id int64) (int64, bool, error) {
	for range 100 {
		var hidden bool
		var next sql.NullInt64
		scope, args := sourceScopeClause(ctx, "m.source_id")
		err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT m.deleted_at IS NOT NULL OR EXISTS (SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND NOT `+calendarEventVisibility("e", "m")+`), `+meetingSuccessorSQL()+` FROM messages m WHERE m.id=?`+scope, append([]any{id}, args...)...).Scan(&hidden, &next)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if !hidden {
			return id, true, nil
		}
		if !next.Valid || next.Int64 == id {
			return 0, false, nil
		}
		id = next.Int64
	}
	return 0, false, errors.New("too many successive merges")
}

// validateMeetingInput checks a dated event reference before normalizing it.
func (s *Store) validateMeetingInput(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) error {
	if endpoint.Kind != meetingRefEvent || endpoint.Key == "" {
		return nil
	}
	if *budget <= 0 {
		return calendarRecurrenceBudgetError()
	}
	scope, args := sourceScopeClause(ctx, "m.source_id")
	var kind, date, originalDate string
	var allDay bool
	var start, original nullableTimestamp
	err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT e.kind,e.all_day,e.start_at,e.start_date,e.original_start_at,e.original_start_date FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE e.message_id=?`+scope, append([]any{endpoint.ID}, args...)...).Scan(&kind, &allDay, &start, &date, &original, &originalDate)
	if err != nil {
		return err
	}
	*budget--
	if kind == "single" && (!start.Valid || (calrecur.Identity{Instant: start.Time, Date: date, AllDay: allDay}).Key() != endpoint.Key) {
		return sql.ErrNoRows
	}
	if kind == calendarExceptionKind {
		identity := calrecur.Identity{Instant: original.Time, Date: originalDate, AllDay: originalDate != ""}
		if originalDate == "" && !original.Valid || identity.Key() != endpoint.Key {
			return sql.ErrNoRows
		}
	}
	return nil
}

// meetingOccurrenceKey identifies an occurrence from its own calendar row.
func (s *Store) meetingOccurrenceKey(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) (meetingDecisionEndpoint, error) {
	id, ok, err := s.resolveMeetingMessage(ctx, endpoint.ID)
	if err != nil {
		return endpoint, err
	}
	if !ok {
		return endpoint, sql.ErrNoRows
	}
	endpoint.ID = id
	if endpoint.Kind == meetingRefMessage {
		var exists bool
		err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=? AND message_type='meeting_transcript')`, id).Scan(&exists)
		if err != nil {
			return endpoint, err
		}
		if !exists {
			return endpoint, sql.ErrNoRows
		}
		endpoint.Key = ""
		return endpoint, nil
	}
	rows, err := queryCalendarEventRowsWithinBudget(ctx, s.meetingSQLReader(ctx), `e.message_id = ?`, []any{id}, *budget)
	if err != nil {
		return endpoint, err
	}
	*budget -= len(rows)
	if len(rows) == 0 {
		return endpoint, sql.ErrNoRows
	}
	row := rows[0]
	switch row.Event.Kind {
	case calendarSeriesKind:
		if endpoint.Key == "" {
			return endpoint, ErrMeetingOccurrenceRequired
		}
	case calendarExceptionKind:
		endpoint.Key = ""
		if row.Event.Original != nil {
			endpoint.Key = row.Event.Original.Key()
		}
		if row.Event.Original != nil && row.Event.SeriesProviderID != "" {
			seriesID, err := s.meetingOccurrenceSeries(ctx, id, row.SourceID, row.Event.SeriesProviderID, *row.Event.Original, budget)
			if err != nil {
				return endpoint, err
			}
			if seriesID != 0 {
				endpoint.ID = seriesID
			}
		}
	default:
		key := ""
		if row.Event.Start != nil {
			key = (calrecur.Identity{Instant: *row.Event.Start, Date: row.Event.StartDate, AllDay: row.Event.AllDay}).Key()
		}
		if endpoint.Key != "" && endpoint.Key != key {
			return endpoint, sql.ErrNoRows
		}
		endpoint.Key = key
	}
	return endpoint, nil
}

func (s *Store) meetingAutomaticRecordForEndpoint(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int, notesOnly bool) (MeetingRecord, error) {
	ref, err := endpoint.meetingRef()
	if err != nil {
		return MeetingRecord{}, err
	}
	var series bool
	if ref.Kind == meetingRefEvent && endpoint.Key != "" {
		rows, err := s.meetingEventRows(ctx, `e.message_id = ?`, []any{endpoint.ID}, budget)
		if err != nil {
			return MeetingRecord{}, err
		}
		if len(rows) > 0 && rows[0].Event.Kind != calendarSeriesKind {
			ref.Identity = calrecur.Identity{}
		} else if len(rows) > 0 {
			series = true
		}
	}
	record, err := s.meetingAutomaticRecord(ctx, ref, budget, notesOnly)
	if err != nil || !series || notesOnly || !slices.ContainsFunc(record.Links, func(link MeetingLink) bool { return link.MessageID == endpoint.ID }) {
		return record, err
	}
	var hasChoices bool
	if err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meeting_link_decisions WHERE (left_kind='evt' AND left_key=?) OR (right_kind='evt' AND right_key=?))`, endpoint.Key, endpoint.Key).Scan(&hasChoices); err != nil || !hasChoices {
		return record, err
	}
	edits, err := s.meetingOccurrenceEdits(ctx, endpoint, budget)
	if err != nil {
		return record, err
	}
	var replacements []MeetingLink
	for _, edit := range edits {
		editRef := MeetingLinkRef{Kind: meetingRefEvent, MessageID: edit.MessageID}
		copyRecord, err := s.meetingAutomaticRecord(ctx, editRef, budget, false)
		if err != nil {
			return record, err
		}
		for _, link := range copyRecord.Links {
			if link.MessageID == edit.MessageID {
				replacements = append(replacements, link)
				record.MessageIDs = append(record.MessageIDs, copyRecord.MessageIDs...)
				record.Unexpandable = append(record.Unexpandable, copyRecord.Unexpandable...)
			}
		}
	}
	if len(replacements) > 0 {
		record.Links = slices.DeleteFunc(record.Links, func(link MeetingLink) bool { return link.MessageID == endpoint.ID })
		record.Links = append(record.Links, replacements...)
	}
	return record, nil
}

// DecideMeetingLinkContext saves or clears a choice between two copies.
func (s *Store) DecideMeetingLinkContext(ctx context.Context, left, right MeetingLinkRef, decision string) (result MeetingLinkDecisionResult, err error) {
	if err := validateMeetingLinkDecision(decision); err != nil {
		return result, err
	}
	a, b := endpointFromRef(left), endpointFromRef(right)
	if compareMeetingEndpoints(a, b) > 0 {
		a, b = b, a
	}
	err = s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockIdentityMutationTxContext(ctx, tx) }, func(tx *loggedTx) error {
		if err := s.lockSyncMaintenanceGate(ctx, tx); err != nil {
			return err
		}
		for _, e := range []meetingDecisionEndpoint{a, b} {
			if err := s.lockMeetingEvidenceWith(ctx, tx, e.ID); err != nil {
				return err
			}
		}
		ctx := withWritableSnapshot(ctx, s.db.DB, tx)
		budget := calrecur.RequestReadBudget
		bareA := a.Kind == meetingRefEvent && a.Key == ""
		bareB := b.Kind == meetingRefEvent && b.Key == ""
		ca, err := s.meetingOccurrenceKey(ctx, a, &budget)
		if err != nil {
			return s.clearStoredMeetingDecision(ctx, tx, a, b, bareA, bareB, decision, err, &result)
		}
		cb, err := s.meetingOccurrenceKey(ctx, b, &budget)
		if err != nil {
			return s.clearStoredMeetingDecision(ctx, tx, a, b, bareA, bareB, decision, err, &result)
		}
		for _, endpoint := range []meetingDecisionEndpoint{a, b} {
			if err := s.validateMeetingInput(ctx, endpoint, &budget); err != nil {
				return err
			}
		}
		if decision != "clear" && compareMeetingEndpoints(ca, cb) == 0 {
			return ErrMeetingLinkSameCopy
		}
		if compareMeetingEndpoints(ca, cb) > 0 {
			ca, cb = cb, ca
		}
		missing := false
		for _, endpoint := range []meetingDecisionEndpoint{ca, cb} {
			if endpoint.Kind != meetingRefEvent {
				continue
			}
			record, err := s.meetingAutomaticRecordForEndpoint(ctx, endpoint, &budget, false)
			if errors.Is(err, sql.ErrNoRows) {
				missing = true
				continue
			}
			if err != nil {
				return err
			}
			if len(record.Links) == 0 {
				missing = true
			}
		}
		if missing && decision != "clear" {
			return sql.ErrNoRows
		}
		var active []meetingDecisionEdge
		if decision != "clear" {
			graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{ca, cb}, &budget, false)
			if err != nil {
				return err
			}
			root, links := graph.reduce()
			for _, link := range links {
				if root(link.a.ref()) == root(link.b.ref()) {
					active = append(active, link)
				}
			}
		}
		state := "automatic"
		switch decision {
		case "link":
			state = "link"
		case "unlink":
			state = "split"
		}
		a, b = ca, cb
		equivalent, err := s.equivalentMeetingDecisions(ctx, ca, cb, &budget)
		if err != nil {
			return err
		}
		if decision == "clear" {
			for _, edge := range equivalent {
				if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id=?`, edge.id); err != nil {
					return err
				}
			}
			var cause error
			if missing && len(equivalent) == 0 {
				cause = sql.ErrNoRows
			}
			var ignored MeetingLinkDecisionResult
			if err := s.clearStoredMeetingDecision(ctx, tx, a, b, true, true, decision, cause, &ignored); err != nil {
				return err
			}
			result = MeetingLinkDecisionResult{Left: ca.ref(), Right: cb.ref(), State: "automatic"}
			return nil
		}
		for _, edge := range equivalent {
			if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id=?`, edge.id); err != nil {
				return err
			}
		}
		for i, edge := range slices.Backward(active) {
			x, y := edge.a, edge.b
			if compareMeetingEndpoints(x, y) > 0 {
				x, y = y, x
			}
			if x == ca && y == cb {
				active = append(active[:i], active[i+1:]...)
			}
		}
		if err := deleteMeetingDecisionPair(ctx, tx, a, b); err != nil {
			return err
		}
		var insertedID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision) VALUES (?,?,?,?,?,?,?) RETURNING id`, a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key, state).Scan(&insertedID)
		if err != nil {
			return err
		}
		{
			budget = calrecur.RequestReadBudget
			graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{ca, cb}, &budget, false)
			if err != nil {
				return err
			}
			root, _ := graph.reduce()
			if (root(ca.ref()) == root(cb.ref())) != (state == "link") {
				for _, edge := range graph.candidates {
					if edge.id != insertedID && edge.decision == "split" && ((root(edge.a.ref()) == root(ca.ref()) && root(edge.b.ref()) == root(cb.ref())) || (root(edge.b.ref()) == root(ca.ref()) && root(edge.a.ref()) == root(cb.ref()))) {
						return fmt.Errorf("%w: %s %s", ErrMeetingLinkConflict, edge.a.ref(), edge.b.ref())
					}
				}
				return fmt.Errorf("%w: %s %s", ErrMeetingLinkConflict, ca.ref(), cb.ref())
			}
			for _, link := range active {
				if root(link.a.ref()) != root(link.b.ref()) {
					for _, edge := range graph.candidates {
						if edge.id != insertedID && edge.decision == "split" && ((root(edge.a.ref()) == root(link.a.ref()) && root(edge.b.ref()) == root(link.b.ref())) || (root(edge.b.ref()) == root(link.a.ref()) && root(edge.a.ref()) == root(link.b.ref()))) {
							return fmt.Errorf("%w: %s %s", ErrMeetingLinkConflict, edge.a.ref(), edge.b.ref())
						}
					}
					return fmt.Errorf("%w: %s %s", ErrMeetingLinkConflict, link.a.ref(), link.b.ref())
				}
			}
		}
		result = MeetingLinkDecisionResult{Left: ca.ref(), Right: cb.ref(), State: state}
		return nil
	})
	return result, err
}

// meetingPairKeys resolves both endpoints; ok is false once either no longer names an occurrence.
func (s *Store) meetingPairKeys(ctx context.Context, a, b meetingDecisionEndpoint, budget *int) (x, y meetingDecisionEndpoint, ok bool, err error) {
	x, y, ok, err = s.meetingPairEndpoints(ctx, a, b, budget)
	return x, y, ok && x != y, err
}

func (s *Store) meetingPairEndpoints(ctx context.Context, a, b meetingDecisionEndpoint, budget *int) (x, y meetingDecisionEndpoint, ok bool, err error) {
	for _, endpoint := range []meetingDecisionEndpoint{a, b} {
		if endpoint.Kind != meetingRefEvent || endpoint.Key != "" {
			continue
		}
		var kind string
		var start nullableTimestamp
		var date string
		err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT kind,start_at,start_date FROM calendar_events WHERE message_id=?`, endpoint.ID).Scan(&kind, &start, &date)
		if errors.Is(err, sql.ErrNoRows) {
			return x, y, false, nil
		}
		if err != nil {
			return x, y, false, err
		}
		if kind == "single" && (start.Valid || date != "") {
			return x, y, false, nil
		}
	}
	if x, err = s.meetingOccurrenceKey(ctx, a, budget); err == nil {
		y, err = s.meetingOccurrenceKey(ctx, b, budget)
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) {
		return x, y, false, nil
	}
	return x, y, err == nil, err
}

// equivalentMeetingDecisions finds saved pairs that resolve to the requested pair.
func (s *Store) equivalentMeetingDecisions(ctx context.Context, a, b meetingDecisionEndpoint, budget *int) ([]meetingDecisionEdge, error) {
	seen := map[int64]bool{}
	var matches []meetingDecisionEdge
	for _, endpoint := range []meetingDecisionEndpoint{a, b} {
		edges, err := s.meetingDecisionEdges(ctx, endpoint, budget)
		if err != nil {
			return nil, err
		}
		for _, edge := range edges {
			if seen[edge.id] {
				continue
			}
			seen[edge.id] = true
			x, y, ok, err := s.meetingPairKeys(ctx, edge.a, edge.b, budget)
			if err != nil {
				return nil, err
			}
			if compareMeetingEndpoints(x, y) > 0 {
				x, y = y, x
			}
			if ok && x == a && y == b {
				matches = append(matches, edge)
			}
		}
	}
	return matches, nil
}

func (s *Store) clearStoredMeetingDecision(ctx context.Context, tx *loggedTx, a, b meetingDecisionEndpoint, matchSingleA, matchSingleB bool, decision string, cause error, result *MeetingLinkDecisionResult) error {
	if decision != "clear" || (cause != nil && !errors.Is(cause, sql.ErrNoRows) && !errors.Is(cause, ErrMeetingOccurrenceRequired)) {
		return cause
	}
	type side struct {
		endpoint meetingDecisionEndpoint
		copies   []int64
		single   bool
	}
	prepare := func(endpoint meetingDecisionEndpoint, matchSingle bool) (side, error) {
		id, ok, err := s.resolveMeetingMessage(ctx, endpoint.ID)
		if err != nil {
			return side{}, err
		}
		if !ok {
			return side{}, sql.ErrNoRows
		}
		endpoint.ID = id
		copies, err := s.meetingCopies(ctx, id)
		if err != nil {
			return side{}, err
		}
		prepared := side{endpoint: endpoint, copies: copies}
		if endpoint.Kind == meetingRefEvent {
			var kind string
			if err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT kind FROM calendar_events WHERE message_id=?`, id).Scan(&kind); err != nil {
				return side{}, err
			}
			prepared.single = matchSingle && kind == "single"
		}
		return prepared, nil
	}
	left, err := prepare(a, matchSingleA)
	if err != nil {
		return err
	}
	right, err := prepare(b, matchSingleB)
	if err != nil {
		return err
	}
	match := func(column string, count int) string {
		return column + `_message_id IN (` + placeholders(count) + `) AND ` + column + `_kind=? AND (` + column + `_key=? OR (? AND EXISTS (SELECT 1 FROM calendar_events WHERE message_id=` + column + `_message_id AND kind='single')))`
	}
	appendSide := func(args []any, s side, ids []int64) []any {
		for _, id := range ids {
			args = append(args, id)
		}
		return append(args, s.endpoint.Kind, s.endpoint.Key, s.single)
	}
	var count int64
	for leftIDs := range slices.Chunk(left.copies, 100) {
		for rightIDs := range slices.Chunk(right.copies, 100) {
			query := `DELETE FROM meeting_link_decisions WHERE (` + match("left", len(leftIDs)) + ` AND ` + match("right", len(rightIDs)) + `) OR (` + match("left", len(rightIDs)) + ` AND ` + match("right", len(leftIDs)) + `)`
			args := appendSide(nil, left, leftIDs)
			args = appendSide(args, right, rightIDs)
			args = appendSide(args, right, rightIDs)
			args = appendSide(args, left, leftIDs)
			deleted, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			n, err := deleted.RowsAffected()
			if err != nil {
				return err
			}
			count += n
		}
	}
	if count == 0 && cause != nil {
		return cause
	}
	*result = MeetingLinkDecisionResult{Left: left.endpoint.ref(), Right: right.endpoint.ref(), State: "automatic"}
	return nil
}

// deleteMeetingDecisionPair removes a saved pair so its replacement gets a newer
// id, which reads and cleanup use as its priority.
func deleteMeetingDecisionPair(ctx context.Context, tx *loggedTx, a, b meetingDecisionEndpoint) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE left_message_id=? AND left_kind=? AND left_key=? AND right_message_id=? AND right_kind=? AND right_key=?`, a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key)
	return err
}

// preservePurgedMeetingDecisions moves choices off rows that are about to disappear.
func (s *Store) preservePurgedMeetingDecisions(ctx context.Context, tx *loggedTx, predicate string, args ...any) error {
	ctx = withWritableSnapshot(ctx, s.db.DB, tx)
	return s.preservePurgedMeetingDecisionsWith(ctx, tx, predicate, args...)
}

func (s *Store) preservePurgedMeetingDecisionsWith(ctx context.Context, tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, predicate string, args ...any) error {
	reader := s.meetingSQLReader(ctx)
	var hasChoices bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meeting_link_decisions)`).Scan(&hasChoices); err != nil {
		return err
	}
	if !hasChoices {
		return nil
	}
	var hasMeetings bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE (`+predicate+`) AND message_type IN ('calendar_event', 'meeting_transcript'))`, args...).Scan(&hasMeetings); err != nil {
		return err
	}
	if !hasMeetings {
		return nil
	}
	// Traverse the copy graph once for all rows being deleted. UNION also
	// prevents a damaged cycle from making cleanup loop forever.
	visibilityAfterPurge := calendarEventVisibilityExcluding("e", "m", " AND om.id NOT IN (SELECT id FROM targets)")
	rows, err := reader.QueryContext(ctx, `WITH RECURSIVE
 targets(id) AS (SELECT id FROM messages WHERE (`+predicate+`) AND message_type IN ('calendar_event', 'meeting_transcript')),
 `+meetingCopyGraphSQL(`SELECT 0,id FROM targets`, "", "", meetingSeriesLinksSQL(""))+`
 SELECT c.id, l.parent, EXISTS(SELECT 1 FROM targets t WHERE t.id=c.id),
 EXISTS(SELECT 1 FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE e.message_id=c.id AND m.deleted_at IS NULL AND m.id NOT IN (SELECT id FROM targets) AND `+visibilityAfterPurge+`),
 sl.parent, sl.original_start_at, sl.original_start_date
 FROM copies c LEFT JOIN links l ON l.id=c.id LEFT JOIN series_links sl ON sl.id=c.id`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	purged := map[int64]bool{}
	children := map[int64][]int64{}
	parents := map[int64]int64{}
	type occurrenceCopy struct {
		id  int64
		key string
	}
	occurrences := map[int64][]occurrenceCopy{}
	var ids []int64
	var visibleAfter []int64
	for rows.Next() {
		var id int64
		var parent sql.NullInt64
		var series sql.NullInt64
		var original nullableTimestamp
		var date sql.NullString
		var deleted, visible bool
		if err := rows.Scan(&id, &parent, &deleted, &visible, &series, &original, &date); err != nil {
			return errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
		purged[id] = deleted
		if visible {
			visibleAfter = append(visibleAfter, id)
		}
		if parent.Valid {
			children[parent.Int64] = append(children[parent.Int64], id)
			parents[id] = parent.Int64
		}
		if series.Valid && (original.Valid || date.String != "") {
			key := (calrecur.Identity{Instant: original.Time, Date: date.String, AllDay: date.String != ""}).Key()
			occurrences[id] = append(occurrences[id], occurrenceCopy{series.Int64, key})
			occurrences[series.Int64] = append(occurrences[series.Int64], occurrenceCopy{id, key})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for id := range children {
		slices.Sort(children[id])
		children[id] = slices.Compact(children[id])
	}
	for id := range occurrences {
		slices.SortFunc(occurrences[id], func(a, b occurrenceCopy) int { return cmp.Compare(a.id, b.id) })
	}
	preferred := map[int64]int64{}
	root := func(id int64) int64 {
		seen := map[int64]bool{}
		for parents[id] != 0 && !seen[id] {
			seen[id] = true
			id = parents[id]
		}
		return id
	}
	for _, id := range visibleAfter {
		if parent := root(id); purged[parent] {
			preferred[parent] = id
		}
	}
	replacement := func(id int64, key string) (int64, bool) {
		seen := map[int64]bool{id: true}
		for parent := parents[id]; parent != 0 && !seen[parent]; parent = parents[parent] {
			if !purged[parent] {
				return parent, true
			}
			seen[parent] = true
		}
		ancestor := root(id)
		if copyID, ok := preferred[ancestor]; ok {
			return copyID, true
		}
		queue := []int64{ancestor}
		seen = map[int64]bool{}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if seen[current] {
				continue
			}
			seen[current] = true
			if !purged[current] {
				return current, true
			}
			queue = append(queue, children[current]...)
		}
		if key != "" {
			queue = []int64{id}
			seen = map[int64]bool{}
			for len(queue) > 0 {
				current := queue[0]
				queue = queue[1:]
				if seen[current] {
					continue
				}
				seen[current] = true
				if !purged[current] {
					return current, true
				}
				queue = append(queue, children[current]...)
				if parent := parents[current]; parent != 0 {
					queue = append(queue, parent)
				}
				for _, occurrence := range occurrences[current] {
					if occurrence.key == key {
						queue = append(queue, occurrence.id)
					}
				}
			}
		}
		return 0, false
	}
	var edges []meetingDecisionEdge
	seenEdges := map[int64]bool{}
	for batch := range slices.Chunk(ids, 200) {
		params := make([]any, 0, len(batch)*2)
		for range 2 {
			for _, id := range batch {
				params = append(params, id)
			}
		}
		marks := placeholders(len(batch))
		if err := func() error {
			rows, err := reader.QueryContext(ctx, `SELECT id,left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision FROM meeting_link_decisions WHERE left_message_id IN (`+marks+`) OR right_message_id IN (`+marks+`)`, params...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var edge meetingDecisionEdge
				if err := rows.Scan(&edge.id, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.decision); err != nil {
					return err
				}
				if !seenEdges[edge.id] {
					seenEdges[edge.id] = true
					edges = append(edges, edge)
				}
			}
			return errors.Join(rows.Err(), rows.Close())
		}(); err != nil {
			return err
		}
	}
	if len(edges) == 0 {
		return nil
	}
	slices.SortFunc(edges, func(a, b meetingDecisionEdge) int { return cmp.Compare(b.id, a.id) })
	// Keep a choice on a surviving copy when possible. All choices resolving
	// to the same deleted visible copy use the same replacement for priority.
	for _, edge := range edges {
		for _, endpoint := range []meetingDecisionEndpoint{edge.a, edge.b} {
			deleted, affected := purged[endpoint.ID]
			if !affected || deleted {
				continue
			}
			id := root(endpoint.ID)
			if purged[id] && preferred[id] == 0 {
				preferred[id] = endpoint.ID
			}
		}
	}
	type pair struct{ a, b meetingDecisionEndpoint }
	type resolved struct {
		edge  meetingDecisionEdge
		pair  pair
		valid bool
	}
	resolvedEdges := make([]resolved, 0, len(edges))
	for _, edge := range edges {
		if !purged[edge.a.ID] && !purged[edge.b.ID] {
			continue
		}
		// A rescheduled occurrence can be inactive while its saved choice survives.
		a, b, ok := edge.a, edge.b, true
		if purged[edge.a.ID] {
			var found bool
			a.ID, found = replacement(edge.a.ID, edge.a.Key)
			ok = ok && found
		}
		if purged[edge.b.ID] {
			var found bool
			b.ID, found = replacement(edge.b.ID, edge.b.Key)
			ok = ok && found
		}
		if compareMeetingEndpoints(a, b) > 0 {
			a, b = b, a
		}
		p := pair{a, b}
		valid := ok && a != b
		resolvedEdges = append(resolvedEdges, resolved{edge, p, valid})
	}
	// Keep surviving hidden copies connected when their intermediate survivor
	// is removed. Their choices remain on the rows where they were saved.
	type redirect struct{ from, to int64 }
	var redirects []redirect
	for id, deleted := range purged {
		if !deleted {
			continue
		}
		next := parents[id]
		visited := map[int64]bool{id: true}
		for next != 0 && purged[next] && !visited[next] {
			visited[next] = true
			next = parents[next]
		}
		if next == 0 || visited[next] {
			var ok bool
			next, ok = replacement(id, "")
			if !ok {
				continue
			}
			// A descendant may point through another surviving copy. Route to
			// that nearer copy so that the redirect cannot create a cycle.
			for parent := parents[next]; parent != 0 && parent != id && !visited[parent]; parent = parents[parent] {
				visited[parent] = true
				if !purged[parent] {
					next = parent
				}
			}
		}
		redirects = append(redirects, redirect{id, next})
	}
	for batch := range slices.Chunk(redirects, 200) {
		values := make([]string, len(batch))
		params := make([]any, 0, len(batch)*2+len(args))
		for i, item := range batch {
			values[i] = "(CAST(? AS BIGINT), CAST(? AS BIGINT))"
			params = append(params, item.from, item.to)
		}
		params = append(params, args...)
		for _, table := range []string{"meeting_copy_survivors", "source_merge_messages"} {
			query := `WITH redirects(id, target) AS (VALUES ` + strings.Join(values, ",") + `)
 UPDATE ` + table + ` SET survivor_message_id=(SELECT target FROM redirects WHERE id=survivor_message_id)
 WHERE survivor_message_id IN (SELECT id FROM redirects)
 AND message_id <> (SELECT target FROM redirects WHERE id=survivor_message_id)
 AND message_id NOT IN (SELECT id FROM messages WHERE ` + predicate + `)`
			if _, err := tx.ExecContext(ctx, s.dialect.Rebind(query), params...); err != nil {
				return err
			}
		}
	}
	seen := map[pair]int64{}
	for _, edge := range edges {
		if !purged[edge.a.ID] && !purged[edge.b.ID] {
			seen[pair{edge.a, edge.b}] = edge.id
		}
	}
	var updates []resolved
	for _, item := range resolvedEdges {
		previous := seen[item.pair]
		if !item.valid || previous > item.edge.id {
			if _, err := tx.ExecContext(ctx, s.dialect.Rebind(`DELETE FROM meeting_link_decisions WHERE id=?`), item.edge.id); err != nil {
				return err
			}
			continue
		}
		if previous != 0 {
			// The unique pair already has an older choice; keep the newer id.
			if _, err := tx.ExecContext(ctx, s.dialect.Rebind(`DELETE FROM meeting_link_decisions WHERE id=?`), previous); err != nil {
				return err
			}
		}
		seen[item.pair] = item.edge.id
		updates = append(updates, item)
	}
	for _, item := range updates {
		p := item.pair
		if _, err := tx.ExecContext(ctx, s.dialect.Rebind(`UPDATE meeting_link_decisions SET left_message_id=?,left_kind=?,left_key=?,right_message_id=?,right_kind=?,right_key=? WHERE id=?`), p.a.ID, p.a.Kind, p.a.Key, p.b.ID, p.b.Kind, p.b.Key, item.edge.id); err != nil {
			return err
		}
	}
	return nil
}
