package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
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

type participantClusterInsertRecorder struct {
	*sql.DB

	batchRows []int
	failAt    int
	insertErr error
}

func (r *participantClusterInsertRecorder) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(query, `INSERT INTO tmp_participant_clusters VALUES `) {
		r.batchRows = append(r.batchRows, strings.Count(query, "("))
		if len(r.batchRows) == r.failAt {
			return nil, r.insertErr
		}
	}
	return r.DB.ExecContext(ctx, query, args...)
}

func participantClusterTestMappings(count int) map[int64]int64 {
	clusters := make(map[int64]int64, count)
	for id := int64(1); id <= int64(count); id++ {
		clusters[id] = id%17 + 1
	}
	return clusters
}

func newParticipantClusterInsertRecorder(t *testing.T) *participantClusterInsertRecorder {
	t.Helper()
	require := require.New(t)
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(db.Close()) })
	return &participantClusterInsertRecorder{DB: db}
}

func TestStageParticipantClustersBatchesInsertsWithoutChangingRows(t *testing.T) {
	for _, tt := range []struct {
		name      string
		count     int
		batchRows []int
	}{
		{name: "below boundary", count: 1023, batchRows: []int{1023}},
		{name: "exact boundary", count: 1024, batchRows: []int{1024}},
		{name: "above boundary", count: 1025, batchRows: []int{1024, 1}},
		{name: "final partial batch", count: 2065, batchRows: []int{1024, 1024, 17}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			recorder := newParticipantClusterInsertRecorder(t)

			clusters := participantClusterTestMappings(tt.count)
			require.NoError(stageParticipantClusters(t.Context(), recorder, clusters))
			assert.Equal(tt.batchRows, recorder.batchRows)

			rows, err := recorder.Query(`SELECT participant_id, canonical_id FROM tmp_participant_clusters`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			got := make(map[int64]int64, len(clusters))
			var rowCount int
			for rows.Next() {
				var participantID, canonicalID int64
				require.NoError(rows.Scan(&participantID, &canonicalID))
				got[participantID] = canonicalID
				rowCount++
			}
			require.NoError(rows.Err())
			assert.Equal(tt.count, rowCount)
			assert.Equal(clusters, got)
		})
	}
}

func TestStageParticipantClustersStopsOnLaterInsertError(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	insertErr := errors.New("insert failed")
	recorder := newParticipantClusterInsertRecorder(t)
	recorder.failAt = 2
	recorder.insertErr = insertErr

	clusters := participantClusterTestMappings(2065)
	err := stageParticipantClusters(t.Context(), recorder, clusters)
	require.ErrorIs(err, insertErr)
	require.ErrorContains(err, "populate participant clusters temp table")
	assert.Equal([]int{1024, 1024}, recorder.batchRows, "staging must stop at the failed second insert")
}
