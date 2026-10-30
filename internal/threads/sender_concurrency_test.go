package threads

import (
	"context"
	"net/url"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func concurrentSenderStore(t *testing.T, st *store.Store) *store.Store {
	t.Helper()
	var dsn string
	if st.IsPostgreSQL() {
		var database, schema string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT current_database(),current_schema()`).Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		u.Path = "/" + database
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		rows, err := st.DB().QueryContext(t.Context(), `PRAGMA database_list`)
		require.NoError(t, err)
		for rows.Next() {
			var seq int
			var name, path string
			require.NoError(t, rows.Scan(&seq, &name, &path))
			if name == "main" {
				dsn = path
			}
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	require.NotEmpty(t, dsn)
	other, err := store.OpenForTest(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	return other
}
func TestThreadsOnePublishConcurrentCalls(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	other := concurrentSenderStore(t, f.st)
	type outcome struct {
		result SendResult
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, st := range []*store.Store{f.st, other} {
		go func() {
			<-start
			result, err := NewSender(st, f.options).Send(t.Context(), f.request())
			results <- outcome{result, err}
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		out := <-results
		if out.err == nil && out.result.MediaID != "" {
			successes++
		} else if out.err != nil {
			assert.ErrorIs(out.err, store.ErrThreadsReplyConflict)
		}
	}
	assert.GreaterOrEqual(successes, 1)
	assert.Equal(int32(1), f.publishes.Load())
	receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal("500", receipt.MediaID)
	_, err = NewSender(other, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsPublishProtocolDefaultWaitCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sender := NewSender(nil, SenderOptions{})
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- sender.options.Wait(ctx, time.Minute) }()
		time.Sleep(10 * time.Second)
		cancel()
		assert.ErrorIs(t, <-result, context.Canceled)
	})
}
