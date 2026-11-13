package store

import (
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
		err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT m.deleted_at IS NOT NULL OR EXISTS (SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND NOT `+calendarEventVisibility("e", "m")+`), COALESCE((SELECT survivor_message_id FROM meeting_copy_survivors WHERE message_id=m.id AND m.delete_batch_id IS NOT NULL), (SELECT survivor_message_id FROM source_merge_messages WHERE message_id=m.id)) FROM messages m WHERE m.id=?`+scope, append([]any{id}, args...)...).Scan(&hidden, &next)
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

// meetingCopies finds the hidden copies that will return when dedup is undone.
func (s *Store) meetingCopies(ctx context.Context, id int64) ([]int64, error) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{id}, args...)
	rows, err := s.meetingSQLReader(ctx).QueryContext(ctx, `WITH RECURSIVE copies(id) AS (
 SELECT CAST(? AS BIGINT) UNION SELECT m.id FROM messages m JOIN copies c ON c.id=COALESCE((SELECT survivor_message_id FROM meeting_copy_survivors WHERE message_id=m.id AND m.delete_batch_id IS NOT NULL),(SELECT survivor_message_id FROM source_merge_messages WHERE message_id=m.id)) WHERE (m.deleted_at IS NOT NULL OR EXISTS (SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND NOT `+calendarEventVisibility("e", "m")+`))`+scope+`
 ) SELECT id FROM copies`, args...)
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
	return ids, errors.Join(rows.Err(), rows.Close())
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
	rows, err := s.meetingEventRows(ctx, `e.message_id = ?`, []any{id}, budget)
	if err != nil {
		return endpoint, err
	}
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
		if row.Event.Original != nil && row.Event.SeriesProviderID != "" {
			series, err := s.meetingEventRows(ctx, `m.source_id = ? AND e.provider_event_id = ? AND e.kind = 'series'`, []any{row.SourceID, row.Event.SeriesProviderID}, budget)
			if err != nil {
				return endpoint, err
			}
			if len(series) > 0 {
				endpoint.ID = series[0].MessageID
				endpoint.Key = row.Event.Original.Key()
			}
		}
	default:
		endpoint.Key = ""
	}
	return endpoint, nil
}

func (s *Store) meetingAutomaticRecordForEndpoint(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) (MeetingRecord, error) {
	ref, err := endpoint.meetingRef()
	if err != nil {
		return MeetingRecord{}, err
	}
	return s.meetingAutomaticRecordContext(ctx, ref, budget)
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
		ca, err := s.meetingOccurrenceKey(ctx, a, &budget)
		if err != nil {
			return s.clearStoredMeetingDecision(ctx, tx, a, b, decision, err, &result)
		}
		cb, err := s.meetingOccurrenceKey(ctx, b, &budget)
		if err != nil {
			return s.clearStoredMeetingDecision(ctx, tx, a, b, decision, err, &result)
		}
		for _, endpoint := range []meetingDecisionEndpoint{a, b} {
			if err := s.validateMeetingInput(ctx, endpoint, &budget); err != nil {
				return err
			}
		}
		if compareMeetingEndpoints(ca, cb) == 0 {
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
			record, err := s.meetingAutomaticRecordForEndpoint(ctx, endpoint, &budget)
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
			graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{ca, cb}, &budget)
			if err != nil {
				return err
			}
			root, links, _ := graph.reduce()
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
		for _, e := range []*meetingDecisionEndpoint{&a, &b} {
			if e.Kind == meetingRefEvent && e.Key == "" {
				rows, err := s.meetingEventRows(ctx, `e.message_id = ?`, []any{e.ID}, &budget)
				if err != nil {
					return err
				}
				if len(rows) > 0 && rows[0].Event.Kind == "single" && rows[0].Event.Start != nil {
					event := rows[0].Event
					e.Key = calrecur.Identity{Instant: *event.Start, Date: event.StartDate, AllDay: event.AllDay}.Key()
				}
			}
		}
		equivalent, err := s.equivalentMeetingDecisions(ctx, ca, cb, &budget)
		if err != nil {
			return err
		}
		if decision == "clear" {
			for _, edge := range equivalent {
				var ignored MeetingLinkDecisionResult
				if err := s.clearStoredMeetingDecision(ctx, tx, edge.a, edge.b, decision, sql.ErrNoRows, &ignored); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			if len(equivalent) == 0 {
				var ignored MeetingLinkDecisionResult
				err := s.clearStoredMeetingDecision(ctx, tx, a, b, decision, sql.ErrNoRows, &ignored)
				if err != nil && (!errors.Is(err, sql.ErrNoRows) || missing) {
					return err
				}
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
			graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{ca, cb}, &budget)
			if err != nil {
				return err
			}
			root, _, _ := graph.reduce()
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
			x, err := s.meetingOccurrenceKey(ctx, edge.a, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) {
				continue
			}
			if err != nil {
				return nil, err
			}
			y, err := s.meetingOccurrenceKey(ctx, edge.b, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if compareMeetingEndpoints(x, y) > 0 {
				x, y = y, x
			}
			if x == a && y == b {
				matches = append(matches, edge)
			}
		}
	}
	return matches, nil
}

func (s *Store) clearStoredMeetingDecision(ctx context.Context, tx *loggedTx, a, b meetingDecisionEndpoint, decision string, cause error, result *MeetingLinkDecisionResult) error {
	if decision != "clear" || (!errors.Is(cause, sql.ErrNoRows) && !errors.Is(cause, ErrMeetingOccurrenceRequired)) {
		return cause
	}
	for _, e := range []*meetingDecisionEndpoint{&a, &b} {
		id, ok, err := s.resolveMeetingMessage(ctx, e.ID)
		if err != nil {
			return err
		}
		if ok {
			e.ID = id
		}
	}
	leftIDs, err := s.meetingCopies(ctx, a.ID)
	if err != nil {
		return err
	}
	rightIDs, err := s.meetingCopies(ctx, b.ID)
	if err != nil {
		return err
	}
	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	var count int64
	for _, left := range leftIDs {
		for _, right := range rightIDs {
			x, y := a, b
			x.ID, y.ID = left, right
			if compareMeetingEndpoints(x, y) > 0 {
				x, y = y, x
			}
			args := []any{x.ID, x.Kind, x.Key, y.ID, y.Kind, y.Key}
			args = append(args, leftArgs...)
			args = append(args, rightArgs...)
			deleted, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id IN (SELECT d.id FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE d.left_message_id=? AND d.left_kind=? AND d.left_key=? AND d.right_message_id=? AND d.right_kind=? AND d.right_key=?`+leftScope+rightScope+`)`, args...)
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
	if count == 0 {
		return cause
	}
	*result = MeetingLinkDecisionResult{Left: a.ref(), Right: b.ref(), State: "automatic"}
	return nil
}

