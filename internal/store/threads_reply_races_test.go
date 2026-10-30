package store_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func threadsReceiptDSN(t *testing.T, st *store.Store) string {
	t.Helper()
	if st.IsPostgreSQL() {
		var database, schema string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT current_database(),current_schema()`).Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		u.Path = "/" + database
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	rows, err := st.DB().QueryContext(t.Context(), `PRAGMA database_list`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var seq int
		var name, path string
		require.NoError(t, rows.Scan(&seq, &name, &path))
		if name == "main" {
			return path
		}
	}
	require.NoError(t, rows.Err())
	require.FailNow(t, "missing main archive")
	return ""
}
func threadsSecondStore(t *testing.T, st *store.Store, application string) *store.Store {
	t.Helper()
	dsn := threadsReceiptDSN(t, st)
	if st.IsPostgreSQL() {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		q := u.Query()
		q.Set("application_name", application)
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	other, err := store.OpenForTest(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	return other
}
func threadsReadyReceipt(t *testing.T, st *store.Store, conv, parent int64) store.ThreadsReplySend {
	t.Helper()
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "reviewed", allowChatDraft)
	require.NoError(t, err)
	receipt, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(t, err)
	at := time.Now().UTC()
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "container_saved", ContainerID: "400", ContainerCreatedAt: &at})
	require.NoError(t, err)
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "ready"})
	require.NoError(t, err)
	return receipt
}
func TestThreadsReceiptConcurrentClaimsAndTransitions(t *testing.T) {
	assert := Assert.New(t)
	require := require.New(t)
	st, _, conv, parent := threadsDraftDestination(t)
	other := threadsSecondStore(t, st, "threads-claim-race")
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "one reviewed body", allowChatDraft)
	require.NoError(err)
	type outcome struct {
		receipt store.ThreadsReplySend
		err     error
	}
	results := make(chan outcome, 8)
	start := make(chan struct{})
	for n := range 8 {
		go func() {
			<-start
			handle := st
			if n%2 == 1 {
				handle = other
			}
			r, err := handle.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
			results <- outcome{r, err}
		}()
	}
	close(start)
	var first store.ThreadsReplySend
	for n := range 8 {
		result := <-results
		require.NoError(result.err)
		if n == 0 {
			first = result.receipt
		}
		assert.Equal(first, result.receipt)
	}
	assert.Equal("one reviewed body", first.Body)
	at := time.Now().UTC()
	start = make(chan struct{})
	for n, handle := range []*store.Store{st, other} {
		go func() {
			<-start
			r, err := handle.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, 1, store.ThreadsReplyTransition{State: "container_saved", ContainerID: strconv.Itoa(400 + n), ContainerCreatedAt: &at})
			results <- outcome{r, err}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			winners++
		} else {
			assert.ErrorIs(result.err, store.ErrThreadsReplyConflict)
		}
	}
	assert.Equal(1, winners)
}

// The advisory barrier holds claim inside INSERT after its draft row lock.
// A different Store connection must wait, then re-read the committed receipt.
func TestThreadsReceiptPGClaimVersusMutation(t *testing.T) {
	for _, operation := range []string{"edit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			st, _, conv, parent := threadsDraftDestination(t)
			if !st.IsPostgreSQL() {
				t.Skip("requires PostgreSQL row-lock interleavings")
			}
			other := threadsSecondStore(t, st, "threads-mutation-race")
			draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "immutable", allowChatDraft)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION hold_threads_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(912230); RETURN NEW; END $$;
   CREATE TRIGGER threads_claim_barrier BEFORE INSERT ON threads_reply_sends FOR EACH ROW EXECUTE FUNCTION hold_threads_claim()`)
			require.NoError(err)
			barrier, err := st.DB().Conn(t.Context())
			require.NoError(err)
			defer func() { require.NoError(barrier.Close()) }()
			_, err = barrier.ExecContext(t.Context(), `SELECT pg_advisory_lock(912230)`)
			require.NoError(err)
			defer func() {
				_, err := barrier.ExecContext(context.WithoutCancel(t.Context()), `SELECT pg_advisory_unlock_all()`)
				require.NoError(err)
			}()
			claimed := make(chan error, 1)
			go func() { _, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1); claimed <- err }()
			require.EventuallyWithT(func(c *Assert.CollectT) {
				assert := Assert.New(c)
				var waiting bool
				err := st.DB().QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=912230 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting)
				assert.NoError(err)
				assert.True(waiting)
			}, 20*time.Second, 10*time.Millisecond)
			mutated := make(chan error, 1)
			go func() {
				if operation == "edit" {
					_, err := other.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "changed")
					mutated <- err
				} else {
					mutated <- other.DeleteChatDraftContext(t.Context(), draft.DraftID, 1)
				}
			}()
			require.EventuallyWithT(func(c *Assert.CollectT) {
				assert := Assert.New(c)
				var waiting bool
				err := st.DB().QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='threads-mutation-race' AND wait_event_type='Lock')`).Scan(&waiting)
				assert.NoError(err)
				assert.True(waiting)
			}, 20*time.Second, 10*time.Millisecond)
			_, err = barrier.ExecContext(t.Context(), `SELECT pg_advisory_unlock(912230)`)
			require.NoError(err)
			require.NoError(<-claimed)
			assert.ErrorIs(<-mutated, store.ErrChatDraftClaimed)
			got, err := st.GetChatDraftContext(t.Context(), draft.DraftID)
			require.NoError(err)
			assert.Equal("immutable", got.Body)
		})
	}
}
func TestThreadsReceiptDispatchCommitFailure(t *testing.T) {
	assert := Assert.New(t)
	require := require.New(t)
	st, _, conv, parent := threadsDraftDestination(t)
	r := threadsReadyReceipt(t, st, conv, parent)
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_threads_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic_commit_failure'; END $$;
   CREATE CONSTRAINT TRIGGER threads_commit_failure AFTER UPDATE ON threads_reply_sends DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state='publish_dispatched') EXECUTE FUNCTION fail_threads_commit()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TABLE threads_commit_parent(id INTEGER PRIMARY KEY); CREATE TABLE threads_commit_child(id INTEGER REFERENCES threads_commit_parent(id) DEFERRABLE INITIALLY DEFERRED);
   CREATE TRIGGER threads_commit_failure AFTER UPDATE ON threads_reply_sends WHEN NEW.state='publish_dispatched' BEGIN INSERT INTO threads_commit_child(id) VALUES(1); END`)
	}
	require.NoError(err)
	won, err := st.TryDispatchThreadsReplyContext(t.Context(), r.DraftID, r.RowRevision)
	require.Error(err)
	assert.False(won, "an UPDATE without a confirmed COMMIT never authorizes publication")
	saved, err := st.GetThreadsReplySendContext(t.Context(), r.DraftID)
	require.NoError(err)
	assert.Equal("ready", saved.State)
	assert.Nil(saved.PublishDispatchedAt)
}

