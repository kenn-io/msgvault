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
	if e.Kind == "evt" {
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
	if a.ID <= 0 || b.ID <= 0 || (a.Kind != "msg" && a.Kind != "evt") || (b.Kind != "msg" && b.Kind != "evt") || compareMeetingEndpoints(a, b) == 0 {
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
			resolved, err := ParseMeetingLinkRef(link.Ref)
			return endpointFromRef(resolved), []int64{endpoint.ID, link.MessageID}, err
		}
	}
	return endpoint, nil, sql.ErrNoRows
}

// copyMeetingLinkDecisionsTx keeps hidden originals for undo and preserves survivor choices.
func copyMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, from, into int64, dedup bool) error {
	if from == into {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,left_message_id,left_kind,left_key,left_anchor,right_message_id,right_kind,right_key,right_anchor,decision FROM meeting_link_decisions WHERE left_message_id=? OR right_message_id=?`, from, from)
	if err != nil {
		return err
	}
	type copiedDecision struct {
		meetingDecisionEdge
		id int64
	}
	var decisions []copiedDecision
	for rows.Next() {
		var d copiedDecision
		if err := rows.Scan(&d.id, &d.a.ID, &d.a.Kind, &d.a.Key, &d.a.Anchor, &d.b.ID, &d.b.Kind, &d.b.Key, &d.b.Anchor, &d.decision); err != nil {
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
		newID, ok, err := insertMeetingDecisionCopy(ctx, tx, d.meetingDecisionEdge)
		if err != nil {
			return fmt.Errorf("copy meeting link decision: %w", err)
		}
		if !ok {
			continue
		}
		if err := addMeetingDecisionPaths(ctx, tx, newID, d.id, from, dedup); err != nil {
			return fmt.Errorf("copy meeting decision origins: %w", err)
		}
	}
	return nil
}

// insertMeetingDecisionCopy returns the copied choice, or the existing choice that other
// merges also support. ok is false when a choice of the owner's own already stands there.
func insertMeetingDecisionCopy(ctx context.Context, tx *loggedTx, d meetingDecisionEdge) (id int64, ok bool, err error) {
	keys := []any{d.a.ID, d.a.Kind, d.a.Key, d.b.ID, d.b.Kind, d.b.Key}
	err = tx.QueryRowContext(ctx, `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,left_anchor,right_anchor,decision) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key) DO NOTHING RETURNING id`, append(keys, d.a.Anchor, d.b.Anchor, d.decision)...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM meeting_link_decisions WHERE left_message_id=? AND left_kind=? AND left_key=? AND right_message_id=? AND right_kind=? AND right_key=? AND decision=? AND id IN (SELECT decision_id FROM meeting_link_decision_origins)`, append(keys, d.decision)...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
	}
	return id, err == nil, err
}

// addMeetingDecisionPaths gives the copy one support path per path of its source. A merge
// extends every path, so undoing any step of a path drops it, and the choice stays while
// another path stands.
func addMeetingDecisionPaths(ctx context.Context, tx *loggedTx, id, source, from int64, dedup bool) error {
	paths, err := meetingDecisionPaths(ctx, tx, source)
	if err != nil {
		return err
	}
	if len(paths) == 0 && !dedup {
		// The source was the owner's own choice, so it now stands without undo.
		_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decision_origins WHERE decision_id=?`, id)
		return err
	}
	if dedup && len(paths) == 0 {
		paths = [][]int64{nil}
	}
	var base int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(path)+1,0) FROM meeting_link_decision_origins WHERE decision_id=?`, id).Scan(&base); err != nil {
		return err
	}
	for i, path := range paths {
		if dedup {
			path = append(path, from)
		}
		for _, origin := range path {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_decision_origins(decision_id,path,origin_message_id) VALUES (?,?,?) ON CONFLICT(decision_id,path,origin_message_id) DO NOTHING`, id, base+int64(i), origin); err != nil {
				return err
			}
		}
	}
	return nil
}

// meetingDecisionPaths returns the origin messages of each support path of a choice.
func meetingDecisionPaths(ctx context.Context, tx *loggedTx, decision int64) ([][]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path,origin_message_id FROM meeting_link_decision_origins WHERE decision_id=? ORDER BY path`, decision)
	if err != nil {
		return nil, err
	}
	var paths [][]int64
	last := int64(-1)
	for rows.Next() {
		var path, origin int64
		if err := rows.Scan(&path, &origin); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if path != last {
			paths, last = append(paths, nil), path
		}
		paths[len(paths)-1] = append(paths[len(paths)-1], origin)
	}
	return paths, errors.Join(rows.Err(), rows.Close())
}

// undoMeetingLinkDecisionsTx drops the support paths that depend on the batch and the
// copied choices left without any path.
func undoMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, batchID string) error {
	const hit = `SELECT 1 FROM meeting_link_decision_origins b JOIN messages m ON m.id=b.origin_message_id WHERE b.decision_id=o.decision_id AND b.path=o.path AND m.delete_batch_id=?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id IN (SELECT decision_id FROM meeting_link_decision_origins) AND NOT EXISTS (SELECT 1 FROM meeting_link_decision_origins o WHERE o.decision_id=meeting_link_decisions.id AND NOT EXISTS (`+hit+`))`, batchID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decision_origins WHERE EXISTS (SELECT 1 FROM meeting_link_decision_origins o WHERE o.decision_id=meeting_link_decision_origins.decision_id AND o.path=meeting_link_decision_origins.path AND EXISTS (`+hit+`))`, batchID)
	return err
}

// deleteMeetingDecisionTx removes a choice. When the owner clears it, the hidden originals
// it was copied from go too, so undoing the merge does not bring it back.
func deleteMeetingDecisionTx(ctx context.Context, tx *loggedTx, d meetingDecisionEdge, clear bool) error {
	keys := []any{d.a.ID, d.a.Kind, d.a.Key, d.b.ID, d.b.Kind, d.b.Key}
	const copied = `c.left_message_id=? AND c.left_kind=? AND c.left_key=? AND c.right_message_id=? AND c.right_kind=? AND c.right_key=?`
	const touches = `(x.%[1]s_message_id=o.origin_message_id AND ((x.%[2]s_message_id=c.left_message_id AND x.%[2]s_kind=c.left_kind AND x.%[2]s_key=c.left_key) OR (x.%[2]s_message_id=c.right_message_id AND x.%[2]s_kind=c.right_kind AND x.%[2]s_key=c.right_key)))`
	if !clear {
		_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE left_message_id=? AND left_kind=? AND left_key=? AND right_message_id=? AND right_kind=? AND right_key=?`, keys...)
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE id IN (SELECT x.id FROM meeting_link_decisions c JOIN meeting_link_decision_origins o ON o.decision_id=c.id JOIN meeting_link_decisions x ON x.id<>c.id AND x.decision=c.decision AND (`+fmt.Sprintf(touches, "left", "right")+` OR `+fmt.Sprintf(touches, "right", "left")+`) WHERE `+copied+`)`, keys...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE left_message_id=? AND left_kind=? AND left_key=? AND right_message_id=? AND right_kind=? AND right_key=?`, keys...)
	return err
}

// readable reports whether both endpoints still name a valid reference.
func (e meetingDecisionEdge) readable() bool {
	_, errA := e.a.meetingRef()
	_, errB := e.b.meetingRef()
	return errA == nil && errB == nil
}
