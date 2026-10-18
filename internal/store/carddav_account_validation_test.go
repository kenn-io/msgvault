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
	assert.ErrorContains(t, err, "FOREIGN KEY constraint failed")
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
			name:  "missing book on account child",
			setup: `INSERT INTO carddav_address_book_urls(account_id, address_book_id, url_role, normalized_url) VALUES (1, 42, 'canonical', 'https://example.com/missing/')`,
			check: `SELECT address_book_id FROM carddav_address_book_urls WHERE account_id = 1 AND normalized_url = 'https://example.com/missing/'`,
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
	st := legacyCardDAVValidationStore(t)
	var before string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'carddav_accounts'`).Scan(&before))
	seedLegacyCardDAVViolation(t, st, `INSERT INTO carddav_retry_gate(account_id, retry_after_at) VALUES (2, CURRENT_TIMESTAMP)`)

	err := st.InitSchemaContext(t.Context())
	assert.ErrorContains(t, err, "dangling references")
	var after string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'carddav_accounts'`).Scan(&after))
	assert.Equal(t, before, after, "failure must roll back the account rebuild")
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM carddav_accounts WHERE id = 1`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE name = 'carddav_accounts_multi_upgrade'`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM applied_migrations WHERE name = 'carddav_multiple_accounts_v1'`).Scan(&count))
	assert.Zero(t, count)
	assertCardDAVForeignKeysEnforced(t, st)

	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM carddav_retry_gate WHERE account_id = 2`)
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()), "repair must allow retry")
	assertCardDAVForeignKeysEnforced(t, st)
}
