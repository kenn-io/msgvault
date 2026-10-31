package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXBudgetTransactionRollbackAndScopedFence(t *testing.T) {
	st, err := OpenForTest(filepath.Join(t.TempDir(), "budget.db"))
	require.NoError(t, err)
	defer func() { assert.NoError(t, st.Close()) }()
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	cost := XRequestCost{Kind: XRequestCreate, OwnerID: "100", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, MonthlyBudgetMicros: 200_000, CreateReservationMicros: 200_000, AllowSend: true, At: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)}
	rollback := errors.New("synthetic draft transaction rollback")
	err = st.withTxLockedContext(t.Context(), nil, func(tx *loggedTx) error { return st.lockSyncSourceTx(t.Context(), tx, source.ID) }, func(tx *loggedTx) error {
		_, err := st.reserveXRequestTx(t.Context(), tx, source.ID, cost)
		if err != nil {
			return err
		}
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Empty(t, state.Reservations)
	assert.Empty(t, state.SpendBuckets)
	assert.Zero(t, state.Generation)
	stale := st.ScopedToSync(source.ID, 999)
	_, err = stale.ReserveXRequestContext(t.Context(), source.ID, cost)
	require.Error(t, err)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Empty(t, state.Reservations)
	reservation, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	require.NoError(t, err)
	require.NoError(t, st.SettleXRequestContext(t.Context(), source.ID, reservation.ID, XResourceUsage{Persisted: true, Created: true}))
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(200_000), state.SpendBuckets[0].ReservedMicros)
	require.NoError(t, st.SettleXRequestContext(t.Context(), source.ID, reservation.ID, XResourceUsage{Persisted: true, Complete: true, Created: true}))
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Zero(t, state.SpendBuckets[0].ReservedMicros)
	assert.Equal(t, int64(200_000), state.SpendBuckets[0].SpentMicros)
}
