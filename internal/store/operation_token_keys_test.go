package store_test

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOperationTokenKeyConcurrentFirstUseCreatesOnePersistentActiveKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())

	const callers = 8
	keys := make([]store.OperationTokenKey, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for index := range callers {
		wg.Go(func() {
			keys[index], errs[index] = st.ActiveOperationTokenKey(t.Context())
		})
	}
	wg.Wait()
	for index := range callers {
		require.NoError(errs[index])
		assert.Equal(keys[0].KeyID, keys[index].KeyID)
		assert.Equal(keys[0].KeyBytes, keys[index].KeyBytes)
	}
	assert.Len(keys[0].KeyBytes, 32)
	require.NoError(st.Close())

	reopened, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	persisted, err := reopened.ActiveOperationTokenKey(t.Context())
	require.NoError(err)
	assert.Equal(keys[0], persisted)
}

func TestOperationTokenKeyRotationLookupAndExplicitRetirementDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.ActiveOperationTokenKey(t.Context())
	require.NoError(err)
	rotatedAt := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	second, err := st.RotateOperationTokenKey(t.Context(), rotatedAt)
	require.NoError(err)
	assert.NotEqual(first.KeyID, second.KeyID)

	retired, err := st.OperationTokenKey(t.Context(), first.KeyID)
	require.NoError(err)
	assert.Equal(store.OperationTokenKeyDecryptOnly, retired.State)
	require.NotNil(retired.RetiredAt)
	assert.Equal(rotatedAt, *retired.RetiredAt)
	active, err := st.ActiveOperationTokenKey(t.Context())
	require.NoError(err)
	assert.Equal(second.KeyID, active.KeyID)

	require.NoError(st.DeleteOperationTokenKey(t.Context(), first.KeyID))
	_, err = st.OperationTokenKey(t.Context(), first.KeyID)
	require.ErrorIs(err, store.ErrOperationTokenKeyNotFound)
	require.Error(st.DeleteOperationTokenKey(t.Context(), second.KeyID), "the active key cannot be deleted")
}
