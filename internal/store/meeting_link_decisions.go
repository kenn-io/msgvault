package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
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

type meetingDecisionEndpoint struct {
	ID                int64
	Kind, Key, Anchor string
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

// ValidateMeetingLinkDecision checks the choice and its references.
func ValidateMeetingLinkDecision(left, right MeetingLinkRef, decision string) error {
	if decision != "link" && decision != "unlink" && decision != "clear" {
		return errors.New("decision must be link, unlink, or clear")
	}
	a, b := endpointFromRef(left), endpointFromRef(right)
	if a.ID <= 0 || b.ID <= 0 || (a.Kind != meetingRefMessage && a.Kind != meetingRefEvent) || (b.Kind != meetingRefMessage && b.Kind != meetingRefEvent) || compareMeetingEndpoints(a, b) == 0 {
		return errors.New("invalid meeting decision endpoints")
	}
	return nil
}

// DecideMeetingLinkContext saves or clears a choice between two copies.
func (s *Store) DecideMeetingLinkContext(ctx context.Context, left, right MeetingLinkRef, decision string) (result MeetingLinkDecisionResult, err error) {
	if err := ValidateMeetingLinkDecision(left, right, decision); err != nil {
		return result, err
	}
	a, b := endpointFromRef(left), endpointFromRef(right)
	if compareMeetingEndpoints(a, b) > 0 {
		a, b = b, a
	}
	err = s.withTxOptionsContext(ctx, nil, func(tx *loggedTx) error {
		if err := s.lockSyncMaintenanceGate(ctx, tx); err != nil {
			return err
		}
		// Queue choice writers before reading their shared graph. The maintenance
		// gate precedes this row lock, as it does for the tables choices touch.
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`), "meeting_link_decision_lock"); err != nil {
			return fmt.Errorf("seed meeting decision lock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE archive_metadata SET value = value WHERE key = ?`, "meeting_link_decision_lock"); err != nil {
			return fmt.Errorf("lock meeting decisions: %w", err)
		}
		for _, endpoint := range []meetingDecisionEndpoint{a, b} {
			if err := s.lockMeetingEvidenceWith(ctx, tx, endpoint.ID); err != nil {
				return err
			}
		}
		ctx := withWritableSnapshot(ctx, s.db.DB, tx)
		clearStored := func(cause error) error {
			if decision != "clear" || !errors.Is(cause, sql.ErrNoRows) {
				return cause
			}
			leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
			rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
			left, args := meetingDecisionSideClause(a, "left")
			right, rightValues := meetingDecisionSideClause(b, "right")
			args = append(args, rightValues...)
			args = append(args, leftArgs...)
			args = append(args, rightArgs...)
			deleted, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE choice_key IN (SELECT d.choice_key FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE `+left+` AND `+right+leftScope+rightScope+`)`, args...)
			if err != nil {
				return err
			}
			if count, err := deleted.RowsAffected(); err != nil {
				return err
			} else if count == 0 {
				return cause
			}
			result = MeetingLinkDecisionResult{Left: a.ref(), Right: b.ref(), State: "automatic"}
			return nil
		}
		budget := calrecur.RequestReadBudget
		for _, endpoint := range []*meetingDecisionEndpoint{&a, &b} {
			if endpoint.Kind == meetingRefEvent && endpoint.Key == "" {
				rows, err := s.meetingEventRows(ctx, `e.message_id = ?`, []any{endpoint.ID}, &budget)
				if err != nil {
					return err
				}
				if len(rows) > 0 && rows[0].Event.Kind == "single" && rows[0].Event.Start != nil {
					event := rows[0].Event
					endpoint.Anchor = calrecur.Identity{Instant: *event.Start, Date: event.StartDate, AllDay: event.AllDay}.Key()
				}
			}
		}
		canonicalA, aliasesA, err := s.materializedMeetingEndpoint(ctx, a, &budget)
		if err != nil {
			return clearStored(err)
		}
		canonicalB, aliasesB, err := s.materializedMeetingEndpoint(ctx, b, &budget)
		if err != nil {
			return clearStored(err)
		}
		if compareMeetingEndpoints(canonicalA, canonicalB) == 0 {
			return ErrMeetingLinkSameCopy
		}
		if compareMeetingEndpoints(canonicalA, canonicalB) > 0 {
			canonicalA, canonicalB = canonicalB, canonicalA
		}
		seen := map[int64]bool{}
		decisions := meetingDecisionLoader{store: s}
		for _, id := range append(aliasesA, aliasesB...) {
			choices, err := decisions.edges(ctx, meetingDecisionEndpoint{ID: id}, nil, &budget)
			if err != nil {
				return err
			}
			for _, choice := range choices {
				if seen[choice.id] {
					continue
				}
				seen[choice.id] = true
				left, _, err := s.materializedMeetingEndpoint(ctx, choice.a, &budget)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				right, _, err := s.materializedMeetingEndpoint(ctx, choice.b, &budget)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if compareMeetingEndpoints(left, right) > 0 {
					left, right = right, left
				}
				if compareMeetingEndpoints(left, canonicalA) == 0 && compareMeetingEndpoints(right, canonicalB) == 0 {
					err = deleteMeetingDecisionTx(ctx, tx, choice, decision == "clear")
					if err != nil {
						return err
					}
				}
			}
		}
		state := "link"
		if decision == "unlink" {
			state = "split"
		}
		if decision == "clear" {
			state = "automatic"
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision,choice_key) VALUES (?,?,?,?,?,?,?,?,?,?)`, a.ID, a.Kind, a.Key, a.Anchor, b.ID, b.Kind, b.Key, b.Anchor, state, rand.Text())
		}
		if err != nil {
			return err
		}
		if state != "automatic" {
			leftRef, err := a.meetingRef()
			if err != nil {
				return err
			}
			var conflict meetingDecisionConflict
			record, err := s.meetingRecordContext(ctx, leftRef, &conflict)
			if err != nil {
				return err
			}
			found := false
			for _, id := range record.MessageIDs {
				found = found || b.Kind == meetingRefMessage && id == b.ID
			}
			for _, link := range record.Links {
				found = found || b.Kind == meetingRefEvent && (link.Ref == b.ref() || b.Key == "" && link.MessageID == b.ID)
			}
			if conflict.link.decision != "" {
				return fmt.Errorf("%w: link %s %s conflicts with split %s %s", ErrMeetingLinkConflict, conflict.link.a.ref(), conflict.link.b.ref(), conflict.split.a.ref(), conflict.split.b.ref())
			}
			if (state == "link") != found {
				return fmt.Errorf("%w: cannot save %s for %s %s", ErrMeetingLinkConflict, state, a.ref(), b.ref())
			}
		}
		result = MeetingLinkDecisionResult{Left: canonicalA.ref(), Right: canonicalB.ref(), State: state}
		return nil
	})
	return result, err
}

