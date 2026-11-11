package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/x"
)

func TestXRestoreRejectsSnapshotBeforePaidReservation(t *testing.T) {
	for _, settled := range []bool{false, true} {
		name := "pending"
		if settled {
			name = "settled"
		}
		t.Run(name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXRestoreFixture(t)
			state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			_testRequire.NoError(err)
			generation := int64(1)
			_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation})
			_testRequire.NoError(err)
			old := f.snapshot(t)
			reservation, err := f.st.ReserveXRequestContext(t.Context(), f.source.ID, store.XRequestCost{
				Kind: store.XRequestTimeline, OwnerID: "1001", Generation: generation,
				GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 1_000_000,
				RequestedResults: 5, PostPriceMicros: 5_000, UserPriceMicros: 10_000,
				At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
			})
			_testRequire.NoError(err)
			if settled {
				_testRequire.NoError(f.st.SettleXRequestContext(t.Context(), f.source.ID, reservation.ID, store.XResourceUsage{Posts: 1, Users: 1, Persisted: true, Complete: true}))
			}
			current, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			_testRequire.NoError(err)
			_testRequire.NotEmpty(current.SpendBuckets)
			record, head := f.record(t)
			target, restoreErr := f.restore(t, old, record, head)
			if restoreErr == nil {
				restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
				_testRequire.NoError(err)
				defer func() { assert.NoError(t, restored.Close()) }()
				got, err := restored.LoadXControlContext(t.Context(), f.source.ID)
				_testRequire.NoError(err)
				_testAssert.Equal(current.SpendBuckets, got.SpendBuckets, "restore cannot reduce committed spending or reservations")
				_testAssert.Equal(current.Reservations, got.Reservations, "restore cannot forget an admitted request")
			}
			_testRequire.Error(restoreErr, "a snapshot before current financial authority is ineligible for publication")
			_testRequire.ErrorContains(restoreErr, "spending or send authority")
			_testAssert.NoFileExists(filepath.Join(target, "msgvault.db"))

			fresh := f.snapshot(t)
			target, err = f.restore(t, fresh, record, head)
			_testRequire.NoError(err, "a current quiescent snapshot must remain usable")
			restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
			_testRequire.NoError(err)
			defer func() { assert.NoError(t, restored.Close()) }()
			got, err := restored.LoadXControlContext(t.Context(), f.source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(current.SpendBuckets, got.SpendBuckets)
			_testAssert.Equal(current.Reservations, got.Reservations)
			_testAssert.Zero(f.providerCalls.Load())
		})
	}
}

func TestXRestoreCannotIgnoreCurrentProofWithoutSnapshotXSources(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	_testRequire.NoError(f.st.RemoveSource(f.source.ID))
	old := f.snapshot(t)
	var err error
	f.source, err = f.st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	generation := int64(1)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{
		OwnerID: "1001", Generation: &generation, InitializeInstance: true,
	})
	_testRequire.NoError(err)
	_, err = f.st.ReserveXRequestContext(t.Context(), f.source.ID, store.XRequestCost{
		Kind: store.XRequestTimeline, OwnerID: "1001", Generation: generation,
		GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 1_000_000,
		RequestedResults: 5, PostPriceMicros: 5_000, UserPriceMicros: 10_000,
		At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
	})
	_testRequire.NoError(err)
	record, head := f.record(t)
	target, err := f.restore(t, old, record, head)
	_testRequire.Error(err, "a supplied current proof cannot be ignored when the snapshot predates every X source")
	_testRequire.ErrorContains(err, "spending or send authority")
	_testAssert.NoFileExists(filepath.Join(target, "msgvault.db"))
	_testAssert.Zero(f.providerCalls.Load())
}

func TestXRestoreRejectsRemovalProofWhenSnapshotHasNoXSources(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	_testRequire.NoError(f.st.RemoveSource(f.source.ID))
	oldSnapshot := f.snapshot(t)

	var err error
	f.source, err = f.st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	generation := int64(1)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{
		OwnerID: "1001", Generation: &generation, InitializeInstance: true,
	})
	_testRequire.NoError(err)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	_, err = cleaner.RemovePost(t.Context(), f.source.ID, "102", "removed")
	_testRequire.NoError(err)
	record, _, err := f.st.ExportXRemovalRecordContext(t.Context())
	_testRequire.NoError(err)
	_testRequire.Len(record.Sources, 1)
	_testRequire.Equal([]string{"102"}, record.Sources[0].RemovedIDs)

	removalPath, head := f.record(t)
	target, err := f.restore(t, oldSnapshot, removalPath, head)
	_testRequire.ErrorContains(err, "no X source to retain them")
	_testAssert.NoFileExists(filepath.Join(target, "msgvault.db"))
	_testAssert.Zero(f.providerCalls.Load())
}

