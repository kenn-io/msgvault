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
	sourceID, alice, bob, carol := cacheExportParityParticipants(t, st)
	var conversations []int64
	for i, sender := range []int64{alice, bob} {
		conv, _ := addCacheExportParityMessage(t, st, sourceID, sender, alice+bob-sender, i)
		conversations = append(conversations, conv)
	}
	note, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: "meeting-note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "meeting-thread", ConversationType: "meeting"},
		RawMIME:      []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.org","startsAt":"2026-03-14T10:00:00Z"}}`), RawFormat: "twenty_json",
	})
	require.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.org")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(calendar.ID, `{"calendar_id":"calendar@example.org","time_zone":"UTC"}`))
	event, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: calendar.ID, SourceMessageID: "event", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "calendar", ConversationType: "calendar"},
		RawMIME:      []byte(`{"id":"event","iCalUID":"meeting@example.org","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"}}`), RawFormat: "gcal_json",
		Metadata: &sql.NullString{String: `{"calendar_id":"calendar@example.org","ical_uid":"meeting@example.org"}`, Valid: true},
	})
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(bob)
	require.NoError(err)
	require.NoError(st.Close())
	_, err = buildCache(dbPath, derivedDir, true)
	require.NoError(err)

	st, err = store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
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

	assertCacheExportParity(t, tmp, derivedDir, fullDir, []string{
		tableParticipants, tableParticipantIdentifiers, tablePersonDisplayNames, tableOwnerParticipants,
		tableParticipantClusters, tableConversations, tableConversationParticipants, tableMeetingKeys,
	})
}

func cacheExportParityParticipants(t *testing.T, st *store.Store) (sourceID, alice, bob, carol int64) {
	t.Helper()
	require := require.New(t)
	source, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "alice@example.com", "manual"))
	alice, err = st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	bob, err = st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	carol, err = st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	return source.ID, alice, bob, carol
}

func addCacheExportParityMessage(t *testing.T, st *store.Store, sourceID, sender, recipient int64, i int) (conversationID, messageID int64) {
	t.Helper()
	require := require.New(t)
	conv, err := st.EnsureConversationWithType(sourceID, fmt.Sprintf("thread-%d", i), "direct_chat", "Thread")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		ConversationID: conv, SourceID: sourceID, SourceMessageID: fmt.Sprintf("message-%d", i), MessageType: "email",
		SenderID: sql.NullInt64{Int64: sender, Valid: true},
		SentAt:   sql.NullTime{Time: time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{""}))
	require.NoError(st.ReplaceMessageRecipients(id, "to", []int64{recipient}, []string{""}))
	_, err = st.DB().Exec(`INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (?, ?), (?, ?)`,
		conv, sender, conv, recipient)
	require.NoError(err)
	return conv, id
}

func assertCacheExportParity(t *testing.T, tmp, actualDir, fullDir string, datasets []string) {
	t.Helper()
	assert, require := assert.New(t), require.New(t)
	duckDB, err := duckdbutil.Open(context.Background(), duckdbutil.BuilderPolicy(filepath.Join(tmp, "parity-duckdb-tmp")))
	require.NoError(err)
	defer func() { require.NoError(duckDB.Close()) }()
	columnSchema := func(path string) []string {
		rows, err := duckDB.Query(`SELECT * FROM read_parquet(?, hive_partitioning=true) LIMIT 0`, path)
		require.NoError(err, path)
		defer func() { require.NoError(rows.Close()) }()
		types, err := rows.ColumnTypes()
		require.NoError(err, path)
		require.NoError(rows.Err())
		var schema []string
		for _, column := range types {
			schema = append(schema, column.Name()+" "+column.DatabaseTypeName())
		}
		return schema
	}
	for _, dataset := range datasets {
		actual := filepath.Join(actualDir, dataset, "**", "*.parquet")
		full := filepath.Join(fullDir, dataset, "**", "*.parquet")
		assert.Equal(columnSchema(full), columnSchema(actual), "%s column names and types", dataset)
		columns := "*"
		if dataset == "relationship_people" {
			// Each build records its own wall-clock watermark.
			columns = "* EXCLUDE (temperature_effective_at, temperature_effective_date)"
		}
		var rows, missing, extra int
		require.NoError(duckDB.QueryRow(fmt.Sprintf(`SELECT
			(SELECT count(*) FROM read_parquet(?, hive_partitioning=true)),
			(SELECT count(*) FROM (SELECT %[1]s FROM read_parquet(?, hive_partitioning=true) EXCEPT ALL SELECT %[1]s FROM read_parquet(?, hive_partitioning=true))),
			(SELECT count(*) FROM (SELECT %[1]s FROM read_parquet(?, hive_partitioning=true) EXCEPT ALL SELECT %[1]s FROM read_parquet(?, hive_partitioning=true)))`, columns),
			full, full, actual, actual, full).Scan(&rows, &missing, &extra), dataset)
		require.Positive(rows, "%s must hold rows for the comparison to mean anything", dataset)
		assert.Zero(missing, "%s: rows the derived refresh did not write", dataset)
		assert.Zero(extra, "%s: rows only the derived refresh wrote", dataset)
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

func TestStageMeetingKeysQuotes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	recorder := newParticipantClusterInsertRecorder(t)
	keys := map[int64][]store.MeetingCountOccurrence{1: {{Key: "uid:example'o@example.test/"}}}
	require.NoError(stageMeetingKeys(t.Context(), recorder, keys, 1))
	var count int
	var key string
	require.NoError(recorder.DB.QueryRow(`SELECT count(*), min(meeting_key) FROM tmp_meeting_keys`).Scan(&count, &key))
	assert.Equal(1, count)
	assert.Equal("uid:example'o@example.test/", key)
}

func TestStageMeetingKeysCopyDates(t *testing.T) {
	db := newParticipantClusterInsertRecorder(t)
	keys := map[int64][]store.MeetingCountOccurrence{
		1: {{Key: "uid:meeting@example.org/", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)}},
		2: {{Key: "uid:meeting@example.org/", Start: time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC)}},
	}
	require.NoError(t, stageMeetingKeys(t.Context(), db, keys, 2))
	var count int
	var at time.Time
	require.NoError(t, db.QueryRow(`SELECT count(DISTINCT occurrence_at), min(occurrence_at) FROM (`+meetingKeysSelectSQL+`)`).Scan(&count, &at))
	assert.Equal(t, 1, count)
	assert.Equal(t, time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), at)
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
