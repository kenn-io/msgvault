package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ensureAccountIdentityAddressKeys brings account_identities in line with the
// persisted-comparison-key invariant: every row's address_key holds
// NormalizeIdentifierForCompare(address), case-variant duplicates of one
// logical identity are merged into a single row, and a partial unique index
// on (source_id, address_key) enforces the invariant at the schema level for
// every keyed writer, whichever process or backend it comes from.
//
// It runs on every store open, after LegacyColumnMigrations has added the
// column. The gate is the scan itself, not a run-once ledger entry: a
// previous-release binary writes rows through the column's ” default, so
// "done once" can go stale at any time, while the scan is cheap because the
// table holds only the confirmed "me" identities per source. Rows already
// carrying their derived key cost one read and no write.
//
// The repair takes the identity-mutation row lock first, mirroring
// AddAccountIdentity / RemoveAccountIdentity / MigrateLegacyIdentityConfig,
// so it serializes with every supported identity writer across processes.
// When duplicate rows collapse, the survivor keeps the earliest confirmed_at
// and the union of the signal sets, message attribution for the affected
// sources is recomputed in the same transaction, and both identity revisions
// are bumped (a collapse changes the exported identity row set and can
// change owner-participant derivation). Key-only backfills touch no derived
// state: comparison consumers still read address, so nothing they produce
// changes.
func (s *Store) ensureAccountIdentityAddressKeys(ctx context.Context) error {
	needsRepair, err := s.accountIdentityKeysNeedRepair(ctx)
	if err != nil {
		return err
	}
	if needsRepair {
		if err := s.repairAccountIdentityAddressKeys(ctx); err != nil {
			return err
		}
	}
	// The index is created after the repair because CREATE UNIQUE INDEX
	// fails while case-variant duplicates still share a derived key. It
	// lives here rather than in schema.sql/schema_pg.sql because on an
	// upgraded archive those scripts execute before the legacy-column loop
	// has added address_key. The WHERE clause exempts the '' sentinel:
	// previous-release inserts land with the column default and must not
	// collide with each other; the next open keys them through the repair
	// above. Ledgered so the DDL runs once per archive, and built through
	// the maintenance escape hatch so a lock held by a concurrent identity
	// writer cannot trip the pool-wide PostgreSQL statement timeout and
	// fail the open; IF NOT EXISTS covers a cancellation between the
	// create and the ledger write.
	return s.runOnceMigration(ctx, migrationAccountIdentityAddressKeyIndex, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				if _, err := tx.ExecContext(ctx, `
					CREATE UNIQUE INDEX IF NOT EXISTS idx_account_identities_address_key
					ON account_identities(source_id, address_key)
					WHERE address_key <> ''
				`); err != nil {
					return fmt.Errorf("create account identity address key index: %w", err)
				}
				return nil
			})
		})
}