func TestXRestorePreservesCurrentTargetUncertainty(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	generation := int64(1)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	_testRequire.NoError(err)
	_, err = f.st.ReserveXRequestContext(t.Context(), f.source.ID, store.XRequestCost{
		Kind: store.XRequestLookup, OwnerID: "1001", Generation: generation,
		GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 1_000_000,
		RequestedResults: 1, PostPriceMicros: 5_000, UserPriceMicros: 10_000,
		At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
	})
	_testRequire.NoError(err)
	before, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	old := f.snapshot(t)
	_testRequire.

		// Refresh records this uncertainty after admission and before provider I/O.
		// A crash or ambiguous lookup must not regain a public target via restore.
		NoError(f.st.MarkXPostsUnverifiedContext(t.Context(), f.source.ID, []string{"102"}))
	current, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "102")
	_testRequire.NoError(err)
	_testRequire.Equal("unverified", current.Availability)
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(before.SpendBuckets, after.SpendBuckets)
	_testAssert.Equal(before.Reservations, after.Reservations)
	record, head := f.record(t)
	check := func(snapshot string, mayRefuse bool) {
		t.Helper()
		target, restoreErr := f.restore(t, snapshot, record, head)
		if restoreErr != nil && mayRefuse {
			assert.NoFileExists(t, filepath.Join(target, "msgvault.db"))
			return
		}
		require.NoError(t, restoreErr)
		restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
		require.NoError(t, err)
		defer func() { assert.NoError(t, restored.Close()) }()
		got, err := restored.ResolveXPostContext(t.Context(), f.source.ID, "102")
		require.NoError(t, err)
		assert.Equal(t, "unverified", got.Availability, "restore must preserve the current no-send target fence")
	}
	check(old, true)
	check(f.snapshot(t), false)
	_testAssert.Zero(f.providerCalls.Load())
}

func TestXRestoreReplaysRemovalOfPreviouslyUnverifiedTarget(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	_testRequire.NoError(f.st.MarkXPostsUnverifiedContext(t.Context(), f.source.ID, []string{"102"}))
	old := f.snapshot(t)
	_, _, err := f.sync(t, "--source-id", strconv.FormatInt(f.source.ID, 10), "--remove-post", "100", "--json")
	_testRequire.NoError(err)
	record, head := f.record(t)
	target, err := f.restore(t, old, record, head)
	_testRequire.NoError(err, "current removal authority must still clean an older unverified chain")
	restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, restored.Close()) }()
	got, err := restored.ResolveXPostContext(t.Context(), f.source.ID, "102")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", got.Availability)
	body, err := restored.GetMessageBodyText(f.message)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
	pending, err := restored.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending)
	_testAssert.Zero(f.providerCalls.Load())
}

// The provider barrier is after real Sender admission and its committed cost.
// No receipt is seeded: the real HTTP outcome supplies unknown/accepted state.
type xRestoreSendProbe struct {
	st         *store.Store
	source     int64
	draft      store.ChatDraft
	manager    *x.TokenManager
	policy     config.XAccountConfig
	server     *httptest.Server
	posts      atomic.Int32
	unexpected atomic.Int32
	entered    chan struct{}
	release    chan struct{}
	done       chan struct{}
	once       sync.Once
	result     x.SendResult
	err        error
	undo       func()
}

