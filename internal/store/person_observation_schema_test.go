package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonObservationSchemaInitializesTwice(t *testing.T) {
	st := testutil.NewTestStore(t)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	for _, table := range []string{"person_observations", "person_observation_evidence", "person_observation_premises", "person_observation_events", "person_observation_suppressions", "person_observation_categories", "person_observation_clocks"} {
		assertPersonFactTableQueryable(t, st, table)
	}
}
func insertObservationRow(t *testing.T, st *store.Store, personID, sequence int64, level personobs.Level, content string) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_observations
 (person_id,sequence,level,origin,kind,content,content_key,normalization_version,directness,sensitive,confidence_bp,status,stability,stability_source)
 VALUES(?,?,?,'owner','state',?,?,'obs-norm-v1','direct-other',FALSE,1000,'active','durable','owner') RETURNING id`), personID, sequence, level, content, personobs.ContentKey(level, "", content)).Scan(&id)
	require.NoError(t, err)
	return id
}
func TestPersonObservationSchemaLiveKeyUnique(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	id := insertObservationRow(t, st, person, 1, personobs.LevelExplicit, "[PERSON] runs")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_observations
 (person_id,sequence,level,origin,kind,content,content_key,normalization_version,directness,sensitive,confidence_bp,status,stability,stability_source)
 SELECT person_id,2,level,origin,kind,content,content_key,normalization_version,directness,sensitive,confidence_bp,status,stability,stability_source FROM person_observations WHERE id=?`), id)
	require.Error(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET status='rejected' WHERE id=?`), id)
	require.NoError(t, err)
	other := insertObservationRow(t, st, person, 2, personobs.LevelExplicit, "[PERSON] runs")
	assert.Greater(t, other, id)
}
func TestPersonObservationIDsNeverReused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite AUTOINCREMENT contract")
	}
	person := personFactSchemaPerson(t, st, "alice@example.com")
	first := insertObservationRow(t, st, person, 1, personobs.LevelExplicit, "[PERSON] runs")
	var event int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_observation_events(person_id,observation_id,event,actor) VALUES(?,?,'created','owner') RETURNING id`), person, first).Scan(&event))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM person_observations WHERE id=?`), first)
	require.NoError(err)
	second := insertObservationRow(t, st, person, 2, personobs.LevelExplicit, "[PERSON] swims")
	assert.Greater(second, first)
	var nextEvent int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_observation_events(person_id,observation_id,event,actor) VALUES(?,?,'created','owner') RETURNING id`), person, second).Scan(&nextEvent))
	assert.Greater(nextEvent, event)
}

func observationLegacyMessageLink(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), `DROP INDEX IF EXISTS idx_person_fact_evidence_message`)
	require.NoError(t, err)
	// SQLite supports DROP COLUMN here: this nullable link has no dependent
	// constraints once its index is gone, so the fixture uses the native DDL.
	_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE person_fact_evidence DROP COLUMN message_id`)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM applied_migrations WHERE name=?`), "person_fact_evidence_message_id_v1")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='11' WHERE key='schema_version'`)
	require.NoError(t, err)
}
func seedObservationArchiveEvidence(t *testing.T, st *store.Store, person int64, key string, message int64) int64 {
	t.Helper()
	id := insertPersonFactSchemaEvidence(t, st, person, key)
	ref, err := peoplesweep.EncodePersonSweepEvidenceRef(peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceConversationText, SourceID: 1, MessageID: message, SpanEnd: 7})
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_evidence SET source_class='archive',source_ref=? WHERE id=?`), ref, id)
	require.NoError(t, err)
	return id
}
func TestPersonFactEvidenceMessageIDUpgrade(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	archive := seedObservationArchiveEvidence(t, st, person, "archive", 42)
	public := insertPersonFactSchemaEvidence(t, st, person, "public")
	observationLegacyMessageLink(t, st)
	require.NoError(st.InitSchemaContext(t.Context()))
	var message sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_id FROM person_fact_evidence WHERE id=?`), archive).Scan(&message))
	assert.Equal(sql.NullInt64{Int64: 42, Valid: true}, message)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_id FROM person_fact_evidence WHERE id=?`), public).Scan(&message))
	assert.False(message.Valid)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	var indexes int
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_person_fact_evidence_message'`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='idx_person_fact_evidence_message'`
	}
	require.NoError(st.DB().QueryRowContext(t.Context(), query).Scan(&indexes))
	assert.Equal(1, indexes)
	require.NoError(st.InitSchemaContext(t.Context()))
}

// Cancellation is attached to the real database logger's committed chunk
// observation rather than a timer, so machine throughput is irrelevant.
func TestPersonFactEvidenceMessageIDBackfillResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	for i := range 1001 {
		seedObservationArchiveEvidence(t, st, person, fmt.Sprintf("archive-%d", i), int64(i+1))
	}
	observationLegacyMessageLink(t, st)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	restore := st.CancelObservationMessageBackfillForTest(cancel)
	defer restore()
	err := st.InitSchemaContext(ctx)
	require.ErrorIs(err, context.Canceled)
	restore()
	var done int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_fact_evidence WHERE message_id IS NOT NULL`).Scan(&done))
	assert.Equal(1000, done)
	require.NoError(st.InitSchemaContext(t.Context()))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_fact_evidence WHERE message_id IS NOT NULL`).Scan(&done))
	assert.Equal(1001, done)
}

func TestPersonFactEvidenceMessageIDBackfillSkipsMalformed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	malformed := insertPersonFactSchemaEvidence(t, st, person, "malformed")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_evidence SET source_class='archive',source_ref='person-sweep/v1:malformed' WHERE id=?`), malformed)
	require.NoError(err)
	for i := range 1000 {
		seedObservationArchiveEvidence(t, st, person, fmt.Sprintf("valid-%d", i), int64(i+1))
	}
	observationLegacyMessageLink(t, st)
	require.NoError(st.InitSchemaContext(t.Context()))
	var message sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_id FROM person_fact_evidence WHERE id=?`), malformed).Scan(&message))
	assert.False(message.Valid)
	var linked int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_fact_evidence WHERE message_id IS NOT NULL`).Scan(&linked))
	assert.Equal(1000, linked)
	require.NoError(st.InitSchemaContext(t.Context()))
}
func TestPersonObservationForeignKeyIndexes(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, pair := range [][2]string{
		{"person_observations", "subject_person_id"},
		{"person_observation_evidence", "generation_id"},
		{"person_observation_premises", "added_generation_id"},
		{"person_observation_events", "person_id"},
		{"person_observation_events", "generation_id"},
		{"person_observation_events", "evidence_id"},
		{"person_observation_suppressions", "person_id"},
	} {
		t.Run(pair[0]+"."+pair[1], func(t *testing.T) {
			query := `SELECT EXISTS(SELECT 1 FROM pragma_index_list(?) indexes JOIN pragma_index_info(indexes.name) cols ON cols.seqno=0 WHERE cols.name=? AND indexes.partial=0)`
			if st.IsPostgreSQL() {
				query = `SELECT EXISTS(SELECT 1 FROM pg_index idx JOIN pg_class rel ON rel.oid=idx.indrelid JOIN pg_namespace ns ON ns.oid=rel.relnamespace JOIN pg_attribute col ON col.attrelid=rel.oid AND col.attnum=idx.indkey[0] WHERE rel.relname=? AND col.attname=? AND ns.nspname=current_schema() AND idx.indpred IS NULL)`
			}
			var indexed bool
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(query), pair[0], pair[1]).Scan(&indexed))
			assert.True(t, indexed)
		})
	}
}
