package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/store"
)

// TestCacheExportParityFullVersusDerived pins the shared export SQL: a derived
// refresh after identity, identifier, display-name, membership and type
// changes must write the same rows a full rebuild writes.
func TestCacheExportParityFullVersusDerived(t *testing.T) {
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	derivedDir := filepath.Join(tmp, "analytics-derived")
	fullDir := filepath.Join(tmp, "analytics-full")
	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "alice@example.com", "manual"))
	alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	bob, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	carol, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	var conversations []int64
	for i, sender := range []int64{alice, bob} {
		conv, err := st.EnsureConversationWithType(source.ID, fmt.Sprintf("thread-%d", i), "direct_chat", "Thread")
		require.NoError(err)
		conversations = append(conversations, conv)
		id, err := st.UpsertMessage(&store.Message{
			ConversationID: conv, SourceID: source.ID, SourceMessageID: fmt.Sprintf("message-%d", i), MessageType: "email",
			SenderID: sql.NullInt64{Int64: sender, Valid: true},
			SentAt:   sql.NullTime{Time: time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		})
		require.NoError(err)
		require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{""}))
		require.NoError(st.ReplaceMessageRecipients(id, "to", []int64{alice + bob - sender}, []string{""}))
		_, err = st.DB().Exec(`INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (?, ?), (?, ?)`,
			conv, alice, conv, bob)
		require.NoError(err)
	}
	person, _, err := st.CreatePersonFromParticipant(bob)
	require.NoError(err)
	require.NoError(st.Close())
	_, err = buildCache(dbPath, derivedDir, true)
	require.NoError(err)

	st, err = store.Open(dbPath)
	require.NoError(err)
	name := "Bob Curated"
	_, err = st.UpdatePersonDisplayName(person.ID, person.Revision, &name)
	require.NoError(err)
	_, err = st.LinkParticipants(bob, carol)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE participants SET display_name = NULL WHERE id = ?`, alice)
	require.NoError(err)
	_, err = st.EnsureParticipantByIdentifier("email", "alice@example.com", "Alice Updated")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(carol, "phone", "+15550142"))
	_, err = st.DB().Exec(`INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (?, ?)`,
		conversations[1], carol)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = ?`, conversations[1])
	require.NoError(err)
	require.NoError(st.Close())

	result, err := buildCacheDerivedOnly(dbPath, derivedDir)
	require.NoError(err)
	require.True(result.IdentityOnly, "the mutations must take the derived refresh path")
	require.False(result.Skipped)
	_, err = buildCache(dbPath, fullDir, true)
	require.NoError(err)

	duckDB, err := duckdbutil.Open(context.Background(), duckdbutil.BuilderPolicy(filepath.Join(tmp, "parity-duckdb-tmp")))
	require.NoError(err)
	defer func() { require.NoError(duckDB.Close()) }()
	for _, dataset := range []string{
		tableParticipants, tableParticipantIdentifiers, tablePersonDisplayNames, tableOwnerParticipants,
		tableParticipantClusters, tableConversations, tableConversationParticipants,
	} {
		derived := filepath.Join(derivedDir, dataset, "*.parquet")
		full := filepath.Join(fullDir, dataset, "*.parquet")
		var rows, missing, extra int
		require.NoError(duckDB.QueryRow(`SELECT
				(SELECT count(*) FROM read_parquet(?)),
				(SELECT count(*) FROM (SELECT * FROM read_parquet(?) EXCEPT ALL SELECT * FROM read_parquet(?))),
				(SELECT count(*) FROM (SELECT * FROM read_parquet(?) EXCEPT ALL SELECT * FROM read_parquet(?)))`,
			full, full, derived, derived, full).Scan(&rows, &missing, &extra), dataset)
		require.Positive(rows, "%s must hold rows for the comparison to mean anything", dataset)
		require.Zero(missing, "%s rows the derived refresh did not write", dataset)
		require.Zero(extra, "%s rows only the derived refresh wrote", dataset)
	}
}

func TestCacheExportParityCopyParquetRoundTrip(t *testing.T) {
	require := require.New(t)
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = db.Close() }()
	dir := filepath.Join(t.TempDir(), "o'brien", "dataset")

	require.NoError(copyParquet(t.Context(), db, dir, "data.parquet", `SELECT 7 AS id, 'seven' AS name`))

	var id int
	var name string
	require.NoError(db.QueryRow(`SELECT id, name FROM read_parquet(?)`, filepath.Join(dir, "data.parquet")).Scan(&id, &name))
	require.Equal(7, id)
	require.Equal("seven", name)
}