func newXRestoreSendProbe(t *testing.T, st *store.Store, source, parent int64, accepted bool) *xRestoreSendProbe {
	t.Helper()
	p := &xRestoreSendProbe{st: st, source: source, entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	message, err := st.GetMessageContext(t.Context(), parent)
	require.NoError(t, err)
	p.draft, err = st.CreateChatDraftContext(t.Context(), message.ConversationID, parent, "reviewed restore reply https://example.com", func(string, string) error { return nil })
	require.NoError(t, err)
	p.policy, err = config.NewDefaultConfig().XAccount("1001")
	require.NoError(t, err)
	p.policy.AllowSend = true
	var entered sync.Once
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/tweets" {
			p.unexpected.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p.posts.Add(1)
		entered.Do(func() { close(p.entered) })
		<-p.release
		if accepted {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"data":{"id":"9001"}}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"title":"Synthetic uncertain outcome"}`)
	}))
	t.Cleanup(p.server.Close)
	tokenDir := t.TempDir()
	require.NoError(t, os.Chmod(tokenDir, 0o700))
	p.manager = x.NewTokenManager(tokenDir, x.TokenManagerOptions{TokenURL: p.server.URL + "/token", HTTPClient: p.server.Client()})
	credential := x.NewTokenRecord("1001", "example-client", x.PublicClient, "example-access", "example-refresh", "")
	credential.GrantedScopes = []string{"tweet.read", "users.read", "tweet.write", "offline.access"}
	credential.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, p.manager.Save(t.Context(), credential))
	installed, err := p.manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	state, err := st.LoadXControlContext(t.Context(), source)
	require.NoError(t, err)
	_, err = st.UpdateXControlContext(t.Context(), source, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &installed.Generation, InitializeInstance: true})
	require.NoError(t, err)
	p.undo = func() {}
	if accepted {
		p.undo = xRestoreFailReplyArchive(t, st)
	}
	go func() {
		defer close(p.done)
		p.result, p.err = p.sender(st).Send(t.Context(), p.request())
	}()
	t.Cleanup(func() {
		p.unblock()
		<-p.done
	})
	select {
	case <-p.entered:
	case <-p.done:
		require.NoError(t, p.err, "sender must reach its actual POST")
		require.FailNow(t, "sender returned before the provider barrier")
	}
	return p
}

func (p *xRestoreSendProbe) request() x.SendRequest {
	return x.SendRequest{Owner: true, DraftID: p.draft.DraftID, Revision: p.draft.Revision}
}

func (p *xRestoreSendProbe) sender(st *store.Store) *x.Sender {
	credentials := p.manager.Credentials("1001", p.server.Client())
	return x.NewSender(st, x.NewClient(p.server.URL, credentials.HTTPClient()), p.policy, credentials)
}

func (p *xRestoreSendProbe) unblock() { p.once.Do(func() { close(p.release) }) }

func (p *xRestoreSendProbe) finish(t *testing.T, accepted bool) store.XReplyAttempt {
	t.Helper()
	p.unblock()
	<-p.done
	require.Error(t, p.err)
	want := "remote_unknown"
	if accepted {
		want = "sent_archive_pending"
		assert.Equal(t, "9001", p.result.RemoteID)
	}
	assert.Equal(t, want, p.result.Status)
	receipt, err := p.st.GetXReplyReceiptContext(t.Context(), p.draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, p.result.AttemptID, receipt.AttemptID)
	assert.Equal(t, p.draft.Revision, receipt.Revision)
	assert.Equal(t, int64(200000), receipt.ReservedMicros)
	if accepted {
		assert.Equal(t, "remote_accepted", receipt.State)
		assert.Equal(t, "9001", receipt.RemoteID)
	} else {
		assert.Equal(t, "remote_unknown", receipt.State)
		assert.Empty(t, receipt.RemoteID)
	}
	p.undo()
	return receipt
}

func (p *xRestoreSendProbe) revoke(t *testing.T) {
	t.Helper()
	installed, err := p.manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	execution, err := p.st.AcquireSyncExecutionContext(t.Context(), p.source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	require.NoError(t, p.manager.WithLifecycleLock(t.Context(), "1001", func() error {
		return p.manager.Delete(t.Context(), "1001", installed.Generation)
	}))
	require.NoError(t, p.st.RecordXCredentialErrorContext(t.Context(), p.source, "1001", installed.Generation, "invalid_grant"))
	p.policy.AllowSend = false
}

func (p *xRestoreSendProbe) recover(t *testing.T, st *store.Store, accepted bool, original store.XReplyAttempt) {
	t.Helper()
	for retry := range 2 {
		sender := p.sender(st)
		prepared, err := sender.PrepareReply(t.Context(), p.request())
		require.NoError(t, err)
		wantMode, wantStatus := "status", "remote_unknown"
		if accepted {
			wantMode, wantStatus = "repair", "sent"
			if retry > 0 {
				wantMode = "receipt"
			}
		}
		assert.Equal(t, wantMode, prepared.Mode)
		assert.Empty(t, prepared.Text)
		result, err := sender.Send(t.Context(), p.request())
		require.NoError(t, err)
		assert.Equal(t, wantStatus, result.Status)
		assert.Equal(t, original.AttemptID, result.AttemptID)
		assert.Equal(t, original.RemoteID, result.RemoteID)
		if accepted {
			_, err := st.GetChatDraftContext(t.Context(), p.draft.DraftID)
			require.ErrorIs(t, err, store.ErrChatDraftNotFound)
		}
	}
	assert.Equal(t, int32(1), p.posts.Load())
	assert.Zero(t, p.unexpected.Load(), "receipt/repair cannot refresh credentials or GET the provider")
}

func xRestoreFailReplyArchive(t *testing.T, st *store.Store) func() {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_x_restore_reply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic archive failure'; END; $$`)
		require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_x_restore_reply BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION fail_x_restore_reply()`)
		require.NoError(t, err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER fail_x_restore_reply BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`)
		require.NoError(t, err)
	}
	var once sync.Once
	undo := func() {
		once.Do(func() {
			if st.IsPostgreSQL() {
				_, err := st.DB().Exec(`DROP TRIGGER fail_x_restore_reply ON message_bodies`)
				assert.NoError(t, err)
				_, err = st.DB().Exec(`DROP FUNCTION fail_x_restore_reply()`)
				assert.NoError(t, err)
			} else {
				_, err := st.DB().Exec(`DROP TRIGGER fail_x_restore_reply`)
				assert.NoError(t, err)
			}
		})
	}
	t.Cleanup(undo)
	return undo
}

