package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVSyncTokenUpgradeSQLite(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	assertCardDAVSyncTokenUpgrade(t, st)
}

func TestCardDAVSyncTokenUpgradePostgreSQL(t *testing.T) {
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("CardDAV PostgreSQL upgrade test requires MSGVAULT_TEST_DB")
	}
	st := testutil.NewTestStore(t)
	require.True(t, st.IsPostgreSQL())
	assertCardDAVSyncTokenUpgrade(t, st)
}

func TestCardDAVRoleReconcileUpgradeSQLite(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	assertCardDAVRoleReconcileUpgrade(t, st)
}

func TestCardDAVRoleReconcileUpgradePostgreSQL(t *testing.T) {
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("CardDAV PostgreSQL role upgrade test requires MSGVAULT_TEST_DB")
	}
	st := testutil.NewTestStore(t)
	require.True(t, st.IsPostgreSQL())
	assertCardDAVRoleReconcileUpgrade(t, st)
}

func TestCardDAVConflictPendingUpgradeSQLite(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	assertCardDAVConflictPendingUpgrade(t, st)
}

func TestCardDAVConflictPendingUpgradePostgreSQL(t *testing.T) {
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("CardDAV PostgreSQL conflict upgrade test requires MSGVAULT_TEST_DB")
	}
	st := testutil.NewTestStore(t)
	require.True(t, st.IsPostgreSQL())
	assertCardDAVConflictPendingUpgrade(t, st)
}

