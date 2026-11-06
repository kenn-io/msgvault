package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func emailPushFixture(t *testing.T) (*store.Store, *config.Config, *store.Source) {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(sourceTypeIMAP, "imap://owner@example.test")
	requirements.NoError(err)
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = dir
	cfg.Data.DataDir = dir
	cfg.EmailPush.IMAPSourceIDs = []int64{src.ID}
	cfg.Accounts = []config.AccountSchedule{{Email: src.Identifier, Schedule: "0 0 * * *", Enabled: true}}
	return st, cfg, src
}

func TestEmailPushBindingValidation(t *testing.T) {
	for _, kind := range []string{"valid", "wrong-type", "missing", "disabled", "alias-key", "duplicate-key"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, cfg, src := emailPushFixture(t)
			s := scheduler.New(func(context.Context, string) error { return nil })
			switch kind {
			case "wrong-type":
				other, err := st.GetOrCreateSource(sourceTypeGmail, "other@example.test")
				requirements.NoError(err)
				cfg.EmailPush.IMAPSourceIDs = []int64{other.ID}
			case "missing":
				cfg.EmailPush.IMAPSourceIDs = []int64{src.ID + 999}
			case "disabled":
				cfg.Accounts[0].Enabled = false
			case "alias-key":
				cfg.Accounts[0].Email = "friendly"
			case "duplicate-key":
				cfg.Accounts = append(cfg.Accounts, cfg.Accounts[0])
			}
			_, errs := s.AddAccountsFromConfig(cfg)
			requirements.Empty(errs)
			bindings, err := emailPushBindings(t.Context(), cfg, st, s)
			if kind == "valid" {
				requirements.NoError(err)
				assertions.Equal(src.ID, bindings[src.Identifier].sourceID)
			} else {
				requirements.Error(err)
			}
			<-s.Stop().Done()
		})
	}
}

func TestEmailPushSourceGuardRejectsRetirementAndReplacement(t *testing.T) {
	for _, change := range []string{"merged", "identifier", "replacement", "alias-collision", "same-id-replacement"} {
		t.Run(change, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, cfg, src := emailPushFixture(t)
			s := scheduler.New(func(context.Context, string) error { return nil })
			_, errs := s.AddAccountsFromConfig(cfg)
			requirements.Empty(errs)
			bindings, err := emailPushBindings(t.Context(), cfg, st, s)
			requirements.NoError(err)
			var ran atomic.Int32
			callback := emailPushWakeSync(bindings, st, func(_ context.Context, selected *store.Source) (*gmail.SyncSummary, error) {
				assertions.Equal(src.ID, selected.ID)
				ran.Add(1)
				return &gmail.SyncSummary{}, nil
			})
			switch change {
			case "merged":
				other, e := st.GetOrCreateSource(sourceTypeIMAP, "imap://destination@example.test")
				requirements.NoError(e)
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
				requirements.NoError(err)
			case "same-id-replacement":
				_, _, err = st.RemoveSourceSerialized(t.Context(), src.ID)
				requirements.NoError(err)
				replacement, e := st.GetOrCreateSource(sourceTypeIMAP, src.Identifier)
				requirements.NoError(e)
				if !st.IsPostgreSQL() {
					assertions.Equal(src.ID, replacement.ID)
				}
			case "identifier", "replacement":
				requirements.NoError(st.UpdateSourceIdentifier(src.ID, "imap://renamed@example.test"))
				if change == "replacement" {
					other, e := st.GetOrCreateSource(sourceTypeIMAP, src.Identifier)
					requirements.NoError(e)
					assertions.NotEqual(src.ID, other.ID)
				}
			case "alias-collision":
				other, e := st.GetOrCreateSource(sourceTypeGmail, "other@example.test")
				requirements.NoError(e)
				// Legacy catalogs can contain colliding display selectors. Exact
				// numeric binding must ignore the ordinary fuzzy dispatcher.
				_, err = st.DB().Exec(st.Rebind("UPDATE sources SET display_name = ? WHERE id = ?"), src.Identifier, other.ID)
				requirements.NoError(err)
			}
			err = callback(t.Context(), src.Identifier)
			if change == "alias-collision" {
				requirements.NoError(err)
				assertions.Equal(int32(1), ran.Load())
			} else {
				requirements.Error(err)
				assertions.Zero(ran.Load())
			}
			<-s.Stop().Done()
		})
	}
}

func TestEmailPushStrictSummaryOutcome(t *testing.T) {
	requirements := require.New(t)
	st, cfg, src := emailPushFixture(t)
	s := scheduler.New(func(context.Context, string) error { return nil })
	_, errs := s.AddAccountsFromConfig(cfg)
	requirements.Empty(errs)
	bindings, err := emailPushBindings(t.Context(), cfg, st, s)
	requirements.NoError(err)
	for _, tc := range []struct {
		name    string
		summary *gmail.SyncSummary
		err     error
		ok      bool
		reason  string
	}{
		{"complete", &gmail.SyncSummary{}, nil, true, ""},
		{"partial with nil error", &gmail.SyncSummary{Errors: 1}, nil, false, "acquisition errors"},
		{"missing summary", nil, nil, false, "completion summary"},
		{"failed", nil, errors.New("synthetic provider error"), false, "provider synchronization failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			callback := emailPushWakeSync(bindings, st, func(context.Context, *store.Source) (*gmail.SyncSummary, error) { return tc.summary, tc.err })
			err := callback(t.Context(), src.Identifier)
			if tc.ok {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, errEmailPushIncomplete)
				requirements.ErrorContains(err, tc.reason)
				assertions.NotContains(err.Error(), "synthetic provider error")
			}
		})
	}
	<-s.Stop().Done()
}

