package store

import (
	"context"
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
	ref := fmt.Sprintf("%s:%d", e.Kind, e.ID)
	if e.Key != "" {
		ref += "@" + e.Key
	}
	return ref
}

func endpointFromRef(ref MeetingLinkRef) meetingDecisionEndpoint {
	e := meetingDecisionEndpoint{ID: ref.MessageID, Kind: ref.Kind}
	if ref.Identity != (calrecur.Identity{}) {
		e.Key = ref.Identity.Key()
	}
	return e
}

func (e meetingDecisionEndpoint) meetingRef() (MeetingLinkRef, error) {
	ref := MeetingLinkRef{Kind: e.Kind, OccurrenceRef: OccurrenceRef{MessageID: e.ID}}
	if e.Key != "" {
		_, identity, ok := calrecur.ParseInstanceID("event_" + e.Key)
		if !ok {
			return ref, fmt.Errorf("invalid stored occurrence key %q", e.Key)
		}
		ref.Identity = identity
	}
	return ref, nil
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

// DecideMeetingLinkContext saves or clears a choice between two copies.
func (s *Store) DecideMeetingLinkContext(ctx context.Context, left, right MeetingLinkRef, decision string) (result MeetingLinkDecisionResult, err error) {
	if decision != "link" && decision != "unlink" && decision != "clear" {
		return result, errors.New("decision must be link, unlink, or clear")
	}
	a, b := endpointFromRef(left), endpointFromRef(right)
	if a.ID <= 0 || b.ID <= 0 || (a.Kind != "msg" && a.Kind != "evt") || (b.Kind != "msg" && b.Kind != "evt") || compareMeetingEndpoints(a, b) == 0 {
		return result, errors.New("invalid meeting decision endpoints")
	}
	if compareMeetingEndpoints(a, b) > 0 {
		a, b = b, a
	}
	err = s.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(tx *loggedTx) error {
		if err := s.lockSyncMaintenanceGate(ctx, tx); err != nil {
			return err
		}
		for _, endpoint := range []meetingDecisionEndpoint{a, b} {
			if err := s.lockMeetingEvidenceWith(ctx, tx, endpoint.ID); err != nil {
				return err
			}
		}
		ctx := withWritableSnapshot(ctx, s.db.DB, tx)
		budget := calrecur.RequestReadBudget
		for _, endpoint := range []*meetingDecisionEndpoint{&a, &b} {
			ref, err := endpoint.meetingRef()
			if err != nil {
				return err
			}
			record, err := s.meetingAutomaticRecordContext(ctx, ref, &budget)
			if err != nil {
				return err
			}
			if endpoint.Kind == "evt" && len(record.Links) == 0 {
				return sql.ErrNoRows
			}
			if endpoint.Kind == "evt" && endpoint.Key == "" {
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
			return err
		}
		canonicalB, aliasesB, err := s.materializedMeetingEndpoint(ctx, b, &budget)
		if err != nil {
			return err
		}
		if compareMeetingEndpoints(canonicalA, canonicalB) == 0 {
			return ErrMeetingLinkSameCopy
		}
		if compareMeetingEndpoints(canonicalA, canonicalB) > 0 {
			canonicalA, canonicalB = canonicalB, canonicalA
		}
		seen := map[string]bool{}
		for _, id := range append(aliasesA, aliasesB...) {
			choices, err := s.meetingDecisionsForMessage(ctx, id, &budget)
			if err != nil {
				return err
			}
			for _, choice := range choices {
				key := choice.a.ref() + "/" + choice.b.ref()
				if seen[key] {
					continue
				}
				seen[key] = true
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
					_, err = tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE left_message_id=? AND left_kind=? AND left_key=? AND right_message_id=? AND right_kind=? AND right_key=?`, choice.a.ID, choice.a.Kind, choice.a.Key, choice.b.ID, choice.b.Kind, choice.b.Key)
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
			_, err = tx.ExecContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision) VALUES (?,?,?,?,?,?,?,?,?)`, a.ID, a.Kind, a.Key, a.Anchor, b.ID, b.Kind, b.Key, b.Anchor, state)
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
				found = found || b.Kind == "msg" && id == b.ID
			}
			for _, link := range record.Links {
				found = found || b.Kind == "evt" && (link.Ref == b.ref() || b.Key == "" && link.MessageID == b.ID)
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
	ref, err := endpoint.meetingRef()
	if err != nil {
		return endpoint, nil, err
	}
	record, err := s.meetingAutomaticRecordContext(ctx, ref, budget)
	if errors.Is(err, ErrMeetingOccurrenceRequired) && endpoint.Anchor != "" {
		anchored := endpoint
		anchored.Key = anchored.Anchor
		ref, err = anchored.meetingRef()
		if err == nil {
			record, err = s.meetingAutomaticRecordContext(ctx, ref, budget)
		}
	}
	if err != nil {
		return endpoint, nil, err
	}
	if endpoint.Kind == "msg" {
		return endpoint, []int64{endpoint.ID}, nil
	}
	for _, link := range record.Links {
		if link.Ref == endpoint.ref() || link.MessageID == endpoint.ID {
			resolved, err := parseStoredMeetingRef(link.Ref)
			return resolved, []int64{endpoint.ID, link.MessageID}, err
		}
	}
	return endpoint, nil, sql.ErrNoRows
}

// copyMeetingLinkDecisionsTx keeps hidden originals for undo and preserves survivor choices.
func copyMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, from, into int64, dedup bool) error {
	if from == into {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision,dedup_origin_message_id FROM meeting_link_decisions WHERE left_message_id=? OR right_message_id=?`, from, from)
	if err != nil {
		return err
	}
	type copiedDecision struct {
		meetingDecisionEdge
		origin sql.NullInt64
	}
	var decisions []copiedDecision
	for rows.Next() {
		var d copiedDecision
		if err := rows.Scan(&d.a.ID, &d.a.Kind, &d.a.Key, &d.a.Anchor, &d.b.ID, &d.b.Kind, &d.b.Key, &d.b.Anchor, &d.decision, &d.origin); err != nil {
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
		var origin any
		if dedup {
			origin = from
		} else if d.origin.Valid {
			origin = d.origin.Int64
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision,dedup_origin_message_id) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key) DO NOTHING`, d.a.ID, d.a.Kind, d.a.Key, d.a.Anchor, d.b.ID, d.b.Kind, d.b.Key, d.b.Anchor, d.decision, origin)
		if err != nil {
			return fmt.Errorf("copy meeting link decision: %w", err)
		}
	}
	return nil
}