func assertCardDAVConflictPendingUpgrade(t *testing.T, st *store.Store) {
	t.Helper()
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/alice/personal/",
			DisplayName:  "Personal", CanCreate: &allowed,
		}},
	})
	require.NoError(t, err)
	require.Len(t, books, 1)
	book := books[0]
	remote := remoteResource(
		book.CanonicalURL+"alice.vcf", "remote-alice", "Alice", "alice@example.test", `"one"`,
	)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote},
	})
	require.NoError(t, err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(t, err)
	require.NotNil(t, mapping.PersonID)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(t, err)
	require.NoError(t, st.DeletePersonContext(t.Context(), person.ID, person.Revision))

	require.NoError(t, recreateE7CardDAVConflicts(t, st))
	var conflictID int64
	err = st.DB().QueryRow(st.Rebind(`INSERT INTO carddav_conflicts (
		address_book_id, href, base_local_hash, local_hash, base_remote_hash,
		base_remote_etag, remote_etag, mapping_revision, local_body, remote_body,
		local_tombstone, remote_tombstone
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, TRUE, FALSE) RETURNING id`),
		book.ID, mapping.Href, mapping.LocalHash, mapping.LocalHash,
		mapping.RemoteSemanticHash, mapping.RemoteETag, `"two"`, mapping.MappingRevision,
		remoteResource(mapping.Href, "remote-alice", "Alice Remote", "remote@example.test", `"two"`).RemoteBody,
	).Scan(&conflictID)
	require.NoError(t, err, "seed an e7-era unresolved tombstone conflict")

	require.NoError(t, st.InitSchema(), "upgrade the e7-era CardDAV conflict table")
	conflict, err := st.GetCardDAVConflictContext(t.Context(), conflictID)
	require.NoError(t, err)
	assert.Equal(t, store.CardDAVConflictUnresolved, conflict.Status)
	assert.True(t, conflict.LocalTombstone)
	assert.Empty(t, conflict.PendingOperation)
	assertCardDAVConflictIndexes(t, st)

	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts
		SET pending_operation = 'delete' WHERE id = ?`), conflictID)
	require.Error(t, err, "the upgraded table must enforce the complete pending-intent invariant")

	retained := remoteResource(
		mapping.Href, "remote-alice", "Alice Remote", "remote@example.test", `"two"`,
	)
	retained.SemanticHash = "semantic-remote-two"
	prepared, err := st.PrepareCardDAVConflictLocalTombstoneContext(
		t.Context(), conflictID, mapping.MappingRevision, retained,
	)
	require.NoError(t, err)
	assert.Equal(t, store.CardDAVMutationDelete, prepared.PendingOperation)
	assert.NotNil(t, prepared.PendingStartedAt)

	recovered, err := st.PrepareCardDAVConflictLocalTombstoneContext(
		t.Context(), conflictID, prepared.MappingRevision, retained,
	)
	require.NoError(t, err)
	assert.True(t, recovered.RecoveryOnly)
	assert.Equal(t, prepared.MappingRevision, recovered.MappingRevision)
	assert.Equal(t, prepared.PreviousMappingRevision, recovered.PreviousMappingRevision)

	require.NoError(t, st.InitSchema(), "the conflict upgrade must be idempotent")
	after, err := st.GetCardDAVConflictContext(t.Context(), conflictID)
	require.NoError(t, err)
	assert.Equal(t, store.CardDAVMutationDelete, after.PendingOperation)
	assert.Equal(t, prepared.MappingRevision, after.MappingRevision)
	assertCardDAVConflictIndexes(t, st)
}

func recreateE7CardDAVConflicts(t *testing.T, st *store.Store) error {
	t.Helper()
	drop := `DROP TABLE carddav_conflicts`
	if _, err := st.DB().Exec(drop); err != nil {
		return err
	}
	id := "INTEGER PRIMARY KEY AUTOINCREMENT"
	body := "BLOB"
	timestamp := "DATETIME"
	integer := "INTEGER"
	if st.IsPostgreSQL() {
		id = "BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY"
		body = "BYTEA"
		timestamp = "TIMESTAMPTZ"
		integer = "BIGINT"
	}
	ddl := `CREATE TABLE carddav_conflicts (
		id ` + id + `,
		address_book_id ` + integer + ` NOT NULL REFERENCES carddav_address_books(id) ON DELETE CASCADE,
		href TEXT NOT NULL,
		base_local_hash TEXT NOT NULL,
		local_hash TEXT NOT NULL,
		base_remote_hash TEXT NOT NULL,
		base_remote_etag TEXT NOT NULL,
		remote_etag TEXT,
		mapping_revision ` + integer + ` NOT NULL CHECK (mapping_revision > 0),
		local_body ` + body + `,
		remote_body ` + body + `,
		local_tombstone BOOLEAN NOT NULL DEFAULT FALSE,
		remote_tombstone BOOLEAN NOT NULL DEFAULT FALSE,
		status TEXT NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved')),
		resolution TEXT CHECK (resolution IN ('keep_local', 'keep_remote')),
		resolved_at ` + timestamp + `,
		created_at ` + timestamp + ` NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at ` + timestamp + ` NOT NULL DEFAULT CURRENT_TIMESTAMP,
		CHECK (local_tombstone OR local_body IS NOT NULL),
		CHECK (remote_tombstone OR remote_body IS NOT NULL),
		CHECK ((status = 'unresolved' AND resolution IS NULL AND resolved_at IS NULL) OR
		       (status = 'resolved' AND resolution IS NOT NULL AND resolved_at IS NOT NULL))
	)`
	if _, err := st.DB().Exec(ddl); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE UNIQUE INDEX idx_carddav_one_unresolved_conflict
		 ON carddav_conflicts(address_book_id, href) WHERE status = 'unresolved'`,
		`CREATE INDEX idx_carddav_conflicts_resolved_at
		 ON carddav_conflicts(status, resolved_at)`,
	} {
		if _, err := st.DB().Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func assertCardDAVConflictIndexes(t *testing.T, st *store.Store) {
	t.Helper()
	var count int
	query := `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name IN (
			'idx_carddav_one_unresolved_conflict', 'idx_carddav_conflicts_resolved_at'
		)`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname IN (
				'idx_carddav_one_unresolved_conflict', 'idx_carddav_conflicts_resolved_at'
			)`
	}
	require.NoError(t, st.DB().QueryRow(query).Scan(&count))
	assert.Equal(t, 2, count)
}

func assertCardDAVRoleReconcileUpgrade(t *testing.T, st *store.Store) {
	t.Helper()
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/alice/personal/",
			DisplayName:  "Personal", CanCreate: &allowed,
		}},
	})
	require.NoError(t, err)
	require.Len(t, books, 1)

	_, err = st.DB().Exec(`ALTER TABLE carddav_address_books DROP COLUMN needs_full_reconcile`)
	require.NoError(t, err, "recreate the pre-role-reconcile address book schema")
	require.NoError(t, st.InitSchema(), "upgrade legacy CardDAV role state")

	upgraded, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, upgraded, 1)
	assert.False(t, upgraded[0].NeedsFullReconcile)
}

func assertCardDAVSyncTokenUpgrade(t *testing.T, st *store.Store) {
	t.Helper()
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/alice/personal/",
			DisplayName:  "Personal", CanCreate: &allowed,
		}},
	})
	require.NoError(t, err)
	require.Len(t, books, 1)

	_, err = st.DB().Exec(`ALTER TABLE carddav_address_books DROP COLUMN sync_token`)
	require.NoError(t, err, "recreate the pre-sync-token address book schema")
	require.NoError(t, st.InitSchema(), "upgrade legacy CardDAV address books")

	upgraded, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, upgraded, 1)
	assert.Empty(t, upgraded[0].SyncToken)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: upgraded[0].SyncRevision, NextSyncToken: "token-after-upgrade",
	})
	require.NoError(t, err)
	upgraded, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "token-after-upgrade", upgraded[0].SyncToken)
}
