package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	migrationDirectoryProjectionV1 = "directory_projection_v1"
	personNamesTableName           = "person_names"
)

// ensureDirectoryProjectionInfrastructure installs the durable, Go-canonical
// Directory projection. Base-table triggers only enqueue changed person IDs;
// Store transactions refresh those IDs with the canonical Go representation
// before commit, which keeps SQLite and PostgreSQL behavior identical.
func (s *Store) ensureDirectoryProjectionInfrastructure(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS directory_people (
			person_id BIGINT PRIMARY KEY,
			order_key TEXT NOT NULL,
			contact_state TEXT NOT NULL,
			primary_channel TEXT NOT NULL,
			last_contact_key TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS directory_person_tokens (
			person_id BIGINT NOT NULL,
			token_key TEXT NOT NULL,
			PRIMARY KEY (person_id, token_key)
		)`,
		`CREATE TABLE IF NOT EXISTS directory_person_token_deletes (
			person_id BIGINT NOT NULL,
			delete_key TEXT NOT NULL,
			PRIMARY KEY (person_id, delete_key)
		)`,
		`CREATE TABLE IF NOT EXISTS directory_person_filters (
			person_id BIGINT NOT NULL,
			filter_kind TEXT NOT NULL,
			value_key TEXT NOT NULL,
			PRIMARY KEY (person_id, filter_kind, value_key)
		)`,
		`CREATE TABLE IF NOT EXISTS directory_projection_dirty (
			person_id BIGINT PRIMARY KEY
		)`,
		`CREATE INDEX IF NOT EXISTS idx_directory_people_order
			ON directory_people(order_key, person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_directory_people_contact
			ON directory_people(contact_state, primary_channel, order_key, person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_directory_person_tokens_lookup
			ON directory_person_tokens(token_key, person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_directory_person_token_deletes_lookup
			ON directory_person_token_deletes(delete_key, person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_directory_person_filters_lookup
			ON directory_person_filters(filter_kind, value_key, person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_directory_people_last_contact
			ON directory_people(last_contact_key, order_key, person_id)`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create directory projection: %w", err)
		}
	}
	if err := s.installDirectoryProjectionTriggers(ctx); err != nil {
		return err
	}
	s.directoryProjectionReady = true
	return nil
}

func (s *Store) installDirectoryProjectionTriggers(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return s.installPostgresDirectoryProjectionTriggers(ctx)
	}
	// The projection is installed once, so every trigger is created IF NOT
	// EXISTS. The migration backfill that follows marks every person dirty, so
	// a base write racing the first install is refreshed anyway.
	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_person_insert AFTER INSERT ON persons BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_person_update AFTER UPDATE ON persons BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_person_delete AFTER DELETE ON persons BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_names_insert AFTER INSERT ON person_names BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_names_update AFTER UPDATE ON person_names BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING;
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_names_delete AFTER DELETE ON person_names BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_points_insert AFTER INSERT ON person_contact_points BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_points_update AFTER UPDATE ON person_contact_points BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING;
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_points_delete AFTER DELETE ON person_contact_points BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_categories_insert AFTER INSERT ON person_categories BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_categories_update AFTER UPDATE ON person_categories BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING;
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_categories_delete AFTER DELETE ON person_categories BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_employments_insert AFTER INSERT ON employments BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_employments_update AFTER UPDATE ON employments BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING;
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_employments_delete AFTER DELETE ON employments BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_contact_state_insert AFTER INSERT ON person_contact_state BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_contact_state_update AFTER UPDATE ON person_contact_state BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING;
			INSERT INTO directory_projection_dirty(person_id) VALUES (NEW.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_contact_state_delete AFTER DELETE ON person_contact_state BEGIN
			INSERT INTO directory_projection_dirty(person_id) VALUES (OLD.person_id) ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_organizations_insert AFTER INSERT ON organizations BEGIN
			INSERT INTO directory_projection_dirty(person_id)
			SELECT person_id FROM employments WHERE organization_id = NEW.id
			ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_organizations_update AFTER UPDATE ON organizations BEGIN
			INSERT INTO directory_projection_dirty(person_id)
			SELECT person_id FROM employments WHERE organization_id = NEW.id
			ON CONFLICT(person_id) DO NOTHING; END`,
		`CREATE TRIGGER IF NOT EXISTS directory_dirty_organizations_delete AFTER DELETE ON organizations BEGIN
			INSERT INTO directory_projection_dirty(person_id)
			SELECT person_id FROM employments WHERE organization_id = OLD.id
			ON CONFLICT(person_id) DO NOTHING; END`,
	}
	for _, trigger := range triggers {
		if _, err := s.db.ExecContext(ctx, trigger); err != nil {
			return fmt.Errorf("install SQLite directory projection trigger: %w", err)
		}
	}
	return nil
}

func (s *Store) installPostgresDirectoryProjectionTriggers(ctx context.Context) error {
	statements := []string{
		`CREATE OR REPLACE FUNCTION directory_projection_mark_dirty() RETURNS trigger AS $$
			BEGIN
				IF TG_OP = 'UPDATE' THEN
					INSERT INTO directory_projection_dirty(person_id)
					VALUES ((to_jsonb(OLD)->>TG_ARGV[0])::bigint), ((to_jsonb(NEW)->>TG_ARGV[0])::bigint)
					ON CONFLICT DO NOTHING;
				ELSIF TG_OP = 'DELETE' THEN
					INSERT INTO directory_projection_dirty(person_id)
					VALUES ((to_jsonb(OLD)->>TG_ARGV[0])::bigint) ON CONFLICT DO NOTHING;
				ELSE
					INSERT INTO directory_projection_dirty(person_id)
					VALUES ((to_jsonb(NEW)->>TG_ARGV[0])::bigint) ON CONFLICT DO NOTHING;
				END IF;
				RETURN NULL;
			END $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION directory_projection_mark_organization_dirty() RETURNS trigger AS $$
			DECLARE organization bigint;
			BEGIN
				IF TG_OP = 'DELETE' THEN organization := OLD.id; ELSE organization := NEW.id; END IF;
				INSERT INTO directory_projection_dirty(person_id)
				SELECT person_id FROM employments WHERE organization_id = organization
				ON CONFLICT DO NOTHING;
				RETURN NULL;
			END $$ LANGUAGE plpgsql`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install PostgreSQL directory projection function: %w", err)
		}
	}
	for _, tableAndColumn := range [][2]string{
		{"persons", "id"}, {personNamesTableName, personMergePersonIDColumn}, {personContactPointsTableName, personMergePersonIDColumn},
		{"person_categories", personMergePersonIDColumn}, {"employments", personMergePersonIDColumn}, {"person_contact_state", personMergePersonIDColumn},
	} {
		name := "directory_dirty_" + tableAndColumn[0]
		if _, err := s.db.ExecContext(ctx, `CREATE OR REPLACE TRIGGER `+name+` AFTER INSERT OR UPDATE OR DELETE ON `+tableAndColumn[0]+`
			FOR EACH ROW EXECUTE FUNCTION directory_projection_mark_dirty('`+tableAndColumn[1]+`')`); err != nil {
			return fmt.Errorf("install PostgreSQL directory projection trigger: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx, `CREATE OR REPLACE TRIGGER directory_dirty_organizations AFTER INSERT OR UPDATE OR DELETE ON organizations
		FOR EACH ROW EXECUTE FUNCTION directory_projection_mark_organization_dirty()`)
	if err != nil {
		return fmt.Errorf("install PostgreSQL organization projection trigger: %w", err)
	}
	return nil
}

func (s *Store) refreshDirectoryProjectionsContext(ctx context.Context) error {
	if !s.directoryProjectionReady {
		return nil
	}
	if s.readOnly {
		dirty, err := directoryProjectionDirty(ctx, s.db)
		if err != nil {
			return err
		}
		if dirty {
			return ErrDirectoryProjectionStale
		}
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return s.refreshDirectoryProjectionsTx(ctx, tx)
	})
}

func directoryProjectionDirty(ctx context.Context, querier contextRowQuerier) (bool, error) {
	var dirty bool
	if err := querier.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM directory_projection_dirty)`).Scan(&dirty); err != nil {
		return false, fmt.Errorf("check stale directory projection: %w", err)
	}
	return dirty, nil
}

func (s *Store) backfillDirectoryProjectionContext(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO directory_projection_dirty(person_id) SELECT id FROM persons`,
		)); err != nil {
			return fmt.Errorf("seed directory projection backfill: %w", err)
		}
		return s.refreshDirectoryProjectionsTx(ctx, tx)
	})
}

// withFreshDirectorySnapshotContext makes the freshness decision and every
// Directory result read from one read-only repeatable-read snapshot. A dirty
// writable view closes that snapshot and repairs the projection in a separate
// retried transaction whose first operation is the dirty-row claim. This keeps
// clean reads lock-free and prevents a deferred SQLite snapshot from failing
// when it tries to upgrade after another writer commits.
func (s *Store) withFreshDirectorySnapshotContext(
	ctx context.Context, fn func(tx *loggedTx) error,
) error {
	for {
		refresh := false
		opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
		err := s.withTxOptionsContext(ctx, opts, func(tx *loggedTx) error {
			dirty, err := directoryProjectionDirty(ctx, tx)
			if err != nil {
				return err
			}
			if dirty {
				if s.readOnly {
					return ErrDirectoryProjectionStale
				}
				refresh = true
				return nil
			}
			return fn(tx)
		})
		if err != nil || !refresh {
			return err
		}
		if err := retryBusyWriteErr(ctx, s, "refresh Directory projection", func() error {
			return s.refreshDirectoryProjectionsContext(ctx)
		}); err != nil {
			return err
		}
	}
}

// RefreshDirectoryProjectionContext flushes dirty projection rows after a
// caller-owned raw write, before a read-only Store serves Directory results.
func (s *Store) RefreshDirectoryProjectionContext(ctx context.Context) error {
	if s.readOnly {
		return ErrDirectoryProjectionStale
	}
	return s.refreshDirectoryProjectionsContext(ctx)
}

// refreshDirectoryProjectionsTx claims every queued person and rebuilds
// their projection rows. Claiming is a DELETE ... RETURNING so it is the
// transaction's first write: SQLite takes the writer lock before any read
// (a deferred transaction that reads first cannot upgrade once another
// writer commits), and a concurrent PostgreSQL refresh waits on the row
// locks and then finds nothing left to claim instead of rebuilding the same
// person and colliding on the projection primary keys.
func (s *Store) refreshDirectoryProjectionsTx(ctx context.Context, tx *loggedTx) error {
	if !s.directoryProjectionReady {
		return nil
	}
	ids, err := claimDirtyDirectoryPeopleTx(ctx, tx)
	if err != nil {
		return err
	}
	return s.refreshDirectoryProjectionIDsTx(ctx, tx, ids)
}

// refreshDirectoryProjectionsBeforeCommitTx keeps ordinary writes independent
// from concurrent profile-table DDL, snapshot tests, and other refreshes.
// PostgreSQL aborts a transaction on a lock or serialization failure, so the
// refresh runs behind a savepoint: contention rolls back only the derived
// projection work and leaves its dirty rows for the next refresh.
func (s *Store) refreshDirectoryProjectionsBeforeCommitTx(ctx context.Context, tx *loggedTx) error {
	if !s.directoryProjectionReady {
		return nil
	}
	dirty, err := directoryProjectionDirty(ctx, tx)
	if err != nil || !dirty {
		return err
	}
	if !s.IsPostgreSQL() {
		return s.refreshDirectoryProjectionsTx(ctx, tx)
	}

	const savepoint = "directory_projection_refresh"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("create Directory projection refresh savepoint: %w", err)
	}
	_, refreshErr := tx.ExecContext(ctx, `LOCK TABLE
		persons, person_names, person_contact_points, employments,
		organizations, person_contact_state
		IN ACCESS SHARE MODE NOWAIT`)
	if refreshErr == nil {
		refreshErr = s.refreshDirectoryProjectionsTx(ctx, tx)
	}
	if refreshErr == nil {
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
			return fmt.Errorf("release Directory projection refresh savepoint: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("rollback Directory projection refresh: refresh: %w; rollback: %w", refreshErr, err)
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("release deferred Directory projection refresh: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if s.dialect.IsBusyError(refreshErr) || s.dialect.IsSerializationFailureError(refreshErr) ||
		s.dialect.IsConflictError(refreshErr) {
		slog.Debug("defer Directory projection refresh after PostgreSQL contention",
			"error", refreshErr.Error())
		return nil
	}
	return refreshErr
}

func claimDirtyDirectoryPeopleTx(ctx context.Context, tx *loggedTx) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM directory_projection_dirty RETURNING person_id`)
	if err != nil {
		return nil, fmt.Errorf("claim dirty directory people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan dirty directory person: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dirty directory people: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close dirty directory people: %w", err)
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *Store) refreshDirectoryProjectionIDsTx(ctx context.Context, tx *loggedTx, ids []int64) error {
	for _, id := range ids {
		if err := s.refreshDirectoryPersonTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) refreshDirectoryPersonTx(ctx context.Context, tx *loggedTx, personID int64) error {
	var displayName sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE id = ?`, personID).Scan(&displayName)
	if err == sql.ErrNoRows {
		return s.deleteDirectoryProjectionPersonTx(ctx, tx, personID)
	}
	if err != nil {
		return fmt.Errorf("load directory person %d: %w", personID, err)
	}
	values := make([]string, 0, 8)
	if displayName.Valid {
		values = append(values, displayName.String)
	}
	for _, query := range []string{
		`SELECT COALESCE(formatted, '') || ' ' || COALESCE(family_name, '') || ' ' || COALESCE(given_name, '') || ' ' || COALESCE(additional_names, '') || ' ' || original_value
			FROM person_names WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`,
		`SELECT normalized_value FROM person_contact_points
			WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`,
		`SELECT organization.name FROM employments
			JOIN organizations organization ON organization.id = employments.organization_id
			WHERE employments.person_id = ? AND ` + s.dialect.BoolTrueExpr("employments.is_current") + `
			  AND organization.merged_into_id IS NULL AND organization.retired_at IS NULL`,
	} {
		rows, err := tx.QueryContext(ctx, query, personID)
		if err != nil {
			return fmt.Errorf("load directory search values: %w", err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan directory search value: %w", err)
			}
			values = append(values, value)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close directory search values: %w", err)
		}
	}
	contactState, primaryChannel, lastContactKey := "inactive", "", ""
	var channel sql.NullString
	var lastContact sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT last_contact_channel, last_contact_at
		FROM person_contact_state WHERE person_id = ?`, personID).Scan(&channel, &lastContact)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("load directory contact state: %w", err)
	}
	if err == nil {
		if lastContact.Valid {
			contactState = "active"
			lastContactKey = directoryLastContactKey(lastContact.Time)
		}
		if channel.Valid {
			primaryChannel = normalizeDirectoryText(channel.String)
		}
	}
	orderKey := directoryKey("")
	if displayName.Valid {
		orderKey = directoryKey(normalizeDirectoryText(displayName.String))
	}
	if err := s.deleteDirectoryProjectionPersonTx(ctx, tx, personID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO directory_people (person_id, order_key, contact_state, primary_channel, last_contact_key)
		VALUES (?, ?, ?, ?, ?)`, personID, orderKey, contactState, primaryChannel, lastContactKey); err != nil {
		return fmt.Errorf("insert directory person: %w", err)
	}
	tokens := make(map[string]struct{})
	for _, value := range values {
		for _, token := range directoryTokens(value) {
			tokens[token] = struct{}{}
		}
	}
	for token := range tokens {
		if _, err := tx.ExecContext(ctx, `INSERT INTO directory_person_tokens (person_id, token_key) VALUES (?, ?)`, personID, directoryKey(token)); err != nil {
			return fmt.Errorf("insert directory token: %w", err)
		}
	}
	deleteKeys := make(map[string]struct{})
	for token := range tokens {
		for _, key := range directoryDeleteKeys(token) {
			deleteKeys[key] = struct{}{}
		}
	}
	for key := range deleteKeys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO directory_person_token_deletes (person_id, delete_key) VALUES (?, ?)`, personID, directoryKey(key)); err != nil {
			return fmt.Errorf("insert directory token delete key: %w", err)
		}
	}
	if err := s.insertDirectoryFilterValuesTx(ctx, tx, personID, "category", `SELECT original_value FROM person_categories
		WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`); err != nil {
		return err
	}
	return s.insertDirectoryFilterValuesTx(ctx, tx, personID, "organization", `SELECT organization.name FROM employments
		JOIN organizations organization ON organization.id = employments.organization_id
		WHERE employments.person_id = ? AND `+s.dialect.BoolTrueExpr("employments.is_current")+`
		  AND organization.merged_into_id IS NULL AND organization.retired_at IS NULL`)
}

func (s *Store) insertDirectoryFilterValuesTx(ctx context.Context, tx *loggedTx, personID int64, kind, query string) error {
	rows, err := tx.QueryContext(ctx, query, personID)
	if err != nil {
		return fmt.Errorf("load directory %s filters: %w", kind, err)
	}
	keys := make(map[string]struct{})
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan directory %s filter: %w", kind, err)
		}
		keys[directoryKey(normalizeDirectoryText(value))] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate directory %s filters: %w", kind, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close directory %s filters: %w", kind, err)
	}
	sortedKeys := make([]string, 0, len(keys))
	for key := range keys {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)
	for _, key := range sortedKeys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO directory_person_filters (person_id, filter_kind, value_key) VALUES (?, ?, ?)`, personID, kind, key); err != nil {
			return fmt.Errorf("insert directory %s filter: %w", kind, err)
		}
	}
	return nil
}

func (s *Store) deleteDirectoryProjectionPersonTx(ctx context.Context, tx *loggedTx, personID int64) error {
	for _, table := range []string{"directory_person_tokens", "directory_person_token_deletes", "directory_person_filters", "directory_people"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE person_id = ?`, personID); err != nil {
			return fmt.Errorf("delete directory projection %s: %w", table, err)
		}
	}
	return nil
}

