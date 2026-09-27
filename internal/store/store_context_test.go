package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenContextCanceledBeforeOpening(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	st, err := OpenContext(ctx, dbPath)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, st)
	_, err = os.Stat(dbPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadArchiveMetadataRevisionContextPropagatesCancellation(t *testing.T) {
	db, err := OpenForTest(filepath.Join(t.TempDir(), "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = db.DerivedDataRevisionContext(ctx)

	assert.ErrorIs(t, err, context.Canceled)
}

func TestOpenReadOnlyContextCanceledBeforeOpening(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	st, err := OpenForTest(dbPath)
	require.NoError(err)
	require.NoError(st.Close())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	st, err = OpenReadOnlyContext(ctx, dbPath)

	require.ErrorIs(err, context.Canceled)
	assert.Nil(t, st)
}
