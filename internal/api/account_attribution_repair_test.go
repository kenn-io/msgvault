package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

type gatedRepairStore struct {
	*stubIdentityRebuildStore

	started chan int64
	release chan struct{}
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
	var conversationID int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'thread','email_thread') RETURNING id`), id).Scan(&conversationID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type) VALUES(?,'one',?,'email')`), id, conversationID)
	require.NoError(t, err)
	wrapped := &failFirstRepairStore{gatedRepairStore: &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 2), release: make(chan struct{})}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	require.True(t, srv.queueAccountAttributionRepair(t.Context(), wrapped, id, true))
	select {
	case <-wrapped.started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "automatic repair did not start")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", id), nil)
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	close(wrapped.release)
	select {
	case <-wrapped.started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "retry did not start after the failed automatic repair")
	}
	done := make(chan struct{})
	go func() { srv.importWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "retry did not finish")
	}
	pending, err := st.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, pending, id)
}

func TestAccountAttributionRepairKeepsWindowAfterGateWait(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	synctest.Test(t, func(t *testing.T) {
		wrapped := &failFirstRepairStore{gatedRepairStore: &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 1), release: make(chan struct{})}}
		gate := NewSerialOperationGate()
		srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: wrapped, Logger: testLogger(), OperationGate: gate})
		release, ok := gate.BeginWorkContext(t.Context())
		require.True(t, ok)
		require.True(t, srv.queueAccountAttributionRepair(t.Context(), wrapped, id, true))
		synctest.Wait()
		time.Sleep(11 * time.Minute)
		release()
		synctest.Wait()
		select {
		case got := <-wrapped.started:
			assert.Equal(t, id, got)
		default:
			assert.Fail(t, "repair did not start after waiting for the operation gate")
		}
		close(wrapped.release)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})
}

func (s *gatedRepairStore) RepairAccountAttributionContext(ctx context.Context, id int64, _ func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error) {
	s.started <- id
	select {
	case <-s.release:
		return s.Store.RepairAccountAttributionContext(ctx, id, nil)
	case <-ctx.Done():
		return store.AccountAttributionRepairSummary{}, ctx.Err()
	}
}

func TestAccountAttributionRepair(t *testing.T) {
	for _, finish := range []string{"complete", "shutdown"} {
		t.Run(finish, func(t *testing.T) {
			_, st, id := newIdentityRebuildTestServer(t)
			var conversationID int64
			err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'thread','email_thread') RETURNING id`), id).Scan(&conversationID)
			require.NoError(t, err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type) VALUES(?,'one',?,'email')`), id, conversationID)
			require.NoError(t, err)
			wrapped := &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 2), release: make(chan struct{})}
			srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				assert.NoError(t, srv.Shutdown(ctx))
			})
			request := func(sourceID int64) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", sourceID), nil)
				srv.Router().ServeHTTP(w, req)
				return w
			}
			if finish == "complete" {
				assert.Equal(t, http.StatusBadRequest, request(0).Code)
				assert.Equal(t, http.StatusNotFound, request(id+100).Code)
			}
			assert.Equal(t, http.StatusAccepted, request(id).Code)
			select {
			case got := <-wrapped.started:
				assert.Equal(t, id, got)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "repair did not start")
			}
			if finish == "complete" {
				assert.Equal(t, http.StatusAccepted, request(id).Code)
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
	wrapped := &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 1), release: make(chan struct{})}
	gate := NewSerialOperationGate()
	tracker := NewIdleTracker(time.Minute, nil)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	srv.operationGate, srv.idleTracker = gate, tracker
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	releaseGate, ok := gate.BeginWorkContext(t.Context())
	require.True(t, ok)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	req.SetPathValue("source_id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	srv.handleAccountAttributionRepair(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	_, idle := tracker.nextWait(time.Now().Add(time.Hour))
	assert.False(t, idle, "queued repair must prevent idle shutdown")
	select {
	case <-wrapped.started:
		require.FailNow(t, "repair began while the operation gate was held")
	default:
	}
	releaseGate()
	select {
	case got := <-wrapped.started:
		assert.Equal(t, id, got)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "repair did not start after the gate was released")
	}
	close(wrapped.release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))
	_, idle = tracker.nextWait(time.Now().Add(time.Hour))
	assert.True(t, idle)
}

