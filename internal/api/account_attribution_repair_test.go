package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// newAccountRepairTestServer serves st behind the real operation gate.
func newAccountRepairTestServer(t *testing.T, st MessageStore) *Server {
	t.Helper()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	return srv
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		require.FailNow(t, what+" did not happen")
		panic("unreachable")
	}
}

func serveAccountRepair(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return w
}

func postAccountRepair(t *testing.T, srv *Server, sourceID int64) int {
	t.Helper()
	return serveAccountRepair(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", sourceID)).Code
}

func waitForImports(t *testing.T, srv *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() { srv.importWG.Wait(); close(done) }()
	receive(t, done, "account matching")
}

func addPendingAccountMessage(t *testing.T, st *store.Store, sourceID int64) {
	t.Helper()
	var conversationID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'thread','email_thread') RETURNING id`), sourceID).Scan(&conversationID))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type) VALUES(?,'one',?,'email')`), sourceID, conversationID)
	require.NoError(t, err)
}

type gatedRepairStore struct {
	*stubIdentityRebuildStore

	started    chan int64
	release    chan struct{}
	contextErr chan error
	finished   chan error
}

type failFirstRepairStore struct {
	*gatedRepairStore

	attempts atomic.Int32
}

func (s *failFirstRepairStore) RepairAccountAttributionContext(ctx context.Context, id int64, progress func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error) {
	if s.attempts.Add(1) == 1 {
		s.started <- id
		select {
		case <-s.release:
			return store.AccountAttributionRepairSummary{}, errors.New("repair failed")
		case <-ctx.Done():
			return store.AccountAttributionRepairSummary{}, ctx.Err()
		}
	}
	return s.gatedRepairStore.RepairAccountAttributionContext(ctx, id, progress)
}

func TestAccountAttributionRetryDuringFailedAutomaticRepair(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	addPendingAccountMessage(t, st.Store, id)
	wrapped := &failFirstRepairStore{gatedRepairStore: &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 2), release: make(chan struct{})}}
	srv := newAccountRepairTestServer(t, wrapped)
	require.True(t, srv.queueAccountAttributionRepair(t.Context(), wrapped, id, true))
	assert.Equal(t, id, receive(t, wrapped.started, "automatic repair"))
	require.Equal(t, http.StatusAccepted, postAccountRepair(t, srv, id))
	close(wrapped.release)
	assert.Equal(t, id, receive(t, wrapped.started, "retry"))
	waitForImports(t, srv)
	pending, err := st.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, pending, id)
}

func (s *gatedRepairStore) RepairAccountAttributionContext(ctx context.Context, id int64, _ func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error) {
	s.started <- id
	if s.contextErr != nil {
		s.contextErr <- ctx.Err()
	}
	select {
	case <-s.release:
		summary, err := s.Store.RepairAccountAttributionContext(ctx, id, nil)
		if s.finished != nil {
			s.finished <- err
		}
		return summary, err
	case <-ctx.Done():
		return store.AccountAttributionRepairSummary{}, ctx.Err()
	}
}

func TestAccountAttributionRepair(t *testing.T) {
	for _, finish := range []string{"complete", "shutdown"} {
		t.Run(finish, func(t *testing.T) {
			_, st, id := newIdentityRebuildTestServer(t)
			addPendingAccountMessage(t, st.Store, id)
			wrapped := &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 2), release: make(chan struct{})}
			srv := newAccountRepairTestServer(t, wrapped)
			if finish == "complete" {
				assert.Equal(t, http.StatusBadRequest, postAccountRepair(t, srv, 0))
				assert.Equal(t, http.StatusNotFound, postAccountRepair(t, srv, id+100))
			}
			assert.Equal(t, http.StatusAccepted, postAccountRepair(t, srv, id))
			assert.Equal(t, id, receive(t, wrapped.started, "repair"))
			if finish == "complete" {
				// The repair holds the operation gate; a repeat request joins it.
				assert.Equal(t, http.StatusAccepted, postAccountRepair(t, srv, id))
				close(wrapped.release)
				requireCacheBuild(t, st)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, srv.Shutdown(ctx))
			if finish == "complete" {
				assert.Empty(t, wrapped.started, "duplicate request started another repair")
			}
		})
	}
}

