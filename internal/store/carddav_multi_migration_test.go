package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVMultipleAccountSchema(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVConcurrentInput("person", "personal"))
			require.NoError(err)
			require.NotEmpty(books)
			// The legacy default identity stays stable, even when additional accounts
			// need IDs outside PostgreSQL's former SMALLINT range.
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_accounts
    (id, connection_name, base_url, username, principal_url, home_url)
    VALUES (?, ?, ?, ?, ?, ?)`), 50000, "work", "https://contacts.example/dav",
				"work@example.com", "https://contacts.example/principal/work/", "https://contacts.example/books/work/")
			require.NoError(err)
			var name string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT connection_name FROM carddav_accounts WHERE id = 1`).Scan(&name))
			assertions.Equal("default", name)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE carddav_address_books
    SET is_write_target = TRUE, is_subscribed = TRUE WHERE id = ?`), books[0].ID)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_address_books
    (account_id, canonical_url, display_name, discovery_index, last_seen_revision, is_write_target, is_subscribed)
    VALUES (?, ?, ?, ?, ?, TRUE, TRUE)`), 50000, books[0].CanonicalURL, "Work", 0, 1)
			require.Error(err, "write target uniqueness must span accounts")
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_address_books
    (account_id, canonical_url, display_name, discovery_index, last_seen_revision)
    VALUES (?, ?, ?, ?, ?)`), 50000, books[0].CanonicalURL, "Work", 0, 1)
			require.NoError(err)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM carddav_accounts`).Scan(&count))
			assertions.Equal(2, count)
		})
	}
}

