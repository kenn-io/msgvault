package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const accountIdentityRevisionKey = "account_identity_revision"

// AccountIdentityRevision returns the current account-identity revision (0
// if never bumped). Unlike IdentityRevision (which also bumps on plain
// participant link/unlink and covers the cheap owner_participants/
// participant_clusters refresh), this revision increments only on identity
// mutations that invalidate baked message data: AddAccountIdentity or
// RemoveAccountIdentity actually changing which participants are owners for
// a source, and participant merges (MergeParticipants, mergeParticipant)
// repointing messages.sender_id. Either invalidates the is_from_me flag
// baked into every message Parquet shard at export time, so callers use
// this revision to detect when a full cache rebuild (not the lightweight
// identity-only refresh) is required.
func (s *Store) AccountIdentityRevision() (int64, error) {
	return readAccountIdentityRevision(s.db)
}

// readAccountIdentityRevision reads the archive_metadata account-identity
// revision through q (0 if the row does not exist yet), mirroring
// readIdentityRevision in participant_links.go.
func readAccountIdentityRevision(q rowQuerier) (int64, error) {
	var value string
	err := q.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, accountIdentityRevisionKey,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read account identity revision: %w", err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse account identity revision %q: %w", value, err)
	}
	return revision, nil
}

// bumpAccountIdentityRevision increments the account-identity revision
// inside tx, seeding the row with 0 first if it does not exist yet. Follows
// bumpIdentityRevision's approach in participant_links.go. Callers are the
// identity mutations that invalidate baked message data: AddAccountIdentity,
// RemoveAccountIdentity, and the participant-merge paths (MergeParticipants,
// mergeParticipant); none of them expose the new value, so unlike
// bumpIdentityRevision this returns only an error.
func (s *Store) bumpAccountIdentityRevision(tx *loggedTx) error {
	if _, err := tx.Exec(s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		accountIdentityRevisionKey); err != nil {
		return fmt.Errorf("seed account identity revision: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE archive_metadata SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ?`,
		accountIdentityRevisionKey); err != nil {
		return fmt.Errorf("bump account identity revision: %w", err)
	}
	return nil
}

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
// Confirming a brand new (source_id, address) pair changes which
// participants are owners for the source, so it bumps both the identity
// revision (the owner_participants cache dataset depends on it) and the
// account-identity revision (the message-baked is_from_me flag depends on
// it and can only be repaired by a full cache rebuild). Merging a new
// signal into an already-confirmed address does not change that mapping,
// so it leaves both revisions untouched.
//
// Concurrency: the read-modify-write runs inside a transaction that first
// takes lockIdentityMutationTx's write lock, mirroring LinkParticipants so
// every identity mutation serializes against the others. PostgreSQL also
// takes a row-level lock on the account_identities row with
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
) error {
	return s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}

		whereAddr := match.WhereClause("address")
		var existing string
		selectSQL := `SELECT source_signal FROM account_identities
			WHERE source_id = ? AND ` + whereAddr + s.dialect.SelectForUpdate()
		err := tx.QueryRowContext(ctx, selectSQL, sourceID, match.BindValue()).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO account_identities (source_id, address, source_signal)
					VALUES (?, ?, ?)`,
				sourceID, addr, signal,
			); err != nil {
				return fmt.Errorf("insert account identity: %w", err)
			}
			// A brand new (source_id, address) pair changes owner_participants;
			// merging a signal into an existing row (the default case below)
			// does not, so only this branch bumps the revisions.
			if _, err := s.bumpIdentityRevision(tx); err != nil {
				return err
			}
			if err := s.bumpAccountIdentityRevision(tx); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("read existing source_signal: %w", err)
		default:
			merged := mergeSignalSet(existing, signal)
			if merged != existing {
				if _, err := tx.ExecContext(ctx,
					`UPDATE account_identities SET source_signal = ?
						WHERE source_id = ? AND `+whereAddr,
					merged, sourceID, match.BindValue(),
				); err != nil {
					return fmt.Errorf("update source_signal: %w", err)
				}
			}
		}
		return nil
	})
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
//
// Removing a confirmed identity changes which participants are owners
// for the source, so an actual deletion bumps both the identity revision
// (the owner_participants cache dataset depends on it) and the
// account-identity revision (the message-baked is_from_me flag depends on
// it); removing an address that was never confirmed is a no-op and leaves
// both unchanged. The delete runs inside a transaction that first takes
// lockIdentityMutationTx's write lock, mirroring LinkParticipants and
// AddAccountIdentity so every identity mutation serializes against the
// others.
func (s *Store) RemoveAccountIdentity(sourceID int64, address string) (int64, error) {
	match := newIdentifierMatch(address)
	var removed int64
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		res, err := tx.Exec(
			`DELETE FROM account_identities WHERE source_id = ? AND `+match.WhereClause("address"),
			sourceID, match.BindValue(),
		)
		if err != nil {
			return fmt.Errorf("remove account identity: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected: %w", err)
		}
		removed = n
		if n == 0 {
			return nil
		}
		if _, err := s.bumpIdentityRevision(tx); err != nil {
			return err
		}
		return s.bumpAccountIdentityRevision(tx)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
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
