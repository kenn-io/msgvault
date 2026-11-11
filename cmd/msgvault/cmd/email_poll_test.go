package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func pollingSource(ctx context.Context, identifier string, s *store.Store, state *invocation) (*store.Source, error) {
	target := pollingCatalog(ctx, []config.AccountSchedule{{Email: identifier}}, s, state)[0]
	return target.source, target.err
}

func TestPollingSourceSelection(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	imapSource, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(imapSource.ID, "user@example.test"))
	state := invocationFromContext(testInvocationContext(t.Context(), config.NewDefaultConfig(), invocationOptions{}))
	selected, err := pollingSource(t.Context(), "user@example.test", st, state)
	requirements.NoError(err)
	checks.Equal(imapSource.ID, selected.ID)
	state.cfg.Sync.IMAPSkipFolders = map[string][]string{imapSource.Identifier: {"Archive"}}
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	requirements.ErrorContains(err, "folder filter")
	state.cfg.Sync.IMAPSkipFolders = nil
	gmailSource, err := st.GetOrCreateSource(sourceTypeGmail, "user@example.test")
	requirements.NoError(err)
	selected, err = pollingSource(t.Context(), "user@example.test", st, state)
	requirements.NoError(err)
	checks.Equal(gmailSource.ID, selected.ID)
	second, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://second@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(second.ID, "shared@example.test"))
	requirements.NoError(st.UpdateSourceDisplayName(imapSource.ID, "shared@example.test"))
	_, err = pollingSource(t.Context(), "shared@example.test", st, state)
	requirements.ErrorContains(err, "multiple sources")
	_, err = st.GetOrCreateSource(sourceTypeIMAP, "user@example.test")
	requirements.NoError(err)
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	requirements.ErrorContains(err, "multiple sources")
	_, err = pollingSource(t.Context(), "missing@example.test", st, state)
	requirements.ErrorContains(err, "no active")
	retired, err := st.GetOrCreateSource(sourceTypeGmail, "retired@example.test")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO source_settings (source_id, merged_into_source_id) VALUES (?, ?)`), retired.ID, gmailSource.ID)
	requirements.NoError(err)
	_, err = pollingSource(t.Context(), retired.Identifier, st, state)
	requirements.ErrorContains(err, "no active")
}

func TestPollingTargetsRejectDuplicateSource(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := storetest.New(t).Store
	cfg := config.NewDefaultConfig()
	state := invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	source, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.test")
	requirements.NoError(err)
	cfg.Accounts = []config.AccountSchedule{{Email: source.Identifier, Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}
	requirements.NoError(st.UpdateSourceDisplayName(source.ID, "mailbox@example.test"))
	cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{Email: "mailbox@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second})
	requirements.ErrorContains(validatePollingTargets(t.Context(), cfg.Accounts, st, state)[1].err, "already has a polling account")
	cfg.Accounts = append([]config.AccountSchedule{{Email: "missing@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}, cfg.Accounts...)
	targets := validatePollingTargets(t.Context(), cfg.Accounts, st, state)
	requirements.ErrorContains(targets[0].err, "no active")
	checks.Equal(1, strings.Count(targets[0].err.Error(), "missing@example.test"))
	requirements.NoError(targets[1].err)
	checks.Equal(source.ID, targets[1].source.ID)
	duplicate := cfg.Accounts[1]
	duplicate.Email = strings.ToUpper(duplicate.Email)
	cfg.Accounts = append(cfg.Accounts, duplicate)
	requirements.ErrorContains(validatePollingTargets(t.Context(), cfg.Accounts, st, state)[3].err, "already has a polling account")
}

func TestPollingSourceArchiveAlias(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource(sourceTypeGmail, "provider@example.test")
	requirements.NoError(err)
	alias := "archive@example.test"
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
	requirements.NoError(err)
	matches, err := st.GetSourcesByIdentifierOrDisplayNameContext(t.Context(), alias)
	requirements.NoError(err)
	requirements.Len(matches, 1)
	state := invocationFromContext(testInvocationContext(t.Context(), config.NewDefaultConfig(), invocationOptions{}))
	selected, err := pollingSource(t.Context(), alias, st, state)
	requirements.NoError(err)
	checks.Equal(source.ID, selected.ID)
	other, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://other@example.test")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET display_name = ? WHERE id = ?`), alias, other.ID)
	requirements.NoError(err)
	_, err = pollingSource(t.Context(), alias, st, state)
	requirements.ErrorContains(err, "multiple sources")
}

