package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	cardDAVSyncRunDefaultLimit       = 25
	cardDAVSyncRunMaxLimit           = 100
	cardDAVSyncRunRetention          = 100
	cardDAVSyncRunErrorMessageMax    = 2000
	cardDAVSyncRunRedactedErrorCode  = "unsafe_error_redacted"
	cardDAVSyncRunRedactedError      = "CardDAV sync failed; sensitive details were removed."
	cardDAVSyncRunRestartedErrorCode = "daemon_restarted"
	cardDAVSyncRunRestartedError     = "CardDAV sync stopped because the daemon restarted."
)

type CardDAVSyncTrigger string

const (
	CardDAVSyncTriggerManual    CardDAVSyncTrigger = "manual"
	CardDAVSyncTriggerScheduled CardDAVSyncTrigger = "scheduled"
)

type CardDAVSyncRunState string

const (
	CardDAVSyncRunRunning   CardDAVSyncRunState = "running"
	CardDAVSyncRunSucceeded CardDAVSyncRunState = "succeeded"
	CardDAVSyncRunFailed    CardDAVSyncRunState = "failed"
	CardDAVSyncRunCancelled CardDAVSyncRunState = "cancelled"
	CardDAVSyncRunPartial   CardDAVSyncRunState = "partial"
)

type CardDAVSyncRunStart struct {
	Trigger CardDAVSyncTrigger
	Full    bool
}

type CardDAVSyncRunFinish struct {
	State        CardDAVSyncRunState
	Books        int64
	Created      int64
	Updated      int64
	Removed      int64
	ErrorCode    string
	ErrorMessage string
}

type CardDAVSyncRun struct {
	ID           int64
	Trigger      CardDAVSyncTrigger
	Full         bool
	State        CardDAVSyncRunState
	StartedAt    time.Time
	FinishedAt   *time.Time
	Books        int64
	Created      int64
	Updated      int64
	Removed      int64
	ErrorCode    string
	ErrorMessage string
}

type CardDAVSyncStatus struct {
	Active           *CardDAVSyncRun
	Latest           *CardDAVSyncRun
	LatestSuccessful *CardDAVSyncRun
}

var (
	ErrCardDAVSyncActive        = errors.New("CardDAV sync already active")
	ErrCardDAVSyncRunInvalid    = errors.New("invalid CardDAV sync run")
	ErrCardDAVSyncRunNotFound   = errors.New("CardDAV sync run not found")
	ErrCardDAVSyncRunTransition = errors.New("invalid CardDAV sync run transition")
)

var (
	cardDAVSyncRunErrorCodePattern        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	cardDAVSyncRunCredentialMarkerPattern = regexp.MustCompile(
		`(?i)(api[[:space:]_-]*key|access[[:space:]_-]*token|refresh[[:space:]_-]*token|credential)`,
	)
)

const cardDAVSyncRunColumns = `id, trigger, full_sync, state, started_at, finished_at,
	books, created, updated, removed, error_code, error_message`

