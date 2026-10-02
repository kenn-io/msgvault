package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVRetryGateRequiresPositiveAccountID(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVConcurrentInput("personal@example.com", "shared"))
	require.NoError(err)
	for _, accountID := range []int64{0, -1} {
		require.ErrorContains(st.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(time.Minute), accountID), "account ID must be positive")
		_, err := st.GetCardDAVRetryAfterContext(t.Context(), accountID)
		require.ErrorContains(err, "account ID must be positive")
		require.ErrorContains(st.CheckCardDAVRetryAfterContext(t.Context(), accountID), "account ID must be positive")
	}
	gate, err := st.GetCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Nil(t, gate, "an invalid scope must not set the default account's gate")
}

func TestCardDAVSyncRunRequiresPositiveAccountID(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: 0, Trigger: store.CardDAVSyncTriggerManual})
	require.ErrorIs(t, err, store.ErrCardDAVSyncRunInvalid)
	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 25, nil, store.AllCardDAVAccounts)
	require.NoError(t, err)
	assert.Empty(t, runs, "an omitted scope must not create a default-account run")
}