func normalizeDirectoryText(value string) string {
	return strings.Join(strings.Fields(foldDirectoryText(value)), " ")
}

func directoryTokens(value string) []string {
	value = foldDirectoryText(value)
	return strings.FieldsFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// foldDirectoryText builds its Caser per call: x/text documents a Caser as
// possibly stateful, so one shared value must not be used across goroutines.
func foldDirectoryText(value string) string {
	return cases.Fold().String(norm.NFC.String(value))
}

func directoryKey(value string) string {
	return hex.EncodeToString([]byte(value))
}

func directoryDeleteKeys(value string) []string {
	runes := []rune(value)
	keys := make([]string, 0, len(runes))
	seen := make(map[string]struct{}, len(runes))
	for index := range runes {
		key := string(append(append([]rune{}, runes[:index]...), runes[index+1:]...))
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys
}

func directoryFuzzyTokenKeys(token string) []string {
	keys := directoryDeleteKeys(token)
	runes := []rune(token)
	seen := make(map[string]struct{}, len(keys)+len(runes))
	for _, key := range keys {
		seen[key] = struct{}{}
	}
	for index := 0; index+1 < len(runes); index++ {
		swapped := append([]rune{}, runes...)
		swapped[index], swapped[index+1] = swapped[index+1], swapped[index]
		key := string(swapped)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys
}