// deleteMeetingDecisionPair removes a saved pair so its replacement gets a newer
// id, which reads and cleanup use as its priority.
func deleteMeetingDecisionPair(ctx context.Context, tx *loggedTx, a, b meetingDecisionEndpoint) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE left_message_id=? AND left_kind=? AND left_key=? AND right_message_id=? AND right_kind=? AND right_key=?`, a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key)
	return err
}

// copyMeetingDecisionsTx carries choices to the copy retained by a merge.
func copyMeetingDecisionsTx(ctx context.Context, tx *loggedTx, fromIDs []int64, into int64) error {
	if len(fromIDs) == 0 {
		return nil
	}
	ids := make(map[int64]bool, len(fromIDs))
	args := make([]any, 0, len(fromIDs)*2)
	for _, id := range fromIDs {
		ids[id] = true
		args = append(args, id)
	}
	args = append(args, args...)
	marks := placeholders(len(fromIDs))
	rows, err := tx.QueryContext(ctx, `SELECT id,left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision FROM meeting_link_decisions WHERE left_message_id IN (`+marks+`) OR right_message_id IN (`+marks+`) ORDER BY id DESC`, args...)
	if err != nil {
		return err
	}
	var choices []meetingDecisionEdge
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.id, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.decision); err != nil {
			_ = rows.Close()
			return err
		}
		choices = append(choices, edge)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, edge := range choices {
		for _, left := range []bool{true, false} {
			a, b := edge.a, edge.b
			if left && ids[a.ID] {
				a.ID = into
			} else if !left && ids[b.ID] {
				b.ID = into
			} else {
				continue
			}
			if compareMeetingEndpoints(a, b) > 0 {
				a, b = b, a
			}
			if compareMeetingEndpoints(a, b) == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision) VALUES (?,?,?,?,?,?,?) ON CONFLICT(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key) DO NOTHING`, a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key, edge.decision); err != nil {
				return err
			}
		}
	}
	return nil
}