// The proxy drops the real server's COMMIT success response. The transaction
// has committed, while the client receives an error and must lose dispatch.
func threadsCommitDropProxy(t *testing.T, dsn string, armed *atomic.Bool) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	socket := q.Get("host")
	port := u.Port()
	if port == "" {
		port = q.Get("port")
	}
	if port == "" {
		port = "5432"
	}
	network, address := "tcp", net.JoinHostPort(u.Hostname(), port)
	if len(socket) > 0 && socket[0] == '/' {
		network, address = "unix", socket+"/.s.PGSQL."+port
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var wg sync.WaitGroup
	t.Cleanup(func() { require.NoError(t, listener.Close()); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = client.Close() }()
				server, err := net.Dial(network, address)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
				for {
					var header [5]byte
					if _, err = io.ReadFull(server, header[:]); err != nil {
						return
					}
					size := int(binary.BigEndian.Uint32(header[1:]))
					if size < 4 || size > 64<<20 {
						return
					}
					body := make([]byte, size-4)
					if _, err = io.ReadFull(server, body); err != nil {
						return
					}
					if header[0] == 'C' && string(body) == "COMMIT\x00" && armed.CompareAndSwap(true, false) {
						return
					}
					if _, err = client.Write(header[:]); err != nil {
						return
					}
					if _, err = client.Write(body); err != nil {
						return
					}
				}
			}()
		}
	}()
	u.Host = listener.Addr().String()
	q.Del("host")
	q.Del("port")
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}
func TestThreadsReceiptDispatchCommitResponseLostPG(t *testing.T) {
	assert := Assert.New(t)
	require := require.New(t)
	st, _, conv, parent := threadsDraftDestination(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires real PostgreSQL COMMIT response loss")
	}
	r := threadsReadyReceipt(t, st, conv, parent)
	var armed atomic.Bool
	dsn := threadsCommitDropProxy(t, threadsReceiptDSN(t, st), &armed)
	viaProxy, err := store.Open(dsn)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(viaProxy.Close()) })
	armed.Store(true)
	won, err := viaProxy.TryDispatchThreadsReplyContext(t.Context(), r.DraftID, r.RowRevision)
	require.Error(err)
	assert.False(won)
	assert.False(armed.Load(), "the proxy observed the server's committed transaction")
	saved, err := st.GetThreadsReplySendContext(t.Context(), r.DraftID)
	require.NoError(err)
	assert.Equal("publish_dispatched", saved.State)
	assert.NotNil(saved.PublishDispatchedAt)
	won, err = st.TryDispatchThreadsReplyContext(t.Context(), r.DraftID, saved.RowRevision)
	assert.ErrorIs(err, store.ErrThreadsReplyConflict)
	assert.False(won)
}
