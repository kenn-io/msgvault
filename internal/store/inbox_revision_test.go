package store_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestInboxConsumerUpgradesPopulatedPredecessor(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	requirements.Equal(5, store.SchemaVersion)
	archiveUID, err := f.Store.ArchiveUIDContext(t.Context())
	requirements.NoError(err)
	const destination = "native-upgrade"
	const route = "synthetic-recording-route"
	requirements.NoError(f.Store.ReconcileRecordingReferences(t.Context(), destination, receipt.Before.Target.ItemID, true, []store.RecordingReferenceInput{{RouteKey: route, RefSHA256: "synthetic-reference-digest", OccurrenceJSON: `{"uri":"https://example.test/recordings/fixture"}`}}))
	recordingSnapshot := func() [4]string {
		var row [4]string
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT operation_id,ref_sha256,occurrence_json,state FROM recording_references WHERE destination_key=? AND message_id=? AND route_key=?`), destination, receipt.Before.Target.ItemID, route).Scan(&row[0], &row[1], &row[2], &row[3]))
		return row
	}
	beforeRecording := recordingSnapshot()
	requirements.NotEmpty(beforeRecording[0])
	// Reconstruct published recording schema 4 by removing only the Inbox
	// consumer tables. Preserve actual recording data, identity and messages.
	for _, table := range []string{"inbox_operation_receipts", "inbox_provider_states", "inbox_source_revisions"} {
		_, err = f.Store.DB().Exec("DROP TABLE " + table)
		requirements.NoError(err)
	}
	_, err = f.Store.DB().Exec("UPDATE archive_metadata SET value='4' WHERE key='schema_version'")
	requirements.NoError(err)
	requirements.ErrorContains(f.Store.ValidateSchemaContext(t.Context()), "does not match")
	var count int
	requirements.Error(f.Store.DB().QueryRow("SELECT COUNT(*) FROM inbox_operation_receipts").Scan(&count), "runtime validation must not run DDL")
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	requirements.NoError(f.Store.ValidateSchemaContext(t.Context()))
	gotUID, err := f.Store.ArchiveUIDContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(archiveUID, gotUID)
	assertions.Equal(beforeRecording, recordingSnapshot(), "recording schema4 data and operation identity survive the Inbox upgrade")
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id=? AND source_id=? AND source_message_id=?"), receipt.Before.Target.ItemID, receipt.SourceID, receipt.Before.Target.ProviderID).Scan(&count))
	assertions.Equal(1, count)
	_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.True(claimed)
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	requirements.NotNil(got)
	assertions.Equal(receipt.ID, got.ID)
}

func reconstructInboxPredecessor(t *testing.T, st *store.Store) {
	t.Helper()
	for _, table := range []string{"inbox_operation_receipts", "inbox_provider_states", "inbox_source_revisions"} {
		_, err := st.DB().Exec("DROP TABLE " + table)
		require.NoError(t, err)
	}
	_, err := st.DB().Exec("UPDATE archive_metadata SET value='4' WHERE key='schema_version'")
	require.NoError(t, err)
}

func TestInboxConsumerRetriesInterruptedUpgrade(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, receipt := inboxReceiptFixture(t)
	reconstructInboxPredecessor(t, f.Store)
	// A real incompatible table interrupts DDL on both database backends.
	_, err := f.Store.DB().Exec("CREATE TABLE inbox_provider_states (wrong_column TEXT)")
	requirements.NoError(err)
	requirements.Error(f.Store.InitSchemaContext(t.Context()))
	version, err := f.Store.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(4, version, "an interrupted migration must retain its predecessor marker")
	_, err = f.Store.DB().Exec("DROP TABLE inbox_provider_states")
	requirements.NoError(err)
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	requirements.NoError(f.Store.ValidateSchemaContext(t.Context()))
	version, err = f.Store.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(5, version)
	_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.True(claimed)
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	requirements.NotNil(got)
	assertions.Equal(receipt.ID, got.ID)
}

func TestInboxConsumerRejectsFutureBeforeDDL(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, _ := inboxReceiptFixture(t)
	reconstructInboxPredecessor(t, f.Store)
	future := store.SchemaVersion + 1
	_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE archive_metadata SET value=? WHERE key='schema_version'"), strconv.Itoa(future))
	requirements.NoError(err)
	requirements.ErrorContains(f.Store.InitSchemaContext(t.Context()), "newer")
	version, err := f.Store.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(future, version)
	var tables int
	query := "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'inbox_%'"
	if f.Store.IsPostgreSQL() {
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name LIKE 'inbox_%'"
	}
	requirements.NoError(f.Store.DB().QueryRow(query).Scan(&tables))
	assertions.Zero(tables, "future archives must be refused before consumer DDL")
}

func TestInboxRevisionBelongsToObservationConsumer(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	state := receipt.Before
	changed, err := f.Store.ObserveInboxState(t.Context(), state)
	requirements.NoError(err)
	assertions.True(changed)
	var revision int64
	readRevision := func() int64 {
		t.Helper()
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT revision FROM inbox_source_revisions WHERE source_id=?"), receipt.SourceID).Scan(&revision))
		return revision
	}
	assertions.Equal(int64(1), readRevision())
	changed, err = f.Store.ObserveInboxState(t.Context(), state)
	requirements.NoError(err)
	assertions.False(changed)
	assertions.Equal(int64(1), readRevision())
	state.Inbox = new(false)
	changed, err = f.Store.ObserveInboxState(t.Context(), state)
	requirements.NoError(err)
	assertions.True(changed)
	assertions.Equal(int64(2), readRevision())
	_, _, err = f.Store.RemoveSourceSerialized(t.Context(), receipt.SourceID)
	requirements.NoError(err)
	for _, table := range []string{"inbox_provider_states", "inbox_source_revisions"} {
		var count int
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM "+table+" WHERE source_id=?"), receipt.SourceID).Scan(&count))
		assertions.Zero(count)
	}
}
