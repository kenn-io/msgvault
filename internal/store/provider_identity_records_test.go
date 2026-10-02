package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestProviderSnapshotPreservesOwnershipAndSkipsUnchangedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "inbox@example.test")
	require.NoError(err)
	records := []store.ProviderIdentityRecord{{ID: "mask-1", Identifier: "mask@example.test", State: "enabled", Kind: "masked-email", Description: "Synthetic"}}
	confirmations := []store.IdentityConfirmation{{Identifier: "mask@example.test", Signals: []string{"provider-alias"}}}
	outcomes, changed, err := st.ApplyProviderIdentitySnapshotContext(t.Context(), source.ID, "fastmail", "one", records, confirmations)
	require.NoError(err)
	assert.True(changed)
	require.Len(outcomes, 1)
	assert.True(outcomes[0].Added)
	// A no-op snapshot must not execute any write, including scheduling metadata.
	if !st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE TABLE snapshot_write_audit (n INTEGER); CREATE TRIGGER snapshot_record_write AFTER UPDATE ON provider_identity_records BEGIN INSERT INTO snapshot_write_audit VALUES (1); END; CREATE TRIGGER snapshot_state_write AFTER UPDATE ON provider_identity_snapshots BEGIN INSERT INTO snapshot_write_audit VALUES (1); END`)
		// SQLite trigger syntax is intentionally verified only on SQLite; both backends
		// exercise the state, removal, metadata and ownership behavior below.
		require.NoError(err)
	}
	outcomes, changed, err = st.ApplyProviderIdentitySnapshotContext(t.Context(), source.ID, "fastmail", "one", records, confirmations)
	require.NoError(err)
	assert.False(changed)
	assert.Empty(outcomes)
	if !st.IsPostgreSQL() {
		var writes int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM snapshot_write_audit`).Scan(&writes))
		assert.Zero(writes)
	}
	records[0].State = "deleted"
	_, changed, err = st.ApplyProviderIdentitySnapshotContext(t.Context(), source.ID, "fastmail", "two", records, confirmations)
	require.NoError(err)
	assert.True(changed)
	got, err := st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "fastmail")
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal("deleted", got[0].State)
	_, _, err = st.ApplyProviderIdentitySnapshotContext(t.Context(), source.ID, "fastmail", "three", nil, nil)
	require.NoError(err)
	got, err = st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "fastmail")
	require.NoError(err)
	require.Len(got, 1)
	assert.True(got[0].Removed)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("mask@example.test", identities[0].Address)
}
