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
		err := s.db.QueryRowContext(ctx, `SELECT m.deleted_at IS NOT NULL, COALESCE((SELECT survivor_message_id FROM meeting_copy_survivors WHERE message_id=m.id AND m.delete_batch_id IS NOT NULL), (SELECT survivor_message_id FROM source_merge_messages WHERE message_id=m.id)) FROM messages m WHERE m.id=?`+scope, append([]any{id}, args...)...).Scan(&hidden, &next)
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
	return 0, false, errors.New("meeting survivor chain exceeds limit")
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
		err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=? AND message_type='meeting_transcript')`, id).Scan(&exists)
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
			root, links := graph.reduce()
			for _, link := range links {
				if root(link.a.ref()) == root(link.b.ref()) {
					active = append(active, link)
				}
			}
		}
		matched := false
		seen := map[int64]bool{}
		for _, e := range []meetingDecisionEndpoint{ca, cb} {
			choices, err := s.meetingDecisionEdges(ctx, e, &budget)
			if err != nil {
				return err
			}
			for _, choice := range choices {
				if seen[choice.id] {
					continue
				}
				seen[choice.id] = true
				x, err := s.meetingOccurrenceKey(ctx, choice.a, &budget)
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) {
					continue
				}
				if err != nil {
					return err
				}
				y, err := s.meetingOccurrenceKey(ctx, choice.b, &budget)
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) {
					continue
				}
				if err != nil {
					return err
				}
				if compareMeetingEndpoints(x, y) > 0 {
					x, y = y, x
				}
				if compareMeetingEndpoints(x, ca) != 0 || compareMeetingEndpoints(y, cb) != 0 {
					continue
				}
				matched = true
				for i, a := range slices.Backward(active) {
					if compareMeetingEndpoints(a.a, x) == 0 && compareMeetingEndpoints(a.b, y) == 0 {
						active = append(active[:i], active[i+1:]...)
					}
				}
				if !choice.direct && (decision == "link" && choice.decision == "link" || decision == "unlink" && choice.decision == "split") {
					continue
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id=?`, choice.id); err != nil {
					return err
				}
			}
		}
		if missing && !matched {
			return sql.ErrNoRows
		}
		state := "automatic"
		if decision != "clear" {
			state = "link"
			if decision == "unlink" {
				state = "split"
			}
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
			var insertedID int64
			err = tx.QueryRowContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision) VALUES (?,?,?,?,?,?,?) RETURNING id`, a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key, state).Scan(&insertedID)
			if err != nil {
				return err
			}
			budget = calrecur.RequestReadBudget
			graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{ca, cb}, &budget)
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

func (s *Store) clearStoredMeetingDecision(ctx context.Context, tx *loggedTx, a, b meetingDecisionEndpoint, decision string, cause error, result *MeetingLinkDecisionResult) error {
	if decision != "clear" || (!errors.Is(cause, sql.ErrNoRows) && !errors.Is(cause, ErrMeetingOccurrenceRequired)) {
		return cause
	}
	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	args := []any{a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key}
	args = append(args, leftArgs...)
	args = append(args, rightArgs...)
	deleted, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id IN (SELECT d.id FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE d.left_message_id=? AND d.left_kind=? AND d.left_key=? AND d.right_message_id=? AND d.right_kind=? AND d.right_key=?`+leftScope+rightScope+`)`, args...)
	if err != nil {
		return err
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return cause
	}
	*result = MeetingLinkDecisionResult{Left: a.ref(), Right: b.ref(), State: "automatic"}
	return nil
}