func TestXRestoreRejectsSnapshotBeforeSenderReceipt(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "unknown"
		if accepted {
			name = "accepted_archive_pending"
		}
		t.Run(name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXRestoreFixture(t)
			p := newXRestoreSendProbe(t, f.st, f.source.ID, f.message, accepted)
			admitted, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			_testRequire.NoError(err)
			old := f.snapshot(t)
			receipt := p.finish(t, accepted)
			current, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(admitted.SpendBuckets, current.SpendBuckets)
			_testAssert.Equal(admitted.Reservations, current.Reservations, "only the actual send receipt changed")
			p.revoke(t)
			record, head := f.record(t)
			target, err := f.restore(t, old, record, head)
			_testRequire.ErrorContains(err, "spending or send authority")
			_testAssert.NoFileExists(filepath.Join(target, "msgvault.db"))
			fresh := f.snapshot(t)
			target, err = f.restore(t, fresh, record, head)
			_testRequire.NoError(err)
			restored, err := store.OpenForTest(filepath.Join(target, "msgvault.db"))
			_testRequire.NoError(err)
			defer func() { assert.NoError(t, restored.Close()) }()
			preserved, err := restored.GetXReplyReceiptContext(t.Context(), p.draft.DraftID)
			_testRequire.NoError(err)
			_testAssert.Equal(receipt.State, preserved.State)
			_testAssert.Equal(receipt.RemoteID, preserved.RemoteID)
			_testAssert.Equal(receipt.AttemptID, preserved.AttemptID)
			p.recover(t, restored, accepted, receipt)
		})
	}
}

func TestXRestoreProofTracksActualSenderReceipts(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "unknown"
		if accepted {
			name = "accepted_archive_pending"
		}
		t.Run(name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			_testRequire.NoError(err)
			at := time.Now().UTC().Add(-time.Hour)
			saved, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "synthetic public parent", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: at}})
			_testRequire.NoError(err)
			p := newXRestoreSendProbe(t, st, source.ID, saved.MessageID, accepted)
			// Export is the Store's metadata snapshot seam; the owner command uses
			// Capture, which still refuses this active source execution.
			before, oldHead, err := st.ExportXRemovalRecordContext(t.Context())
			_testRequire.NoError(err)
			admitted, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			receipt := p.finish(t, accepted)
			current, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(admitted.SpendBuckets, current.SpendBuckets)
			_testAssert.Equal(admitted.Reservations, current.Reservations)
			after, head, err := st.ExportXRemovalRecordContext(t.Context())
			_testRequire.NoError(err)
			_testRequire.Len(before.Sources, 1)
			_testRequire.Len(after.Sources, 1)
			_testAssert.NotEqual(before.Sources[0].FinancialHead, after.Sources[0].FinancialHead)
			_testAssert.Equal(before.Sources[0].TargetHead, after.Sources[0].TargetHead)
			_testRequire.ErrorContains(st.ValidateXRemovalRecordContext(t.Context(), before, oldHead), "spending or send authority")
			_testRequire.NoError(st.ValidateXRemovalRecordContext(t.Context(), after, head))
			p.revoke(t)
			p.recover(t, st, accepted, receipt)
		})
	}
}
