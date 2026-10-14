package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncChatwootSelfAgentsWritesOnlyChangedConfiguration(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("chatwoot", "https://chat.example.com/accounts/1/inboxes/7")
	require.NoError(err)
	first := "https://chat.example.com/accounts/1/user/7"
	second := "https://chat.example.com/accounts/1/user/8"
	require.NoError(st.SyncChatwootSelfAgents(t.Context(), source.ID, []string{first}))

	sentinel := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET updated_at = ? WHERE id = ?`), sentinel, source.ID)
	require.NoError(err)
	untouched := func() bool {
		var count int
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM sources WHERE id = ? AND updated_at = ?`), source.ID, sentinel).Scan(&count))
		return count == 1
	}
	identities := func() []string {
		rows, err := st.DB().Query(st.Rebind(`SELECT address FROM account_identities WHERE source_id = ? ORDER BY address`), source.ID)
		require.NoError(err)
		defer func() { assert.NoError(rows.Close()) }()
		var addresses []string
		for rows.Next() {
			var address string
			require.NoError(rows.Scan(&address))
			addresses = append(addresses, address)
		}
		require.NoError(rows.Err())
		return addresses
	}

	require.NoError(st.SyncChatwootSelfAgents(t.Context(), source.ID, []string{first, first}))
	assert.True(untouched(), "an unchanged agent list leaves the source row alone")
	assert.Equal([]string{first}, identities())

	require.NoError(st.SyncChatwootSelfAgents(t.Context(), source.ID, []string{second}))
	assert.False(untouched(), "a changed agent list is saved")
	assert.Equal([]string{second}, identities())
}

func TestSyncChatwootSelfAgentsReportsMissingSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	err := st.SyncChatwootSelfAgents(t.Context(), 999999, nil)
	require.ErrorContains(t, err, "load Chatwoot source 999999 self agents")
}
