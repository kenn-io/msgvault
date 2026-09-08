package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// The store is the sweep's brief archive and its durable brief state. Both
// assertions are compile-time so a signature change in either interface is a
// build error here rather than a nil field in the daemon.
var (
	_ peoplesweep.BriefArchive = (*Store)(nil)
	_ peoplesweep.BriefStore   = (*Store)(nil)
)

// PersonSweepLastContact reports the deterministic last-contact coordinate the
// hourly activity projection computed for one person. The person brief window
// uses it to guarantee that the brief and person_contact_state.last_contact_at
// agree on what "last time we talked" refers to.
//
// person_contact_state stores the coordinate as last_contact_message_id rather
// than a source ref, so the brief resolves the item by message ID. The second
// return value is false when the person has no contact state row yet.
func (s *Store) PersonSweepLastContact(
	ctx context.Context, personID int64,
) (peoplesweep.BriefLastContact, bool, error) {
	if personID <= 0 {
		return peoplesweep.BriefLastContact{}, false, errors.New(
			"person sweep last contact requires a positive person id")
	}
	var messageID, sourceID sql.NullInt64
	var channel sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT last_contact_message_id, last_contact_source_id, last_contact_channel
		FROM person_contact_state WHERE person_id = ?`), personID).
		Scan(&messageID, &sourceID, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return peoplesweep.BriefLastContact{}, false, nil
	}
	if err != nil {
		return peoplesweep.BriefLastContact{}, false, fmt.Errorf(
			"read person sweep last contact: %w", err)
	}
	return peoplesweep.BriefLastContact{
		MessageID: messageID.Int64, SourceID: sourceID.Int64, Channel: channel.String,
	}, true, nil
}

// PersonSweepBriefEligibility reports one person's brief state in the sweep's
// vocabulary, and false when the person is not both enrolled and tracked. It
// reuses the same enrollment listing the scheduler uses, then reads the latest
// version for its stored input boundary, which the eligibility listing reports
// only as a sequence.
func (s *Store) PersonSweepBriefEligibility(
	ctx context.Context, personID int64,
) (peoplesweep.BriefEligibility, bool, error) {
	if personID <= 0 {
		return peoplesweep.BriefEligibility{}, false, errors.New(
			"person sweep brief eligibility requires a positive person id")
	}
	enrolled, err := s.ListBriefEligiblePeopleContext(ctx, personID-1, 1)
	if err != nil {
		return peoplesweep.BriefEligibility{}, false, err
	}
	if len(enrolled) == 0 || enrolled[0].PersonID != personID {
		return peoplesweep.BriefEligibility{}, false, nil
	}
	eligibility := peoplesweep.BriefEligibility{
		PersonID: personID, EnabledAt: enrolled[0].EnabledAt, Version: enrolled[0].Version,
		Status: enrolled[0].Status, GeneratedAt: enrolled[0].GeneratedAt,
	}
	if eligibility.Version == 0 {
		return eligibility, true, nil
	}
	versions, err := s.ListPersonBriefVersionsContext(ctx, personID, 1)
	if err != nil {
		return peoplesweep.BriefEligibility{}, false, err
	}
	if len(versions) == 0 {
		return eligibility, true, nil
	}
	var boundary peoplesweep.BriefBoundary
	if err := json.Unmarshal(versions[0].Boundary, &boundary); err != nil {
		return peoplesweep.BriefEligibility{}, false, fmt.Errorf(
			"decode person %d brief boundary: %w", personID, err)
	}
	eligibility.Boundary = &boundary
	return eligibility, true, nil
}

// PersonSweepCadenceDueAt reports person_contact_state's cadence due date, and
// false when the person has no contact state or the projection computed no due
// date. The brief uses it to regenerate ahead of a contact the owner owes.
func (s *Store) PersonSweepCadenceDueAt(
	ctx context.Context, personID int64, now time.Time,
) (time.Time, bool, error) {
	if personID <= 0 {
		return time.Time{}, false, errors.New(
			"person sweep cadence lookup requires a positive person id")
	}
	state, err := s.ContactStateContext(ctx, personID, now)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read person %d contact cadence: %w", personID, err)
	}
	if state.CadenceDueAt == nil {
		return time.Time{}, false, nil
	}
	return state.CadenceDueAt.UTC(), true, nil
}

// HasPersonSweepChangesAfter reports whether the person's immutable change
// journal holds a row past a durable sequence. It is the "new activity since
// the current brief" test, evaluated against the same journal the sweep's
// cursors advance through.
func (s *Store) HasPersonSweepChangesAfter(
	ctx context.Context, personID, sequence int64,
) (bool, error) {
	if personID <= 0 {
		return false, errors.New("person sweep change lookup requires a positive person id")
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT EXISTS (SELECT 1 FROM person_sweep_changes
		WHERE person_id = ? AND sequence > ?)`), personID, sequence).Scan(&exists); err != nil {
		return false, fmt.Errorf("read person %d sweep change activity: %w", personID, err)
	}
	return exists, nil
}
