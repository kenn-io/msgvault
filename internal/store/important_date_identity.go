package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ImportantDateIdentityStableContext permits an untouched identity or an
// explicitly reconciled lineage whose topology has not changed since review.
func (s *Store) ImportantDateIdentityStableContext(ctx context.Context, personID int64) (bool, error) {
	var stable bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM persons WHERE id = ?)
		AND NOT EXISTS (SELECT 1 FROM person_merges WHERE current_person_id = ? OR survivor_person_id_at_merge = ? OR absorbed_person_id = ?)
		AND NOT EXISTS (SELECT 1 FROM person_splits WHERE source_person_id = ? OR new_person_id = ?)
		AND NOT EXISTS (SELECT 1 FROM person_uid_aliases WHERE surviving_person_id = ?)`, personID, personID, personID, personID, personID, personID, personID).Scan(&stable)
	if err != nil {
		return false, fmt.Errorf("check important-date identity: %w", err)
	}
	if stable {
		return true, nil
	}
	var uid, topology string
	err = s.db.QueryRowContext(ctx, `SELECT p.vcard_uid, r.topology_digest FROM persons p
		JOIN important_date_identity_bindings b ON b.original_subject = 'person/' || p.vcard_uid AND b.resolved_subject = 'person/' || p.vcard_uid AND b.meaning = 'birthday'
		JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
		WHERE p.id = ?`, personID).Scan(&uid, &topology)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read important-date identity decision: %w", err)
	}
	var ambiguous bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_identity_bindings b
		JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
		JOIN important_date_identity_bindings other ON other.reconciliation_uid = b.reconciliation_uid
			AND other.original_subject = b.original_subject AND other.meaning = b.meaning
		WHERE b.resolved_subject = 'person/' || ? AND other.resolved_subject <> b.resolved_subject)`, uid).Scan(&ambiguous)
	if err != nil {
		return false, fmt.Errorf("check ambiguous important-date identity: %w", err)
	}
	if ambiguous {
		return false, nil
	}
	lineage, err := s.ImportantDateLineageContext(ctx, personID)
	if err != nil {
		return false, err
	}
	return uid != "" && topology == lineage.TopologyDigest, nil
}

// importantDateOccurrenceGuardedContext detects an equivalent occurrence
// retained by a current explicit identity reconciliation.
func (s *Store) importantDateOccurrenceGuardedContext(ctx context.Context, key string) (bool, error) {
	subject, meaning, year, err := importantDateOccurrenceParts(key)
	if err != nil {
		return false, err
	}
	var guarded bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_occurrence_guards g
		JOIN important_date_reconciliations r ON r.uid = g.reconciliation_uid AND r.active = 1
		JOIN important_date_deliveries d ON d.occurrence_key = g.original_occurrence_key
		WHERE g.resolved_subject = ? AND g.meaning = ? AND g.occurrence_year = ? AND d.state <> 'retry_issue')`, subject, meaning, year).Scan(&guarded)
	if err != nil {
		return false, fmt.Errorf("check important-date occurrence guards: %w", err)
	}
	if guarded {
		return true, nil
	}
	// A historical subject mapped to several possible live subjects must not
	// produce two new issues in the same year after the reconciliation.
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_identity_bindings b
		JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
		JOIN important_date_identity_bindings other ON other.reconciliation_uid = b.reconciliation_uid
			AND other.original_subject = b.original_subject AND other.meaning = b.meaning
		JOIN important_date_deliveries d ON d.occurrence_key = other.resolved_subject || ':' || other.meaning || ':' || CAST(? AS TEXT)
		WHERE b.resolved_subject = ? AND b.meaning = ? AND other.resolved_subject <> b.resolved_subject
		AND d.state <> 'retry_issue')`, year, subject, meaning).Scan(&guarded)
	if err != nil {
		return false, fmt.Errorf("check equivalent important-date occurrences: %w", err)
	}
	return guarded, nil
}

func importantDateOccurrenceParts(key string) (string, string, int, error) {
	yearAt := strings.LastIndexByte(key, ':')
	if yearAt < 1 {
		return "", "", 0, ErrImportantDateTransition
	}
	meaningAt := strings.LastIndexByte(key[:yearAt], ':')
	if meaningAt < 1 {
		return "", "", 0, ErrImportantDateTransition
	}
	year, err := strconv.Atoi(key[yearAt+1:])
	if err != nil || year < 1 || year > 9999 {
		return "", "", 0, ErrImportantDateTransition
	}
	return key[:meaningAt], key[meaningAt+1 : yearAt], year, nil
}