func TestAccountAttributionRepairWaitsForGateAndPreventsIdle(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	synctest.Test(t, func(t *testing.T) {
		wrapped := &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 1), release: make(chan struct{}), contextErr: make(chan error, 1), finished: make(chan error, 1)}
		gate := NewSerialOperationGate()
		tracker := NewIdleTracker(time.Minute, nil)
		srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: wrapped, Logger: testLogger(), OperationGate: gate})
		srv.idleTracker = tracker
		releaseGate, ok := gate.BeginWorkContext(t.Context())
		require.True(t, ok)
		require.Equal(t, http.StatusAccepted, postAccountRepair(t, srv, id))
		_, idle := tracker.nextWait(time.Now().Add(time.Hour))
		assert.False(t, idle, "queued repair must prevent idle shutdown")
		// The ten-minute bound starts once the repair holds the gate.
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		assert.Empty(t, wrapped.started, "repair began while the operation gate was held")
		releaseGate()
		synctest.Wait()
		select {
		case got := <-wrapped.started:
			assert.Equal(t, id, got)
			require.NoError(t, <-wrapped.contextErr)
		default:
			assert.Fail(t, "repair did not start after the gate was released")
		}
		close(wrapped.release)
		require.NoError(t, <-wrapped.finished)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
		_, idle = tracker.nextWait(time.Now().Add(time.Hour))
		assert.True(t, idle)
	})
}

type observedRecoveryStore struct {
	*stubIdentityRebuildStore

	scanned chan struct{}
	done    chan store.AccountAttributionRepairSummary
}

func (s *observedRecoveryStore) PendingAccountAttributionSourcesContext(ctx context.Context) ([]int64, error) {
	ids, err := s.Store.PendingAccountAttributionSourcesContext(ctx)
	s.scanned <- struct{}{}
	return ids, err
}

func (s *observedRecoveryStore) RepairAccountAttributionContext(ctx context.Context, id int64, progress func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error) {
	summary, err := s.Store.RepairAccountAttributionContext(ctx, id, progress)
	s.done <- summary
	return summary, err
}

func TestAccountAttributionRecoveryAfterChildCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	parent := testutil.NewSQLiteTestStoreAt(t, path)
	child, err := store.OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, child.Close()) })
	source, err := parent.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	other, err := parent.GetOrCreateSource("gmail", "other@example.net")
	require.NoError(t, err)
	_, err = parent.DB().ExecContext(t.Context(), `UPDATE sources SET sync_cursor = CASE id WHEN ? THEN 'first-cursor' ELSE 'other-cursor' END WHERE id IN (?, ?)`, source.ID, source.ID, other.ID)
	require.NoError(t, err)
	_, err = parent.DB().ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,?,'first','email_thread'),(2,?,'other','email_thread'); INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,account_address,account_path) VALUES(1,?,'first',1,'email','work@example.org','inbound'),(2,?,'other',2,'email','other@example.net','inbound')`, source.ID, other.ID, source.ID, other.ID)
	require.NoError(t, err)
	wrapped := &observedRecoveryStore{stubIdentityRebuildStore: &stubIdentityRebuildStore{Store: parent, buildCh: make(chan bool, 4)}, scanned: make(chan struct{}, 4), done: make(chan store.AccountAttributionRepairSummary, 4)}
	srv := newAccountRepairTestServer(t, wrapped)
	ticks := make(chan time.Time)
	srv.importWG.Go(func() { srv.observeAccountAttribution(wrapped, ticks) })
	receive(t, wrapped.scanned, "initial discovery")
	// The child's identity transaction commits, then its request is canceled
	// before it can match the rows it returned to pending.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	child.DB().SetMaxOpenConns(1)
	conn, err := child.DB().Conn(t.Context())
	require.NoError(t, err)
	err = conn.Raw(func(raw any) error {
		sqliteConn, ok := raw.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected sqlite connection type %T", raw)
		}
		sqliteConn.RegisterCommitHook(func() int { cancel(); return 0 })
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, child.AddAccountIdentityContext(ctx, source.ID, "work@example.org", "manual"))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	pending, err := parent.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	require.Contains(t, pending, source.ID)
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "observer did not accept the next sweep")
	}
	assert.Equal(t, int64(1), receive(t, wrapped.done, "recovery").Scanned)
	pending, err = parent.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, pending, source.ID)
	var otherAddress, otherPath, firstCursor, otherCursor string
	require.NoError(t, parent.DB().QueryRowContext(t.Context(), `SELECT account_address, account_path FROM messages WHERE id = 2`).Scan(&otherAddress, &otherPath))
	require.NoError(t, parent.DB().QueryRowContext(t.Context(), `SELECT sync_cursor FROM sources WHERE id = ?`, source.ID).Scan(&firstCursor))
	require.NoError(t, parent.DB().QueryRowContext(t.Context(), `SELECT sync_cursor FROM sources WHERE id = ?`, other.ID).Scan(&otherCursor))
	assert.Equal(t, "other@example.net", otherAddress)
	assert.Equal(t, "inbound", otherPath)
	assert.Equal(t, "first-cursor", firstCursor)
	assert.Equal(t, "other-cursor", otherCursor)
}

func TestAccountAttributionRecoveryObservesCommittedRevision(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL source lock coordinates the post-commit cancellation")
	}
	addPendingAccountMessage(t, st.Store, id)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET account_address = 'work@example.org', account_path = 'inbound' WHERE source_id = ?`), id)
	require.NoError(t, err)
	wrapped := &observedRecoveryStore{stubIdentityRebuildStore: st, scanned: make(chan struct{}, 4), done: make(chan store.AccountAttributionRepairSummary, 4)}
	srv := newAccountRepairTestServer(t, wrapped)
	ticks := make(chan time.Time)
	srv.importWG.Go(func() { srv.observeAccountAttribution(wrapped, ticks) })
	receive(t, wrapped.scanned, "initial discovery")
	before, err := st.AccountIdentityRevisionContext(t.Context())
	require.NoError(t, err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), st.Rebind(`SELECT id FROM sources WHERE id = ? FOR NO KEY UPDATE`), id)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- st.AddAccountIdentityContext(ctx, id, "work@example.org", "manual") }()
	require.Eventually(t, func() bool {
		revision, err := st.AccountIdentityRevisionContext(t.Context())
		return err == nil && revision > before
	}, 10*time.Second, 20*time.Millisecond)
	pending, err := st.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	require.Contains(t, pending, id)
	cancel()
	require.NoError(t, tx.Rollback())
	require.NoError(t, receive(t, finished, "canceled identity request"))
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "observer did not accept the next sweep")
	}
	assert.Equal(t, int64(1), receive(t, wrapped.done, "recovery").Scanned)
	pending, err = st.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, pending, id)
}

type revisionRecoveryStore struct {
	*stubIdentityRebuildStore

	revision atomic.Int64
	id       int64
	reads    chan struct{}
	scans    chan struct{}
	attempts chan int64
	release  chan struct{}
}

func (s *revisionRecoveryStore) AccountIdentityRevisionContext(context.Context) (int64, error) {
	s.reads <- struct{}{}
	return s.revision.Load(), nil
}

func (s *revisionRecoveryStore) PendingAccountAttributionSourcesContext(context.Context) ([]int64, error) {
	s.scans <- struct{}{}
	return []int64{s.id}, nil
}

