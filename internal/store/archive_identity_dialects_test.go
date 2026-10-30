package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// This test runs through the selected real store dialect. The default suite
// exercises SQLite; make test-pg exercises the same migration on PostgreSQL.
func TestArchiveIdentityMigrationExecutesOnSelectedDialect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	uid, err := st.ArchiveUID()
	require.NoError(err)
	require.Len(uid, 64)
	revision, err := st.ArchiveRevision()
	require.NoError(err)
	assert.Equal("1", revision)

	_, err = st.DB().Exec(`DELETE FROM archive_metadata WHERE key = 'archive_uid'`)
	require.NoError(err)
	revision, err = st.ArchiveRevision()
	require.Error(err)
	assert.Empty(revision)

	err = st.InitSchema()
	require.Error(err)
	assert.ErrorIs(err, store.ErrArchiveIdentityCorrupt)
}
