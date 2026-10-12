package store_test

import (
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestInboxConsumerUpgradesPopulatedPredecessor(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f, receipt := inboxReceiptFixture(t)
	require.Equal(5, store.SchemaVersion)
	archiveUID, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	const destination = "native-upgrade"
	const route = "synthetic-recording-route"
	require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), destination, receipt.Before.Target.ItemID, true, []store.RecordingReferenceInput{{RouteKey: route, RefSHA256: "synthetic-reference-digest", OccurrenceJSON: `{"uri":"https://example.test/recordings/fixture"}`}}))
	recordingSnapshot := func() [4]string {
		var row [4]string
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT operation_id,ref_sha256,occurrence_json,state FROM recording_references WHERE destination_key=? AND message_id=? AND route_key=?`), destination, receipt.Before.Target.ItemID, route).Scan(&row[0], &row[1], &row[2], &row[3]))
		return row
	}
	beforeRecording := recordingSnapshot()
	require.NotEmpty(beforeRecording[0])
	// Reconstruct published recording schema 4 by removing only the Inbox
	// consumer tables. Preserve actual recording data, identity and messages.
	for _, table := range []string{"inbox_operation_receipts", "inbox_provider_states", "inbox_source_revisions"} {
		_, err = f.Store.DB().Exec("DROP TABLE " + table)
		require.NoError(err)
	}
	_, err = f.Store.DB().Exec("UPDATE archive_metadata SET value='4' WHERE key='schema_version'")
	require.NoError(err)
	require.ErrorContains(f.Store.ValidateSchemaContext(t.Context()), "does not match")
	var count int
	require.Error(f.Store.DB().QueryRow("SELECT COUNT(*) FROM inbox_operation_receipts").Scan(&count), "runtime validation must not run DDL")
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	require.NoError(f.Store.ValidateSchemaContext(t.Context()))
	gotUID, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	assert.Equal(archiveUID, gotUID)
	assert.Equal(beforeRecording, recordingSnapshot(), "recording schema4 data and operation identity survive the Inbox upgrade")
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id=? AND source_id=? AND source_message_id=?"), receipt.Before.Target.ItemID, receipt.SourceID, receipt.Before.Target.ProviderID).Scan(&count))
	assert.Equal(1, count)
	_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	require.NoError(err)
	assert.True(claimed)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(receipt.ID, got.ID)
}

func reconstructInboxPredecessor(t *testing.T, st *store.Store) {
	t.Helper()
	for _, table := range []string{"inbox_operation_receipts", "inbox_provider_states", "inbox_source_revisions"} {
		_, err := st.DB().Exec("DROP TABLE " + table)
		Require.NoError(t, err)
	}
	_, err := st.DB().Exec("UPDATE archive_metadata SET value='4' WHERE key='schema_version'")
	Require.NoError(t, err)
}

func TestInboxConsumerRetriesInterruptedUpgrade(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	f, receipt := inboxReceiptFixture(t)
	reconstructInboxPredecessor(t, f.Store)
	// A real incompatible table interrupts DDL on both database backends.
	_, err := f.Store.DB().Exec("CREATE TABLE inbox_provider_states (wrong_column TEXT)")
	require.NoError(err)
	require.Error(f.Store.InitSchemaContext(t.Context()))
	version, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(4, version, "an interrupted migration must retain its predecessor marker")
	_, err = f.Store.DB().Exec("DROP TABLE inbox_provider_states")
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	require.NoError(f.Store.ValidateSchemaContext(t.Context()))
	version, err = f.Store.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(5, version)
	_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	require.NoError(err)
	assert.True(claimed)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(receipt.ID, got.ID)
}

func TestInboxConsumerRejectsFutureBeforeDDL(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	f, _ := inboxReceiptFixture(t)
	reconstructInboxPredecessor(t, f.Store)
	future := store.SchemaVersion + 1
	_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE archive_metadata SET value=? WHERE key='schema_version'"), strconv.Itoa(future))
	require.NoError(err)
	require.ErrorContains(f.Store.InitSchemaContext(t.Context()), "newer")
	version, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(future, version)
	var tables int
	query := "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'inbox_%'"
	if f.Store.IsPostgreSQL() {
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name LIKE 'inbox_%'"
	}
	require.NoError(f.Store.DB().QueryRow(query).Scan(&tables))
	assert.Zero(tables, "future archives must be refused before consumer DDL")
}

func TestInboxRevisionBelongsToObservationConsumer(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f, receipt := inboxReceiptFixture(t)
	state := receipt.Before
	changed, err := f.Store.ObserveInboxState(t.Context(), state)
	require.NoError(err)
	assert.True(changed)
	var revision int64
	readRevision := func() int64 {
		t.Helper()
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT revision FROM inbox_source_revisions WHERE source_id=?"), receipt.SourceID).Scan(&revision))
		return revision
	}
	assert.Equal(int64(1), readRevision())
	changed, err = f.Store.ObserveInboxState(t.Context(), state)
	require.NoError(err)
	assert.False(changed)
	assert.Equal(int64(1), readRevision())
	state.ObservedAt = state.ObservedAt.Add(time.Second)
	state.Inbox = new(false)
	changed, err = f.Store.ObserveInboxState(t.Context(), state)
	require.NoError(err)
	assert.True(changed)
	assert.Equal(int64(2), readRevision())
	_, _, err = f.Store.RemoveSourceSerialized(t.Context(), receipt.SourceID)
	require.NoError(err)
	for _, table := range []string{"inbox_provider_states", "inbox_source_revisions"} {
		var count int
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM "+table+" WHERE source_id=?"), receipt.SourceID).Scan(&count))
		assert.Zero(count)
	}
}