func (s *Store) StartCardDAVSyncRunContext(
	ctx context.Context, input CardDAVSyncRunStart,
) (*CardDAVSyncRun, error) {
	if input.Trigger != CardDAVSyncTriggerManual && input.Trigger != CardDAVSyncTriggerScheduled {
		return nil, fmt.Errorf("%w: trigger must be manual or scheduled", ErrCardDAVSyncRunInvalid)
	}
	var run *CardDAVSyncRun
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		row := tx.QueryRowContext(ctx, `INSERT INTO carddav_sync_runs (trigger, full_sync, state)
			VALUES (?, ?, 'running') RETURNING `+cardDAVSyncRunColumns, input.Trigger, input.Full)
		var err error
		run, err = scanCardDAVSyncRun(row)
		if err != nil {
			if s.dialect.IsConflictError(err) {
				return ErrCardDAVSyncActive
			}
			return fmt.Errorf("insert CardDAV sync run: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Store) FinishCardDAVSyncRunContext(
	ctx context.Context, runID int64, input CardDAVSyncRunFinish,
) (*CardDAVSyncRun, error) {
	if runID <= 0 {
		return nil, fmt.Errorf("%w: run ID must be positive", ErrCardDAVSyncRunInvalid)
	}
	code, message, err := validateCardDAVSyncRunFinish(input)
	if err != nil {
		return nil, err
	}
	var run *CardDAVSyncRun
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		row := tx.QueryRowContext(ctx, fmt.Sprintf(`UPDATE carddav_sync_runs SET
			state = ?, finished_at = %s, books = ?, created = ?, updated = ?, removed = ?,
			error_code = ?, error_message = ?
			WHERE id = ? AND state = 'running' RETURNING %s`, s.dialect.Now(), cardDAVSyncRunColumns),
			input.State, input.Books, input.Created, input.Updated, input.Removed, code, message, runID)
		run, err = scanCardDAVSyncRun(row)
		if errors.Is(err, sql.ErrNoRows) {
			var state CardDAVSyncRunState
			lookupErr := tx.QueryRowContext(ctx, `SELECT state FROM carddav_sync_runs WHERE id = ?`, runID).Scan(&state)
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return ErrCardDAVSyncRunNotFound
			}
			if lookupErr != nil {
				return fmt.Errorf("load CardDAV sync run transition: %w", lookupErr)
			}
			return ErrCardDAVSyncRunTransition
		}
		if err != nil {
			return fmt.Errorf("finish CardDAV sync run: %w", err)
		}
		return pruneCardDAVSyncRuns(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Store) CardDAVSyncStatusContext(ctx context.Context) (CardDAVSyncStatus, error) {
	var status CardDAVSyncStatus
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		status.Active, err = getCardDAVSyncRun(ctx, tx, `state = 'running'`, nil)
		if err != nil {
			return err
		}
		status.Latest, err = getCardDAVSyncRun(ctx, tx, `1 = 1`, nil)
		if err != nil {
			return err
		}
		status.LatestSuccessful, err = getCardDAVSyncRun(ctx, tx, `state = 'succeeded'`, nil)
		return err
	})
	if err != nil {
		return CardDAVSyncStatus{}, fmt.Errorf("read CardDAV sync status: %w", err)
	}
	return status, nil
}

func (s *Store) ListCardDAVSyncRunsContext(
	ctx context.Context, limit int, beforeID *int64,
) ([]CardDAVSyncRun, error) {
	if limit == 0 {
		limit = cardDAVSyncRunDefaultLimit
	}
	if limit < 1 || limit > cardDAVSyncRunMaxLimit || (beforeID != nil && *beforeID <= 0) {
		return nil, fmt.Errorf("%w: limit must be 1-100 and before ID positive", ErrCardDAVSyncRunInvalid)
	}
	query := `SELECT ` + cardDAVSyncRunColumns + ` FROM carddav_sync_runs`
	args := make([]any, 0, 2)
	if beforeID != nil {
		query += ` WHERE id < ?`
		args = append(args, *beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV sync runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]CardDAVSyncRun, 0, limit)
	for rows.Next() {
		run, scanErr := scanCardDAVSyncRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan CardDAV sync run: %w", scanErr)
		}
		runs = append(runs, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate CardDAV sync runs: %w", err)
	}
	return runs, nil
}

