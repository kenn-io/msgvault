package store

import (
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveUIDInitializedOnceAndStableAcrossReopen(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	first, err := OpenForTest(dbPath)
	require.NoError(err)
	require.NoError(first.InitSchema())
	uid, err := first.ArchiveUID()
	require.NoError(err)
	require.Len(uid, 64)
	_, err = hex.DecodeString(uid)
	require.NoError(err, "archive UID must be 256 bits from crypto/rand")
	require.NoError(first.Close())

	second, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = second.Close() })
	require.NoError(second.InitSchema())
	reopenedUID, err := second.ArchiveUID()
	require.NoError(err)
	assert.Equal(t, uid, reopenedUID)
	var rows int
	require.NoError(second.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key = 'archive_uid'`).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestArchiveUIDMigrationRejectsMissingIdentityAfterLedgerRecorded(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.DB().Exec(`DELETE FROM archive_metadata WHERE key = 'archive_uid'`)
	require.NoError(err)

	err = st.InitSchema()
	require.Error(err)
	require.ErrorIs(err, ErrArchiveIdentityCorrupt)

	var rows int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key = 'archive_uid'`).Scan(&rows))
	assert.Zero(t, rows, "a missing durable identity must never be silently replaced")
}

func TestArchiveUIDMigrationInitializesLegacyArchiveWithoutLedger(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy-unmigrated.db")
	st, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.DB().Exec(`DELETE FROM archive_metadata WHERE key = 'archive_uid'`)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationArchiveIdentity)
	require.NoError(err)

	require.NoError(st.InitSchema())
	uid, err := st.ArchiveUID()
	require.NoError(err)
	assert.Len(t, uid, 64)
}

func TestArchiveUIDConcurrentInitializationConverges(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")
	setup, err := OpenForTest(dbPath)
	require.NoError(err)
	require.NoError(setup.InitSchema())
	_, err = setup.DB().Exec(`DELETE FROM archive_metadata WHERE key = 'archive_uid'`)
	require.NoError(err)
	_, err = setup.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationArchiveIdentity)
	require.NoError(err)
	require.NoError(setup.Close())

	first, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	stores := []*Store{first, second}
	start := make(chan struct{})
	errs := make(chan error, len(stores))
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			<-start
			errs <- st.ensureArchiveUID()
		}(st)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(err)
	}

	firstUID, err := first.ArchiveUID()
	require.NoError(err)
	secondUID, err := second.ArchiveUID()
	require.NoError(err)
	assert.Equal(t, firstUID, secondUID)
}

func TestArchiveUIDDiffersAcrossArchives(t *testing.T) {
	require := require.New(t)
	first, err := OpenForTest(filepath.Join(t.TempDir(), "first.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = first.Close() })
	require.NoError(first.InitSchema())
	second, err := OpenForTest(filepath.Join(t.TempDir(), "second.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = second.Close() })
	require.NoError(second.InitSchema())
	firstUID, err := first.ArchiveUID()
	require.NoError(err)
	secondUID, err := second.ArchiveUID()
	require.NoError(err)
	assert.NotEqual(t, firstUID, secondUID)
}