func TestCardDAVMultiAccountUpgrade(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			input := cardDAVConcurrentInput("person", "personal")
			input.Books[0].CanCreate = new(true)
			account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			resource := remoteResource(books[0].CanonicalURL+"person.vcf", "synthetic-person", "Example Person", "person@example.com", `"one"`)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{resource},
			})
			require.NoError(err)
			before, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, resource.Href)
			require.NoError(err)
			require.NotNil(before.PersonID)
			snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *before.PersonID)
			require.NoError(err)
			pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
				PersonID: *before.PersonID, Desired: true, AddressBookID: books[0].ID, Href: resource.Href,
				OutgoingBody: resource.RemoteBody, OutgoingSemanticHash: "changed", LocalHash: snapshot.Fingerprint,
			})
			require.NoError(err)
			require.NotEmpty(pending.PendingOperation)
			other := remoteResource(books[0].CanonicalURL+"other.vcf", "synthetic-other", "Other Example", "other@example.com", `"one"`)
			currentBooks, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: currentBooks[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{other},
			})
			require.NoError(err)
			otherMapping, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, other.Href)
			require.NoError(err)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(otherMapping))
			require.NoError(err)
			approval := conflictApprovalPlan(t, st, conflict.ID)
			require.NoError(st.ApproveCardDAVConflictLocalContext(t.Context(), approval))
			conflictBefore, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(err)
			run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerManual})
			require.NoError(err)
			_, err = st.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunSucceeded})
			require.NoError(err)
			before, err = st.GetCardDAVResourceContext(t.Context(), books[0].ID, resource.Href)
			require.NoError(err)
			pending, err = st.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
			require.NoError(err)
			recreateLegacyCardDAVAccount(t, st)
			if st.IsPostgreSQL() {
				for table, column := range map[string]string{
					"carddav_accounts":          "id",
					"carddav_account_home_urls": "account_id",
					"carddav_retry_gate":        "account_id",
					"carddav_address_books":     "account_id",
					"carddav_address_book_urls": "account_id",
				} {
					var dataType string
					require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT data_type
    FROM information_schema.columns WHERE table_schema = current_schema()
      AND table_name = $1 AND column_name = $2`, table, column).Scan(&dataType))
					assertions.Equal("smallint", dataType, "legacy %s.%s uses SMALLINT", table, column)
				}
			}
			_, err = st.DB().ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'carddav_multiple_accounts_v1'`)
			require.NoError(err)
			require.NoError(st.InitSchemaContext(t.Context()))
			reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(reopened.Close()) })
			require.NoError(reopened.InitSchemaContext(t.Context()), "upgrade must be repeatable after reopening")
			var name string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT connection_name FROM carddav_accounts WHERE id = 1`).Scan(&name))
			assertions.Equal("default", name)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_accounts
    (id, connection_name, base_url, username, principal_url, home_url) VALUES (?, ?, ?, ?, ?, ?)`),
				0, "invalid", account.BaseURL, account.Username, account.PrincipalURL, account.HomeURL)
			require.Error(err, "upgraded accounts must retain positive IDs")
			if st.IsPostgreSQL() {
				const accountID = int64(50000)
				_, err = st.DB().ExecContext(t.Context(), `INSERT INTO carddav_accounts
    (id, connection_name, base_url, username, principal_url, home_url)
    VALUES ($1, 'work', 'https://contacts.example/dav', 'work@example.com',
      'https://contacts.example/principal/work/', 'https://contacts.example/books/work/')`, accountID)
				require.NoError(err)
				_, err = st.DB().ExecContext(t.Context(), `INSERT INTO carddav_account_home_urls
    (account_id, home_url, discovery_index) VALUES ($1, 'https://contacts.example/books/work/', 0)`, accountID)
				require.NoError(err)
				_, err = st.DB().ExecContext(t.Context(), `INSERT INTO carddav_retry_gate
    (account_id, retry_after_at) VALUES ($1, CURRENT_TIMESTAMP)`, accountID)
				require.NoError(err)
				var bookID int64
				require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO carddav_address_books
    (account_id, canonical_url, display_name, discovery_index, last_seen_revision)
    VALUES ($1, 'https://contacts.example/books/work/', 'Work', 0, 0) RETURNING id`, accountID).Scan(&bookID))
				_, err = st.DB().ExecContext(t.Context(), `INSERT INTO carddav_address_book_urls
    (account_id, address_book_id, url_role, normalized_url)
    VALUES ($1, $2, 'canonical', 'https://contacts.example/books/work/')`, accountID, bookID)
				require.NoError(err)
				fkChecks := []struct {
					table    string
					query    string
					args     []any
					countSQL string
				}{
					{
						table: "carddav_account_home_urls",
						query: `INSERT INTO carddav_account_home_urls (account_id, home_url, discovery_index)
    VALUES ($1, 'https://contacts.example/books/missing/', 0)`,
						args:     []any{int64(50001)},
						countSQL: `SELECT COUNT(*) FROM carddav_account_home_urls WHERE account_id = $1`,
					},
					{
						table:    "carddav_retry_gate",
						query:    `INSERT INTO carddav_retry_gate (account_id, retry_after_at) VALUES ($1, CURRENT_TIMESTAMP)`,
						args:     []any{int64(50001)},
						countSQL: `SELECT COUNT(*) FROM carddav_retry_gate WHERE account_id = $1`,
					},
					{
						table: "carddav_address_books",
						query: `INSERT INTO carddav_address_books
    (account_id, canonical_url, display_name, discovery_index, last_seen_revision)
    VALUES ($1, 'https://contacts.example/books/missing/', 'Missing', 0, 0)`,
						args:     []any{int64(50001)},
						countSQL: `SELECT COUNT(*) FROM carddav_address_books WHERE account_id = $1`,
					},
					{
						table: "carddav_address_book_urls",
						query: `INSERT INTO carddav_address_book_urls
    (account_id, address_book_id, url_role, normalized_url)
    VALUES ($1, $2, 'alias', 'https://contacts.example/books/work-alias/')`,
						args:     []any{int64(50001), bookID},
						countSQL: `SELECT COUNT(*) FROM carddav_address_book_urls WHERE account_id = $1`,
					},
				}
				for _, check := range fkChecks {
					_, err = st.DB().ExecContext(t.Context(), check.query, check.args...)
					assertions.Error(err, "foreign key must be enforced for %s", check.table)
				}
				_, err = st.DB().ExecContext(t.Context(), `DELETE FROM carddav_accounts WHERE id = $1`, accountID)
				require.NoError(err)
				for _, check := range fkChecks {
					var count int
					require.NoError(st.DB().QueryRowContext(t.Context(), check.countSQL, accountID).Scan(&count))
					assertions.Zero(count, "account deletion must cascade through %s", check.table)
				}
			}
			got, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
			require.NoError(err)
			require.NotNil(got)
			assertions.Equal(account.HomeURLs, got.HomeURLs)
			assertions.Equal(account.ConnectionGeneration, got.ConnectionGeneration)
			after, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, resource.Href)
			require.NoError(err)
			assertions.Equal(before, after, "account rebuild must preserve dependent mappings and profiles")
			pendingAfter, err := st.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
			require.NoError(err)
			assertions.Equal(pending, pendingAfter, "pending publication intent must survive upgrade")
			conflictAfter, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(err)
			assertions.Equal(conflictBefore, conflictAfter, "conflict evidence and approvals must survive upgrade")
			var runAccount int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT account_id FROM carddav_sync_runs WHERE id = ?`), run.ID).Scan(&runAccount))
			assertions.Equal(int64(1), runAccount)
			if backend == "sqlite" {
				rows, err := st.DB().QueryContext(t.Context(), `PRAGMA foreign_key_check`)
				require.NoError(err)
				defer func() { _ = rows.Close() }()
				assertions.False(rows.Next(), "upgrade must not leave dangling foreign keys")
				require.NoError(rows.Err())
				require.NoError(rows.Close())
			}
		})
	}
}

