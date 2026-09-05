//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

// insertScopedMessage adds a message (with a body, so it embeds rather than
// skip-stamping as empty) belonging to the given source.
func insertScopedMessage(t *testing.T, db *sql.DB, id int64, sourceID int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO messages (id, subject, source_id, message_type) VALUES (?, ?, ?, 'email')`,
		id, fmt.Sprintf("msg %d", id), sourceID)
	require.NoError(t, err, "insert message")
	_, err = db.Exec(
		`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`,
		id, fmt.Sprintf("body %d", id))
	require.NoError(t, err, "insert body")
}

// TestWorker_SourceScopeSkipsOutOfScopeMessages runs the drain against a
// corpus spanning three sources with the build scope pinned to one of them:
// only in-scope messages may be embedded and stamped, even with a batch size
// of 1 forcing many scans past out-of-scope rows.
func TestWorker_SourceScopeSkipsOutOfScopeMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 0)

	insertScopedMessage(t, f.MainDB, 1, 1)
	insertScopedMessage(t, f.MainDB, 2, 2)
	insertScopedMessage(t, f.MainDB, 3, 2)
	insertScopedMessage(t, f.MainDB, 4, 3)

	w := NewWorker(WorkerDeps{
		Backend:    f.Backend,
		VectorsDB:  f.VectorsDB,
		MainDB:     f.MainDB,
		Store:      f.Store,
		Client:     f.FakeClient,
		BatchSize:  1,
		BuildScope: vector.NewBuildScope(nil, []int64{2}),
		Recorder:   f.Recorder,
	})
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")
	assert.Equal(2, res.Claimed, "only source 2's messages are claimed")
	assert.Equal(2, res.Succeeded, "only source 2's messages embed")

	gen := int64(f.BuildingGen)
	for _, id := range []int64{2, 3} {
		got, isNull := embedGenOf(t, f.MainDB, id)
		require.False(isNull, "message %d stamped", id)
		assert.Equal(gen, got, "message %d stamped for the building generation", id)
	}
	for _, id := range []int64{1, 4} {
		_, isNull := embedGenOf(t, f.MainDB, id)
		assert.True(isNull, "out-of-scope message %d keeps embed_gen NULL", id)
	}

	// A second run finds no remaining work for the scoped generation.
	res, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce again")
	assert.Equal(0, res.Claimed, "scoped drain is complete")
}
