package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	ErrDailyNoteDateRequired      = errors.New("daily note entry requires a local date")
	ErrDailyNoteBodyRequired      = errors.New("daily note entry requires a body")
	ErrDailyNoteEntryNotFound     = errors.New("daily note entry not found")
	errInvalidDailyNotePagination = errors.New("invalid daily note pagination")
)

const (
	// DailyNoteDefaultLimit is used when callers omit a positive page size.
	DailyNoteDefaultLimit = 100
	// DailyNoteMaxLimit bounds a single authored-entry page.
	DailyNoteMaxLimit = 500

	dailyNoteMaxTransactionAttempts = 5
)

// DailyNoteEntry is one human-authored entry on a calendar day, together with
// the persons it explicitly targets. It is deliberately not activity evidence.
type DailyNoteEntry struct {
	ID        int64     `json:"id"`
	LocalDate string    `json:"local_date"`
	Ordinal   int64     `json:"ordinal"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	Source    string    `json:"source"`
	SourceRef *string   `json:"source_ref,omitempty"`
	PersonIDs []int64   `json:"person_ids"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DailyNoteEntryInput struct {
	LocalDate string
	Body      string
	Author    string
	PersonIDs []int64
}

// IsValidLocalDate reports whether value is an exact ASCII YYYY-MM-DD calendar
// date. time.Parse alone accepts only ASCII digits; the round trip also pins
// the exact shape.
func IsValidLocalDate(value string) bool {
	if len(value) != len(time.DateOnly) {
		return false
	}
	for index, character := range []byte(value) {
		if index == 4 || index == 7 {
			if character != '-' {
				return false
			}
			continue
		}
		if character < '0' || character > '9' {
			return false
		}
	}
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

func (s *Store) CreateDailyNoteEntry(input DailyNoteEntryInput) (*DailyNoteEntry, error) {
	return s.CreateDailyNoteEntryContext(context.Background(), input)
}

// CreateDailyNoteEntryContext appends one entry. The allocator update is the
// first statement in each attempted transaction and serializes writers by day.
func (s *Store) CreateDailyNoteEntryContext(
	ctx context.Context, input DailyNoteEntryInput,
) (*DailyNoteEntry, error) {
	if !IsValidLocalDate(input.LocalDate) {
		return nil, ErrDailyNoteDateRequired
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		return nil, ErrDailyNoteBodyRequired
	}
	targets := slices.Clone(input.PersonIDs)
	slices.Sort(targets)
	targets = slices.Compact(targets)
	for _, personID := range targets {
		if personID <= 0 {
			return nil, ErrPersonNotFound
		}
	}

	for attempt := 1; attempt <= dailyNoteMaxTransactionAttempts; attempt++ {
		var entry *DailyNoteEntry
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			var ordinal int64
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO daily_note_day_sequences (local_date, last_ordinal)
				VALUES (?, 1)
				ON CONFLICT(local_date) DO UPDATE
				SET last_ordinal = daily_note_day_sequences.last_ordinal + 1
				RETURNING last_ordinal
			`, input.LocalDate).Scan(&ordinal); err != nil {
				return fmt.Errorf("allocate daily note ordinal: %w", err)
			}

			for _, personID := range targets {
				var lockedID int64
				err := tx.QueryRowContext(ctx,
					`SELECT id FROM persons WHERE id = ?`+s.dialect.SelectForUpdate(),
					personID).Scan(&lockedID)
				if errors.Is(err, sql.ErrNoRows) {
					return ErrPersonNotFound
				}
				if err != nil {
					return fmt.Errorf("verify daily note target: %w", err)
				}
			}

			var id int64
			if err := tx.QueryRowContext(ctx, fmt.Sprintf(`
				INSERT INTO daily_note_entries (
					local_date, ordinal, body, author, source, source_ref, created_at, updated_at
				) VALUES (?, ?, ?, ?, 'user', NULL, %s, %s)
				RETURNING id
			`, s.dialect.Now(), s.dialect.Now()),
				input.LocalDate, ordinal, body, input.Author).Scan(&id); err != nil {
				return fmt.Errorf("insert daily note entry: %w", err)
			}
			for _, personID := range targets {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO daily_note_entry_persons (entry_id, person_id) VALUES (?, ?)
				`, id, personID); err != nil {
					return fmt.Errorf("insert daily note target: %w", err)
				}
			}
			var err error
			entry, err = loadDailyNoteEntryTx(ctx, tx, id)
			return err
		})
		if err == nil {
			return entry, nil
		}
		if attempt == dailyNoteMaxTransactionAttempts || !dailyNoteRetryable(ctx, s, err) {
			return nil, err
		}
	}
	return nil, errors.New("daily note transaction attempts exhausted")
}