func (s *revisionRecoveryStore) RepairAccountAttributionContext(ctx context.Context, id int64, _ func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error) {
	s.attempts <- id
	select {
	case <-s.release:
		return store.AccountAttributionRepairSummary{}, errors.New("permanent failure")
	case <-ctx.Done():
		return store.AccountAttributionRepairSummary{}, ctx.Err()
	}
}

func TestAccountAttributionRecoveryRearmsOnlyOnNewRevisionOrRetry(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	wrapped := &revisionRecoveryStore{stubIdentityRebuildStore: st, id: id, reads: make(chan struct{}, 4), scans: make(chan struct{}, 4), attempts: make(chan int64, 4), release: make(chan struct{})}
	srv := newAccountRepairTestServer(t, wrapped)
	ticks := make(chan time.Time)
	observerDone := make(chan struct{})
	go func() { srv.observeAccountAttribution(wrapped, ticks); close(observerDone) }()
	t.Cleanup(func() {
		srv.cancelImports()
		receive(t, observerDone, "observer shutdown")
	})
	sweep := func() {
		select {
		case ticks <- time.Now():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "observer did not accept tick")
		}
		receive(t, wrapped.reads, "revision read")
	}
	receive(t, wrapped.reads, "revision read")
	receive(t, wrapped.scans, "initial discovery")
	assert.Equal(t, id, receive(t, wrapped.attempts, "initial repair"))
	sweep()
	sweep()
	assert.Empty(t, wrapped.scans, "an unchanged revision must not rediscover work")
	// A new revision during the active repair reruns it once it stops.
	wrapped.revision.Add(1)
	sweep()
	receive(t, wrapped.scans, "discovery after a new revision")
	sweep()
	close(wrapped.release)
	assert.Equal(t, id, receive(t, wrapped.attempts, "rerun after a new revision"))
	waitForImports(t, srv)
	// A permanent failure waits for another revision or an explicit Retry.
	sweep()
	assert.Empty(t, wrapped.attempts)
	require.Equal(t, http.StatusAccepted, postAccountRepair(t, srv, id))
	assert.Equal(t, id, receive(t, wrapped.attempts, "explicit retry"))
}

func TestAccountCatalogRefreshesAfterRepairAndIgnoresOlderRead(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	addPendingAccountMessage(t, st.Store, id)
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	catalog := &catalogReadStore{Store: st.Store, readCatalog: func(ctx context.Context) (map[int64][]store.VirtualAccount, error) {
		data, err := st.ListVirtualAccountsContext(ctx)
		if reads.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return data, err
	}}
	srv := newAccountRepairTestServer(t, catalog)
	get := func() string {
		w := serveAccountRepair(t, srv, http.MethodGet, "/api/v1/cli/accounts")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"virtual_accounts_unavailable":false`)
		return w.Body.String()
	}
	oldDone := make(chan string, 1)
	go func() { oldDone <- serveAccountRepair(t, srv, http.MethodGet, "/api/v1/cli/accounts").Body.String() }()
	receive(t, started, "pre-repair catalog read")
	// Another process repairs the source without changing an identity.
	summary, err := st.RepairAccountAttributionContext(t.Context(), id, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.Scanned)
	assert.NotContains(t, get(), `"pending_count":1`)
	close(release)
	receive(t, oldDone, "pre-repair catalog completion")
	assert.NotContains(t, get(), `"pending_count":1`)
}

func TestAccountCatalogRefreshesAfterExternalRepair(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	var conversationID int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'thread','email_thread') RETURNING id`), id).Scan(&conversationID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type) VALUES(?,'one',?,'email')`), id, conversationID)
	require.NoError(t, err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	get := func() string {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return w.Body.String()
	}
	require.Contains(t, get(), `"pending_count":1`)
	_, err = st.RepairAccountAttributionContext(t.Context(), id, nil)
	require.NoError(t, err)
	assert.NotContains(t, get(), `"pending_count":1`)
}
