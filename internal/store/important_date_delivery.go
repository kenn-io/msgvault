package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
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
func (s *Store) ClaimImportantDateDeliveryContext(ctx context.Context, key, fingerprint string) (bool, error) {
	if strings.TrimSpace(key) == "" || len(key) > 512 || strings.TrimSpace(fingerprint) == "" || len(fingerprint) > 128 {
		return false, ErrImportantDateTransition
	}
	separator := strings.LastIndexByte(key, ':')
	if separator < 1 || separator == len(key)-1 {
		return false, ErrImportantDateTransition
	}
	var claimed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		fenced := withWritableSnapshot(ctx, s.db.DB, tx)
		pending, err := s.ImportantDateHasOtherPendingContext(fenced, key[:separator+1], key, fingerprint)
		if err != nil {
			return err
		}
		if pending {
			return nil
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO important_date_deliveries (occurrence_key, fingerprint, state, issue_uid)
			VALUES (?, ?, 'claimed', '') ON CONFLICT (occurrence_key) DO NOTHING`, key, fingerprint)
		if err != nil {
			return fmt.Errorf("claim important-date delivery: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count important-date claims: %w", err)
		}
		claimed = count == 1
		return nil
	})
	return claimed, err
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

// ImportantDateHasOtherPendingContext detects a pending or uncertain prior
// occurrence for the same subject and meaning, including across year changes.
// A changed date fingerprint also quarantines a previously delivered occurrence.
func (s *Store) ImportantDateHasOtherPendingContext(ctx context.Context, prefix, currentKey, fingerprint string) (bool, error) {
	if prefix == "" || !strings.HasPrefix(currentKey, prefix) || fingerprint == "" {
		return false, ErrImportantDateTransition
	}
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_deliveries
		WHERE substr(occurrence_key, 1, ?) = ? AND occurrence_key <> ?
		AND (fingerprint <> ? OR state IN ('claimed', 'issue_in_flight', 'issue_created', 'notify_in_flight', 'unknown')))`, utf8.RuneCountInString(prefix), prefix, currentKey, fingerprint).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check pending important-date occurrences: %w", err)
	}
	return pending, nil
}

// ImportantDateDeliveryExistsContext reports whether an occurrence has any
// retained state, including dismissed, completed, or uncertain deliveries.
func (s *Store) ImportantDateDeliveryExistsContext(ctx context.Context, key string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_deliveries WHERE occurrence_key = ?)`, key).Scan(&exists); err != nil {
		return false, fmt.Errorf("check important-date delivery: %w", err)
	}
	return exists, nil
}

// ImportantDateDelivery is a retained occurrence and its delivery status.
type ImportantDateDelivery struct {
	OccurrenceKey string                     `json:"occurrence_key"`
	State         ImportantDateDeliveryState `json:"state"`
	IssueUID      string                     `json:"issue_uid,omitempty"`
}

// ListImportantDateDeliveriesContext returns a bounded owner-only status view.
func (s *Store) ListImportantDateDeliveriesContext(ctx context.Context) ([]ImportantDateDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT occurrence_key, state, issue_uid FROM important_date_deliveries ORDER BY occurrence_key LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("list important-date deliveries: %w", err)
	}
	defer rows.Close()
	var result []ImportantDateDelivery
	for rows.Next() {
		var item ImportantDateDelivery
		if err := rows.Scan(&item.OccurrenceKey, &item.State, &item.IssueUID); err != nil {
			return nil, fmt.Errorf("read important-date delivery: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list important-date deliveries: %w", err)
	}
	return result, nil
}