type sqlStateError interface {
	SQLState() string
}

func dailyNoteRetryable(ctx context.Context, s *Store, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if s.dialect.DriverName() != "pgx" {
		return s.dialect.IsBusyError(err)
	}
	var state sqlStateError
	if !errors.As(err, &state) {
		return false
	}
	return state.SQLState() == "40P01" || state.SQLState() == "40001"
}

func loadDailyNoteEntryTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*DailyNoteEntry, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.local_date, e.ordinal, e.body, e.author, e.source,
		       e.source_ref, e.created_at, e.updated_at, target.person_id
		FROM daily_note_entries e
		LEFT JOIN daily_note_entry_persons target ON target.entry_id = e.id
		WHERE e.id = ?
		ORDER BY target.person_id
	`, id)
	if err != nil {
		return nil, fmt.Errorf("read daily note entry: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries, err := scanDailyNoteRows(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, ErrDailyNoteEntryNotFound
	}
	return &entries[0], nil
}

func (s *Store) ListDailyNoteEntriesContext(
	ctx context.Context, localDate string, limit, offset int,
) ([]DailyNoteEntry, error) {
	if !IsValidLocalDate(localDate) {
		return nil, ErrDailyNoteDateRequired
	}
	limit, err := normalizeDailyNotePage(limit, offset)
	if err != nil {
		return nil, err
	}
	return s.listDailyNoteEntries(ctx, `
		WITH page AS (
			SELECT e.id, e.local_date, e.ordinal, e.body, e.author, e.source,
			       e.source_ref, e.created_at, e.updated_at
			FROM daily_note_entries e
			WHERE e.local_date = ?
			ORDER BY e.ordinal, e.id
			LIMIT ? OFFSET ?
		)
		SELECT page.id, page.local_date, page.ordinal, page.body, page.author,
		       page.source, page.source_ref, page.created_at, page.updated_at,
		       target.person_id
		FROM page
		LEFT JOIN daily_note_entry_persons target ON target.entry_id = page.id
		ORDER BY page.ordinal, page.id, target.person_id
	`, localDate, limit, offset)
}

func (s *Store) ListDailyNoteEntriesForPersonContext(
	ctx context.Context, personID int64, localDate string, limit, offset int,
) ([]DailyNoteEntry, error) {
	if personID <= 0 {
		return nil, ErrPersonNotFound
	}
	if localDate != "" && !IsValidLocalDate(localDate) {
		return nil, ErrDailyNoteDateRequired
	}
	limit, err := normalizeDailyNotePage(limit, offset)
	if err != nil {
		return nil, err
	}
	return s.listDailyNoteEntries(ctx, `
		WITH page AS (
			SELECT e.id, e.local_date, e.ordinal, e.body, e.author, e.source,
			       e.source_ref, e.created_at, e.updated_at
			FROM daily_note_entries e
			WHERE EXISTS (
				SELECT 1 FROM daily_note_entry_persons filter_target
				WHERE filter_target.entry_id = e.id AND filter_target.person_id = ?
			)
			  AND (? = '' OR e.local_date = ?)
			ORDER BY e.local_date, e.ordinal, e.id
			LIMIT ? OFFSET ?
		)
		SELECT page.id, page.local_date, page.ordinal, page.body, page.author,
		       page.source, page.source_ref, page.created_at, page.updated_at,
		       target.person_id
		FROM page
		LEFT JOIN daily_note_entry_persons target ON target.entry_id = page.id
		ORDER BY page.local_date, page.ordinal, page.id, target.person_id
	`, personID, localDate, localDate, limit, offset)
}

func normalizeDailyNotePage(limit, offset int) (int, error) {
	if offset < 0 {
		return 0, errInvalidDailyNotePagination
	}
	if limit <= 0 {
		limit = DailyNoteDefaultLimit
	}
	if limit > DailyNoteMaxLimit {
		limit = DailyNoteMaxLimit
	}
	return limit, nil
}

func (s *Store) listDailyNoteEntries(
	ctx context.Context, query string, args ...any,
) ([]DailyNoteEntry, error) {
	rows, err := queryDailyNoteRowsDB(ctx, s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list daily note entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanDailyNoteRows(rows)
}

func queryDailyNoteRowsDB(
	ctx context.Context,
	db *loggedDB,
	query string,
	args ...any,
) (rowsScanner, error) {
	return db.QueryContext(ctx, query, args...)
}

func queryDailyNoteRowsTx(
	ctx context.Context,
	tx *loggedTx,
	query string,
	args ...any,
) (rowsScanner, error) {
	return tx.QueryContext(ctx, query, args...)
}

type dailyNoteRow struct {
	id, ordinal                     sql.NullInt64
	localDate, body, author, source sql.NullString
	sourceRef                       sql.NullString
	createdAt, updatedAt            nullableTimestamp
	personID                        sql.NullInt64
}

func (row *dailyNoteRow) destinations() []any {
	return []any{
		&row.id, &row.localDate, &row.ordinal,
		&row.body, &row.author, &row.source,
		&row.sourceRef, &row.createdAt, &row.updatedAt,
		&row.personID,
	}
}

func (row *dailyNoteRow) build() (DailyNoteEntry, bool, error) {
	if !row.id.Valid {
		return DailyNoteEntry{}, false, nil
	}
	if !row.ordinal.Valid || !row.localDate.Valid || !row.body.Valid ||
		!row.author.Valid || !row.source.Valid ||
		!row.createdAt.Valid || !row.updatedAt.Valid {
		return DailyNoteEntry{}, false,
			errors.New("scan daily note entry: required column is NULL")
	}
	entry := DailyNoteEntry{
		ID:        row.id.Int64,
		LocalDate: row.localDate.String,
		Ordinal:   row.ordinal.Int64,
		Body:      row.body.String,
		Author:    row.author.String,
		Source:    row.source.String,
		CreatedAt: row.createdAt.Time,
		UpdatedAt: row.updatedAt.Time,
		PersonIDs: []int64{},
	}
	if row.sourceRef.Valid {
		entry.SourceRef = &row.sourceRef.String
	}
	return entry, true, nil
}

func scanDailyNoteRow(
	scanner scanner,
	suffix ...any,
) (dailyNoteRow, error) {
	var row dailyNoteRow
	destinations := make([]any, 0, 10+len(suffix))
	destinations = append(destinations, row.destinations()...)
	destinations = append(destinations, suffix...)
	if err := scanner.Scan(destinations...); err != nil {
		return dailyNoteRow{}, fmt.Errorf("scan daily note entry: %w", err)
	}
	return row, nil
}

func appendDailyNoteRow(
	entries []DailyNoteEntry,
	row dailyNoteRow,
) ([]DailyNoteEntry, error) {
	entry, present, err := row.build()
	if err != nil {
		return nil, err
	}
	if !present {
		return entries, nil
	}
	if len(entries) == 0 || entries[len(entries)-1].ID != entry.ID {
		entries = append(entries, entry)
	}
	if row.personID.Valid {
		entries[len(entries)-1].PersonIDs = append(
			entries[len(entries)-1].PersonIDs, row.personID.Int64,
		)
	}
	return entries, nil
}

func scanDailyNoteRows(rows rowsScanner) ([]DailyNoteEntry, error) {
	entries := make([]DailyNoteEntry, 0)
	for rows.Next() {
		row, err := scanDailyNoteRow(rows)
		if err != nil {
			return nil, err
		}
		entries, err = appendDailyNoteRow(entries, row)
		if err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily note entries: %w", err)
	}
	return entries, nil
}

func scanDailyNotePageRows(rows rowsScanner) ([]DailyNoteEntry, int64, error) {
	entries := make([]DailyNoteEntry, 0)
	var total int64
	for rows.Next() {
		var rowTotal int64
		var sentinel int
		row, err := scanDailyNoteRow(rows, &rowTotal, &sentinel)
		if err != nil {
			return nil, 0, err
		}
		total = rowTotal
		if sentinel == 0 {
			entries, err = appendDailyNoteRow(entries, row)
			if err != nil {
				return nil, 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate daily note page: %w", err)
	}
	return entries, total, nil
}

func (s *Store) DeleteDailyNoteEntryContext(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM daily_note_entries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete daily note entry: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check daily note deletion: %w", err)
	}
	if affected == 0 {
		return ErrDailyNoteEntryNotFound
	}
	return nil
}
