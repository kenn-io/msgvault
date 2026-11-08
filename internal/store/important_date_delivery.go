package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ImportantDateDeliveryState records progress across external Kata writes.
type ImportantDateDeliveryState string

const (
	ImportantDateClaimed        ImportantDateDeliveryState = "claimed"
	ImportantDateIssueInFlight  ImportantDateDeliveryState = "issue_in_flight"
	ImportantDateRetryIssue     ImportantDateDeliveryState = "retry_issue"
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
		guarded, err := s.importantDateOccurrenceGuardedContext(fenced, key)
		if err != nil {
			return err
		}
		if guarded {
			return nil
		}
		pending, err := s.ImportantDateHasOtherPendingContext(fenced, key[:separator+1], key, fingerprint)
		if err != nil {
			return err
		}
		if pending {
			return nil
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO important_date_deliveries (occurrence_key, fingerprint, state, issue_uid)
			VALUES (?, ?, 'claimed', '') ON CONFLICT (occurrence_key) DO UPDATE SET state = 'claimed', fingerprint = excluded.fingerprint WHERE important_date_deliveries.state = 'retry_issue' AND important_date_deliveries.issue_uid = ''`, key, fingerprint)
		if err != nil {
			return fmt.Errorf("claim important-date delivery: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count important-date claims: %w", err)
		}
		claimed = count == 1
		if !claimed {
			return nil
		}
		subject, meaning, year, err := importantDateOccurrenceParts(key)
		if err != nil {
			return err
		}
		// Persist equivalence at claim time so a later split or narrowed
		// mapping cannot resurrect this occurrence for a possible subject.
		// A subject-scoped guard records correction approval separately from
		// any historical identity that also refers to this occurrence.
		if _, err := tx.ExecContext(ctx, `INSERT INTO important_date_occurrence_guards
			(reconciliation_uid, resolved_subject, meaning, occurrence_year, original_occurrence_key)
			SELECT b.reconciliation_uid, b.resolved_subject, b.meaning, ?, ?
			FROM important_date_identity_bindings b
			JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
			WHERE b.resolved_subject = ? AND b.meaning = ?
			ON CONFLICT DO NOTHING`, year, key, subject, meaning); err != nil {
			return fmt.Errorf("retain subject important-date guard: %w", err)
		}
		// Preserve the original identity association even for a single-target
		// mapping. A later explicit reassignment must carry this occurrence to
		// the newly selected subject instead of creating it again.
		if _, err := tx.ExecContext(ctx, `INSERT INTO important_date_occurrence_guards
			(reconciliation_uid, resolved_subject, meaning, occurrence_year, original_occurrence_key)
			SELECT b.reconciliation_uid, b.original_subject, b.meaning, ?, ?
			FROM important_date_identity_bindings b
			JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
			WHERE b.resolved_subject = ? AND b.meaning = ? AND b.original_subject <> b.resolved_subject
			ON CONFLICT DO NOTHING`, year, key, subject, meaning); err != nil {
			return fmt.Errorf("retain original important-date identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO important_date_occurrence_guards
			(reconciliation_uid, resolved_subject, meaning, occurrence_year, original_occurrence_key)
			SELECT b.reconciliation_uid, other.resolved_subject, b.meaning, ?, ?
			FROM important_date_identity_bindings b
			JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
			JOIN important_date_identity_bindings other ON other.reconciliation_uid = b.reconciliation_uid
				AND other.original_subject = b.original_subject AND other.meaning = b.meaning
			WHERE b.resolved_subject = ? AND b.meaning = ? AND other.resolved_subject <> b.resolved_subject
			ON CONFLICT DO NOTHING`, year, key, subject, meaning); err != nil {
			return fmt.Errorf("retain equivalent important-date guards: %w", err)
		}
		return nil
	})
	return claimed, err
}