func TestEmailPushRealFactoryKeepsCredentialsSourceBound(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, cfg, src := emailPushFixture(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	imapCfg := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	encoded, err := imapCfg.ToJSON()
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(src.ID, encoded))
	src, err = st.GetSourceByID(src.ID)
	requirements.NoError(err)
	requirements.NoError(imaplib.SaveCredentials(cfg.TokensDir(), src.Identifier, testutil.IMAPTestPassword))
	state := testInvocationWithConfig(cfg)
	watcher, err := newEmailPushIMAPWatcher(t.Context(), src, state)
	requirements.NoError(err)
	client, ok := watcher.(*imaplib.Client)
	requirements.True(ok)
	profile, err := client.GetProfile(t.Context())
	requirements.NoError(err)
	assertions.Equal(int64(1), profile.MessagesTotal)
	requirements.NoError(watcher.Close())
	other, err := st.GetOrCreateSource(sourceTypeIMAP, "imap://other@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(other.ID, encoded))
	other, err = st.GetSourceByID(other.ID)
	requirements.NoError(err)
	_, err = newEmailPushIMAPWatcher(t.Context(), other, state)
	requirements.Error(err, "another source's credentials must not supply this source")
}

type emailPushReadyHandler struct {
	ready chan struct{}
	once  sync.Once
}

func (h *emailPushReadyHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *emailPushReadyHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "email push IMAP listener connected" {
		h.once.Do(func() { close(h.ready) })
	}
	return nil
}
func (h *emailPushReadyHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *emailPushReadyHandler) WithGroup(string) slog.Handler      { return h }

func TestEmailPushHintArchivesThroughRealIMAPSync(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, cfg, src := emailPushFixture(t)
	addr, user := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{MessagesPerMailbox: map[string]int{"INBOX": 1}, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapIdle: {}}})
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	imapCfg := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	encoded, err := imapCfg.ToJSON()
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(src.ID, encoded))
	requirements.NoError(imaplib.SaveCredentials(cfg.TokensDir(), src.Identifier, testutil.IMAPTestPassword))
	ready := &emailPushReadyHandler{ready: make(chan struct{})}
	state := testInvocationWithConfig(cfg)
	state.logger = slog.New(ready)
	// Use the actual cache policy, including its invocation-context check.
	// SQL/live-data mode needs no subprocess or generated analytics files.
	cfg.Analytics.AutoBuildCache = false
	var ordinary atomic.Int32
	s := scheduler.New(func(context.Context, string) error {
		ordinary.Add(1)
		return errors.New("unexpected ordinary dispatcher")
	}).WithLogger(slog.New(slog.DiscardHandler))
	_, errs := s.AddAccountsFromConfig(cfg)
	requirements.Empty(errs)
	runtime, err := startEmailPush(t.Context(), cfg, st, s, state, nil, nil)
	requirements.NoError(err)
	t.Cleanup(func() { runtime.Stop(); <-s.Stop().Done() })
	<-ready.ready
	count := func(want int) bool {
		var got int
		err := st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), src.ID).Scan(&got)
		return err == nil && got == want
	}
	requirements.Eventually(func() bool { return count(1) }, 10*time.Second, 20*time.Millisecond)
	testutil.AppendIMAPMessageWithoutMessageID(t, user, "INBOX", "second synthetic message")
	requirements.Eventually(func() bool { return count(2) }, 10*time.Second, 20*time.Millisecond)
	assertions.True(s.IsScheduled(src.Identifier), "poll fallback remains registered")
	assertions.Zero(ordinary.Load())
	requirements.NoError(s.WakeAccountAndWait(t.Context(), src.Identifier), "a complete real sync with cache disabled must produce a successful receipt")
}

type emailPushHeldGate struct{ entered, release chan struct{} }

func (g *emailPushHeldGate) BeginWork() (func(), bool) {
	return g.BeginWorkContext(context.Background())
}
func (g *emailPushHeldGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return func() {}, true
	case <-ctx.Done():
		return nil, false
	}
}

func TestEmailPushSourceRetiredBetweenAdmissionAndGate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, cfg, src := emailPushFixture(t)
	gate := &emailPushHeldGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var calls atomic.Int32
	s := scheduler.New(func(context.Context, string) error { return errors.New("ordinary callback must not run") }).WithWorkTracker(gate)
	_, errs := s.AddAccountsFromConfig(cfg)
	requirements.Empty(errs)
	bindings, err := emailPushBindings(t.Context(), cfg, st, s)
	requirements.NoError(err)
	s.WithAccountWakeSyncFunc(emailPushWakeSync(bindings, st, func(context.Context, *store.Source) (*gmail.SyncSummary, error) {
		calls.Add(1)
		return &gmail.SyncSummary{}, nil
	}))
	result := make(chan error, 1)
	go func() { result <- s.WakeAccountAndWait(t.Context(), src.Identifier) }()
	<-gate.entered
	other, err := st.GetOrCreateSource(sourceTypeIMAP, "imap://destination@example.test")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	requirements.NoError(err)
	close(gate.release)
	requirements.Error(<-result)
	assertions.Zero(calls.Load())
	<-s.Stop().Done()
}
