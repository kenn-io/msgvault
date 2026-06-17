package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AccountIdentity is one confirmed "me" address for one source.
type AccountIdentity struct {
	SourceID     int64
	Address      string
	SourceSignal string
	ConfirmedAt  time.Time
}

// looksLikeEmail returns true for tokens that have the shape of an
// email address. Emails are matched case-insensitively in the identity
// store; other identifier shapes (phone E.164, Matrix MXIDs like
// "@user:server.org", Slack/IRC handles) preserve case. The check is:
// at least one "@" not at index 0 and the substring after the last "@"
// contains a ".". This excludes Matrix MXIDs (which start with "@")
// and bare handles, and accepts conventional emails.
func looksLikeEmail(addr string) bool {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	return strings.Contains(addr[at+1:], ".")
}

// AddAccountIdentity confirms an identifier for one source.
//
// Behavior:
//   - If (source_id, address) does not exist: insert with the given signal
//     and confirmed_at = now. An empty signal inserts an empty source_signal.
//   - If it exists and the signal is already in the row's source_signal set:
//     no-op.
//   - If it exists and the signal is not yet in the set: add it (set is kept
//     sorted alphabetically, comma-delimited). confirmed_at is NOT updated;
//     it records first confirmation.
//   - Empty signal on an existing row: no-op (no new evidence to record).
//   - All-whitespace identifier: no-op (returns nil).
//   - Comma in signal: error. Comma is reserved as the in-column delimiter.
//
// The function trims the identifier; case is preserved (the identifier
// column accommodates email, phone E.164, and synthetic identifiers like
// chat handles where case can be significant).
//
// Concurrency: the read-modify-write runs in a writer-locked transaction.
// SQLite acquires the writer lock at BEGIN IMMEDIATE so concurrent
// callers serialize. PostgreSQL takes a row-level lock with
// SELECT ... FOR UPDATE so the merge sees the latest committed value.
// On a still-empty row two callers may both fall through INSERT — the
// unique-key violation is caught by the retry loop, which then sees the
// other writer's row and merges into it.
func (s *Store) AddAccountIdentity(sourceID int64, address, signal string) error {
	addr := strings.TrimSpace(address)
	if addr == "" {
		return nil
	}
	if strings.Contains(signal, ",") {
		return fmt.Errorf("signal names cannot contain commas: %q", signal)
	}
	match := newIdentifierMatch(addr)
	ctx := context.Background()

	const maxAttempts = 5
	for range maxAttempts {
		err := s.addAccountIdentityOnce(ctx, sourceID, addr, signal, match)
		if err == nil {
			return nil
		}
		if !s.dialect.IsConflictError(err) && !s.dialect.IsBusyError(err) {
			return err
		}
	}
	return fmt.Errorf("add account identity: gave up after %d retries", maxAttempts)
}

// addAccountIdentityOnce runs one merge attempt in a writer-locked
// transaction. The caller's retry loop handles unique-violation
// (concurrent INSERT race) and busy/snapshot errors (SQLite).
func (s *Store) addAccountIdentityOnce(
	ctx context.Context, sourceID int64, addr, signal string, match identifierMatch,
) (retErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, s.dialect.BeginWriteSQL()); err != nil {
		return fmt.Errorf("begin write tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	rebind := s.dialect.Rebind
	whereAddr := match.WhereClause("address")
	var existing string
	selectSQL := `SELECT source_signal FROM account_identities
		WHERE source_id = ? AND ` + whereAddr + s.dialect.SelectForUpdate()
	err = conn.QueryRowContext(ctx, rebind(selectSQL), sourceID, match.BindValue()).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := conn.ExecContext(ctx,
			rebind(`INSERT INTO account_identities (source_id, address, source_signal)
				VALUES (?, ?, ?)`),
			sourceID, addr, signal,
		); err != nil {
			return fmt.Errorf("insert account identity: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read existing source_signal: %w", err)
	default:
		merged := mergeSignalSet(existing, signal)
		if merged != existing {
			if _, err := conn.ExecContext(ctx,
				rebind(`UPDATE account_identities SET source_signal = ?
					WHERE source_id = ? AND `+whereAddr),
				merged, sourceID, match.BindValue(),
			); err != nil {
				return fmt.Errorf("update source_signal: %w", err)
			}
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// mergeSignalSet returns the comma-joined sorted union of the existing
// signal set and the new signal. Empty strings (in either argument) are
// treated as the empty set.
func mergeSignalSet(existing, signal string) string {
	set := make(map[string]struct{})
	if existing != "" {
		for s := range strings.SplitSeq(existing, ",") {
			if s != "" {
				set[s] = struct{}{}
			}
		}
	}
	if signal != "" {
		set[signal] = struct{}{}
	}
	if len(set) == 0 {
		return ""
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// ListAccountIdentities returns all identities for one source, ordered by address.
func (s *Store) ListAccountIdentities(sourceID int64) ([]AccountIdentity, error) {
	rows, err := s.db.Query(`
		SELECT source_id, address, source_signal, confirmed_at
		FROM account_identities
		WHERE source_id = ?
		ORDER BY address
	`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list account identities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AccountIdentity
	for rows.Next() {
		var ai AccountIdentity
		if err := rows.Scan(&ai.SourceID, &ai.Address, &ai.SourceSignal, &ai.ConfirmedAt); err != nil {
			return nil, fmt.Errorf("scan account identity: %w", err)
		}
		out = append(out, ai)
	}
	return out, rows.Err()
}

// RemoveAccountIdentity deletes (source_id, address) rows that match
// under the helper's case-aware rule. Returns the number of rows
// deleted (typically 0 or 1, but can be >1 in legacy databases that
// hold case-variant duplicates pre-dating the case-folding work).
//
// Email-shaped identifiers match case-insensitively because email is
// case-insensitive in practice; this avoids the UX trap where a row
// was inserted as foo@x.com but the user types Foo@x.com on remove.
// Synthetic identifiers (Matrix MXIDs, chat handles, phone numbers)
// match case-sensitively because case can be significant there. The
// shape check is in looksLikeEmail.
func (s *Store) RemoveAccountIdentity(sourceID int64, address string) (int64, error) {
	match := newIdentifierMatch(address)
	res, err := s.db.Exec(
		`DELETE FROM account_identities WHERE source_id = ? AND `+match.WhereClause("address"),
		sourceID, match.BindValue(),
	)
	if err != nil {
		return 0, fmt.Errorf("remove account identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// GetIdentitiesForScope returns the union of confirmed identifier addresses
// across the given source IDs. Empty input returns an empty map — no global
// default; an explicit empty scope means no identity matching.
//
// Identifiers are returned with the case the user stored. Callers comparing
// against email-shaped strings should lowercase both sides at compare time.
func (s *Store) GetIdentitiesForScope(sourceIDs []int64) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(sourceIDs) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(sourceIDs))
	args := make([]any, len(sourceIDs))
	for i, id := range sourceIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT address FROM account_identities WHERE source_id IN (` +
		strings.Join(placeholders, ",") + `)`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get identities for scope: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, fmt.Errorf("scan identity address: %w", err)
		}
		out[addr] = struct{}{}
	}
	return out, rows.Err()
}
