package rederive

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEmailFullWalkCheckpointWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	const rows = 256
	for i := range rows {
		archiveCurrentEmail(t, st, src, strconv.Itoa(i))
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
	t.Cleanup(func() {
		slog.SetDefault(previous)
		store.ConfigureSQLLogging(store.SQLLogOptions{})
	})
	start := time.Now()
	var before int64
	if !st.IsPostgreSQL() {
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scanned := 0
	first, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) {
		scanned++
		if scanned == rows/2 {
			cancel()
		}
	})
	require.ErrorIs(err, context.Canceled)
	second, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	reads := strings.Count(logs.String(), `"kind":"query`)
	t.Logf("rows=%d scanned=%d reads=%d elapsed=%s", rows,
		first.MessagesScanned+second.MessagesScanned, reads, time.Since(start))
	assert.Equal(int64(rows), first.MessagesScanned+second.MessagesScanned)
	assert.LessOrEqual(reads, 2*rows+16, "resume adds bounded ledger reads, not a prefix rescan")
	if !st.IsPostgreSQL() {
		var after int64
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after))
		t.Logf("SQLite row changes during current-text repair: %d", after-before)
		assert.LessOrEqual(after-before, int64(4), "two batched cursor writes, one old-cursor deletion, and completion")
	}
	logs.Reset()
	_, ran, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.False(ran)
}
