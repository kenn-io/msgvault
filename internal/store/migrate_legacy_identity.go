package store

import (
	"database/sql"
	"fmt"
	"strings"
)

const migrationLegacyIdentity = "legacy_identity_to_per_account"

// MigrateLegacyIdentityConfig migrates a list of legacy global identity
// addresses into per-account confirmed records. It runs at most once:
// subsequent calls are no-ops, marked by the
// "legacy_identity_to_per_account" entry in applied_migrations. An
// empty or blank-only address list still marks the migration applied so
// a later config change does not re-run the migration unexpectedly.
//
// Returns (applied bool, deferred bool, sourceCount int, addressCount int, err error).
//
//	applied:      true if this call performed the migration; false if
//	              already applied or no addresses to migrate.
//	deferred:     true when legacy addresses are configured but no
//	              sources exist yet, so the migration is parked until
//	              the user adds an account. Distinguishable from the
//	              "already applied" / "no addresses" no-ops.
//	sourceCount:  number of accounts that received identity records.
//	addressCount: number of distinct addresses migrated (per source).
//
// Migration semantics: every existing source receives a copy of every
// legacy address. After this call, the legacy [identity] config block
// is no longer load-bearing; the dedup engine should read from
// account_identities instead.
func (s *Store) MigrateLegacyIdentityConfig(addresses []string) (applied, deferred bool, sourceCount, addressCount int, err error) {
	already, err := s.IsMigrationApplied(migrationLegacyIdentity)
	if err != nil {
		return false, false, 0, 0, err
	}
	if already {
		return false, false, 0, 0, nil
	}

	// Normalize addresses: trim whitespace, drop empties, deduplicate
	// using the same case-aware rule as the rest of the identity
	// subsystem (NormalizeIdentifierForCompare). Preserves first-seen
	// casing for storage so synthetic identifiers (Matrix MXIDs, chat
	// handles) keep their original case.
	seen := make(map[string]struct{}, len(addresses))
	var normalized []string
	for _, addr := range addresses {
		a := strings.TrimSpace(addr)
		if a == "" {
			continue
		}
		key := NormalizeIdentifierForCompare(a)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, a)
	}

	if len(normalized) == 0 {
		if err := s.MarkMigrationApplied(migrationLegacyIdentity); err != nil {
			return false, false, 0, 0, err
		}
		return false, false, 0, 0, nil
	}

	sources, err := s.ListSources("")
	if err != nil {
		return false, false, 0, 0, fmt.Errorf("list sources for identity migration: %w", err)
	}

	// If the user has legacy [identity] addresses configured but no
	// sources exist yet (typical at init-db time, or before the first
	// `add-account`), defer the migration. Marking it applied now would
	// permanently drop the addresses on the floor: the next account the
	// user adds would never receive them. Leave the sentinel unmarked
	// and let the next command run after a source exists pick it up.
	//
	// Report the post-normalization address count so the deferred
	// notice doesn't overstate (raw input may include blanks/dupes).
	if len(sources) == 0 {
		return false, true, 0, len(normalized), nil
	}

	// Legacy [identity] block holds email-shaped addresses only. If no
	// existing source has an email-shaped identity column, defer rather
	// than mark the migration applied — otherwise the addresses get
	// permanently dropped on the floor when the user later adds an
	// email source. Same defer contract as the no-sources branch above.
	eligibleSources := 0
	for _, src := range sources {
		if sourceTypeUsesEmailIdentity(src.SourceType) {
			eligibleSources++
		}
	}
	if eligibleSources == 0 {
		return false, true, 0, len(normalized), nil
	}

	if err := s.withTx(func(tx *loggedTx) error {
		var appliedMarker string
		err := tx.QueryRow(
			`SELECT name FROM applied_migrations WHERE name = ?`,
			migrationLegacyIdentity,
		).Scan(&appliedMarker)
		switch {
		case err == nil:
			return nil
		case err != sql.ErrNoRows:
			return fmt.Errorf("check migration %q in tx: %w", migrationLegacyIdentity, err)
		}

		for _, src := range sources {
			// Legacy [identity] block holds email-shaped addresses only.
			// Skip non-email source types (whatsapp, imessage, sms,
			// google_voice*) so phone-keyed sources don't get email
			// identities written to them, which would distort dedup
			// sent-copy detection.
			if !sourceTypeUsesEmailIdentity(src.SourceType) {
				continue
			}
			for _, addr := range normalized {
				// Comparison rule (email-shaped → case-insensitive;
				// everything else → case-sensitive) is shared with
				// AddAccountIdentity via identifierMatch — see
				// identifier_match.go.
				match := newIdentifierMatch(addr)
				var existing string
				qerr := tx.QueryRow(
					`SELECT source_signal FROM account_identities
					 WHERE source_id = ? AND `+match.WhereClause("address"),
					src.ID, match.BindValue(),
				).Scan(&existing)
				switch {
				case qerr == sql.ErrNoRows:
					_, txErr := tx.Exec(
						`INSERT INTO account_identities (source_id, address, source_signal)
						 VALUES (?, ?, ?)`,
						src.ID, addr, "config_migration",
					)
					if txErr != nil {
						return fmt.Errorf("insert identity (source=%d, addr=%s): %w", src.ID, addr, txErr)
					}
				case qerr != nil:
					return fmt.Errorf("read existing identity (source=%d, addr=%s): %w", src.ID, addr, qerr)
				default:
					merged := mergeSignalSet(existing, "config_migration")
					if merged != existing {
						_, uerr := tx.Exec(
							`UPDATE account_identities
							 SET source_signal = ?
							 WHERE source_id = ? AND `+match.WhereClause("address"),
							merged, src.ID, match.BindValue(),
						)
						if uerr != nil {
							return fmt.Errorf("update identity (source=%d, addr=%s): %w", src.ID, addr, uerr)
						}
					}
				}
			}
		}

		_, txErr := tx.Exec(
			s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`),
			migrationLegacyIdentity,
		)
		return txErr
	}); err != nil {
		return false, false, 0, 0, fmt.Errorf("migrate legacy identity config: %w", err)
	}

	return true, false, eligibleSources, len(normalized), nil
}

// StartupMigrationResult describes the outcome of RunStartupMigrations
// so callers can log accurately. Notice is the user-facing string to
// print to stderr (empty when nothing happened).
type StartupMigrationResult struct {
	// Applied is true when the legacy identity migration actually
	// inserted per-account identity rows on this call.
	Applied bool
	// Deferred is true when the migration was parked because no
	// source exists yet; addresses remain in the legacy config and
	// will migrate on the next command after a source is created.
	Deferred bool
	// SourceCount is the number of sources the addresses were
	// distributed across (only meaningful when Applied).
	SourceCount int
	// AddressCount is the post-normalization count of legacy
	// addresses (meaningful for both Applied and Deferred).
	AddressCount int
	// Notice is the user-facing string the caller should print.
	// Empty when there was nothing to report.
	Notice string
}

// RunStartupMigrations runs all one-time data migrations that should execute
// on every command launch. It is idempotent: already-applied migrations are
// skipped. legacyIdentityAddresses comes from cfg.Identity.Addresses.
//
// The returned StartupMigrationResult's Notice field is non-empty when the
// migration was performed (Applied) or when legacy addresses are parked
// because no source exists yet (Deferred). Caller should print Notice to
// stderr. The structured fields let the caller log the deferred and applied
// paths distinctly.
func (s *Store) RunStartupMigrations(legacyIdentityAddresses []string) (StartupMigrationResult, error) {
	applied, deferred, sources, addrs, err := s.MigrateLegacyIdentityConfig(legacyIdentityAddresses)
	if err != nil {
		return StartupMigrationResult{}, err
	}
	res := StartupMigrationResult{
		Applied:      applied,
		Deferred:     deferred,
		SourceCount:  sources,
		AddressCount: addrs,
	}
	switch {
	case deferred:
		res.Notice = fmt.Sprintf(
			"Notice: legacy [identity] config has %d address(es) but no accounts exist yet.\n"+
				"The migration will run on the next command after you add an account\n"+
				"(e.g. 'msgvault add-account ...').",
			addrs,
		)
	case applied:
		res.Notice = fmt.Sprintf(
			"Migrated legacy [identity] config to per-account identities (%d addresses across %d accounts).\n"+
				"Run 'msgvault identity list' to review per-account identities;\n"+
				"the [identity] block in config.toml is no longer used.",
			addrs, sources,
		)
	}
	return res, nil
}

// sourceTypeUsesEmailIdentity reports whether a source type's identity
// column holds email-shaped addresses. Used by the legacy [identity]
// migration to skip phone/handle-keyed sources (whatsapp, imessage,
// google_voice*, sms) so email addresses don't get written to them.
func sourceTypeUsesEmailIdentity(sourceType string) bool {
	switch sourceType {
	case "gmail", "imap", "o365", "mbox", "hey", "apple-mail":
		return true
	}
	return false
}