func TestCardDAVMultiAccountHistoryUpgradeWithoutDiscovery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerManual})
			require.NoError(err)
			for _, stmt := range []string{
				`DROP INDEX idx_carddav_sync_runs_one_active`,
				`ALTER TABLE carddav_sync_runs DROP COLUMN account_id`,
				`CREATE UNIQUE INDEX idx_carddav_sync_runs_one_active ON carddav_sync_runs((1)) WHERE state = 'running'`,
				`DELETE FROM applied_migrations WHERE name = 'carddav_multiple_accounts_v1'`,
			} {
				_, err := st.DB().ExecContext(t.Context(), stmt)
				require.NoError(err)
			}
			require.NoError(st.InitSchemaContext(t.Context()))
			var accountID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT account_id FROM carddav_sync_runs WHERE id = ?`), run.ID).Scan(&accountID))
			assertions.Equal(int64(1), accountID)
			account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
			require.NoError(err)
			assertions.Nil(account, "history attribution must not invent discovery")
		})
	}
}

func cardDAVMultiBackendStore(t *testing.T, backend string) *store.Store {
	t.Helper()
	if backend == "sqlite" {
		return testutil.NewSQLiteTestStore(t)
	}
	if os.Getenv("MSGVAULT_TEST_DB") == "" {
		t.Skip("requires MSGVAULT_TEST_DB")
	}
	st := testutil.NewTestStore(t)
	require.True(t, st.IsPostgreSQL())
	return st
}

func recreateLegacyCardDAVAccount(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()
	if st.IsPostgreSQL() {
		for _, statement := range []string{
			`ALTER TABLE carddav_account_home_urls DROP CONSTRAINT carddav_account_home_urls_account_id_fkey`,
			`ALTER TABLE carddav_retry_gate DROP CONSTRAINT carddav_retry_gate_account_id_fkey`,
			`ALTER TABLE carddav_address_books DROP CONSTRAINT carddav_address_books_account_id_fkey`,
			`ALTER TABLE carddav_address_book_urls DROP CONSTRAINT carddav_address_book_urls_account_id_fkey`,
			`ALTER TABLE carddav_accounts DROP CONSTRAINT carddav_accounts_id_positive`,
			`ALTER TABLE carddav_accounts DROP COLUMN connection_name`,
			`ALTER TABLE carddav_accounts ALTER COLUMN id TYPE SMALLINT`,
			`ALTER TABLE carddav_account_home_urls ALTER COLUMN account_id TYPE SMALLINT`,
			`ALTER TABLE carddav_retry_gate ALTER COLUMN account_id TYPE SMALLINT`,
			`ALTER TABLE carddav_address_books ALTER COLUMN account_id TYPE SMALLINT`,
			`ALTER TABLE carddav_address_book_urls ALTER COLUMN account_id TYPE SMALLINT`,
			`ALTER TABLE carddav_accounts ADD CONSTRAINT carddav_accounts_legacy_singleton CHECK (id = 1)`,
			`ALTER TABLE carddav_account_home_urls ADD CONSTRAINT carddav_account_home_urls_account_id_fkey FOREIGN KEY (account_id) REFERENCES carddav_accounts(id) ON DELETE CASCADE`,
			`ALTER TABLE carddav_retry_gate ADD CONSTRAINT carddav_retry_gate_account_id_fkey FOREIGN KEY (account_id) REFERENCES carddav_accounts(id) ON DELETE CASCADE`,
			`ALTER TABLE carddav_address_books ADD CONSTRAINT carddav_address_books_account_id_fkey FOREIGN KEY (account_id) REFERENCES carddav_accounts(id) ON DELETE CASCADE`,
			`ALTER TABLE carddav_address_book_urls ADD CONSTRAINT carddav_address_book_urls_account_id_fkey FOREIGN KEY (account_id) REFERENCES carddav_accounts(id) ON DELETE CASCADE`,
		} {
			_, err := st.DB().ExecContext(ctx, statement)
			require.NoError(t, err)
		}
		return
	}
	conn, err := st.DB().Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	defer func() {
		_, err := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)
		require.NoError(t, err)
	}()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	statements := []string{
		`CREATE TABLE carddav_accounts_legacy (
   id INTEGER PRIMARY KEY CHECK (id = 1), base_url TEXT NOT NULL, username TEXT NOT NULL,
   principal_url TEXT NOT NULL, home_url TEXT NOT NULL,
   connection_generation INTEGER NOT NULL DEFAULT 1 CHECK (connection_generation > 0),
   discovery_revision INTEGER NOT NULL DEFAULT 0 CHECK (discovery_revision >= 0),
   discovered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
   created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
   updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO carddav_accounts_legacy SELECT id, base_url, username, principal_url, home_url,
   connection_generation, discovery_revision, discovered_at, created_at, updated_at FROM carddav_accounts`,
		`DROP TABLE carddav_accounts`,
		`ALTER TABLE carddav_accounts_legacy RENAME TO carddav_accounts`,
	}
	for _, stmt := range statements {
		_, err := tx.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}
