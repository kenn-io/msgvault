package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Publishing ready must yield another scheduler pass while an old generation
// still needs cleanup. Readiness and completion of maintenance are separate.
func TestFilenameDiscoveryBootstrapReadyWorkReschedule(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	createJournalAttachment(t, f, f.CreateMessage("filename-ready-work"), "part:1", "a")
	old := filenameReady(t, f.Store)
	require.NoError(f.Store.ResetFilenameDiscoveryContext(t.Context()))
	scan, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	require.Equal("catchup", scan.Status.Phase)
	ready, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	require.Equal("ready", ready.Status.Phase)
	assert.True(ready.More, "old generations require another bounded pass")
	assert.Equal(1, filenameEntries(t, f.Store, old.Generation))
	gc, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(1, gc.Processed)
	assert.False(gc.More)
	assert.Zero(filenameEntries(t, f.Store, old.Generation))
}

// An idle daemon runs a pass every five seconds. A pass with no journal
// events or old generations must not need the archive writer, so it cannot
// block or be blocked by an import holding it. Real work still runs.
func TestFilenameDiscoveryIdlePassNeedsNoWriter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-idle-writer")
	createJournalAttachment(t, f, message, "part:1", "a")
	ready := filenameReady(t, f.Store)
	drained, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	require.False(drained.More)

	writer, err := f.Store.DB().Conn(t.Context())
	require.NoError(err)
	defer func() { assert.NoError(writer.Close()) }()
	if f.Store.IsPostgreSQL() {
		_, err = writer.ExecContext(t.Context(), `BEGIN`)
		require.NoError(err)
		_, err = writer.ExecContext(t.Context(), `LOCK TABLE attachments IN ROW EXCLUSIVE MODE`)
	} else {
		_, err = writer.ExecContext(t.Context(), `BEGIN IMMEDIATE`)
	}
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	idle, err := f.Store.FilenameDiscoveryBatchContext(ctx)
	require.NoError(err, "an idle pass must not wait for the archive writer")
	assert.Zero(idle.Processed)
	assert.False(idle.More)
	assert.Equal(ready.Revision, idle.Status.Revision)
	_, err = writer.ExecContext(t.Context(), `ROLLBACK`)
	require.NoError(err)

	id := createJournalAttachment(t, f, message, "part:2", "b")
	work, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(1, work.Processed)
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, ready.Generation, id))
}