func TestAccountAttributionRepairDuplicatesBypassGate(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	wrapped := &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 2), release: make(chan struct{})}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: wrapped, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	request := func() int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", id), nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w.Code
	}
	require.Equal(t, http.StatusAccepted, request())
	select {
	case got := <-wrapped.started:
		assert.Equal(t, id, got)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "repair did not start")
	}
	assert.Equal(t, http.StatusAccepted, request())
	close(wrapped.release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))
	assert.Empty(t, wrapped.started)
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

func TestAccountAttributionRecoveryObservesCommittedRevision(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	other, err := st.GetOrCreateSource("gmail", "other@example.net")
	require.NoError(t, err)
	var conversationID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'thread','email_thread') RETURNING id`), id).Scan(&conversationID)
	require.NoError(t, err)
	var messageID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,account_path) VALUES(?,'one',?,'email','inbound') RETURNING id`), id, conversationID).Scan(&messageID)
	require.NoError(t, err)
	wrapped := &observedRecoveryStore{stubIdentityRebuildStore: st, scanned: make(chan struct{}, 4), done: make(chan store.AccountAttributionRepairSummary, 4)}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	ticks := make(chan time.Time)
	srv.importWG.Go(func() { srv.observeAccountAttribution(wrapped, ticks) })
	select {
	case <-wrapped.scanned:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "initial discovery did not finish")
	}
	// A separate connection commits the pending row and revision together.
	conn, err := st.DB().Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), st.Rebind(`UPDATE messages SET account_path = NULL WHERE id = ?`), messageID)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO archive_metadata(key,value) VALUES('account_identity_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(archive_metadata.value AS INTEGER)+1 AS TEXT)`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "observer did not accept the next sweep")
	}
	select {
	case <-wrapped.scanned:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "changed revision was not discovered")
	}
	select {
	case summary := <-wrapped.done:
		assert.Equal(t, int64(1), summary.Scanned)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "pending matching did not recover")
	}
	var pending int
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND account_path IS NULL`), id).Scan(&pending)
	require.NoError(t, err)
	assert.Zero(t, pending)
	ids, err := st.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, ids, other.ID)
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
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	ticks := make(chan time.Time)
	srv.importWG.Go(func() { srv.observeAccountAttribution(wrapped, ticks) })
	select {
	case <-wrapped.scanned:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "initial discovery did not finish")
	}
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
	select {
	case summary := <-wrapped.done:
		assert.Equal(t, int64(1), summary.Scanned)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "pending matching did not recover")
	}
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

func TestAccountCatalogRefreshesAfterRepair(t *testing.T) {
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
	assert.Contains(t, get(), `"pending_count":1`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", id), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	done := make(chan struct{})
	go func() { srv.importWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "repair did not finish")
	}
	assert.NotContains(t, get(), `"pending_count":1`)
}

type revisionRecoveryStore struct {
	*gatedRepairStore

	revision atomic.Int64
	id       int64
	reads    chan int64
	scans    chan struct{}
}

func (s *revisionRecoveryStore) AccountIdentityRevisionContext(context.Context) (int64, error) {
	revision := s.revision.Load()
	s.reads <- revision
	return revision, nil
}

func (s *revisionRecoveryStore) PendingAccountAttributionSourcesContext(context.Context) ([]int64, error) {
	s.scans <- struct{}{}
	return []int64{s.id}, nil
}

func TestAccountAttributionRecoveryRearmsOnlyOnNewRevision(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	wrapped := &revisionRecoveryStore{
		gatedRepairStore: &gatedRepairStore{stubIdentityRebuildStore: st, started: make(chan int64, 4), release: make(chan struct{})},
		id:               id, reads: make(chan int64, 4), scans: make(chan struct{}, 4),
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	ticks := make(chan time.Time)
	srv.importWG.Go(func() { srv.observeAccountAttribution(wrapped, ticks) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	read := func() {
		select {
		case <-wrapped.reads:
		case <-ctx.Done():
			require.FailNow(t, "observer did not read revision")
		}
	}
	scan := func() {
		select {
		case <-wrapped.scans:
		case <-ctx.Done():
			require.FailNow(t, "observer did not scan")
		}
	}
	tick := func() {
		select {
		case ticks <- time.Now():
		case <-ctx.Done():
			require.FailNow(t, "observer did not accept tick")
		}
	}
	read()
	scan()
	select {
	case <-wrapped.started:
	case <-ctx.Done():
		require.FailNow(t, "repair did not start")
	}
	tick()
	read()
	tick()
	read()
	assert.Empty(t, wrapped.scans, "an unchanged revision must not rediscover work")
	wrapped.revision.Add(1)
	tick()
	read()
	scan()
	// The revised sweep reaches the same source while the first pass is active.
	tick()
	read()
	close(wrapped.release)
	select {
	case got := <-wrapped.started:
		assert.Equal(t, id, got)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "new revision did not rearm the active source")
	}
}

func TestAccountCatalogIgnoresPreRepairFlight(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	var conversationID int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'thread','email_thread') RETURNING id`), id).Scan(&conversationID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type) VALUES(?,'one',?,'email')`), id, conversationID)
	require.NoError(t, err)
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
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, catalog, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	oldDone := make(chan struct{})
	go func() {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil))
		close(oldDone)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "old catalog did not start")
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", id), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	done := make(chan struct{})
	go func() { srv.importWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "repair did not finish")
	}
	get := func() string {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil))
		require.Equal(t, http.StatusOK, w.Code)
		return w.Body.String()
	}
	assert.NotContains(t, get(), `"pending_count":1`)
	close(release)
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "old catalog did not finish")
	}
	assert.NotContains(t, get(), `"pending_count":1`)
}

type failingRecoveryStore struct {
	*stubIdentityRebuildStore

	id       int64
	reads    chan struct{}
	attempts chan int64
}

func (s *failingRecoveryStore) AccountIdentityRevisionContext(context.Context) (int64, error) {
	s.reads <- struct{}{}
	return 1, nil
}

func (s *failingRecoveryStore) PendingAccountAttributionSourcesContext(context.Context) ([]int64, error) {
	return []int64{s.id}, nil
}

func (s *failingRecoveryStore) RepairAccountAttributionContext(_ context.Context, id int64, _ func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error) {
	s.attempts <- id
	return store.AccountAttributionRepairSummary{}, errors.New("permanent failure")
}

func TestAccountAttributionRecoveryWaitsForRevisionOrRetryAfterFailure(t *testing.T) {
	_, st, id := newIdentityRebuildTestServer(t)
	wrapped := &failingRecoveryStore{stubIdentityRebuildStore: st, id: id, reads: make(chan struct{}, 4), attempts: make(chan int64, 4)}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	ticks := make(chan time.Time)
	observerDone := make(chan struct{})
	go func() { srv.observeAccountAttribution(wrapped, ticks); close(observerDone) }()
	t.Cleanup(func() {
		srv.cancelImports()
		select {
		case <-observerDone:
		case <-time.After(5 * time.Second):
			require.Fail(t, "observer did not stop")
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	read := func() {
		select {
		case <-wrapped.reads:
		case <-ctx.Done():
			require.FailNow(t, "observer did not read revision")
		}
	}
	tick := func() {
		select {
		case ticks <- time.Now():
		case <-ctx.Done():
			require.FailNow(t, "observer did not accept tick")
		}
	}
	read()
	select {
	case got := <-wrapped.attempts:
		assert.Equal(t, id, got)
	case <-ctx.Done():
		require.FailNow(t, "initial repair did not start")
	}
	tick()
	read()
	tick()
	read()
	assert.Empty(t, wrapped.attempts)
	done := make(chan struct{})
	go func() { srv.importWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		require.FailNow(t, "failed repair did not finish")
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/sources/%d/account-attribution/repair", id), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	select {
	case got := <-wrapped.attempts:
		assert.Equal(t, id, got)
	case <-ctx.Done():
		require.FailNow(t, "explicit retry did not start")
	}
}