// AdvanceImportantDateDeliveryContext applies a compare-and-set transition.
// Unknown outcomes cannot automatically return to an executable state.
func (s *Store) AdvanceImportantDateDeliveryContext(ctx context.Context, key string, from, to ImportantDateDeliveryState, issueUID string) (bool, error) {
	valid := (from == ImportantDateClaimed && (to == ImportantDateIssueInFlight || to == ImportantDateCancelled)) ||
		(from == ImportantDateIssueInFlight && (to == ImportantDateIssueCreated || to == ImportantDateUnknown || to == ImportantDateCancelled || to == ImportantDateRetryIssue)) ||
		(from == ImportantDateIssueCreated && to == ImportantDateCancelled) ||
		(from == ImportantDateNotifyInFlight && (to == ImportantDateDelivered || to == ImportantDateUnknown || to == ImportantDateCancelled || to == ImportantDateIssueCreated))
	if !valid || strings.TrimSpace(key) == "" || ((to == ImportantDateIssueCreated || from == ImportantDateIssueCreated || from == ImportantDateNotifyInFlight) && strings.TrimSpace(issueUID) == "") {
		return false, ErrImportantDateTransition
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE important_date_deliveries SET state = ?, issue_uid = ?
			WHERE occurrence_key = ? AND state = ? AND (issue_uid = '' OR issue_uid = ?)`, to, issueUID, key, from, issueUID)
		if err != nil {
			return fmt.Errorf("advance important-date delivery: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count important-date transitions: %w", err)
		}
		changed = count == 1
		return nil
	})
	return changed, err
}

// ClaimImportantDateNotificationContext resumes only the same date that
// created the retained issue. A correction requires operator reconciliation.
func (s *Store) ClaimImportantDateNotificationContext(ctx context.Context, key, fingerprint, issueUID string) (bool, error) {
	if key == "" || fingerprint == "" || issueUID == "" {
		return false, ErrImportantDateTransition
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE important_date_deliveries SET state = 'notify_in_flight'
			WHERE occurrence_key = ? AND fingerprint = ? AND issue_uid = ? AND state = 'issue_created'`, key, fingerprint, issueUID)
		if err != nil {
			return fmt.Errorf("claim important-date notification: %w", err)
		}
		count, err := result.RowsAffected()
		changed = count == 1
		return err
	})
	return changed, err
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
	subject, meaning, err := importantDatePrefixParts(prefix)
	if err != nil {
		return false, err
	}
	var pending bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_deliveries d
		WHERE substr(d.occurrence_key, 1, ?) = ? AND d.occurrence_key <> ? AND d.state <> 'retry_issue'
		AND ((d.fingerprint <> ? AND d.accepted_fingerprint <> ? AND NOT EXISTS (
			SELECT 1 FROM important_date_occurrence_guards g JOIN important_date_reconciliations r ON r.uid = g.reconciliation_uid AND r.active = 1
			WHERE g.original_occurrence_key = d.occurrence_key AND g.resolved_subject = ? AND g.meaning = ? AND g.accepted_fingerprint = ?))
			OR d.state IN ('claimed', 'issue_in_flight', 'issue_created', 'notify_in_flight', 'unknown')))`, utf8.RuneCountInString(prefix), prefix, currentKey, fingerprint, fingerprint, subject, meaning, fingerprint).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check pending important-date occurrences: %w", err)
	}
	if pending {
		return true, nil
	}
	year, err := strconv.Atoi(strings.TrimPrefix(currentKey, prefix))
	if err != nil {
		return false, ErrImportantDateTransition
	}
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_occurrence_guards g
		JOIN important_date_reconciliations r ON r.uid = g.reconciliation_uid AND r.active = 1
		JOIN important_date_deliveries d ON d.occurrence_key = g.original_occurrence_key
		WHERE g.resolved_subject = ? AND g.meaning = ? AND g.occurrence_year <> ? AND d.state <> 'retry_issue'
		AND ((d.fingerprint <> ? AND g.accepted_fingerprint <> ?) OR d.state IN ('claimed', 'issue_in_flight', 'issue_created', 'notify_in_flight', 'unknown')))`, subject, meaning, year, fingerprint, fingerprint).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check reconciled important-date occurrences: %w", err)
	}
	if pending {
		return true, nil
	}
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_identity_bindings b
		JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
		JOIN important_date_identity_bindings other ON other.reconciliation_uid = b.reconciliation_uid
			AND other.original_subject = b.original_subject AND other.meaning = b.meaning
		JOIN important_date_deliveries d ON substr(d.occurrence_key, 1, length(other.resolved_subject || ':' || other.meaning || ':')) = other.resolved_subject || ':' || other.meaning || ':'
		WHERE b.resolved_subject = ? AND b.meaning = ? AND other.resolved_subject <> b.resolved_subject
		AND d.occurrence_key <> ? AND d.state <> 'retry_issue'
		AND ((d.fingerprint <> ? AND NOT EXISTS (SELECT 1 FROM important_date_occurrence_guards g
			WHERE g.reconciliation_uid = b.reconciliation_uid AND g.original_occurrence_key = d.occurrence_key
			AND g.resolved_subject = b.resolved_subject AND g.meaning = b.meaning AND g.accepted_fingerprint = ?))
			OR d.state IN ('claimed', 'issue_in_flight', 'issue_created', 'notify_in_flight', 'unknown')))`, subject, meaning, currentKey, fingerprint, fingerprint).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check equivalent pending important-date occurrences: %w", err)
	}
	return pending, nil
}

func importantDatePrefixParts(prefix string) (string, string, error) {
	if !strings.HasSuffix(prefix, ":") {
		return "", "", ErrImportantDateTransition
	}
	trimmed := strings.TrimSuffix(prefix, ":")
	separator := strings.LastIndexByte(trimmed, ':')
	if separator < 1 || separator == len(trimmed)-1 {
		return "", "", ErrImportantDateTransition
	}
	return trimmed[:separator], trimmed[separator+1:], nil
}

// ImportantDateDeliveryExistsContext reports whether an occurrence is not
// eligible for a safe issue or notification preflight retry.
func (s *Store) ImportantDateDeliveryExistsContext(ctx context.Context, key string) (bool, error) {
	guarded, err := s.importantDateOccurrenceGuardedContext(ctx, key)
	if err != nil || guarded {
		return guarded, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_deliveries WHERE occurrence_key = ? AND state NOT IN ('retry_issue', 'issue_created'))`, key).Scan(&exists); err != nil {
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

// ListImportantDateDeliveriesContext returns the first owner-only status page.
func (s *Store) ListImportantDateDeliveriesContext(ctx context.Context) ([]ImportantDateDelivery, error) {
	return s.ListImportantDateDeliveriesPageContext(ctx, 100, 0)
}

// ListImportantDateDeliveriesPageContext lets the owner inspect every retained
// occurrence, including older uncertain writes.
func (s *Store) ListImportantDateDeliveriesPageContext(ctx context.Context, limit, offset int) ([]ImportantDateDelivery, error) {
	if limit < 1 || limit > 500 || offset < 0 {
		return nil, errors.New("invalid important-date status page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT occurrence_key, state, issue_uid FROM important_date_deliveries ORDER BY occurrence_key LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list important-date deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

// ResolveImportantDateDeliveryContext records an operator's reconciliation of
// an unresolved attempt. It never sends a request or makes a claim retryable.
func (s *Store) ResolveImportantDateDeliveryContext(ctx context.Context, key string, resolution ImportantDateDeliveryState, issueUID string) (bool, error) {
	if strings.TrimSpace(key) == "" || (resolution != ImportantDateDelivered && resolution != ImportantDateCancelled) || (resolution == ImportantDateDelivered && strings.TrimSpace(issueUID) == "") {
		return false, ErrImportantDateTransition
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE important_date_deliveries SET state = ?, issue_uid = CASE WHEN issue_uid = '' THEN ? ELSE issue_uid END
			WHERE occurrence_key = ? AND state IN ('claimed', 'issue_in_flight', 'issue_created', 'notify_in_flight', 'unknown')
			AND (issue_uid = '' OR ? = '' OR issue_uid = ?)`, resolution, issueUID, key, issueUID, issueUID)
		if err != nil {
			return fmt.Errorf("resolve important-date delivery: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count resolved important-date deliveries: %w", err)
		}
		changed = count == 1
		return nil
	})
	return changed, err
}

// AcceptImportantDateCorrectionContext records an operator's approval of the
// current date after inspecting older issues. Unresolved writes must be resolved
// first. Original fingerprints and issue states remain in the archive.
func (s *Store) AcceptImportantDateCorrectionContext(ctx context.Context, prefix, fingerprint string) (int64, error) {
	if prefix == "" || strings.TrimSpace(fingerprint) == "" || len(fingerprint) > 128 {
		return 0, ErrImportantDateTransition
	}
	subject, meaning, err := importantDatePrefixParts(prefix)
	if err != nil {
		return 0, err
	}
	var changed int64
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var ambiguous bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_identity_bindings b
			JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
			JOIN important_date_identity_bindings other ON other.reconciliation_uid = b.reconciliation_uid
				AND other.original_subject = b.original_subject AND other.meaning = b.meaning
			WHERE b.resolved_subject = ? AND b.meaning = ? AND other.resolved_subject <> b.resolved_subject)`, subject, meaning).Scan(&ambiguous); err != nil {
			return fmt.Errorf("check ambiguous date corrections: %w", err)
		}
		if ambiguous {
			return errors.New("resolve ambiguous identity mappings before accepting a correction")
		}
		var unresolved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_deliveries d
			WHERE (substr(d.occurrence_key, 1, ?) = ? OR EXISTS (SELECT 1 FROM important_date_occurrence_guards g
				JOIN important_date_reconciliations r ON r.uid = g.reconciliation_uid AND r.active = 1
				WHERE g.original_occurrence_key = d.occurrence_key AND g.resolved_subject = ? AND g.meaning = ?))
			AND d.state IN ('claimed', 'issue_in_flight', 'issue_created', 'notify_in_flight', 'unknown'))`, utf8.RuneCountInString(prefix), prefix, subject, meaning).Scan(&unresolved); err != nil {
			return fmt.Errorf("check unresolved date corrections: %w", err)
		}
		if unresolved {
			return errors.New("resolve pending deliveries before accepting a correction")
		}
		result, err := tx.ExecContext(ctx, `UPDATE important_date_deliveries SET accepted_fingerprint = ?
			WHERE substr(occurrence_key, 1, ?) = ? AND state IN ('delivered', 'cancelled')
			AND NOT EXISTS (SELECT 1 FROM important_date_occurrence_guards g
				JOIN important_date_reconciliations r ON r.uid = g.reconciliation_uid AND r.active = 1
				WHERE g.original_occurrence_key = important_date_deliveries.occurrence_key AND g.resolved_subject <> ?)`, fingerprint, utf8.RuneCountInString(prefix), prefix, subject)
		if err != nil {
			return fmt.Errorf("accept important-date correction: %w", err)
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE important_date_occurrence_guards SET accepted_fingerprint = ?
			WHERE resolved_subject = ? AND meaning = ? AND EXISTS (SELECT 1 FROM important_date_reconciliations r
				WHERE r.uid = important_date_occurrence_guards.reconciliation_uid AND r.active = 1)
			AND EXISTS (SELECT 1 FROM important_date_deliveries d WHERE d.occurrence_key = important_date_occurrence_guards.original_occurrence_key AND d.state IN ('delivered', 'cancelled'))`, fingerprint, subject, meaning)
		if err != nil {
			return fmt.Errorf("accept reconciled important-date correction: %w", err)
		}
		count, err := result.RowsAffected()
		changed += count
		return err
	})
	return changed, err
}