// accountIdentityKeysNeedRepair reports whether any row's stored key differs
// from its derived key. Read-only fast path for every open; the shape check
// runs in Go because looksLikeEmail is Go-owned and deliberately has no SQL
// reimplementation.
func (s *Store) accountIdentityKeysNeedRepair(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT address, address_key FROM account_identities`)
	if err != nil {
		return false, fmt.Errorf("scan account identity keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var address, key string
		if err := rows.Scan(&address, &key); err != nil {
			return false, fmt.Errorf("scan account identity key row: %w", err)
		}
		if key != NormalizeIdentifierForCompare(address) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// accountIdentityKeyRow is one account_identities row as read by the repair.
type accountIdentityKeyRow struct {
	sourceID    int64
	address     string
	addressKey  string
	signals     string
	confirmedAt time.Time
}

// accountIdentityGroupKey identifies one logical identity: a source and the
// derived comparison key its rows share.
type accountIdentityGroupKey struct {
	sourceID int64
	key      string
}

// repairAccountIdentityAddressKeys runs through the maintenance escape hatch:
// a duplicate collapse refreshes source-wide message attribution, whose cost
// scales with archive size, and the ordinary pool-wide PostgreSQL statement
// timeout would cancel it on a large upgraded source and fail every
// subsequent open. The identity-mutation lock is taken first, matching the
// lock order of every other identity writer.
func (s *Store) repairAccountIdentityAddressKeys(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		// Re-read under the lock: the lock-free fast path may have raced a
		// writer, and the repair must act on the committed state it now owns.
		all, err := readAccountIdentityKeyRows(ctx, tx)
		if err != nil {
			return err
		}

		groups := make(map[accountIdentityGroupKey][]accountIdentityKeyRow)
		order := make([]accountIdentityGroupKey, 0, len(all))
		for _, row := range all {
			gk := accountIdentityGroupKey{
				sourceID: row.sourceID,
				key:      NormalizeIdentifierForCompare(row.address),
			}
			if _, seen := groups[gk]; !seen {
				order = append(order, gk)
			}
			groups[gk] = append(groups[gk], row)
		}

		collapsed := false
		collapsedSources := make(map[int64]struct{})
		for _, gk := range order {
			group := groups[gk]
			if len(group) == 1 && group[0].addressKey == gk.key {
				continue
			}
			survivor := pickAccountIdentitySurvivor(group, gk.key)
			mergedSignals := ""
			earliest := survivor.confirmedAt
			for _, row := range group {
				for signal := range strings.SplitSeq(row.signals, ",") {
					if signal != "" {
						mergedSignals = mergeSignalSet(mergedSignals, signal)
					}
				}
				if row.confirmedAt.Before(earliest) {
					earliest = row.confirmedAt
				}
			}
			for _, row := range group {
				if row.address == survivor.address {
					continue
				}
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM account_identities WHERE source_id = ? AND address = ?`,
					row.sourceID, row.address,
				); err != nil {
					return fmt.Errorf("collapse duplicate account identity: %w", err)
				}
				collapsed = true
				collapsedSources[row.sourceID] = struct{}{}
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE account_identities
				 SET address_key = ?, source_signal = ?, confirmed_at = ?
				 WHERE source_id = ? AND address = ?`,
				gk.key, mergedSignals, earliest, survivor.sourceID, survivor.address,
			); err != nil {
				return fmt.Errorf("backfill account identity key: %w", err)
			}
		}

		if !collapsed {
			return nil
		}
		if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		if err := s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		for sourceID := range collapsedSources {
			if err := refreshSourceMessageAttributionContext(ctx, tx, sourceID, ""); err != nil {
				return fmt.Errorf("refresh attribution after identity collapse (source=%d): %w", sourceID, err)
			}
		}
		return nil
	})
}

func readAccountIdentityKeyRows(
	ctx context.Context, tx *loggedTx,
) ([]accountIdentityKeyRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_id, address, address_key, source_signal, confirmed_at
		FROM account_identities`)
	if err != nil {
		return nil, fmt.Errorf("read account identity rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []accountIdentityKeyRow
	for rows.Next() {
		var row accountIdentityKeyRow
		if err := rows.Scan(
			&row.sourceID, &row.address, &row.addressKey,
			&row.signals, &row.confirmedAt,
		); err != nil {
			return nil, fmt.Errorf("scan account identity row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// pickAccountIdentitySurvivor chooses which duplicate row keeps the logical
// identity. A row already carrying the correct key wins (it is the one the
// unique index vouches for and the one keyed lookups have been matching);
// otherwise the earliest-confirmed row wins, with the lexicographically
// smallest address as a deterministic tie-break.
func pickAccountIdentitySurvivor(
	group []accountIdentityKeyRow, want string,
) accountIdentityKeyRow {
	sorted := make([]accountIdentityKeyRow, len(group))
	copy(sorted, group)
	sort.Slice(sorted, func(i, j int) bool {
		if (sorted[i].addressKey == want) != (sorted[j].addressKey == want) {
			return sorted[i].addressKey == want
		}
		if !sorted[i].confirmedAt.Equal(sorted[j].confirmedAt) {
			return sorted[i].confirmedAt.Before(sorted[j].confirmedAt)
		}
		return sorted[i].address < sorted[j].address
	})
	return sorted[0]
}
