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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "budget.db"))
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, st.Close()) }()
	_testRequire.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	cost := XRequestCost{Kind: XRequestCreate, OwnerID: "100", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, MonthlyBudgetMicros: 200_000, CreateReservationMicros: 200_000, AllowSend: true, At: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)}
	rollback := errors.New("synthetic draft transaction rollback")
	err = st.withTxLockedContext(t.Context(), nil, func(tx *loggedTx) error { return st.lockSyncSourceTx(t.Context(), tx, source.ID) }, func(tx *loggedTx) error {
		_, err := st.reserveXRequestTx(t.Context(), tx, source.ID, cost)
		if err != nil {
			return err
		}
		return rollback
	})
	_testRequire.ErrorIs(err, rollback)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(state.Reservations)
	_testAssert.Empty(state.SpendBuckets)
	_testAssert.Zero(state.Generation)
	stale := st.ScopedToSync(source.ID, 999)
	_, err = stale.ReserveXRequestContext(t.Context(), source.ID, cost)
	_testRequire.Error(err)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(state.Reservations)
	reservation, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	_testRequire.NoError(err)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), source.ID, reservation.ID, XResourceUsage{Persisted: true, Created: true}))
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(200_000), state.SpendBuckets[0].ReservedMicros)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), source.ID, reservation.ID, XResourceUsage{Persisted: true, Complete: true, Created: true}))
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Zero(state.SpendBuckets[0].ReservedMicros)
	_testAssert.Equal(int64(200_000), state.SpendBuckets[0].SpentMicros)
}
