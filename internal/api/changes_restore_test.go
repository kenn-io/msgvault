package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// openChangesArchive opens the archive at path and serves the feed over it. The
// caller closes the store, because these tests copy the database file around
// between opens and a copy of an open SQLite database is not a database.
func openChangesArchive(t *testing.T, path string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.OpenForTest(path)
	require.NoError(t, err, "open archive %s", path)
	require.NoError(t, st.InitSchema(), "init schema at %s", path)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	return srv, st
}

// copyArchiveFile copies a closed SQLite archive, including the sidecars a
// closed database may still carry, and removes any sidecar the source does not
// have so the destination cannot be left holding a stale one.
func copyArchiveFile(t *testing.T, dst, src string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(src + suffix)
		if os.IsNotExist(err) {
			require.NoError(t, ignoreNotExist(os.Remove(dst+suffix)),
				"remove stale %s", dst+suffix)
			continue
		}
		require.NoError(t, err, "read %s", src+suffix)
		require.NoError(t, os.WriteFile(dst+suffix, data, 0o600), "write %s", dst+suffix)
	}
}

func ignoreNotExist(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// TestChangesEndpoint_RestoringAnOlderSnapshotSilentlyDivergesTheConsumer pins
// behaviour this build does NOT fix, so that nobody later reads the cursor's
// archive binding as protection against it.
//
// A cursor names the archive that issued it, and a file-level restore of the
// same archive keeps that name — deliberately, because a restore of the same
// archive is exactly the case a stored cursor is supposed to survive. But a
// cursor issued AFTER the snapshot still validates against the restored
// database, and everything the rollback undid sits below it: rows reverted to
// older watermarks are never re-sent, and rows the rollback removed produce no
// event at all. The consumer's mirror diverges from the archive with nothing on
// the wire to say so. A copied database forks the same way, for the same reason.
//
// Fixing it needs a monotonic sequence or a change ledger rather than a
// wall-clock watermark, which this feed does not have. Until then it is
// documented beside the backward-clock exception in docs/api-server.md, and this
// test is here so a change in the behaviour is a decision rather than an
// accident.
func TestChangesEndpoint_RestoringAnOlderSnapshotSilentlyDivergesTheConsumer(t *testing.T) {
	testutil.SkipIfPostgres(t,
		"a file-level snapshot and restore of the archive is a SQLite-only operation")
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.db")
	snapshot := filepath.Join(dir, "snapshot.db")

	// The archive as the backup found it: two messages, both already walked.
	srv, st := openChangesArchive(t, archive)
	kept := seedChangedMessages(t, st, 2)
	now := changesServerTime(t, srv)
	setChangesWatermarkAt(t, st, now.Add(-2*time.Hour), kept...)
	require.NoError(st.Close(), "close the archive before copying it")
	copyArchiveFile(t, snapshot, archive)

	// Work done after the snapshot: one message edited, one message added.
	srv, st = openChangesArchive(t, archive)
	edited := kept[0]
	setChangesWatermarkAt(t, st, now.Add(-time.Hour), edited)
	added := seedMoreChangedMessages(t, st, "after-snapshot", 1)[0]
	setChangesWatermarkAt(t, st, now.Add(-time.Hour), added)

	// The consumer walks to the end and stores the cursor it was handed.
	page := getChangesPage(t, srv, changesTarget("", 100))
	require.ElementsMatch([]int64{kept[0], kept[1], added}, changedIDs(page),
		"the consumer must have seen the whole archive before the restore")
	cursor := page.NextCursor
	require.NotEmpty(cursor, "a page carrying rows must hand back a cursor")
	require.NoError(st.Close(), "close the archive before restoring over it")

	// The restore. Same archive, same durable UID, older contents.
	copyArchiveFile(t, archive, snapshot)
	srv, st = openChangesArchive(t, archive)
	t.Cleanup(func() { _ = st.Close() })

	// 1. The cursor is honoured, not rejected: a file-level restore of the same
	//    archive keeps its identity, which is what makes a stored cursor survive
	//    one — and what makes this failure reachable. getChangesPage requires
	//    200, so a rejection fails here.
	resumed := getChangesPage(t, srv, changesTarget(cursor, 100))

	// 2. And it delivers nothing. The edit was rolled back to a watermark below
	//    the cursor, so the consumer keeps the post-snapshot content of a message
	//    the archive no longer has.
	assert.Emptyf(changedIDs(resumed),
		"the restore reverted message %d to a watermark below the consumer's "+
			"cursor, so the feed has nothing to report and the mirror silently "+
			"keeps content the archive no longer holds", edited)

	// 3. Nor does it ever come back: the cursor only rises from here.
	later := getChangesPage(t, srv, changesTarget(resumed.NextCursor, 100))
	assert.Empty(changedIDs(later),
		"a later poll cannot reach back under the cursor the consumer holds")

	// 4. The message the rollback removed produces no event either — the same
	//    blind spot hard deletions have.
	var missing int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`), added).Scan(&missing),
		"count the removed message")
	require.Zero(missing, "the restore must really have removed the added message")

	// 5. The documented repair is the same one the backward-clock exception
	//    names: a full re-read from an empty cursor. It restores the reverted
	//    row's tracked fields, and it is the only thing that reveals the removal
	//    — by never returning the row.
	reread := getChangesPage(t, srv, changesTarget("", 100))
	assert.ElementsMatchf([]int64{kept[0], kept[1]}, changedIDs(reread),
		"a full re-read returns the restored archive as it now stands, including "+
			"message %d at its reverted watermark, and never mentions message %d",
		edited, added)
}