func TestPollingSourceRecoversOnNextAttempt(t *testing.T) {
	requirements := require.New(t)
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(77, 2, 1, newScriptedRFC7162Message(1, "existing@example.test"))}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	ic := &imap.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = t.TempDir(), t.TempDir()
	requirements.NoError(imap.SaveCredentials(cfg.TokensDir(), ic.Identifier(), testutil.IMAPTestPassword))
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource(sourceTypeIMAP, ic.Identifier())
	requirements.NoError(err)
	blob, err := ic.ToJSON()
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(t.Context(), source.ID, blob))
	cfg.Accounts = []config.AccountSchedule{{Email: "mailbox@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}
	state := invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	state.pollAccounts = append([]config.AccountSchedule(nil), cfg.Accounts...)
	state.syncFailures = &telemetry.SyncFailures{}
	original := rebuildCacheAfterScheduledSourceRun
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = original })
	// Database drivers can retain timers on pooled connections. Run provider
	// and store operations outside the simulated-time bubble that owns ticks.
	type externalWork struct {
		run  func() error
		done chan error
	}
	work := make(chan externalWork)
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for item := range work {
			item.done <- item.run()
		}
	}()
	defer func() { close(work); <-stopped }()
	runExternal := func(fn func() error) error {
		work <- externalWork{run: fn, done: done}
		return <-done
	}
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		sched := scheduler.New(func(context.Context, string) error { return nil }).WithAccountPoll(func(ctx context.Context, identifier string) error {
			return runExternal(func() error { return runEmailPoll(withInvocation(ctx, state), identifier, st, nil, state) })
		})
		defer func() { <-sched.Stop().Done() }()
		count, errs := sched.AddAccountsFromConfig(cfg)
		requirements.Empty(errs)
		requirements.Equal(1, count)
		sched.Start()
		synctest.Wait()
		requirements.Contains(sched.Status()[0].LastError, "no active")
		requirements.NoError(runExternal(func() error { return st.UpdateSourceDisplayName(source.ID, cfg.Accounts[0].Email) }))
		time.Sleep(15 * time.Second)
		synctest.Wait()
		requirements.Empty(sched.Status()[0].LastError)
		requirements.False(sched.Status()[0].LastCompleted.IsZero())
		connections := server.connectionCount()
		ic.Folders = []string{"INBOX"}
		filtered, err := ic.ToJSON()
		requirements.NoError(err)
		requirements.NoError(runExternal(func() error { return st.UpdateSourceSyncConfig(t.Context(), source.ID, filtered) }))
		time.Sleep(15 * time.Second)
		synctest.Wait()
		requirements.Contains(sched.Status()[0].LastError, "folder filter")
		requirements.Equal(connections, server.connectionCount())
	})
}

func TestPollingExactUnsupportedSourceWins(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	gmail, err := st.GetOrCreateSource(sourceTypeGmail, "gmail@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(gmail.ID, "other@example.test"))
	_, err = st.GetOrCreateSource(sourceTypeMSMail, "other@example.test")
	requirements.NoError(err)
	state := invocationFromContext(testInvocationContext(t.Context(), config.NewDefaultConfig(), invocationOptions{}))
	_, err = pollingSource(t.Context(), "other@example.test", st, state)
	requirements.Error(err)
	accounts := []config.AccountSchedule{{Email: "other@example.test", Enabled: true, PollInterval: 15 * time.Second}}
	requirements.Error(validatePollingTargets(t.Context(), accounts, st, state)[0].err)
}

func TestPollingWhileAccountsChange(t *testing.T) {
	st := storetest.New(t).Store
	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{{Email: "missing@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	invocationFromContext(ctx).configurePolling(cfg.Accounts, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{})
		}
	}()
	for range 100 {
		require.ErrorContains(t, runEmailPoll(ctx, "missing@example.test", st, nil, invocationFromContext(ctx)), "no active")
	}
	<-done
}

func TestPollingTargetsUseOneCatalogQuery(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	cfg := config.NewDefaultConfig()
	for _, email := range []string{"one@example.test", "two@example.test"} {
		_, err := st.GetOrCreateSource(sourceTypeGmail, email)
		requirements.NoError(err)
		cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{Email: email, Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second})
	}
	state := invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}); slog.SetDefault(old) })
	targets := validatePollingTargets(store.WithRequestID(t.Context(), "poll-catalog-test"), cfg.Accounts, st, state)
	requirements.NoError(targets[0].err)
	requirements.NoError(targets[1].err)
	count := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "FROM sources") && strings.Contains(line, "request_id=poll-catalog-test") {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestInvalidCronDoesNotClaimPollingSource(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource(sourceTypeGmail, "provider@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(source.ID, "mailbox@example.test"))
	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{
		{Email: source.Identifier, Enabled: true, Schedule: "bad", PollInterval: 15 * time.Second},
		{Email: "mailbox@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second},
	}
	state := invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	state.configurePolling(cfg.Accounts, nil)
	requirements.Len(state.pollAccounts, 1)
	assert.Equal(t, "mailbox@example.test", state.pollAccounts[0].Email)
}