// RecoverCardDAVSyncRunsContext terminalizes runs whose owning process ended
// before recording a terminal outcome. Call once during daemon startup after
// schema initialization and before accepting new CardDAV work.
func (s *Store) RecoverCardDAVSyncRunsContext(ctx context.Context) (int64, error) {
	var recovered int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE carddav_sync_runs SET
			state = 'failed', finished_at = %s, error_code = ?, error_message = ?
			WHERE state = 'running'`, s.dialect.Now()),
			cardDAVSyncRunRestartedErrorCode, cardDAVSyncRunRestartedError)
		if err != nil {
			return fmt.Errorf("recover CardDAV sync runs: %w", err)
		}
		recovered, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count recovered CardDAV sync runs: %w", err)
		}
		return pruneCardDAVSyncRuns(ctx, tx)
	})
	return recovered, err
}

func validateCardDAVSyncRunFinish(input CardDAVSyncRunFinish) (string, string, error) {
	if input.State != CardDAVSyncRunSucceeded && input.State != CardDAVSyncRunFailed &&
		input.State != CardDAVSyncRunCancelled && input.State != CardDAVSyncRunPartial {
		return "", "", fmt.Errorf("%w: state must be terminal", ErrCardDAVSyncRunInvalid)
	}
	if input.Books < 0 || input.Created < 0 || input.Updated < 0 || input.Removed < 0 {
		return "", "", fmt.Errorf("%w: counters must be nonnegative", ErrCardDAVSyncRunInvalid)
	}
	code := strings.TrimSpace(input.ErrorCode)
	message := strings.TrimSpace(strings.ToValidUTF8(input.ErrorMessage, "�"))
	if input.State == CardDAVSyncRunSucceeded {
		if code != "" || message != "" {
			return "", "", fmt.Errorf("%w: successful runs cannot have errors", ErrCardDAVSyncRunInvalid)
		}
		return "", "", nil
	}
	if !cardDAVSyncRunErrorCodePattern.MatchString(code) {
		return "", "", fmt.Errorf("%w: terminal failure code is invalid", ErrCardDAVSyncRunInvalid)
	}
	if cardDAVSyncRunMessageUnsafe(code) || cardDAVSyncRunMessageUnsafe(message) {
		return cardDAVSyncRunRedactedErrorCode, cardDAVSyncRunRedactedError, nil
	}
	return code, truncateCardDAVSyncRunMessage(message), nil
}

func cardDAVSyncRunMessageUnsafe(message string) bool {
	if cardDAVSyncRunCredentialMarkerPattern.MatchString(message) {
		return true
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"authorization", "bearer ", "basic ", "password", "passwd", "secret", "cookie",
		"begin:vcard", "http://", "https://", "href", "cursor", "request body",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func truncateCardDAVSyncRunMessage(message string) string {
	if len(message) <= cardDAVSyncRunErrorMessageMax {
		return message
	}
	end := cardDAVSyncRunErrorMessageMax
	for end > 0 && !utf8.ValidString(message[:end]) {
		end--
	}
	return message[:end]
}

func pruneCardDAVSyncRuns(ctx context.Context, tx *loggedTx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM carddav_sync_runs
		WHERE state <> 'running' AND id NOT IN (
			SELECT id FROM carddav_sync_runs WHERE state <> 'running' ORDER BY id DESC LIMIT ?
		)`, cardDAVSyncRunRetention)
	if err != nil {
		return fmt.Errorf("prune CardDAV sync runs: %w", err)
	}
	return nil
}

func getCardDAVSyncRun(
	ctx context.Context, tx *loggedTx, condition string, args []any,
) (*CardDAVSyncRun, error) {
	query := `SELECT ` + cardDAVSyncRunColumns + ` FROM carddav_sync_runs WHERE ` + condition + ` ORDER BY id DESC LIMIT 1`
	run, err := scanCardDAVSyncRun(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No matching CardDAV run is a valid empty status.
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

func scanCardDAVSyncRun(sc scanner) (*CardDAVSyncRun, error) {
	var run CardDAVSyncRun
	var started requiredTimestamp
	var finished nullableTimestamp
	if err := sc.Scan(&run.ID, &run.Trigger, &run.Full, &run.State, &started, &finished,
		&run.Books, &run.Created, &run.Updated, &run.Removed, &run.ErrorCode, &run.ErrorMessage); err != nil {
		return nil, err
	}
	run.StartedAt = started.Time.UTC()
	if finished.Valid {
		value := finished.Time.UTC()
		run.FinishedAt = &value
	}
	return &run, nil
}