// materializedMeetingEndpoint follows an event reference to its current occurrence.
func (s *Store) materializedMeetingEndpoint(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) (meetingDecisionEndpoint, []int64, error) {
	resolved, aliases, _, err := s.materializedMeetingEndpointRecord(ctx, endpoint, budget)
	return resolved, aliases, err
}

func (s *Store) meetingAutomaticRecordForEndpoint(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) (MeetingRecord, error) {
	ref, err := endpoint.meetingRef()
	if err != nil {
		return MeetingRecord{}, err
	}
	record, err := s.meetingAutomaticRecordContext(ctx, ref, budget)
	if errors.Is(err, ErrMeetingOccurrenceRequired) && endpoint.Anchor != "" {
		endpoint.Key = endpoint.Anchor
		ref, err = endpoint.meetingRef()
		if err == nil {
			record, err = s.meetingAutomaticRecordContext(ctx, ref, budget)
		}
	}
	return record, err
}

func (s *Store) materializedMeetingEndpointRecord(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) (meetingDecisionEndpoint, []int64, MeetingRecord, error) {
	record, err := s.meetingAutomaticRecordForEndpoint(ctx, endpoint, budget)
	if err != nil {
		return endpoint, nil, MeetingRecord{}, err
	}
	if endpoint.Kind == meetingRefMessage {
		return endpoint, []int64{endpoint.ID}, record, nil
	}
	for _, link := range record.Links {
		if link.Ref == endpoint.ref() || link.MessageID == endpoint.ID {
			resolved, err := ParseMeetingLinkRef(link.Ref)
			return endpointFromRef(resolved), []int64{endpoint.ID, link.MessageID}, record, err
		}
	}
	return endpoint, nil, MeetingRecord{}, sql.ErrNoRows
}

// copyMeetingLinkDecisionsTx keeps hidden originals for undo and preserves survivor choices.
func copyMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, from, into int64, dedup bool) error {
	if from == into {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision,choice_key FROM meeting_link_decisions WHERE left_message_id=? OR right_message_id=?`, from, from)
	if err != nil {
		return err
	}
	type copiedDecision struct {
		meetingDecisionEdge

		choiceKey string
	}
	var decisions []copiedDecision
	for rows.Next() {
		var d copiedDecision
		if err := rows.Scan(&d.id, &d.a.ID, &d.a.Kind, &d.a.Key, &d.a.Anchor, &d.b.ID, &d.b.Kind, &d.b.Key, &d.b.Anchor, &d.decision, &d.choiceKey); err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, d := range decisions {
		if d.a.ID == from {
			d.a.ID = into
		}
		if d.b.ID == from {
			d.b.ID = into
		}
		if compareMeetingEndpoints(d.a, d.b) == 0 {
			continue
		}
		if compareMeetingEndpoints(d.a, d.b) > 0 {
			d.a, d.b = d.b, d.a
		}
		var copyID int64
		err := tx.QueryRowContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision,choice_key,is_explicit) VALUES (?,?,?,?,?,?,?,?,?,?,FALSE) RETURNING id`, d.a.ID, d.a.Kind, d.a.Key, d.a.Anchor, d.b.ID, d.b.Kind, d.b.Key, d.b.Anchor, d.decision, d.choiceKey).Scan(&copyID)
		if err != nil {
			return fmt.Errorf("copy meeting link decision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_decision_dependencies(decision_id,origin_message_id) SELECT ?,origin_message_id FROM meeting_link_decision_dependencies WHERE decision_id=?`, copyID, d.id); err != nil {
			return err
		}
		if dedup {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_decision_dependencies(decision_id,origin_message_id) VALUES (?,?) ON CONFLICT DO NOTHING`, copyID, from); err != nil {
				return err
			}
		}
	}
	return nil
}

// undoMeetingLinkDecisionsTx removes contributions that depend on a restored message.
func undoMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, batchID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id IN (SELECT decision_id FROM meeting_link_decision_dependencies WHERE origin_message_id IN (SELECT id FROM messages WHERE delete_batch_id=?))`, batchID)
	return err
}

// deleteMeetingDecisionTx clears source choices or replaces a direct choice.
func deleteMeetingDecisionTx(ctx context.Context, tx *loggedTx, d meetingDecisionEdge, clearing bool) error {
	if clearing || d.isExplicit {
		_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE choice_key=(SELECT choice_key FROM meeting_link_decisions WHERE id=?)`, d.id)
		return err
	}
	return nil
}
