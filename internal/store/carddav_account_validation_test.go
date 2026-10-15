package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func legacyCardDAVValidationStore(t *testing.T) *store.Store {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	// Restoration assertions must use the connection that performed the rebuild.
	st.DB().SetMaxOpenConns(1)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVConcurrentInput("person", "personal"))
	require.NoError(t, err)
	recreateLegacyCardDAVAccount(t, st)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'carddav_multiple_accounts_v1'`)
	require.NoError(t, err)
	return st
}

func seedLegacyCardDAVViolation(t *testing.T, st *store.Store, statements ...string) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	defer func() {
		_, err := st.DB().ExecContext(context.WithoutCancel(t.Context()), `PRAGMA foreign_keys = ON`)
		require.NoError(t, err)
	}()
	for _, statement := range statements {
		_, err := st.DB().ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
}

func assertCardDAVForeignKeysEnforced(t *testing.T, st *store.Store) {
	t.Helper()
	var enabled int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&enabled))
	assert.Equal(t, 1, enabled)
	_, err := st.DB().ExecContext(t.Context(), `INSERT INTO carddav_retry_gate(account_id, retry_after_at) VALUES (99, CURRENT_TIMESTAMP)`)
	require.ErrorContains(t, err, "FOREIGN KEY constraint failed")
}

func TestCardDAVAccountUpgradePreservesUnrelatedReferences(t *testing.T) {
	for _, tc := range []struct {
		name, setup, check string
	}{
		{
			name:  "attachment without message",
			setup: `INSERT INTO attachments(id, message_id, storage_path) VALUES (7, 42, 'example')`,
			check: `SELECT message_id FROM attachments WHERE id = 7`,
		},
		{
			name: "another parent on account child",
			setup: `CREATE TABLE legacy_account_links (
 id INTEGER PRIMARY KEY, account_id INTEGER REFERENCES CARDDAV_ACCOUNTS(id), message_id INTEGER REFERENCES messages(id));
 INSERT INTO legacy_account_links VALUES (7, 1, 42)`,
			check: `SELECT message_id FROM legacy_account_links WHERE id = 7`,
		},
		{
			name:  "malformed unrelated foreign key",
			setup: `CREATE TABLE legacy_bad_links (id INTEGER REFERENCES messages(subject))`,
			check: `SELECT 42 FROM sqlite_master WHERE name = 'legacy_bad_links'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := legacyCardDAVValidationStore(t)
			seedLegacyCardDAVViolation(t, st, tc.setup)
			require.NoError(st.InitSchemaContext(t.Context()))
			var preserved int
			require.NoError(st.DB().QueryRowContext(t.Context(), tc.check).Scan(&preserved))
			assertions.Equal(42, preserved)
			var name string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT connection_name FROM carddav_accounts WHERE id = 1`).Scan(&name))
			assertions.Equal("default", name)
			assertCardDAVForeignKeysEnforced(t, st)
			reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(reopened.Close()) })
			require.NoError(reopened.InitSchemaContext(t.Context()))
			require.NoError(reopened.DB().QueryRowContext(t.Context(), tc.check).Scan(&preserved))
			assertions.Equal(42, preserved)
		})
	}
}

func TestCardDAVAccountUpgradeRejectsDanglingAccounts(t *testing.T) {
	for _, tc := range []struct {
		name, table, setup, repair string
	}{
		{
			name: "built-in account child", table: "carddav_retry_gate",
			setup:  `INSERT INTO carddav_retry_gate(account_id, retry_after_at) VALUES (2, CURRENT_TIMESTAMP)`,
			repair: `DELETE FROM carddav_retry_gate WHERE account_id = 2`,
		},
		{
			name: "mixed-case parent with another relationship", table: "legacy_account_links",
			setup: `CREATE TABLE legacy_account_links (
 id INTEGER PRIMARY KEY, account_id INTEGER REFERENCES CARDDAV_ACCOUNTS(id), message_id INTEGER REFERENCES messages(id));
 INSERT INTO legacy_account_links VALUES (7, 2, 42)`,
			repair: `UPDATE legacy_account_links SET account_id = 1`,
		},
		{
			name: "without rowid", table: "legacy_account_links",
			setup: `CREATE TABLE legacy_account_links (
 id TEXT PRIMARY KEY, account_id INTEGER REFERENCES carddav_accounts(id)) WITHOUT ROWID;
 INSERT INTO legacy_account_links VALUES ('example', 2)`,
			repair: `UPDATE legacy_account_links SET account_id = 1`,
		},
		{
			name: "malformed relationship on account child", table: "legacy_account_links",
			setup: `CREATE TABLE legacy_account_links (
 id INTEGER PRIMARY KEY, account_id INTEGER REFERENCES carddav_accounts(id), message_id INTEGER REFERENCES messages(subject))`,
			repair: `DROP TABLE legacy_account_links`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := legacyCardDAVValidationStore(t)
			var before string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'carddav_accounts'`).Scan(&before))
			seedLegacyCardDAVViolation(t, st, tc.setup)
			err := st.InitSchemaContext(t.Context())
			require.ErrorContains(err, tc.table)
			if tc.name == "malformed relationship on account child" {
				require.ErrorContains(err, "foreign key mismatch")
			} else {
				require.ErrorContains(err, "dangling references")
			}
			if tc.name == "without rowid" {
				assertions.Contains(err.Error(), "rowid NULL")
			}
			var after string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'carddav_accounts'`).Scan(&after))
			assertions.Equal(before, after, "failure must roll back the account rebuild")
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM carddav_accounts WHERE id = 1`).Scan(&count))
			assertions.Equal(1, count)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE name = 'carddav_accounts_multi_upgrade'`).Scan(&count))
			assertions.Zero(count)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM applied_migrations WHERE name = 'carddav_multiple_accounts_v1'`).Scan(&count))
			assertions.Zero(count)
			assertCardDAVForeignKeysEnforced(t, st)
			_, err = st.DB().ExecContext(t.Context(), tc.repair)
			require.NoError(err)
			require.NoError(st.InitSchemaContext(t.Context()), "repair must allow retry")
			assertCardDAVForeignKeysEnforced(t, st)
		})
	}
}
