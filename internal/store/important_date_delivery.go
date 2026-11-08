package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ImportantDateDeliveryState records progress across external Kata writes.
type ImportantDateDeliveryState string

const (
	ImportantDateClaimed        ImportantDateDeliveryState = "claimed"
	ImportantDateIssueInFlight  ImportantDateDeliveryState = "issue_in_flight"
	ImportantDateIssueCreated   ImportantDateDeliveryState = "issue_created"
	ImportantDateNotifyInFlight ImportantDateDeliveryState = "notify_in_flight"
	ImportantDateDelivered      ImportantDateDeliveryState = "delivered"
	ImportantDateUnknown        ImportantDateDeliveryState = "unknown"
	ImportantDateCancelled      ImportantDateDeliveryState = "cancelled"
)

var ErrImportantDateTransition = errors.New("invalid important-date delivery transition")

// ClaimImportantDateDeliveryContext creates a single durable claim. Existing
// records, including unknown or completed ones, are never reclaimed.
func (s *Store) ClaimImportantDateDeliveryContext(ctx context.Context, key string) (bool, error) {
	if strings.TrimSpace(key) == "" || len(key) > 512 {
		return false, ErrImportantDateTransition
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO important_date_deliveries (occurrence_key, state, issue_uid)
		VALUES (?, 'claimed', '') ON CONFLICT (occurrence_key) DO NOTHING`, key)
	if err != nil {
		return false, fmt.Errorf("claim important-date delivery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count important-date claims: %w", err)
	}
	return count == 1, nil
}

// AdvanceImportantDateDeliveryContext applies a compare-and-set transition.
// Unknown outcomes cannot automatically return to an executable state.
func (s *Store) AdvanceImportantDateDeliveryContext(ctx context.Context, key string, from, to ImportantDateDeliveryState, issueUID string) (bool, error) {
	valid := (from == ImportantDateClaimed && (to == ImportantDateIssueInFlight || to == ImportantDateCancelled)) ||
		(from == ImportantDateIssueInFlight && (to == ImportantDateIssueCreated || to == ImportantDateUnknown)) ||
		(from == ImportantDateIssueCreated && (to == ImportantDateNotifyInFlight || to == ImportantDateCancelled)) ||
		(from == ImportantDateNotifyInFlight && (to == ImportantDateDelivered || to == ImportantDateUnknown))
	if !valid || strings.TrimSpace(key) == "" || ((to == ImportantDateIssueCreated || from == ImportantDateIssueCreated || from == ImportantDateNotifyInFlight) && strings.TrimSpace(issueUID) == "") {
		return false, ErrImportantDateTransition
	}
	result, err := s.db.ExecContext(ctx, `UPDATE important_date_deliveries SET state = ?, issue_uid = ?
		WHERE occurrence_key = ? AND state = ? AND (issue_uid = '' OR issue_uid = ?)`, to, issueUID, key, from, issueUID)
	if err != nil {
		return false, fmt.Errorf("advance important-date delivery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count important-date transitions: %w", err)
	}
	return count == 1, nil
}

// ImportantDateDeliveryContext reads a retained occurrence without changing it.
func (s *Store) ImportantDateDeliveryContext(ctx context.Context, key string) (ImportantDateDeliveryState, string, error) {
	var state ImportantDateDeliveryState
	var issueUID string
	if err := s.db.QueryRowContext(ctx, `SELECT state, issue_uid FROM important_date_deliveries WHERE occurrence_key = ?`, key).Scan(&state, &issueUID); err != nil {
		return "", "", fmt.Errorf("read important-date delivery: %w", err)
	}
	return state, issueUID, nil
}
