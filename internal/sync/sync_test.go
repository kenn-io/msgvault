package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// panicOnBatchAPI wraps a MockAPI and panics when GetMessagesRawBatch is called.
// Used to test that Full() recovers from panics gracefully.
type panicOnBatchAPI struct {
	*gmail.MockAPI
}

func (p *panicOnBatchAPI) GetMessagesRawBatch(_ context.Context, _ []string) ([]*gmail.RawMessage, error) {
	panic("unexpected nil pointer in batch processing")
}

func (p *panicOnBatchAPI) GetMessagesRawBatchWithErrors(_ context.Context, _ []string) ([]gmail.RawMessageBatchResult, error) {
	panic("unexpected nil pointer in batch processing")
}

type batchErrorAPI struct {
	*gmail.MockAPI
}

func (b *batchErrorAPI) GetMessagesRawBatchWithErrors(_ context.Context, _ []string) ([]gmail.RawMessageBatchResult, error) {
	return nil, errors.New("batch fetch unavailable")
}

type acknowledgingAPI struct {
	*gmail.MockAPI

	acknowledged []string
}

func (a *acknowledgingAPI) AcknowledgeMessages(_ context.Context, messageIDs []string) {
	a.acknowledged = append(a.acknowledged, messageIDs...)
}

// syncLogCapture records everything a Syncer logs and, optionally, reacts to
// each record inline on the syncing goroutine. Reacting to a real log event is
// how the retry tests hit an exact point in the retry loop without sleeping.
// Attrs and groups are dropped: the sync code logs flat records only.
type syncLogCapture struct {
	inner    slog.Handler
	buf      *bytes.Buffer
	onRecord func(slog.Record)
}

func newSyncLogCapture(onRecord func(slog.Record)) *syncLogCapture {
	buf := &bytes.Buffer{}
	return &syncLogCapture{
		inner:    slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
		buf:      buf,
		onRecord: onRecord,
	}
}

func (c *syncLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *syncLogCapture) Handle(ctx context.Context, record slog.Record) error {
	err := c.inner.Handle(ctx, record)
	if c.onRecord != nil {
		c.onRecord(record)
	}
	if err != nil {
		return fmt.Errorf("capture sync log record: %w", err)
	}
	return nil
}

func (c *syncLogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *syncLogCapture) WithGroup(string) slog.Handler { return c }

func (c *syncLogCapture) String() string { return c.buf.String() }

func (c *syncLogCapture) logger() *slog.Logger { return slog.New(c) }

// shortenIdentityDiscoveryRetry keeps the bounded discovery retry from adding
// seconds of real backoff to a test run.
func shortenIdentityDiscoveryRetry(t *testing.T, backoff time.Duration) {
	t.Helper()
	previous := identityDiscoveryRetryBackoff
	identityDiscoveryRetryBackoff = backoff
	t.Cleanup(func() { identityDiscoveryRetryBackoff = previous })
}

const forcedDiscoveryFailure = "forced identity discovery failure"

// installFailingIdentityDiscovery aborts every account_identities write.
// Sync-time discovery only merges signals into already-confirmed identities,
// so the failure seam has to intercept that update rather than an insert.
func installFailingIdentityDiscovery(t *testing.T, env *TestEnv, trigger string) {
	t.Helper()
	_, err := env.Store.DB().Exec(`
		CREATE TRIGGER ` + trigger + `
		BEFORE UPDATE ON account_identities
		BEGIN
			SELECT RAISE(ABORT, '` + forcedDiscoveryFailure + `');
		END
	`)
	require.NoError(t, err, "install identity discovery failure seam")
	t.Cleanup(func() {
		_, _ = env.Store.DB().Exec("DROP TRIGGER IF EXISTS " + trigger)
	})
}

type staticLabelsAPI struct {
	*gmail.MockAPI

	labels []*gmail.Label
}

type supersedingProfileAPI struct {
	*gmail.MockAPI

	supersede func()
}

func (a *supersedingProfileAPI) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	a.supersede()
	return a.MockAPI.GetProfile(ctx)
}

func (a *staticLabelsAPI) ListLabels(_ context.Context) ([]*gmail.Label, error) {
	return a.labels, nil
}

func TestFullSync_PanicReturnsError(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg1")

	// Replace the client with one that panics during batch fetch
	env.Syncer = New(&panicOnBatchAPI{MockAPI: env.Mock}, env.Store, nil)

	// Should return an error, NOT panic and crash the program
	_, err := env.Syncer.Full(env.Context, testEmail)
	require.Error(t, err, "expected error from panic recovery")
	assert.ErrorContains(t, err, "panic")
}

func TestFullSyncBatchFetchErrorUpdatesFailedSyncErrorCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")
	env.Syncer = New(&batchErrorAPI{MockAPI: env.Mock}, env.Store, nil)

	_, err := env.Syncer.Full(env.Context, testEmail)
	require.Error(err, "full sync should fail on whole-batch fetch error")

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync")
	assert.Equal(store.SyncStatusFailed, run.Status, "Status")
	assert.Equal(int64(2), run.ErrorsCount, "ErrorsCount")

	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 2, "error items")
	for _, item := range items {
		assert.Equal("fetch", item.Phase, "Phase")
		assert.Equal("batch_fetch_error", item.ErrorKind, "ErrorKind")
	}
}

// panicOnHistoryAPI wraps a MockAPI and panics when ListHistory is called.
// Used to test that Incremental() recovers from panics gracefully.
type panicOnHistoryAPI struct {
	*gmail.MockAPI
}

func (p *panicOnHistoryAPI) ListHistory(_ context.Context, _ uint64, _ string) (*gmail.HistoryResponse, error) {
	panic("unexpected nil pointer in history processing")
}

func TestIncrementalSync_PanicReturnsError(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12350

	// Replace the client with one that panics during history fetch
	env.Syncer = New(&panicOnHistoryAPI{MockAPI: env.Mock}, env.Store, nil)

	// Should return an error, NOT panic and crash the program
	_, err := env.Syncer.Incremental(env.Context, source)
	require.Error(t, err, "expected error from panic recovery")
	assert.ErrorContains(t, err, "panic")
}

func TestFullSync(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")
	env.Mock.Messages["msg2"].LabelIDs = []string{"INBOX", "SENT"}
	env.Mock.Messages["msg3"].LabelIDs = []string{"SENT"}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(3)), Errors: new(int64(0))})
	assert.Equal(t, uint64(12345), summary.FinalHistoryID, "history ID")

	assertMockCalls(t, env, 1, 1, 3)
	assertMessageCount(t, env.Store, 3)
}

func TestFullSyncProviderHookFailureWarnsOnceAfterSuccessfulCompletion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	env := newTestEnv(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	const hookFailure = "provider inventory unavailable"
	hookCalls := 0
	env.Syncer = env.Syncer.WithLogger(logger).WithSuccessfulSyncHook(
		"provider identity refresh",
		func(_ context.Context, source *store.Source, _ bool) error {
			hookCalls++
			assertions.Positive(source.ID)
			return errors.New(hookFailure)
		},
	)

	summary, err := env.Syncer.Full(env.Context, testEmail)

	requirements.NoError(err)
	requirements.NotNil(summary)
	assertions.Equal(1, hookCalls)
	assertions.Equal(1, strings.Count(logs.String(), "successful sync hook failed"))
	assertions.Contains(logs.String(), "provider identity refresh")
	assertions.Contains(logs.String(), hookFailure,
		"a warning without the cause leaves the operator nothing to act on")
	source, err := env.Store.GetSourceByIdentifier(testEmail)
	requirements.NoError(err)
	run, err := env.Store.GetLatestSync(source.ID)
	requirements.NoError(err)
	assertions.Equal(store.SyncStatusCompleted, run.Status)
}

func TestFullSyncProviderHookDoesNotRunAfterFailedSync(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg1")
	hookCalls := 0
	env.Syncer = New(&batchErrorAPI{MockAPI: env.Mock}, env.Store, nil).WithSuccessfulSyncHook(
		"provider identity refresh",
		func(context.Context, *store.Source, bool) error {
			hookCalls++
			return nil
		},
	)

	_, err := env.Syncer.Full(env.Context, testEmail)

	require.Error(t, err)
	assert.Zero(t, hookCalls)
}

func TestFullSyncSupersededGenerationDoesNotPublishCursorOrReturnSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	require.NoError(env.Store.UpdateSourceSyncCursor(source.ID, "baseline-cursor"))
	var newerSyncID int64
	env.Syncer = New(&supersedingProfileAPI{
		MockAPI: env.Mock,
		supersede: func() {
			var err error
			newerSyncID, err = env.Store.StartSync(source.ID, "full")
			require.NoError(err)
		},
	}, env.Store, nil)

	summary, err := env.Syncer.Full(env.Context, testEmail)

	require.ErrorIs(err, store.ErrSyncRunSuperseded)
	assert.Nil(summary)
	source, err = env.Store.GetSourceByID(source.ID)
	require.NoError(err)
	assert.Equal("baseline-cursor", source.SyncCursor.String)
	active, err := env.Store.GetActiveSync(source.ID)
	require.NoError(err)
	assert.Equal(newerSyncID, active.ID)
}

func TestFullSyncCompletionFailureMarksRunFailed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject the completion failure")
	env := newTestEnv(t)
	_, err := env.Store.DB().Exec(`
		CREATE TRIGGER fail_sync_completion
		BEFORE UPDATE OF status ON sync_runs
		FOR EACH ROW WHEN NEW.status = 'completed'
		BEGIN
			SELECT RAISE(ABORT, 'forced sync completion failure');
		END
	`)
	require.NoError(err)
	t.Cleanup(func() {
		_, _ = env.Store.DB().Exec("DROP TRIGGER IF EXISTS fail_sync_completion")
	})

	summary, err := env.Syncer.Full(env.Context, testEmail)

	require.ErrorContains(err, "forced sync completion failure")
	assert.Nil(summary)
	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err)
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.Contains(run.ErrorMessage.String, "forced sync completion failure")
}

func TestIncrementalSyncSupersededGenerationDoesNotPublishCursorOrReturnSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")
	env.Mock.Profile.HistoryID = 1000
	var newerSyncID int64
	env.Syncer = New(&supersedingProfileAPI{
		MockAPI: env.Mock,
		supersede: func() {
			var err error
			newerSyncID, err = env.Store.StartSync(source.ID, "incremental")
			require.NoError(err)
		},
	}, env.Store, nil)

	summary, err := env.Syncer.Incremental(env.Context, source)

	require.ErrorIs(err, store.ErrSyncRunSuperseded)
	assert.Nil(summary)
	source, err = env.Store.GetSourceByID(source.ID)
	require.NoError(err)
	assert.Equal("1000", source.SyncCursor.String)
	active, err := env.Store.GetActiveSync(source.ID)
	require.NoError(err)
	assert.Equal(newerSyncID, active.ID)
}

// TestIncrementalSyncProviderHookRunsAfterSuccessfulCompletion also pins the
// no-op flag: an unchanged mailbox still runs the hook — provider inventory
// can change while a mailbox is idle — but flagged as unchanged so the
// installer can skip provider round trips it has made recently.
func TestIncrementalSyncProviderHookRunsAfterSuccessfulCompletion(t *testing.T) {
	tests := []struct {
		name               string
		profileHistoryID   uint64
		wantMailboxChanged bool
	}{
		{name: "already up to date", profileHistoryID: 1000, wantMailboxChanged: false},
		{name: "history advanced", profileHistoryID: 1001, wantMailboxChanged: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "1000")
			env.Mock.Profile.HistoryID = tt.profileHistoryID
			env.Mock.HistoryID = tt.profileHistoryID
			var hookChangedFlags []bool
			env.Syncer = env.Syncer.WithSuccessfulSyncHook(
				"provider identity refresh",
				func(_ context.Context, completedSource *store.Source, mailboxChanged bool) error {
					hookChangedFlags = append(hookChangedFlags, mailboxChanged)
					assertions.Equal(source.ID, completedSource.ID)
					return nil
				},
			)

			summary, err := env.Syncer.Incremental(env.Context, source)

			requirements.NoError(err)
			requirements.NotNil(summary)
			assertions.Equal([]bool{tt.wantMailboxChanged}, hookChangedFlags)

			run, err := env.Store.GetLatestSync(source.ID)
			requirements.NoError(err)
			assertions.Equal(store.SyncStatusCompleted, run.Status,
				"running the hook must not run before completing the run")
		})
	}
}

func TestSyncLabelsPersistsRoleFromCanonicalGmailIDNotName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	env.Mock.Labels = []*gmail.Label{
		{ID: "SENT", Name: "Envoyes", Type: "system"},
		{ID: "Label_1", Name: "Sent", Type: "user"},
	}

	_, err := env.Syncer.syncLabels(env.Context, source.ID)
	require.NoError(err, "sync labels")

	roles := make(map[string]sql.NullString)
	rows, err := env.Store.DB().Query(
		"SELECT name, system_role FROM labels WHERE source_id = ?", source.ID,
	)
	require.NoError(err, "query label roles")
	defer func() { require.NoError(rows.Close(), "close label roles") }()
	for rows.Next() {
		var name string
		var role sql.NullString
		require.NoError(rows.Scan(&name, &role), "scan label role")
		roles[name] = role
	}
	require.NoError(rows.Err(), "iterate label roles")

	assert.Equal(store.LabelSystemRoleSent, roles["Envoyes"].String)
	assert.False(roles["Sent"].Valid, "display names are not trusted as canonical roles")
}

// TestSyncPageRefreshesOnlyUnambiguousSentAliasesOnce pins which addresses a
// sync page may contribute Sent evidence to. Every address here is confirmed up
// front because sync-time discovery is refresh-only: it merges signals into
// identities the source already owns and never confirms a new one.
func TestSyncPageRefreshesOnlyUnambiguousSentAliasesOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source, err := env.Store.GetOrCreateSource("imap", testEmail)
	require.NoError(err, "GetOrCreateSource")
	for _, address := range []string{
		"masked-one@example.test",
		"masked-two@example.test",
		"recipient-only@example.test",
	} {
		require.NoError(env.Store.AddAccountIdentity(source.ID, address, "manual"), "seed confirmed identity")
	}

	labelMap, err := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"Envoyes": {
			Name:       "Envoyes",
			Type:       "system",
			SystemRole: store.LabelSystemRoleSent,
		},
	})
	require.NoError(err, "persist localized IMAP \\Sent role")

	env.Mock.AddMessage("m-existing", testemail.NewMessage().
		From("Masked-One@Example.test").
		To("recipient-only@example.test").
		Header("Message-ID", "<existing@example.test>").
		Bytes(), []string{"Envoyes"})
	env.Mock.AddMessage("m-new", testemail.NewMessage().
		From("masked-two@example.test, delegate@example.test").
		To("another-recipient@example.test").
		Header("Message-ID", "<new@example.test>").
		Bytes(), []string{"Envoyes"})
	_, err = env.Syncer.ingestMessage(
		t.Context(),
		source.ID,
		env.Mock.Messages["m-existing"],
		"thread-existing",
		labelMap,
	)
	require.NoError(err, "pre-persist existing page row")
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.MessagePages = [][]string{{"m-existing", "m-new"}}
	env.Syncer = New(&staticLabelsAPI{
		MockAPI: env.Mock,
		labels: []*gmail.Label{{
			ID: "Envoyes", Name: "Envoyes", Type: "system", SystemRole: store.LabelSystemRoleSent,
		}},
	}, env.Store, &Options{SourceType: "imap"})

	beforeRevision, err := env.Store.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision before page")
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var sqlTrace bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&sqlTrace, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "full sync")
	assert.Equal(int64(1), summary.MessagesAdded, "new rows added")
	assert.Equal(int64(1), summary.MessagesSkipped, "existing rows skipped")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 3, "a sync page must not confirm a first-time identity")
	byAddress := make(map[string]store.AccountIdentity, len(identities))
	for _, identity := range identities {
		byAddress[store.NormalizeIdentifierForCompare(identity.Address)] = identity
	}
	assert.Equal("masked-one@example.test", byAddress["masked-one@example.test"].Address)
	assert.Equal("manual,sent-folder", byAddress["masked-one@example.test"].SourceSignal,
		"the unambiguous From address gains the page's Sent evidence")
	assert.Equal("manual", byAddress["masked-two@example.test"].SourceSignal, "multiple From authors remain weak")
	assert.Equal("manual", byAddress["recipient-only@example.test"].SourceSignal,
		"recipient-only evidence must stay weak")
	assert.NotContains(byAddress, "delegate@example.test", "multiple From authors remain weak")
	assert.NotContains(byAddress, "another-recipient@example.test", "recipient-only evidence must stay weak")

	afterRevision, err := env.Store.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after page")
	assert.Equal(beforeRevision, afterRevision, "a signal-only refresh must not bump ownership revision")
	assert.Equal(1, strings.Count(strings.ToLower(sqlTrace.String()), "coalesce(m.source_is_from_me, false)"),
		"one store observation query per sync page")
}

func TestSyncPageDoesNotTrustNameOnlySentMailbox(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.AddMessage("m-name-only", testemail.NewMessage().
		From("untrusted-alias@example.test").
		To("recipient-only@example.test").
		Bytes(), []string{"Sent"})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.MessagePages = [][]string{{"m-name-only"}}
	env.Syncer = New(&staticLabelsAPI{
		MockAPI: env.Mock,
		labels:  []*gmail.Label{{ID: "Sent", Name: "Sent", Type: "system"}},
	}, env.Store, &Options{SourceType: "imap"})

	_, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "full sync")
	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Empty(identities, "a mailbox display name is not trusted sent evidence")
}

func TestSyncPageRetryAfterCheckpointFailureIsCaseFoldedAndIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	require.NoError(env.Store.AddAccountIdentity(
		source.ID,
		"retry-alias@example.test",
		"manual",
	), "seed lower-case identity")

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.MessagePages = [][]string{{"m-retry"}, {"m-next"}}
	env.Mock.AddMessage("m-retry", testemail.NewMessage().
		From("Retry-Alias@Example.test").
		To("recipient-only@example.test").
		Bytes(), []string{"SENT"})
	env.Mock.AddMessage("m-next", testemail.NewMessage().
		From("other-sender@example.test").
		Bytes(), []string{"INBOX"})

	_, err := env.Store.DB().Exec(`
		CREATE TRIGGER fail_sync_checkpoint
		BEFORE UPDATE ON sync_runs
		WHEN NEW.messages_processed <> OLD.messages_processed
		BEGIN
			SELECT RAISE(ABORT, 'checkpoint unavailable');
		END
	`)
	require.NoError(err, "install checkpoint failure seam")
	t.Cleanup(func() {
		_, _ = env.Store.DB().Exec("DROP TRIGGER IF EXISTS fail_sync_checkpoint")
	})

	env.Syncer = New(&cancelOnSecondListAPI{MockAPI: env.Mock}, env.Store, nil)
	_, err = env.Syncer.Full(env.Context, testEmail)
	require.ErrorIs(err, context.Canceled, "interrupt after first persisted page")
	assertMessageCount(t, env.Store, 1)
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync")
	assert.Equal(store.SyncStatusRunning, run.Status, "cancelled run remains resumable")
	assert.Equal(int64(0), run.MessagesProcessed, "failed checkpoint does not advance the page")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities after interrupted page")
	require.Len(identities, 1, "case variants merge into the existing row")
	assert.Equal("retry-alias@example.test", identities[0].Address, "first-confirmed spelling")
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal,
		"sorted provider-native evidence; the identity's own derived attribution is not evidence for itself")
	revisionAfterFirstPage, err := env.Store.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after first page")

	_, err = env.Store.DB().Exec("DROP TRIGGER fail_sync_checkpoint")
	require.NoError(err, "restore checkpoint updates")
	env.Syncer = New(env.Mock, env.Store, nil)
	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "resume sync")
	assert.True(summary.WasResumed, "retry resumes the uncheckpointed run")

	identities, err = env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities after retry")
	require.Len(identities, 1, "retry must not create a case variant")
	assert.Equal("retry-alias@example.test", identities[0].Address, "first-confirmed spelling after retry")
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal, "signals after retry")
	revisionAfterRetry, err := env.Store.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after retry")
	assert.Equal(revisionAfterFirstPage, revisionAfterRetry, "idempotent retry does not bump ownership revision")
}

func TestFullSyncResume(t *testing.T) {
	env := newTestEnv(t)

	// Create mock with pagination
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 4)

	summary1 := runFullSync(t, env)
	assertSummary(t, summary1, WantSummary{Added: new(int64(4))})

	// Second sync should skip already-synced messages
	env.Mock.Reset()
	env.Mock.Profile = &gmail.Profile{
		EmailAddress:  testEmail,
		MessagesTotal: 4,
		HistoryID:     12346,
	}
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg3", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg4", testMIME(), []string{"INBOX"})

	summary2 := runFullSync(t, env)
	assertSummary(t, summary2, WantSummary{Added: new(int64(0))})
}

// cancelOnSecondListAPI surfaces a context cancellation on the second
// ListMessages call, after the first page's checkpoint has been saved.
type cancelOnSecondListAPI struct {
	*gmail.MockAPI

	calls int
}

func (c *cancelOnSecondListAPI) ListMessages(ctx context.Context, query, pageToken string) (*gmail.MessageListResponse, error) {
	c.calls++
	if c.calls == 2 {
		return nil, fmt.Errorf("list messages: %w", context.Canceled)
	}
	return c.MockAPI.ListMessages(ctx, query, pageToken)
}

func TestFullSyncCanceledKeepsRunResumable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 4)

	env.Syncer = New(&cancelOnSecondListAPI{MockAPI: env.Mock}, env.Store, nil)
	_, err := env.Syncer.Full(env.Context, testEmail)
	require.ErrorIs(err, context.Canceled, "sync should surface cancellation")

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync")
	assert.Equal(store.SyncStatusRunning, run.Status, "cancelled run keeps status running")
	assert.Equal(int64(2), run.MessagesProcessed, "checkpoint keeps first page progress")

	env.Syncer = New(env.Mock, env.Store, nil)
	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "resumed sync")
	assert.True(summary.WasResumed, "second sync resumes the cancelled run")
	assert.Equal("page_1", summary.ResumedFromToken, "resume picks up at the saved page token")
	assert.Equal(int64(4), summary.MessagesAdded, "resumed summary carries pre-cancellation progress")
	assertMessageCount(t, env.Store, 4)
}

func TestFullSyncAcknowledgesOnlySafelyHandledMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	ackClient := &acknowledgingAPI{MockAPI: env.Mock}
	env.Syncer = New(ackClient, env.Store, DefaultOptions())

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.MessagePages = [][]string{{"msg-ok", "msg-fail"}}
	env.Mock.AddMessage("msg-ok", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["msg-fail"] = errors.New("temporary fetch failure")

	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "full sync")

	assert.Equal(int64(1), summary.Errors, "errors")
	assert.Equal([]string{"msg-ok"}, ackClient.acknowledged)
}

// seedConfirmedSentIdentity archives nothing but sets up the common shape of
// the discovery tests: one Sent message whose From address the source has
// already confirmed, so a sync page has real signals to merge.
func seedConfirmedSentIdentity(t *testing.T, env *TestEnv) *store.Source {
	t.Helper()
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.MessagePages = [][]string{{"msg-sent"}}
	env.Mock.AddMessage("msg-sent", testemail.NewMessage().
		From("sent-alias@example.test").
		Bytes(), []string{"SENT"})
	source := env.CreateSource(t)
	require.NoError(t, env.Store.AddAccountIdentity(source.ID, "sent-alias@example.test", "manual"),
		"seed the confirmed identity whose signals the sync page refreshes")
	return source
}

// TestFullSyncCompletesAndSetsBacklogWhenDiscoveryFails pins the durability
// trade-off behind the non-fatal discovery path. Identity evidence is
// recomputable from the archived messages, so a page whose discovery keeps
// failing parks a durable backlog marker rather than unwinding a run whose
// messages are already safely stored.
func TestFullSyncCompletesAndSetsBacklogWhenDiscoveryFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	shortenIdentityDiscoveryRetry(t, time.Millisecond)
	logs := newSyncLogCapture(nil)
	ackClient := &acknowledgingAPI{MockAPI: env.Mock}
	env.Syncer = New(ackClient, env.Store, DefaultOptions()).WithLogger(logs.logger())
	seedConfirmedSentIdentity(t, env)
	installFailingIdentityDiscovery(t, env, "fail_identity_discovery")

	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "persistent discovery failure must not fail the sync run")
	require.NotNil(summary)
	assert.Equal(int64(1), summary.MessagesAdded, "the page is archived despite discovery failing")
	assertMessageCount(t, env.Store, 1)
	assert.Equal([]string{"msg-sent"}, ackClient.acknowledged,
		"a durably archived message is acknowledged once its discovery debt is recorded")

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync")
	assert.Equal(store.SyncStatusCompleted, run.Status, "the run completes")
	assert.Equal(int64(1), run.MessagesProcessed, "the page checkpoint advanced")
	require.True(source.SyncCursor.Valid, "the source cursor advanced")
	assert.Equal("12345", source.SyncCursor.String, "SyncCursor")

	found, lastError, err := env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.True(found, "the failed page records a durable backlog marker")
	assert.Contains(lastError, forcedDiscoveryFailure, "the marker keeps the underlying cause")
	assert.Contains(logs.String(), forcedDiscoveryFailure,
		"the operator sees the discovery error in the log")

	assert.NotContains(logs.String(), identityDiscoveryDrainLogMessage,
		"a whole-archive refresh right after three failed attempts costs a full scan to learn nothing")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities after discovery failure")
	require.Len(identities, 1)
	assert.Equal("manual", identities[0].SourceSignal, "failed discovery writes no partial evidence")
}

// TestFullSyncSettlesBacklogItParkedOnceDiscoveryRecovers covers the other side
// of that gate: a long run whose later pages show discovery working again pays
// the refresh immediately instead of leaving the archive inconsistent until
// whenever the next sync happens to run.
func TestFullSyncSettlesBacklogItParkedOnceDiscoveryRecovers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	shortenIdentityDiscoveryRetry(t, time.Millisecond)

	recovered := false
	logs := newSyncLogCapture(func(record slog.Record) {
		// Let the first page exhaust its attempts and park the debt, then let
		// discovery work again for the second page.
		if record.Message != identityDiscoveryBacklogLogMessage {
			return
		}
		recovered = true
		_, err := env.Store.DB().Exec("DROP TRIGGER IF EXISTS fail_identity_discovery")
		require.NoError(err, "let discovery recover for the next page")
	})
	env.Syncer = New(env.Mock, env.Store, DefaultOptions()).WithLogger(logs.logger())

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.MessagePages = [][]string{{"msg-sent"}, {"msg-inbox"}}
	env.Mock.AddMessage("msg-sent", testemail.NewMessage().
		From("sent-alias@example.test").
		Bytes(), []string{"SENT"})
	env.Mock.AddMessage("msg-inbox", testemail.NewMessage().
		From("stranger@example.test").
		Bytes(), []string{"INBOX"})
	source := env.CreateSource(t)
	require.NoError(env.Store.AddAccountIdentity(source.ID, "sent-alias@example.test", "manual"),
		"seed the confirmed identity whose signals the failed page never merged")
	installFailingIdentityDiscovery(t, env, "fail_identity_discovery")

	_, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "full sync")
	require.True(recovered, "the first page parked a backlog marker")
	assertMessageCount(t, env.Store, 2)

	found, _, err := env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.False(found, "the recovered run settles the debt it parked")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1)
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal,
		"the completion drain re-derives what the failed page never wrote")
}

// TestFullSyncDiscoveryRetriesTransientFailureWithoutBacklog clears the failure
// seam from the retry log record itself, so the retry happens at an exact point
// in the loop rather than after a sleep the test hopes is long enough.
func TestFullSyncDiscoveryRetriesTransientFailureWithoutBacklog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	shortenIdentityDiscoveryRetry(t, time.Millisecond)

	retries := 0
	logs := newSyncLogCapture(func(record slog.Record) {
		if record.Message != identityDiscoveryRetryLogMessage {
			return
		}
		retries++
		_, err := env.Store.DB().Exec("DROP TRIGGER IF EXISTS fail_identity_discovery")
		require.NoError(err, "clear the transient discovery failure")
	})
	env.Syncer = New(env.Mock, env.Store, DefaultOptions()).WithLogger(logs.logger())
	seedConfirmedSentIdentity(t, env)
	installFailingIdentityDiscovery(t, env, "fail_identity_discovery")

	_, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "full sync")
	assert.Equal(1, retries, "one failed attempt is retried")

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	found, _, err := env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.False(found, "a retry that succeeds leaves no backlog behind")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1)
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal,
		"the retried attempt merges the page's Sent evidence")
}

// TestSyncCancellationDuringDiscoveryStaysResumable separates "discovery is
// broken" from "the operator stopped the sync". Only the former is a debt worth
// recording; cancellation must leave the run exactly as resumable as any other
// interruption.
func TestSyncCancellationDuringDiscoveryStaysResumable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	shortenIdentityDiscoveryRetry(t, time.Minute)
	ctx, cancel := context.WithCancel(env.Context)
	t.Cleanup(cancel)
	env.Context = ctx

	logs := newSyncLogCapture(func(record slog.Record) {
		if record.Message == identityDiscoveryRetryLogMessage {
			cancel()
		}
	})
	env.Syncer = New(env.Mock, env.Store, DefaultOptions()).WithLogger(logs.logger())
	source := seedConfirmedSentIdentity(t, env)
	installFailingIdentityDiscovery(t, env, "fail_identity_discovery")

	_, err := env.Syncer.Full(ctx, testEmail)
	require.ErrorIs(err, context.Canceled, "cancellation surfaces as cancellation")
	assertMessageCount(t, env.Store, 1)

	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync")
	assert.Equal(store.SyncStatusRunning, run.Status, "a cancelled run stays resumable")

	found, _, err := env.Store.IdentityDiscoveryBacklogContext(context.Background(), source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.False(found, "cancellation is not a discovery failure")

	_, err = env.Store.DB().Exec("DROP TRIGGER fail_identity_discovery")
	require.NoError(err, "restore identity discovery")
	env.Syncer = New(env.Mock, env.Store, DefaultOptions())
	_, err = env.Syncer.Full(context.Background(), testEmail)
	require.NoError(err, "resumed sync")

	run, err = env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync after resume")
	assert.Equal(store.SyncStatusCompleted, run.Status, "the resumed run completes")
	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities after resume")
	require.Len(identities, 1)
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal,
		"the resumed run applies the discovery the cancellation interrupted")
}

// TestNextSyncDrainsIdentityDiscoveryBacklog proves the backlog is a real
// repair path, not just a breadcrumb: the next sync re-derives the owed
// evidence from the whole archive, which is why the page that fails can be
// allowed to complete. The second sync lists only an unrelated message, so
// merging the Sent evidence can come from nowhere but the drain.
func TestNextSyncDrainsIdentityDiscoveryBacklog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := seedConfirmedSentIdentity(t, env)
	installFailingIdentityDiscovery(t, env, "fail_identity_discovery")
	shortenIdentityDiscoveryRetry(t, time.Millisecond)
	env.Syncer = New(env.Mock, env.Store, DefaultOptions()).WithLogger(newSyncLogCapture(nil).logger())

	_, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "first full sync")
	found, _, err := env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	require.True(found, "the first sync parks the discovery debt")

	_, err = env.Store.DB().Exec("DROP TRIGGER fail_identity_discovery")
	require.NoError(err, "restore identity discovery")
	env.Mock.Profile.HistoryID = 12346
	env.Mock.MessagePages = [][]string{{"msg-inbox"}}
	env.Mock.AddMessage("msg-inbox", testemail.NewMessage().
		From("stranger@example.test").
		Bytes(), []string{"INBOX"})

	_, err = env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "second full sync")

	found, _, err = env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext after drain")
	assert.False(found, "a successful refresh clears the backlog marker")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities after drain")
	require.Len(identities, 1, "the drain must not confirm a first-time identity")
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal,
		"the drain re-derives the evidence the failed page never wrote")
}

// TestNoOpIncrementalSyncDrainsIdentityDiscoveryBacklog covers the account
// shape msgvault is built for: an archive that has been wound down and whose
// scheduled incremental syncs are all no-ops. If the drain sat behind the
// up-to-date early return, debt parked by the last page that ever ran would
// never be settled, because no later sync would reach the drain.
func TestNoOpIncrementalSyncDrainsIdentityDiscoveryBacklog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.MessagePages = [][]string{{"msg-sent"}}
	env.Mock.AddMessage("msg-sent", testemail.NewMessage().
		From("sent-alias@example.test").
		Bytes(), []string{"SENT"})
	runFullSync(t, env)

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	require.NoError(env.Store.AddAccountIdentity(source.ID, "sent-alias@example.test", "manual"),
		"confirm the identity after the archive already holds its Sent evidence")
	require.NoError(
		env.Store.SetIdentityDiscoveryBacklogContext(env.Context, source.ID, errors.New("earlier page failed")),
		"park discovery debt")

	// The mailbox is now idle: the cursor already equals the current history.
	env.Mock.HistoryID = 12345
	var hookChangedFlags []bool
	env.Syncer = New(env.Mock, env.Store, DefaultOptions()).WithSuccessfulSyncHook(
		"provider identity refresh",
		func(_ context.Context, _ *store.Source, mailboxChanged bool) error {
			hookChangedFlags = append(hookChangedFlags, mailboxChanged)
			return nil
		},
	)
	source, err = env.Store.GetSourceByID(source.ID)
	require.NoError(err, "GetSourceByID")
	require.True(source.SyncCursor.Valid, "the full sync left a cursor")

	summary, err := env.Syncer.Incremental(env.Context, source)
	require.NoError(err, "no-op incremental sync")
	require.NotNil(summary)

	assert.Equal([]bool{false}, hookChangedFlags,
		"a no-op run flags the hook as unchanged so the installer can avoid a provider call")
	found, _, err := env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.False(found, "an idle mailbox must not strand discovery debt forever")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1)
	assert.Equal("manual,sent-folder,sent-label", identities[0].SourceSignal,
		"the drain re-derives evidence from the archive even with nothing new to sync")
}

// TestFullSyncDoesNotConfirmFirstTimeIdentity pins the refresh-only contract at
// the sync boundary: a Sent-placed message from an unknown address is archived,
// but never claims that address as one of the account's identities.
func TestFullSyncDoesNotConfirmFirstTimeIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.MessagePages = [][]string{{"msg-sent"}}
	env.Mock.AddMessage("msg-sent", testemail.NewMessage().
		From("stranger@example.test").
		Bytes(), []string{"SENT"})

	_, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "full sync")
	assertMessageCount(t, env.Store, 1)

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Empty(identities, "one Sent-placed message must not confirm a brand-new identity")
}

func TestFullSyncWithErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	// Make msg2 fail to fetch
	env.Mock.GetMessageError["msg2"] = errors.New("temporary fetch failure")

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2)), Errors: new(int64(1))})

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "error items")
	assert.Equal("msg2", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("fetch", items[0].Phase, "Phase")
	assert.Equal("fetch_error", items[0].ErrorKind, "ErrorKind")
}

func TestFullSyncSkipsGmailNotFoundBeforeFetch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	env.Mock.GetMessageError["msg2"] = &gmail.NotFoundError{Path: "/messages/msg2"}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2)), Errors: new(int64(0)), Skipped: new(int64(1))})

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusSkipped, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "skipped items")
	assert.Equal("msg2", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("fetch", items[0].Phase, "Phase")
	assert.Equal("gmail_not_found", items[0].ErrorKind, "ErrorKind")
}

func TestMIMEParsing(t *testing.T) {
	env := newTestEnv(t)

	pdfData := []byte{0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x34, 0x0a, 0x25, 0xe2, 0xe3, 0xcf, 0xd3, 0x0a, 0x31, 0x20, 0x30, 0x20, 0x6f, 0x62, 0x6a, 0x0a, 0x3c, 0x3c, 0x2f, 0x54, 0x79, 0x70, 0x65, 0x2f, 0x43, 0x61, 0x74, 0x61, 0x6c, 0x6f, 0x67, 0x2f, 0x50, 0x61, 0x67, 0x65, 0x73, 0x20, 0x32, 0x20, 0x30, 0x20, 0x52, 0x3e, 0x3e, 0x0a, 0x65, 0x6e, 0x64, 0x6f, 0x62, 0x6a}
	complexMIME := testemail.NewMessage().
		From(`"John Doe" <john@example.com>`).
		To(`"Jane Smith" <jane@example.com>, bob@example.com`).
		Cc("cc@example.com").
		Subject("Re: Meeting Notes").
		Date("Tue, 15 Jan 2024 14:30:00 -0500").
		Header("Message-ID", "<msg123@example.com>").
		Header("In-Reply-To", "<msg122@example.com>").
		Body("Hello,\n\nThis is the message body.\n\nBest regards,\nJohn\n").
		WithAttachment("document.pdf", "application/pdf", pdfData).
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("complex1", complexMIME, []string{"INBOX"})

	env.SetOptions(t, func(o *Options) {
		o.AttachmentsDir = filepath.Join(env.TmpDir, "attachments")
	})

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertAttachmentCount(t, env.Store, 1)
}

func TestStoreAttachment_ComputesHashWhenMissing(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)

	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.SetOptions(t, func(o *Options) {
		o.AttachmentsDir = attachmentsDir
	})

	src := env.CreateSource(t)
	convID, err := env.Store.EnsureConversation(src.ID, "t1", "Thread")
	require.NoError(err, "EnsureConversation")
	messageID, err := env.Store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
	})
	require.NoError(err, "UpsertMessage")

	content := []byte("hello")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	att := mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Disposition: "attachment",
		PartKey:     "mime:2",
		Size:        len(content),
		ContentHash: "",
		Content:     content,
	}
	require.NoError(env.Syncer.storeAttachment(messageID, &att), "storeAttachment")
	require.Equal(wantHash, att.ContentHash, "ContentHash")

	var gotHash, storagePath, role, roleSource, sourcePartKey string
	require.NoError(env.Store.DB().QueryRow(`
		SELECT content_hash, storage_path, attachment_role, role_source, source_part_key
		FROM attachments WHERE message_id = ?`, messageID).
		Scan(&gotHash, &storagePath, &role, &roleSource, &sourcePartKey), "select attachment")
	require.Equal(wantHash, gotHash, "db content_hash")
	require.Equal("standalone", role)
	require.Equal("mime_disposition", roleSource)
	require.Equal("mime:2", sourcePartKey)

	fullPath := filepath.Join(attachmentsDir, filepath.FromSlash(storagePath))
	b, err := os.ReadFile(fullPath)
	require.NoError(err, "read attachment file")
	require.Equal(string(content), string(b), "attachment file contents")
}

func TestStoreAttachmentPersistsInlineMIMEEvidence(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) { o.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	src := env.CreateSource(t)
	convID, err := env.Store.EnsureConversation(src.ID, "inline-thread", "Thread")
	require.NoError(err)
	messageID, err := env.Store.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID, SourceMessageID: "inline-message", MessageType: "email",
	})
	require.NoError(err)

	att := mime.Attachment{
		Filename: "inline.png", ContentType: "image/png", Content: []byte("png"),
		Disposition: "inline", PartKey: "mime:3", ContentID: "inline-1", IsInline: true,
	}
	require.NoError(env.Syncer.storeAttachment(messageID, &att))

	var role, roleSource, sourcePartKey, contentID string
	require.NoError(env.Store.DB().QueryRow(`
		SELECT attachment_role, role_source, source_part_key, content_id
		FROM attachments WHERE message_id = ?`, messageID).
		Scan(&role, &roleSource, &sourcePartKey, &contentID))
	require.Equal("inline", role)
	require.Equal("mime_disposition", roleSource)
	require.Equal("mime:3", sourcePartKey)
	require.Equal("inline-1", contentID)
}

func TestStoreAttachment_InvalidContentHash_ReturnsError(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)

	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.SetOptions(t, func(o *Options) {
		o.AttachmentsDir = attachmentsDir
	})

	src := env.CreateSource(t)
	convID, err := env.Store.EnsureConversation(src.ID, "t1", "Thread")
	require.NoError(err, "EnsureConversation")
	messageID, err := env.Store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
	})
	require.NoError(err, "UpsertMessage")

	content := []byte("hello")
	att := mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: "nope", // malformed
		Content:     content,
	}
	require.Error(env.Syncer.storeAttachment(messageID, &att), "expected error")

	_, statErr := os.Stat(attachmentsDir)
	require.Error(statErr, "attachments dir should not have been created for invalid content hash")

	var count int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID).Scan(&count), "count attachments")
	require.Zero(count, "count")
}

func TestFullSyncEmptyInbox(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 0
	env.Mock.Profile.HistoryID = 12345

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0)), Found: new(int64(0))})
}

func TestFullSyncProfileError(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.ProfileError = errors.New("auth failed")

	_, err := env.Syncer.Full(env.Context, testEmail)
	assert.Error(t, err, "expected error when profile fails")
}

func TestFullSyncAllDuplicates(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	// First sync
	runFullSync(t, env)

	// Second sync with same messages - all should be skipped
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0)), Skipped: new(int64(3))})
}

func TestFullSyncNoResume(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")

	env.SetOptions(t, func(o *Options) {
		o.NoResume = true
	})

	summary := runFullSync(t, env)
	assert.False(t, summary.WasResumed, "expected WasResumed to be false with NoResume option")
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
}

func TestFullSyncAllErrors(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	env.Mock.GetMessageError["msg1"] = errors.New("temporary fetch failure 1")
	env.Mock.GetMessageError["msg2"] = errors.New("temporary fetch failure 2")
	env.Mock.GetMessageError["msg3"] = errors.New("temporary fetch failure 3")

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0)), Errors: new(int64(3))})
}

func TestFullSyncWithQuery(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")

	env.SetOptions(t, func(o *Options) {
		o.Query = "before:2024/06/01"
	})

	summary := runFullSync(t, env)

	assert.Equal(t, "before:2024/06/01", env.Mock.LastQuery, "query")
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
}

func TestFullSyncPagination(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 6)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(6))})
	assertListMessagesCalls(t, env, 3)
}

func TestSyncerWithLogger(t *testing.T) {
	env := newTestEnv(t)
	syncer := env.Syncer.WithLogger(nil)
	assert.NotNil(t, syncer, "WithLogger should return syncer for chaining")
}

func TestSyncerWithProgress(t *testing.T) {
	env := newTestEnv(t)
	syncer := env.Syncer.WithProgress(gmail.NullProgress{})
	assert.NotNil(t, syncer, "WithProgress should return syncer for chaining")
}

// Tests for incremental sync

func TestIncrementalSyncNilSource(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.Syncer.Incremental(env.Context, nil)
	assert.Error(t, err, "expected error for nil source")
}

func TestIncrementalSyncNoHistoryID(t *testing.T) {
	env := newTestEnv(t)

	source := env.CreateSource(t)

	_, err := env.Syncer.Incremental(env.Context, source)
	assert.Error(t, err, "expected error for incremental sync without history ID")
}

func TestIncrementalSyncAlreadyUpToDate(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12345")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12345 // Same as cursor

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})
}

func TestIncrementalSyncWithChanges(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-msg-1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("new-msg-2", testMIME(), []string{"INBOX"})

	env.SetHistory(12350,
		historyAdded("new-msg-1"),
		historyAdded("new-msg-2"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
}

func TestIncrementalSyncDiscoversOnlySuccessfulChangesBeforeAdvancingCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("existing-message", testemail.NewMessage().
		From("existing-alias@example.test").
		To("recipient-only@example.test").
		Bytes(), []string{"INBOX"})
	runFullSync(t, env)

	env.Mock.AddMessage("new-success", testemail.NewMessage().
		From("new-alias@example.test").
		To("recipient-only@example.test").
		Bytes(), []string{"SENT"})
	env.Mock.AddMessage("new-failed", testemail.NewMessage().
		From("failed-alias@example.test").
		Bytes(), []string{"SENT"})
	env.Mock.GetMessageError["new-failed"] = errors.New("temporary fetch failure")
	env.SetHistory(12350,
		historyLabelAdded("existing-message", "SENT"),
		historyAdded("new-success"),
		historyAdded("new-failed"),
	)

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier before incremental sync")
	// Confirming all three addresses keeps the failed fetch observable: if its
	// page reached discovery, failed-alias would gain Sent evidence too.
	for _, address := range []string{
		"existing-alias@example.test",
		"new-alias@example.test",
		"failed-alias@example.test",
	} {
		require.NoError(env.Store.AddAccountIdentity(source.ID, address, "manual"), "seed confirmed identity")
	}

	_, err = env.Syncer.Incremental(env.Context, source)
	require.NoError(err, "incremental sync")

	source, err = env.Store.GetSourceByID(source.ID)
	require.NoError(err, "GetSourceByID after incremental sync")
	require.True(source.SyncCursor.Valid, "source cursor remains valid")
	assert.Equal("12350", source.SyncCursor.String, "a completed sync advances the history cursor")
	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 3, "incremental sync must not confirm a first-time identity")
	signalsByAddress := make(map[string]string, len(identities))
	for _, identity := range identities {
		signalsByAddress[identity.Address] = identity.SourceSignal
	}
	assert.Equal("manual,sent-folder,sent-label", signalsByAddress["existing-alias@example.test"],
		"a label update provides strong evidence")
	assert.Equal("manual,sent-folder,sent-label", signalsByAddress["new-alias@example.test"],
		"a successful addition provides strong evidence")
	assert.Equal("manual", signalsByAddress["failed-alias@example.test"],
		"failed additions are not discovered")
}

// TestIncrementalSyncCompletesAndSetsBacklogWhenDiscoveryFails is the
// incremental analog of TestFullSyncCompletesAndSetsBacklogWhenDiscoveryFails:
// the history cursor advances, because holding it back would replay the same
// already-archived changes forever over a debt the backlog can settle.
func TestIncrementalSyncCompletesAndSetsBacklogWhenDiscoveryFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	shortenIdentityDiscoveryRetry(t, time.Millisecond)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("existing-message", testemail.NewMessage().
		From("existing-alias@example.test").
		Bytes(), []string{"INBOX"})
	runFullSync(t, env)

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier before incremental sync")
	require.NoError(env.Store.AddAccountIdentity(source.ID, "existing-alias@example.test", "manual"),
		"seed confirmed identity")
	env.SetHistory(12350, historyLabelAdded("existing-message", "SENT"))
	logs := newSyncLogCapture(nil)
	env.Syncer = New(env.Mock, env.Store, DefaultOptions()).WithLogger(logs.logger())
	installFailingIdentityDiscovery(t, env, "fail_incremental_identity_discovery")

	summary, err := env.Syncer.Incremental(env.Context, source)
	require.NoError(err, "persistent discovery failure must not fail the incremental run")
	require.NotNil(summary)

	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err, "GetLatestSync")
	assert.Equal(store.SyncStatusCompleted, run.Status, "the run completes")
	source, err = env.Store.GetSourceByID(source.ID)
	require.NoError(err, "GetSourceByID after discovery failure")
	require.True(source.SyncCursor.Valid, "source cursor remains valid")
	assert.Equal("12350", source.SyncCursor.String, "the history cursor advances")

	found, lastError, err := env.Store.IdentityDiscoveryBacklogContext(env.Context, source.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.True(found, "the failed page records a durable backlog marker")
	assert.Contains(lastError, forcedDiscoveryFailure, "the marker keeps the underlying cause")
	assert.Contains(logs.String(), forcedDiscoveryFailure, "the operator sees the discovery error")

	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities after discovery failure")
	require.Len(identities, 1)
	assert.Equal("manual", identities[0].SourceSignal, "failed discovery writes no partial evidence")
}

func TestIncrementalSyncWithDeletions(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12340, "msg1", "msg2")

	runFullSync(t, env)

	// Now simulate deletion via incremental
	env.SetHistory(12350, historyDeleted("msg1"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// Verify deletion was persisted
	assertDeletedFromSource(t, env.Store, "msg1", true)
	assertDeletedFromSource(t, env.Store, "msg2", false)
}

func TestIncrementalSyncHistoryExpired(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12350
	env.Mock.HistoryError = &gmail.NotFoundError{Path: "/history"}

	_, err := env.Syncer.Incremental(env.Context, source)
	require.Error(t, err, "expected error for expired history")
	// Callers (sync CLI, daemon scheduler) key their full-sync fallback on
	// this sentinel, so it must survive wrapping.
	assert.ErrorIs(t, err, ErrHistoryExpired)
}

func TestIncrementalSyncProfileError(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12345")
	env.Mock.ProfileError = errors.New("auth failed")

	_, err := env.Syncer.Incremental(env.Context, source)
	assert.Error(t, err, "expected error when profile fails")
}

func TestIncrementalSyncWithLabelAdded(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})

	runFullSync(t, env)

	// Record call count after full sync
	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Now simulate label addition via incremental
	env.SetHistory(12350, historyLabelAdded("msg1", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// No additional GetMessageRaw calls should have been made for the existing message
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assert.Equal(t, callsAfterFull, callsAfterIncr,
		"expected 0 GetMessageRaw calls during incremental")

	// Verify the label was actually added in the database
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")
}

func TestIncrementalSyncWithLabelRemoved(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX", "STARRED"})

	runFullSync(t, env)

	// Verify STARRED exists after full sync
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")

	// Record call count after full sync
	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Now simulate label removal via incremental
	env.SetHistory(12350, historyLabelRemoved("msg1", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// No additional GetMessageRaw calls should have been made
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assert.Equal(t, callsAfterFull, callsAfterIncr,
		"expected 0 GetMessageRaw calls during incremental")

	// Verify the label was actually removed in the database
	assertMessageNotHasLabel(t, env.Store, "msg1", "STARRED")
	// INBOX should still be there
	assertMessageHasLabel(t, env.Store, "msg1", "INBOX")
}

func TestIncrementalSyncLabelAddedToNewMessage(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")
	_, err := env.Store.EnsureLabel(source.ID, "INBOX", "Inbox", labelTypeSystem)
	require.NoError(t, err, "EnsureLabel INBOX")
	_, err = env.Store.EnsureLabel(source.ID, "STARRED", "Starred", labelTypeSystem)
	require.NoError(t, err, "EnsureLabel STARRED")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-msg", testMIME(), []string{"INBOX", "STARRED"})

	env.SetHistory(12350, historyLabelAdded("new-msg", "STARRED"))

	_, err = env.Syncer.Incremental(env.Context, source)
	require.NoError(t, err, "incremental sync")

	assertMessageCount(t, env.Store, 1)
}

func TestIncrementalSyncLabelRemovedFromMissingMessage(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350

	env.SetHistory(12350, historyLabelRemoved("unknown-msg", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})
}

func TestFullSyncWithAttachment(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-with-attachment", testMIMEWithAttachment(), []string{"INBOX"})

	attachDir := withAttachmentsDir(t, env)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})

	_, statErr := os.Stat(attachDir)
	assert.False(t, os.IsNotExist(statErr), "attachments directory should have been created")

	assertAttachmentCount(t, env.Store, 1)
}

func TestFullSyncWithEmptyAttachment(t *testing.T) {
	env := newTestEnv(t)

	emptyAttachMIME := testemail.NewMessage().
		Subject("Empty Attachment").
		Body("Body text.").
		WithAttachment("empty.bin", "application/octet-stream", nil).
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-empty-attach", emptyAttachMIME, []string{"INBOX"})

	withAttachmentsDir(t, env)

	runFullSync(t, env)
	assertAttachmentCount(t, env.Store, 0)
}

func TestFullSyncAttachmentDeduplication(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg1-attach", testMIMEWithAttachment(), []string{"INBOX"})
	env.Mock.AddMessage("msg2-attach", testMIMEWithAttachment(), []string{"INBOX"})

	attachDir := withAttachmentsDir(t, env)

	runFullSync(t, env)
	assertAttachmentCount(t, env.Store, 2)

	assert.Equal(t, 1, countFiles(t, attachDir), "files in attachments dir (deduped)")
}

// TestFullSync_MessageVariations consolidates tests for various MIME message formats.
func TestFullSync_MessageVariations(t *testing.T) {
	tests := []struct {
		name  string
		mime  func() []byte
		check func(*testing.T, *TestEnv)
	}{
		{
			name: "NoSubject",
			mime: testMIMENoSubject,
		},
		{
			name: "MultipleRecipients",
			mime: testMIMEMultipleRecipients,
		},
		{
			name: "HTMLOnly",
			mime: func() []byte {
				return testemail.NewMessage().
					Subject("HTML Only").
					ContentType(`text/html; charset="utf-8"`).
					Body("<html><body><p>This is HTML only content.</p></body></html>").
					Bytes()
			},
		},
		{
			name: "DuplicateRecipients",
			mime: testMIMEDuplicateRecipients,
			check: func(t *testing.T, env *TestEnv) {
				t.Helper()
				assertRecipientCount(t, env.Store, "msg", "to", 2)
				assertRecipientCount(t, env.Store, "msg", "cc", 1)
				assertRecipientCount(t, env.Store, "msg", "bcc", 1)
				assertDisplayName(t, env.Store, "msg", "to", "duplicate@example.com", "Duplicate Person")
				assertDisplayName(t, env.Store, "msg", "cc", "cc-dup@example.com", "CC Duplicate")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			seedMessages(env, 1, 12345, "msg")
			raw := tt.mime()
			env.Mock.Messages["msg"].Raw = raw
			env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

			summary := runFullSync(t, env)
			assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})
			assertMessageCount(t, env.Store, 1)

			if tt.check != nil {
				tt.check(t, env)
			}
		})
	}
}

func TestFullSync_Latin1InFromName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg")

	// Build a MIME message with an RFC 2047 encoded From name that claims UTF-8
	// but actually contains Latin-1 bytes. This is a real-world scenario where a
	// sender's MUA mis-labels the charset, producing invalid UTF-8 after decoding.
	// The =C9 byte is Latin-1 É, which is not valid UTF-8 when surrounded by ASCII.
	raw := []byte("From: =?UTF-8?Q?Jane_Doe=C9ric?= <sender@example.com>\n" +
		"To: recipient@example.com\n" +
		"Subject: Test\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n" +
		"\n" +
		"Body text.\n")

	env.Mock.Messages["msg"].Raw = raw
	env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	// Verify the participant display_name in the participants table is valid UTF-8.
	// Before the fix, raw Latin-1 bytes would be stored as-is, causing DuckDB errors
	// when exporting to Parquet.
	displayName, err := env.Store.InspectParticipantDisplayName("sender@example.com")
	require.NoError(err, "InspectParticipantDisplayName")
	// EnsureUTF8 should convert the Latin-1 \xC9 to the UTF-8 É (U+00C9)
	want := "Jane DoeÉric"
	assert.Equal(want, displayName, "participant display_name")

	// Also verify the message_recipients display_name is valid
	recipDisplayName, err := env.Store.InspectDisplayName("msg", "from", "sender@example.com")
	require.NoError(err, "InspectDisplayName")
	assert.Equal(want, recipDisplayName, "recipient display_name")
}

func TestFullSync_InvalidUTF8InAllAddressFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg")

	// Test that UTF-8 validation applies to all address fields (To, Cc, Bcc),
	// not just From. Uses Windows-1252 smart quotes (\x93, \x94) mis-labeled as UTF-8,
	// a common real-world scenario from Outlook emails.
	raw := []byte("From: =?UTF-8?Q?=93From=94_Name?= <from@example.com>\n" +
		"To: =?UTF-8?Q?=93To=94_Name?= <to@example.com>\n" +
		"Cc: =?UTF-8?Q?=93Cc=94_Name?= <cc@example.com>\n" +
		"Bcc: =?UTF-8?Q?=93Bcc=94_Name?= <bcc@example.com>\n" +
		"Subject: Test\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n" +
		"\n" +
		"Body text.\n")

	env.Mock.Messages["msg"].Raw = raw
	env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	// EnsureUTF8 should detect Windows-1252 and convert smart quotes to their
	// proper Unicode equivalents: \x93 → U+201C ("), \x94 → U+201D (")
	tests := []struct {
		recipType string
		email     string
	}{
		{"from", "from@example.com"},
		{"to", "to@example.com"},
		{"cc", "cc@example.com"},
		{"bcc", "bcc@example.com"},
	}
	for _, tt := range tests {
		// Verify participants table has valid UTF-8
		displayName, err := env.Store.InspectParticipantDisplayName(tt.email)
		require.NoError(err, "InspectParticipantDisplayName(%s)", tt.email)
		titled := strings.ToUpper(tt.recipType[:1]) + tt.recipType[1:]
		want := "\u201c" + titled + "\u201d Name"
		assert.Equal(want, displayName, "participant %s display_name", tt.email)

		// Verify message_recipients table has valid UTF-8
		recipName, err := env.Store.InspectDisplayName("msg", tt.recipType, tt.email)
		require.NoError(err, "InspectDisplayName(%s, %s)", tt.recipType, tt.email)
		assert.Equal(want, recipName, "recipient %s/%s display_name", tt.recipType, tt.email)
	}
}

func TestFullSync_InvalidUTF8InAttachmentFilename(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)

	// Construct a MIME message with raw Latin-1 byte \xE9 (é) in the attachment
	// filename. Enmime sanitizes invalid bytes to U+FFFD before our code sees them;
	// the sync-level EnsureUTF8 call is defense-in-depth for any future parser changes.
	raw := []byte("From: sender@example.com\n" +
		"To: recipient@example.com\n" +
		"Subject: Attachment Test\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"MIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\n" +
		"\n" +
		"--b\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n\nBody text.\n" +
		"--b\n" +
		"Content-Type: application/pdf; name=\"caf\xe9.pdf\"\n" +
		"Content-Disposition: attachment; filename=\"caf\xe9.pdf\"\n" +
		"Content-Transfer-Encoding: base64\n\n" +
		"SGVsbG8gV29ybGQh\n" +
		"--b--\n")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-attach", raw, []string{"INBOX"})

	withAttachmentsDir(t, env)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertAttachmentCount(t, env.Store, 1)

	filename, mimeType, err := env.Store.InspectAttachment("msg-attach")
	require.NoError(t, err, "InspectAttachment")

	// Enmime replaces the invalid \xE9 byte with U+FFFD (replacement character).
	// Our EnsureUTF8 would convert it to the proper é if enmime didn't sanitize first.
	// Either way, the stored filename must be valid UTF-8 and preserve the base name.
	assert.True(utf8.ValidString(filename), "attachment filename %q is not valid UTF-8", filename)
	assert.True(strings.HasPrefix(filename, "caf"), "attachment filename = %q, want caf*.pdf pattern", filename)
	assert.True(strings.HasSuffix(filename, ".pdf"), "attachment filename = %q, want caf*.pdf pattern", filename)

	// Content-type should be the clean base MIME type
	assert.Equal("application/pdf", mimeType, "attachment mime_type")
}

func TestFullSync_InvalidPartContentType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	raw := testemail.NewMessage().
		Subject("Statement ready").
		Body("Attached is a synthetic statement.").
		WithAttachment(
			"statement.pdf",
			"cannot open (No such file or directory)",
			[]byte("synthetic pdf bytes"),
		).
		CRLF().
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-invalid-content-type", raw, []string{"INBOX"})
	withAttachmentsDir(t, env)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})
	assertBodyContains(t, env.Store, "msg-invalid-content-type", "Attached is a synthetic statement.")
	assertAttachmentCount(t, env.Store, 1)

	results, total, err := env.Store.SearchMessages("Statement ready", 0, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal("Statement ready", results[0].Subject)

	filename, mimeType, err := env.Store.InspectAttachment("msg-invalid-content-type")
	require.NoError(err)
	assert.Equal("statement.pdf", filename)
	assert.Equal("application/octet-stream", mimeType)
}

func TestFullSync_MultipleEncodingIssuesSameMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg")

	// Real-world scenario: a single email with multiple encoding issues.
	// Latin-1 É (\xC9) in From name, and Windows-1252 smart quote (\x93) in To name.
	raw := []byte("From: =?UTF-8?Q?Doe=C9ric?= <from@example.com>\n" +
		"To: =?UTF-8?Q?=93Quoted=94?= <to@example.com>\n" +
		"Subject: =?UTF-8?Q?Caf=E9?=\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n" +
		"\n" +
		"Body text.\n")

	env.Mock.Messages["msg"].Raw = raw
	env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	// From name: Latin-1 \xC9 → UTF-8 É
	fromName, err := env.Store.InspectParticipantDisplayName("from@example.com")
	require.NoError(err, "InspectParticipantDisplayName(from)")
	assert.Equal("DoeÉric", fromName, "from display_name")

	// To name: Windows-1252 \x93/\x94 → Unicode left/right double quotes
	toName, err := env.Store.InspectParticipantDisplayName("to@example.com")
	require.NoError(err, "InspectParticipantDisplayName(to)")
	assert.Equal("\u201cQuoted\u201d", toName, "to display_name")

	// Subject: Latin-1 \xE9 → UTF-8 é (already validated by existing code path)
	insp, err := env.Store.InspectMessage("msg")
	require.NoError(err, "InspectMessage")
	assert.Contains(insp.RecipientDisplayName["from:from@example.com"], "É", "from recipient display_name should contain É")
}

func TestFullSyncWithMIMEParseError(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-good", testMIME(), []string{"INBOX"})
	env.Mock.Messages["msg-bad"] = &gmail.RawMessage{
		ID:           "msg-bad",
		ThreadID:     "thread_msg-bad",
		LabelIDs:     []string{"INBOX"},
		Raw:          []byte("not valid mime at all - just garbage"),
		Snippet:      "This is the snippet preview",
		SizeEstimate: 100,
	}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2)), Errors: new(int64(0))})

	// Verify the bad message was stored with placeholder content
	assertBodyContains(t, env.Store, "msg-bad", "MIME parsing failed")
	assertRawDataExists(t, env.Store, "msg-bad")
}

func TestFullSyncMessageFetchError(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-good", testMIME(), []string{"INBOX"})

	env.Mock.MessagePages = [][]string{{"msg-good", "msg-missing"}}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
}

func TestIncrementalSyncLabelsError(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.LabelsError = errors.New("labels API error")

	_, err := env.Syncer.Incremental(env.Context, source)
	assert.Error(t, err, "expected error when labels sync fails")
}

func TestFullSyncResumeWithCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 4)

	source := env.CreateSource(t)

	// Process just page 1
	env.Mock.MessagePages = [][]string{{"msg1", "msg2"}}
	runFullSync(t, env)
	assertMessageCount(t, env.Store, 2)

	// Restore both pages and create an "interrupted" sync
	env.Mock.MessagePages = [][]string{
		{"msg1", "msg2"},
		{"msg3", "msg4"},
	}
	env.Mock.ListMessagesCalls = 0

	syncID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err, "StartSync")

	checkpoint := &store.Checkpoint{
		PageToken:         "page_1",
		MessagesProcessed: 2,
		MessagesAdded:     2,
	}
	require.NoError(env.Store.UpdateSyncCheckpoint(syncID, checkpoint), "UpdateSyncCheckpoint")

	summary := runFullSync(t, env)

	assert.True(summary.WasResumed, "expected WasResumed = true")
	assert.Equal("page_1", summary.ResumedFromToken, "ResumedFromToken")
	assertSummary(t, summary, WantSummary{Added: new(int64(4))})

	assertListMessagesCalls(t, env, 1)
	assertMessageCount(t, env.Store, 4)
}

func TestFullSyncDateFallbackToInternalDate(t *testing.T) {
	env := newTestEnv(t)

	badDateMIME := testemail.NewMessage().
		Subject("Bad Date").
		Date("This is not a valid date").
		Body("Message with invalid date header.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.Messages["msg-bad-date"] = &gmail.RawMessage{
		ID:           "msg-bad-date",
		ThreadID:     "thread-bad-date",
		LabelIDs:     []string{"INBOX"},
		Raw:          badDateMIME,
		InternalDate: 1705320000000, // 2024-01-15T12:00:00Z
	}
	env.Mock.MessagePages = [][]string{{"msg-bad-date"}}

	runFullSync(t, env)

	assertDateFallback(t, env.Store, "msg-bad-date", "2024-01-15", "12:00:00")
}

func TestFullSyncImplausibleDateUsesOldestReceivedTimestamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)

	raw := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Date: Thu, 01 Jan 1970 00:00:00 +0000\r\n" +
		"Received: from relay.example.net by mx.example.net; Wed, 03 Jan 2007 15:04:05 +0000\r\n" +
		"Received: from sender.example.net by relay.example.net; Tue, 02 Jan 2007 15:04:05 +0000\r\n" +
		"Subject: date resolution\r\n\r\nbody\r\n")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.Messages["msg-implausible-date"] = &gmail.RawMessage{
		ID:           "msg-implausible-date",
		ThreadID:     "thread-implausible-date",
		LabelIDs:     []string{"INBOX"},
		Raw:          raw,
		InternalDate: 1430827200000, // 2015-05-05T12:00:00Z
	}
	env.Mock.MessagePages = [][]string{{"msg-implausible-date"}}

	runFullSync(t, env)

	sentAt, internalDate, err := env.Store.InspectMessageDates("msg-implausible-date")
	require.NoError(err, "InspectMessageDates")
	assert.Contains(sentAt, "2007-01-02", "sent_at")
	assert.Contains(internalDate, "2015-05-05", "internal_date")
	assert.NotEqual(internalDate, sentAt)
}

func TestFullSyncEmptyRawMIME(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345

	env.Mock.AddMessage("msg-good", testMIME(), []string{"INBOX"})
	env.Mock.Messages["msg-empty-raw"] = &gmail.RawMessage{
		ID:           "msg-empty-raw",
		ThreadID:     "thread-empty-raw",
		LabelIDs:     []string{"INBOX"},
		Raw:          []byte{},
		SizeEstimate: 0,
	}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(1))})

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err, "GetSourceByIdentifier")
	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "error items")
	assert.Equal("msg-empty-raw", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("ingest_error", items[0].ErrorKind, "ErrorKind")
}

func TestFullSyncEmptyThreadID(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.UseRawThreadID = true

	raw := testMIME()
	env.Mock.Messages["msg-no-thread"] = &gmail.RawMessage{
		ID:           "msg-no-thread",
		ThreadID:     "",
		LabelIDs:     []string{"INBOX"},
		Raw:          raw,
		SizeEstimate: int64(len(raw)),
	}
	env.Mock.MessagePages = [][]string{{"msg-no-thread"}}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	assertThreadSourceID(t, env.Store, "msg-no-thread", "msg-no-thread")
}

func TestFullSyncListEmptyThreadIDRawPresent(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345

	raw := testMIME()
	env.Mock.ListThreadIDOverride = map[string]string{
		"msg-list-empty": "",
	}
	env.Mock.Messages["msg-list-empty"] = &gmail.RawMessage{
		ID:           "msg-list-empty",
		ThreadID:     "actual-thread-from-raw",
		LabelIDs:     []string{"INBOX"},
		Raw:          raw,
		SizeEstimate: int64(len(raw)),
	}
	env.Mock.MessagePages = [][]string{{"msg-list-empty"}}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	assertThreadSourceID(t, env.Store, "msg-list-empty", "actual-thread-from-raw")
}

// Tests for initSyncState

func TestInitSyncState_NewSync(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)

	state, err := env.Syncer.initSyncState(source.ID)
	require.NoError(t, err, "initSyncState")

	assert.False(state.wasResumed, "expected wasResumed = false for new sync")
	assert.Empty(state.pageToken, "pageToken")
	assert.NotZero(state.syncID, "expected non-zero syncID")
	assert.Equal(int64(0), state.checkpoint.MessagesProcessed, "MessagesProcessed")
}

func TestInitSyncState_Resume(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)

	// Create an active sync with checkpoint
	syncID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err, "StartSync")
	checkpoint := &store.Checkpoint{
		PageToken:         "resume_token_123",
		MessagesProcessed: 50,
		MessagesAdded:     45,
		MessagesUpdated:   3,
		ErrorsCount:       2,
	}
	require.NoError(env.Store.UpdateSyncCheckpoint(syncID, checkpoint), "UpdateSyncCheckpoint")

	state, err := env.Syncer.initSyncState(source.ID)
	require.NoError(err, "initSyncState")

	assert.True(state.wasResumed, "expected wasResumed = true")
	assert.Equal("resume_token_123", state.pageToken, "pageToken")
	assert.Equal(syncID, state.syncID, "syncID")
	assert.Equal(int64(50), state.checkpoint.MessagesProcessed, "MessagesProcessed")
	assert.Equal(int64(45), state.checkpoint.MessagesAdded, "MessagesAdded")
}

func TestInitSyncState_NoResumeOption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) {
		o.NoResume = true
	})
	source := env.CreateSource(t)

	// Create an active sync with checkpoint
	syncID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err, "StartSync")
	checkpoint := &store.Checkpoint{
		PageToken:         "resume_token_123",
		MessagesProcessed: 50,
	}
	require.NoError(env.Store.UpdateSyncCheckpoint(syncID, checkpoint), "UpdateSyncCheckpoint")

	state, err := env.Syncer.initSyncState(source.ID)
	require.NoError(err, "initSyncState")

	assert.False(state.wasResumed, "expected wasResumed = false with NoResume option")
	assert.Empty(state.pageToken, "pageToken with NoResume")
	assert.NotEqual(syncID, state.syncID, "expected new syncID, not the existing one")
}

// Tests for processBatch

func TestProcessBatch_EmptyBatch(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap := make(map[string]int64)
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	listResp := &gmail.MessageListResponse{
		Messages: nil,
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(t, err, "processBatch")

	assert.Equal(int64(0), result.processed, "processed")
	assert.Equal(int64(0), result.added, "added")
	assert.Equal(int64(0), result.skipped, "skipped")
}

func TestProcessBatch_AllNew(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: labelTypeSystem},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(t, err, "processBatch")

	assert.Equal(int64(2), result.processed, "processed")
	assert.Equal(int64(2), result.added, "added")
	assert.Equal(int64(0), result.skipped, "skipped")
}

func TestProcessBatch_AllExisting(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")

	// First sync to add messages
	runFullSync(t, env)

	source, _ := env.Store.GetOrCreateSource("gmail", testEmail)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: labelTypeSystem},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(t, err, "processBatch")

	assert.Equal(int64(2), result.processed, "processed")
	assert.Equal(int64(0), result.added, "added (all existing)")
	assert.Equal(int64(2), result.skipped, "skipped")
}

func TestProcessBatch_MixedNewAndExisting(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg1")

	// First sync to add msg1
	runFullSync(t, env)

	source, _ := env.Store.GetOrCreateSource("gmail", testEmail)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: labelTypeSystem},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	// Add msg2 to mock
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(t, err, "processBatch")

	assert.Equal(int64(2), result.processed, "processed")
	assert.Equal(int64(1), result.added, "added")
	assert.Equal(int64(1), result.skipped, "skipped")
}

func TestProcessBatch_OldestDatePropagation(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: labelTypeSystem},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	// Add messages with specific internal dates
	// msg1: Jan 15, 2024, msg2: Jan 10, 2024 (older)
	env.Mock.Messages["msg1"] = &gmail.RawMessage{
		ID:           "msg1",
		ThreadID:     "thread1",
		LabelIDs:     []string{"INBOX"},
		Raw:          testMIME(),
		InternalDate: 1705320000000, // 2024-01-15T12:00:00Z
	}
	env.Mock.Messages["msg2"] = &gmail.RawMessage{
		ID:           "msg2",
		ThreadID:     "thread2",
		LabelIDs:     []string{"INBOX"},
		Raw:          testMIME(),
		InternalDate: 1704888000000, // 2024-01-10T12:00:00Z
	}

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(t, err, "processBatch")

	// oldestDate should be Jan 10, 2024
	assert.False(result.oldestDate.IsZero(), "expected oldestDate to be set")
	gotYear, gotMonth, gotDay := result.oldestDate.Year(), int(result.oldestDate.Month()), result.oldestDate.Day()
	assert.Equal(2024, gotYear, "year")
	assert.Equal(1, gotMonth, "month")
	assert.Equal(10, gotDay, "day")
}

func TestProcessBatch_ErrorsCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: labelTypeSystem},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	// msg2 will return nil (simulating fetch failure)
	env.Mock.GetMessageError["msg2"] = errors.New("temporary fetch failure")

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(err, "processBatch")

	assert.Equal(int64(1), result.added, "added")
	assert.Equal(int64(1), checkpoint.ErrorsCount, "ErrorsCount")

	items, err := env.Store.ListSyncRunItems(syncID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "error items")
	assert.Equal("msg2", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("fetch_error", items[0].ErrorKind, "ErrorKind")
}

func TestProcessBatch_GmailNotFoundIsSkipped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: labelTypeSystem},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["msg2"] = &gmail.NotFoundError{Path: "/messages/msg2"}

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	syncID := startSyncRun(t, env, source.ID)
	result, err := env.Syncer.processBatch(env.Context, syncID, source.ID, listResp, labelMap, checkpoint, summary)
	require.NoError(err, "processBatch")

	assert.Equal(int64(1), result.added, "added")
	assert.Equal(int64(1), result.skipped, "skipped")
	assert.Equal(int64(0), checkpoint.ErrorsCount, "ErrorsCount")

	items, err := env.Store.ListSyncRunItems(syncID, store.SyncRunItemStatusSkipped, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "skipped items")
	assert.Equal("msg2", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("gmail_not_found", items[0].ErrorKind, "ErrorKind")
}

// TestAttachmentFilePermissions verifies that attachment files are saved with
// restrictive permissions (0600) to protect email content.
func TestAttachmentFilePermissions(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-with-attachment", testMIMEWithAttachment(), []string{"INBOX"})

	attachDir := withAttachmentsDir(t, env)

	runFullSync(t, env)

	// Find the attachment file
	var attachmentPath string
	err := filepath.WalkDir(attachDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			attachmentPath = path
		}
		return nil
	})
	require.NoError(err, "WalkDir(%s)", attachDir)
	require.NotEmpty(attachmentPath, "no attachment file found")

	info, err := os.Stat(attachmentPath)
	require.NoError(err, "Stat(%s)", attachmentPath)

	// File should have 0600 permissions (owner read/write only)
	// Windows does not support Unix permissions.
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "attachment file permissions")
	}
}

// TestIncrementalSyncLabelAddAndRemoveOnExisting verifies that adding and removing
// labels on the same existing message in a single history page applies correctly
// and makes NO API calls to re-fetch the message.
func TestIncrementalSyncLabelAddAndRemoveOnExisting(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX", "STARRED"})

	runFullSync(t, env)

	// Verify starting labels
	assertMessageHasLabel(t, env.Store, "msg1", "INBOX")
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")

	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Simulate: TRASH added + INBOX removed (what Gmail does for delete)
	env.SetHistory(12350,
		historyLabelAdded("msg1", "TRASH"),
		historyLabelRemoved("msg1", "INBOX"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// Zero additional API calls
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assert.Equal(t, callsAfterFull, callsAfterIncr, "expected 0 GetMessageRaw calls during incremental")

	// Verify label state: TRASH and STARRED remain, INBOX removed
	assertMessageHasLabel(t, env.Store, "msg1", "TRASH")
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")
	assertMessageNotHasLabel(t, env.Store, "msg1", "INBOX")
}

// TestIncrementalSyncBatchDeletions verifies that multiple deletions in a single
// history page are applied in batch.
func TestIncrementalSyncBatchDeletions(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 4, 12340, "msg1", "msg2", "msg3", "msg4")

	runFullSync(t, env)
	assertMessageCount(t, env.Store, 4)

	// Delete 3 messages in a single history page
	env.SetHistory(12350,
		historyDeleted("msg1"),
		historyDeleted("msg2"),
		historyDeleted("msg4"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(3))})

	assertDeletedFromSource(t, env.Store, "msg1", true)
	assertDeletedFromSource(t, env.Store, "msg2", true)
	assertDeletedFromSource(t, env.Store, "msg3", false)
	assertDeletedFromSource(t, env.Store, "msg4", true)
}

// TestIncrementalSyncBatchNewMessages verifies that multiple new messages in a
// single history page are fetched via GetMessagesRawBatch (not one at a time).
func TestIncrementalSyncBatchNewMessages(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 5
	env.Mock.Profile.HistoryID = 12350
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("new-%d", i)
		env.Mock.AddMessage(id, testMIME(), []string{"INBOX"})
	}

	env.SetHistory(12350,
		historyAdded("new-1"),
		historyAdded("new-2"),
		historyAdded("new-3"),
		historyAdded("new-4"),
		historyAdded("new-5"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(5))})
	assertMessageCount(t, env.Store, 5)
}

func TestIncrementalSyncSkipsGmailNotFoundBeforeFetch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-ok", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["new-gone"] = &gmail.NotFoundError{Path: "/messages/new-gone"}

	env.SetHistory(12350,
		historyAdded("new-ok"),
		historyAdded("new-gone"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(2)), Added: new(int64(1)), Errors: new(int64(0))})
	assertMessageCount(t, env.Store, 1)

	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusSkipped, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "skipped items")
	assert.Equal("new-gone", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("gmail_not_found", items[0].ErrorKind, "ErrorKind")
}

func TestIncrementalSyncRecordsFetchErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-ok", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["new-error"] = errors.New("temporary fetch failure")

	env.SetHistory(12350,
		historyAdded("new-ok"),
		historyAdded("new-error"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(2)), Added: new(int64(1)), Errors: new(int64(1))})
	assertMessageCount(t, env.Store, 1)

	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "error items")
	assert.Equal("new-error", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("fetch_error", items[0].ErrorKind, "ErrorKind")
}

func TestIncrementalSyncRecordsLabelAddFetchErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.GetMessageError["label-fetch-error"] = errors.New("temporary fetch failure")

	env.SetHistory(12350,
		historyLabelAdded("label-fetch-error", "INBOX"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1)), Added: new(int64(0)), Errors: new(int64(1))})
	assertMessageCount(t, env.Store, 0)

	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "error items")
	assert.Equal("label-fetch-error", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("fetch", items[0].Phase, "Phase")
	assert.Equal("fetch_error", items[0].ErrorKind, "ErrorKind")
}

func TestIncrementalSyncRecordsLabelAddGmailNotFound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.GetMessageError["label-gone"] = &gmail.NotFoundError{Path: "/messages/label-gone"}

	env.SetHistory(12350,
		historyLabelAdded("label-gone", "INBOX"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1)), Added: new(int64(0)), Errors: new(int64(0))})
	assertMessageCount(t, env.Store, 0)

	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusSkipped, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "skipped items")
	assert.Equal("label-gone", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("fetch", items[0].Phase, "Phase")
	assert.Equal("gmail_not_found", items[0].ErrorKind, "ErrorKind")
}

func TestIncrementalSyncRecordsLabelAddIngestErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.Messages["label-ingest-error"] = &gmail.RawMessage{
		ID:       "label-ingest-error",
		ThreadID: "thread_label-ingest-error",
		LabelIDs: []string{"INBOX"},
		Raw:      []byte{},
	}

	env.SetHistory(12350,
		historyLabelAdded("label-ingest-error", "INBOX"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1)), Added: new(int64(0)), Errors: new(int64(1))})
	assertMessageCount(t, env.Store, 0)

	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	items, err := env.Store.ListSyncRunItems(run.ID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "error items")
	assert.Equal("label-ingest-error", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("ingest", items[0].Phase, "Phase")
	assert.Equal("ingest_error", items[0].ErrorKind, "ErrorKind")
}

func TestIncrementalSyncDedupesMessageAddedAndLabelAddedForSameUnknownMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	raw := testMIME()
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-with-label", raw, []string{"INBOX", "STARRED"})

	env.SetHistory(12350, gmail.HistoryRecord{
		MessagesAdded: []gmail.HistoryMessage{
			{Message: gmail.MessageID{ID: "new-with-label", ThreadID: "thread_new-with-label"}},
		},
		LabelsAdded: []gmail.HistoryLabelChange{
			{
				Message:  gmail.MessageID{ID: "new-with-label", ThreadID: "thread_new-with-label"},
				LabelIDs: []string{"STARRED"},
			},
		},
	})

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1)), Added: new(int64(1)), Errors: new(int64(0))})
	assert.Equal(int64(len(raw)), summary.BytesDownloaded, "BytesDownloaded")
	assertMessageCount(t, env.Store, 1)
	assert.Equal([]string{"new-with-label"}, env.Mock.GetMessageCalls, "GetMessageRaw calls")
	assertMessageHasLabel(t, env.Store, "new-with-label", "STARRED")

	run, err := env.Store.GetLastSuccessfulSync(source.ID)
	require.NoError(err, "GetLastSuccessfulSync")
	itemCount, err := env.Store.CountSyncRunItems(run.ID, "")
	require.NoError(err, "CountSyncRunItems")
	assert.Zero(itemCount, "sync_run_items")
}

// TestIncrementalSyncMixedOperations tests a history page with adds, deletes,
// and label changes all at once.
func TestIncrementalSyncMixedOperations(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12340, "existing-1", "existing-2")

	runFullSync(t, env)
	assertMessageCount(t, env.Store, 2)

	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Add new messages to mock
	env.Mock.AddMessage("new-1", testMIME(), []string{"INBOX"})

	// Mixed history: add a new msg, delete an existing msg, change labels on another
	env.SetHistory(12350,
		historyAdded("new-1"),
		historyDeleted("existing-1"),
		historyLabelAdded("existing-2", "STARRED"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(3)), Added: new(int64(1))})

	// 1 new message fetched (batch), 0 for label change on existing
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	// GetMessagesRawBatch calls GetMessageRaw internally in MockAPI, so 1 call for new-1
	newCalls := callsAfterIncr - callsAfterFull
	assert.Equal(t, 1, newCalls, "GetMessageRaw call count for new message")

	assertDeletedFromSource(t, env.Store, "existing-1", true)
	assertMessageHasLabel(t, env.Store, "existing-2", "STARRED")
	// GetStats now applies the live-message predicate: source-deleted rows are
	// excluded. Count is 1 surviving original + 1 new = 2.
	assertMessageCount(t, env.Store, 2)
}

// TestDeriveThreadKey verifies the MIME-based thread key derivation used for
// IMAP sources that lack server-side threading.
func TestDeriveThreadKey(t *testing.T) {
	tests := []struct {
		name      string
		msg       *mime.Message
		wantKey   string
		wantEmpty bool
	}{
		{
			name:    "References uses first entry (thread root)",
			msg:     &mime.Message{References: []string{"root@ex", "mid@ex"}, InReplyTo: "<mid@ex>", MessageID: "<self@ex>"},
			wantKey: "root@ex",
		},
		{
			name:    "InReplyTo fallback when no References",
			msg:     &mime.Message{InReplyTo: "<parent@ex>", MessageID: "<self@ex>"},
			wantKey: "parent@ex",
		},
		{
			name:    "MessageID fallback for standalone",
			msg:     &mime.Message{MessageID: "<self@ex>"},
			wantKey: "self@ex",
		},
		{
			name:    "Multi-ID InReplyTo uses first entry",
			msg:     &mime.Message{InReplyTo: "<a@ex> <b@ex>", MessageID: "<self@ex>"},
			wantKey: "a@ex",
		},
		{
			name:    "InReplyTo with leading comment",
			msg:     &mime.Message{InReplyTo: "(comment) <root@ex>"},
			wantKey: "root@ex",
		},
		{
			name:    "InReplyTo with folded whitespace",
			msg:     &mime.Message{InReplyTo: "\r\n <root@ex>"},
			wantKey: "root@ex",
		},
		{
			name:      "Bare token without angle brackets ignored",
			msg:       &mime.Message{InReplyTo: "bare-token"},
			wantEmpty: true,
		},
		{
			name:      "Empty when no threading info",
			msg:       &mime.Message{},
			wantEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveThreadKey(tt.msg)
			if tt.wantEmpty {
				assert.Empty(t, got, "expected empty")
			} else {
				assert.Equal(t, tt.wantKey, got, "thread key")
			}
		})
	}
}

// TestIMAPThreading verifies that IMAP messages sharing an email thread
// (via References/In-Reply-To headers) are grouped into the same conversation.
func TestIMAPThreading(t *testing.T) {
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) {
		o.SourceType = sourceTypeIMAP
	})

	// Build three messages in a thread:
	// msg-root -> msg-reply -> msg-reply2
	rootMIME := testemail.NewMessage().
		Subject("Thread root").
		Header("Message-ID", "<root@example.com>").
		Body("Root message.").
		Bytes()

	replyMIME := testemail.NewMessage().
		Subject("Re: Thread root").
		Header("Message-ID", "<reply@example.com>").
		Header("In-Reply-To", "<root@example.com>").
		Header("References", "<root@example.com>").
		Body("Reply message.").
		Bytes()

	reply2MIME := testemail.NewMessage().
		Subject("Re: Thread root").
		Header("Message-ID", "<reply2@example.com>").
		Header("In-Reply-To", "<reply@example.com>").
		Header("References", "<root@example.com> <reply@example.com>").
		Body("Second reply.").
		Bytes()

	// Standalone message (no threading headers except Message-ID)
	standaloneMIME := testemail.NewMessage().
		Subject("Unrelated").
		Header("Message-ID", "<standalone@example.com>").
		Body("Standalone message.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 4
	env.Mock.Profile.HistoryID = 100
	env.Mock.AddMessage("INBOX|1", rootMIME, []string{"INBOX"})
	env.Mock.AddMessage("INBOX|2", replyMIME, []string{"INBOX"})
	env.Mock.AddMessage("INBOX|3", reply2MIME, []string{"INBOX"})
	env.Mock.AddMessage("INBOX|4", standaloneMIME, []string{"INBOX"})

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(4))})

	// All three thread messages should share the same conversation
	// (thread key = References[0] = root@example.com, brackets stripped)
	assertThreadSourceID(t, env.Store, "INBOX|1", "root@example.com")
	assertThreadSourceID(t, env.Store, "INBOX|2", "root@example.com")
	assertThreadSourceID(t, env.Store, "INBOX|3", "root@example.com")

	// Standalone should use its own Message-ID (brackets stripped)
	assertThreadSourceID(t, env.Store, "INBOX|4", "standalone@example.com")

	// Verify conversation grouping: thread msgs share 1 conversation,
	// standalone gets its own.
	var convCount int
	err := env.Store.DB().QueryRow(`SELECT COUNT(DISTINCT conversation_id) FROM messages`).Scan(&convCount)
	require.NoError(t, err, "count conversations")
	assert.Equal(t, 2, convCount, "expected 2 conversations (1 thread + 1 standalone)")
}

// TestIMAPCrossSyncDedup verifies that a message imported from one mailbox
// is not re-imported when it appears under a different mailbox|uid on a
// subsequent sync (e.g. moved from All Mail to Trash).
func TestIMAPCrossSyncDedup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) {
		o.SourceType = sourceTypeIMAP
	})

	msg := testemail.NewMessage().
		Subject("Dedup test").
		Header("Message-ID", "<dedup@example.com>").
		Body("Same message, different mailbox.").
		Bytes()

	// First sync: message is in INBOX
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 100
	env.Mock.AddMessage("INBOX|42", msg, []string{"INBOX"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertMessageHasLabel(t, env.Store, "INBOX|42", "INBOX")

	// Second sync: message moved to Trash (different composite ID)
	delete(env.Mock.Messages, "INBOX|42")
	env.Mock.AddMessage("TRASH|99", msg, []string{"TRASH"})
	validationAPI := &sourceValidationAPI{MockAPI: env.Mock}
	env.Syncer = New(validationAPI, env.Store, env.Syncer.opts)
	summary = runFullSync(t, env)
	// Should be skipped via RFC822 Message-ID dedup, not re-imported
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})

	// Only one message should exist in the database
	var count int
	err := env.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`).Scan(&count)
	require.NoError(err, "count messages")
	assert.Equal(1, count, "expected 1 message (duplicate imported)")

	// The existing row's source_message_id should be updated to the
	// new composite ID so future syncs don't re-download the message.
	var srcMsgID string
	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&srcMsgID)
	require.NoError(err, "get source_message_id")
	assert.Equal("TRASH|99", srcMsgID, "source_message_id not updated")

	// Labels should reflect the new mailbox.
	assertMessageHasLabel(t, env.Store, "TRASH|99", "TRASH")
	assertMessageNotHasLabel(t, env.Store, "TRASH|99", "INBOX")
}

func TestIMAPFullRescanWithoutAllReconcilesMovedMessage(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	const messageID = "moved-between-folders@example.com"
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"INBOX": 0,
		"Trash": 0,
	})
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertMessageHasLabel(t, env.Store, "INBOX|1", "INBOX")
	require.NoError(firstClient.TrashMessage(env.Context, "INBOX|1"))
	require.NoError(firstClient.Close())

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)

	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal(t, "Trash|1", sourceMessageID)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Trash")
	assertMessageNotHasLabel(t, env.Store, sourceMessageID, "INBOX")
}

func TestIMAPFullRescanWithoutAllPreservesExistingIDForOverlappingFolders(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	const messageID = "overlapping-folders@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	var originalSourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&originalSourceMessageID)
	require.NoError(err)
	assertMessageHasLabel(t, env.Store, originalSourceMessageID, "Archive")
	assertMessageHasLabel(t, env.Store, originalSourceMessageID, "INBOX")

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})

	var rescannedSourceMessageID string
	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&rescannedSourceMessageID)
	require.NoError(err)
	assert.Equal(t, originalSourceMessageID, rescannedSourceMessageID)
	assertMessageHasLabel(t, env.Store, rescannedSourceMessageID, "Archive")
	assertMessageHasLabel(t, env.Store, rescannedSourceMessageID, "INBOX")
}

func TestIMAPCompleteSnapshotAdoptsAllMailCanonicalID(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"All Mail": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{
			"All Mail": {imapv2.MailboxAttrAll},
		},
	)
	const messageID = "canonical-all-mail@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "All Mail", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)

	firstClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderFilter([]string{"INBOX"}, nil))
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	require.Equal("INBOX|1", sourceMessageID)
	require.NoError(firstClient.Close())

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Updated: new(int64(1))})

	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal(t, "All Mail|1", sourceMessageID)
}

func TestIMAPUnsupportedQresyncMoveUsesFullFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"INBOX": 0,
		"Trash": 0,
	})
	const messageID = "high-water-move@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	saved := firstClient.ObservedFolderStates()
	require.NotEmpty(saved)
	require.NoError(firstClient.TrashMessage(env.Context, "INBOX|1"))
	require.NoError(firstClient.Close())

	secondClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal("Trash|1", sourceMessageID)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Trash")
	assertMessageNotHasLabel(t, env.Store, sourceMessageID, "INBOX")
}

func TestIMAPUIDValidityReusePreservesOldAndArchivesNewMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	const oldRFC822ID = "uid-reuse-old@example.com"
	const newRFC822ID = "uid-reuse-new@example.com"
	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Archive", oldRFC822ID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	saved := firstClient.ObservedFolderStates()
	require.NotEmpty(saved)
	require.NoError(firstClient.Close())

	require.NoError(user.Delete("Archive"))
	require.NoError(user.Create("Archive", nil))
	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Archive", newRFC822ID)

	secondClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(1)),
		Updated: new(int64(0)),
		Skipped: new(int64(0)),
	})
	saved = secondClient.ObservedFolderStates()
	require.NotEmpty(saved)
	require.NoError(secondClient.Close())

	type archivedIdentity struct {
		id              int64
		sourceMessageID string
	}
	identities := make(map[string]archivedIdentity)
	rows, err := env.Store.DB().Query(
		`SELECT id, source_message_id, rfc822_message_id FROM messages`,
	)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var id int64
		var sourceMessageID string
		var rfc822MessageID sql.NullString
		require.NoError(rows.Scan(
			&id, &sourceMessageID, &rfc822MessageID))
		identities[strings.Trim(rfc822MessageID.String, "<>")] =
			archivedIdentity{
				id:              id,
				sourceMessageID: sourceMessageID,
			}
	}
	require.NoError(rows.Err())
	require.Len(identities, 2)

	oldIdentity := identities[oldRFC822ID]
	newIdentity := identities[newRFC822ID]
	assert.True(strings.HasPrefix(
		oldIdentity.sourceMessageID,
		"msgvault-invalidated:",
	))
	assert.Equal("Archive|1", newIdentity.sourceMessageID)

	oldRaw, err := env.Store.GetMessageRaw(oldIdentity.id)
	require.NoError(err)
	assert.Contains(string(oldRaw), "Message-ID: <"+oldRFC822ID+">")
	newRaw, err := env.Store.GetMessageRaw(newIdentity.id)
	require.NoError(err)
	assert.Contains(string(newRaw), "Message-ID: <"+newRFC822ID+">")

	thirdClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(thirdClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
	})

	var currentSourceMessageID string
	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages
		 WHERE rfc822_message_id = ?`,
		"<"+newRFC822ID+">",
	).Scan(&currentSourceMessageID)
	require.NoError(err)
	assert.Equal("Archive|1", currentSourceMessageID)
}

func TestIMAPNoResumeUIDValidityReuseArchivesReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.NoResume = true

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	const oldRFC822ID = "noresume-uid-reuse-old@example.com"
	const newRFC822ID = "noresume-uid-reuse-new@example.com"
	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Archive", oldRFC822ID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	require.NoError(user.Delete("Archive"))
	require.NoError(user.Create("Archive", nil))
	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Archive", newRFC822ID)

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(1)),
		Updated: new(int64(0)),
		Skipped: new(int64(0)),
	})
	saved := secondClient.ObservedFolderStates()
	require.NotEmpty(saved)
	require.NoError(secondClient.Close())

	type archivedIdentity struct {
		id              int64
		sourceMessageID string
	}
	identities := make(map[string]archivedIdentity)
	rows, err := env.Store.DB().Query(
		`SELECT id, source_message_id, rfc822_message_id FROM messages`,
	)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var id int64
		var sourceMessageID string
		var rfc822MessageID sql.NullString
		require.NoError(rows.Scan(
			&id, &sourceMessageID, &rfc822MessageID))
		identities[strings.Trim(rfc822MessageID.String, "<>")] =
			archivedIdentity{
				id:              id,
				sourceMessageID: sourceMessageID,
			}
	}
	require.NoError(rows.Err())
	require.Len(identities, 2)

	oldIdentity := identities[oldRFC822ID]
	newIdentity := identities[newRFC822ID]
	assert.True(strings.HasPrefix(
		oldIdentity.sourceMessageID,
		invalidatedIMAPSourceIDPrefix,
	))
	assert.Equal("Archive|1", newIdentity.sourceMessageID)

	oldRaw, err := env.Store.GetMessageRaw(oldIdentity.id)
	require.NoError(err)
	assert.Contains(string(oldRaw), "Message-ID: <"+oldRFC822ID+">")
	newRaw, err := env.Store.GetMessageRaw(newIdentity.id)
	require.NoError(err)
	assert.Contains(string(newRaw), "Message-ID: <"+newRFC822ID+">")

	opts.NoResume = false
	thirdClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(thirdClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
		Skipped: new(int64(1)),
	})
}

func TestIMAPForcedUIDReuseWithoutMessageIDArchivesReplacement(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.NoResume = true

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	testutil.AppendIMAPMessageWithoutMessageID(
		t, user, "Archive", "old missing-ID body")

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	saved := firstClient.ObservedFolderStates()
	require.NotEmpty(saved)
	require.NoError(firstClient.Close())

	require.NoError(user.Delete("Archive"))
	require.NoError(user.Create("Archive", nil))
	testutil.AppendIMAPMessageWithoutMessageID(
		t, user, "Archive", "replacement missing-ID body")

	secondClient := newSyncTestIMAPClient(
		t,
		addr,
		imapclient.WithFolderStates(saved),
		imapclient.WithForceFullEnumeration(),
	)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(1)),
		Updated: new(int64(0)),
		Skipped: new(int64(0)),
	})
	assertMissingIDArchiveState(
		t,
		env.Store,
		"old missing-ID body",
		"replacement missing-ID body",
	)
}

func TestIMAPMissingIdentityRawComparisonKeepsEqualMessage(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	testutil.AppendIMAPMessageWithoutMessageID(
		t, user, "Archive", "unchanged missing-ID body")

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
		Skipped: new(int64(1)),
	})
	assert.Positive(t, summary.BytesDownloaded,
		"inconclusive identity should fetch raw MIME for comparison")
	assertMessageCount(t, env.Store, 1)
}

func TestIMAPAsymmetricMessageIDUsesRawComparison(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	const messageID = "asymmetric-identity@example.com"
	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Archive", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	_, err := env.Store.DB().Exec(
		`UPDATE messages SET rfc822_message_id = NULL`,
	)
	require.NoError(err)

	for range 2 {
		client := newSyncTestIMAPClient(t, addr)
		env.Syncer = New(client, env.Store, opts)
		summary = runFullSync(t, env)
		assertSummary(t, summary, WantSummary{
			Added:   new(int64(0)),
			Updated: new(int64(0)),
			Skipped: new(int64(1)),
		})
		assert.Positive(t, summary.BytesDownloaded,
			"inconclusive identity should fetch raw MIME for comparison")
		require.NoError(client.Close())
		assertMessageCount(t, env.Store, 1)
	}
}

func TestIMAPMissingIdentityRawComparisonArchivesDifferentMessage(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	testutil.AppendIMAPMessageWithoutMessageID(
		t, user, "Archive", "old unknown-epoch body")

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	require.NoError(user.Delete("Archive"))
	require.NoError(user.Create("Archive", nil))
	testutil.AppendIMAPMessageWithoutMessageID(
		t, user, "Archive", "replacement unknown-epoch body")

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(1)),
		Updated: new(int64(0)),
		Skipped: new(int64(0)),
	})
	assertMissingIDArchiveState(
		t,
		env.Store,
		"old unknown-epoch body",
		"replacement unknown-epoch body",
	)
}

func assertMissingIDArchiveState(
	t *testing.T,
	st *store.Store,
	oldBody string,
	newBody string,
) {
	t.Helper()
	require := require.New(t)

	type archivedMessage struct {
		id              int64
		sourceMessageID string
		raw             string
	}
	var archived []archivedMessage
	rows, err := st.DB().Query(`SELECT id, source_message_id FROM messages`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var msg archivedMessage
		require.NoError(rows.Scan(&msg.id, &msg.sourceMessageID))
		raw, err := st.GetMessageRaw(msg.id)
		require.NoError(err)
		msg.raw = string(raw)
		archived = append(archived, msg)
	}
	require.NoError(rows.Err())
	require.Len(archived, 2)

	var oldMessage, newMessage *archivedMessage
	for i := range archived {
		switch {
		case strings.Contains(archived[i].raw, oldBody):
			oldMessage = &archived[i]
		case strings.Contains(archived[i].raw, newBody):
			newMessage = &archived[i]
		}
	}
	require.NotNil(oldMessage)
	require.NotNil(newMessage)
	assert.True(t, strings.HasPrefix(
		oldMessage.sourceMessageID,
		invalidatedIMAPSourceIDPrefix,
	))
	assert.Equal(t, "Archive|1", newMessage.sourceMessageID)
}

func TestIMAPFilteredMoveReconcilesBeforeAdvancingFolderState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"INBOX": 0,
		"Trash": 0,
	})
	const messageID = "filtered-high-water-move@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	saved := firstClient.ObservedFolderStates()
	require.NotEmpty(saved)
	trashBefore := saved["Trash"]
	require.NoError(firstClient.TrashMessage(env.Context, "INBOX|1"))
	require.NoError(firstClient.Close())

	secondClient := newSyncTestIMAPClient(
		t,
		addr,
		imapclient.WithFolderStates(saved),
		imapclient.WithFolderFilter([]string{"Trash"}, nil),
		imapclient.WithFolderStateSave(
			func(mailbox string, state imapclient.FolderState) {
				saved[mailbox] = state
			},
		),
	)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})
	require.NoError(secondClient.Close())

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal("INBOX|1", sourceMessageID)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "INBOX")
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Trash")
	assert.Equal(trashBefore, saved["Trash"])

	thirdClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(thirdClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})

	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal("Trash|1", sourceMessageID)
}

func TestIMAPUnsupportedQresyncRenameUsesFullFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive": 0,
		"INBOX":   0,
	})
	const messageID = "high-water-mailbox-rename@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	saved := firstClient.ObservedFolderStates()
	require.NotEmpty(saved)
	require.NoError(firstClient.Close())
	require.NoError(user.Rename("Archive", "Projects", nil))

	acknowledged := make(map[string]imapclient.FolderState)
	secondClient := newSyncTestIMAPClient(
		t,
		addr,
		imapclient.WithFolderStates(saved),
		imapclient.WithFolderStateSave(
			func(mailbox string, state imapclient.FolderState) {
				acknowledged[mailbox] = state
			},
		),
	)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
		Errors:  new(int64(0)),
	})

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal("Projects|1", sourceMessageID)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Projects")
	assertMessageNotHasLabel(t, env.Store, sourceMessageID, "Archive")
	assert.Contains(acknowledged, "Projects")
}

func TestIMAPHighWaterOverlapPreservesValidID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive":  0,
		"INBOX":    0,
		"Projects": 0,
	})
	const messageID = "high-water-overlap@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	saved := firstClient.ObservedFolderStates()
	require.NotEmpty(saved)

	var originalSourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&originalSourceMessageID)
	require.NoError(err)
	require.NoError(firstClient.Close())

	testutil.AppendIMAPMessageWithMessageID(t, user, "Projects", messageID)
	secondClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})

	var rescannedSourceMessageID string
	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&rescannedSourceMessageID)
	require.NoError(err)
	assert.Equal(originalSourceMessageID, rescannedSourceMessageID)
	assertMessageHasLabel(t, env.Store, rescannedSourceMessageID, "Archive")
	assertMessageHasLabel(t, env.Store, rescannedSourceMessageID, "INBOX")
	assertMessageHasLabel(t, env.Store, rescannedSourceMessageID, "Projects")
}

func TestIMAPCompleteCrossPageOverlapPreservesValidID(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Archive":  0,
		"INBOX":    0,
		"Projects": 0,
	})
	const messageID = "complete-cross-page-overlap@example.com"
	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Projects", messageID)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	var originalSourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages
		 WHERE rfc822_message_id = ?`,
		"<"+messageID+">",
	).Scan(&originalSourceMessageID)
	require.NoError(err)
	require.Equal("Projects|1", originalSourceMessageID)

	testutil.AppendIMAPMessageWithMessageID(
		t, user, "Archive", messageID)
	for i := range 99 {
		testutil.AppendIMAPMessageWithMessageID(
			t,
			user,
			"Archive",
			fmt.Sprintf(
				"complete-page-filler-%03d@example.com", i),
		)
	}

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(99)),
		Updated: new(int64(1)),
		Skipped: new(int64(1)),
	})

	var rescannedSourceMessageID string
	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages
		 WHERE rfc822_message_id = ?`,
		"<"+messageID+">",
	).Scan(&rescannedSourceMessageID)
	require.NoError(err)
	assert.Equal(t, originalSourceMessageID, rescannedSourceMessageID)
	assertMessageHasLabel(
		t, env.Store, rescannedSourceMessageID, "Archive")
	assertMessageHasLabel(
		t, env.Store, rescannedSourceMessageID, "Projects")
}

type sourceValidationAPI struct {
	*gmail.MockAPI

	sourceMatches bool
}

func (a *sourceValidationAPI) SourceMessageMatches(
	context.Context, string, string,
) (bool, bool, error) {
	return a.sourceMatches, true, nil
}

type highWaterValidationAPI struct {
	*gmail.MockAPI

	validationErr error
	acknowledged  []string
}

func (*highWaterValidationAPI) LabelsSnapshotComplete() bool {
	return false
}

func (*highWaterValidationAPI) LabelsSnapshotFiltered() bool {
	return false
}

func (a *highWaterValidationAPI) SourceMessageMatches(
	context.Context, string, string,
) (bool, bool, error) {
	return false, true, a.validationErr
}

func (a *highWaterValidationAPI) AcknowledgeMessages(
	_ context.Context, messageIDs []string,
) {
	a.acknowledged = append(a.acknowledged, messageIDs...)
}

func TestIMAPHighWaterValidationFailureRemainsRetryable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	env.Syncer = New(env.Mock, env.Store, opts)

	msg := testemail.NewMessage().
		Subject("High-water validation failure").
		Header("Message-ID", "<high-water-validation@example.com>").
		Body("The old source ID cannot be validated yet.").
		Bytes()
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage("INBOX|42", msg, []string{"INBOX"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})

	delete(env.Mock.Messages, "INBOX|42")
	env.Mock.AddMessage("TRASH|99", msg, []string{"TRASH"})
	validationErr := errors.New("synthetic validation timeout")
	highWaterAPI := &highWaterValidationAPI{
		MockAPI:       env.Mock,
		validationErr: validationErr,
	}
	env.Syncer = New(highWaterAPI, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
		Errors:  new(int64(1)),
	})

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal("INBOX|42", sourceMessageID)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "INBOX")
	assertMessageNotHasLabel(t, env.Store, sourceMessageID, "TRASH")
	assert.NotContains(highWaterAPI.acknowledged, "TRASH|99")
}

type immediateLabelIMAPClient struct {
	*imapclient.Client
}

func (*immediateLabelIMAPClient) DefersAuthoritativeLabelReconciliation() bool {
	return false
}

func newSyncTestIMAPClient(
	t *testing.T, addr string, clientOpts ...imapclient.Option,
) *immediateLabelIMAPClient {
	t.Helper()
	host, portString, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)

	client := &immediateLabelIMAPClient{Client: imapclient.NewClient(&imapclient.Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, clientOpts...)}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type incompleteLabelSnapshotAPI struct {
	*gmail.MockAPI

	sourceMatches bool
}

func (*incompleteLabelSnapshotAPI) LabelsSnapshotComplete() bool {
	return false
}

func (*incompleteLabelSnapshotAPI) DefersAuthoritativeLabelReconciliation() bool {
	return true
}

func (*incompleteLabelSnapshotAPI) LabelsSnapshotFiltered() bool {
	return true
}

func (a *incompleteLabelSnapshotAPI) SourceMessageMatches(
	context.Context, string, string,
) (bool, bool, error) {
	return a.sourceMatches, true, nil
}

type labelMetadataSnapshotAPI struct {
	*gmail.MockAPI

	complete    bool
	deferLabels bool
	filtered    bool
	labelCalls  [][]string
	labelErrors map[string]error
	seedCalls   [][2]string
}

func (a *labelMetadataSnapshotAPI) LabelsSnapshotComplete() bool {
	return a.complete
}

func (a *labelMetadataSnapshotAPI) DefersAuthoritativeLabelReconciliation() bool {
	return a.deferLabels
}

func (a *labelMetadataSnapshotAPI) LabelsSnapshotFiltered() bool {
	return a.filtered
}

func (*labelMetadataSnapshotAPI) FetchedSourceMessageMatches(
	string, string, string,
) (bool, bool, error) {
	return true, true, nil
}

func (a *labelMetadataSnapshotAPI) SeedValidatedMessageDedup(
	messageID, rfc822MessageID string,
) error {
	a.seedCalls = append(
		a.seedCalls,
		[2]string{messageID, rfc822MessageID},
	)
	return nil
}

func (a *labelMetadataSnapshotAPI) GetMessageLabelsBatch(_ context.Context, messageIDs []string) ([]gmail.MessageLabelsBatchResult, error) {
	a.labelCalls = append(a.labelCalls, append([]string(nil), messageIDs...))

	results := make([]gmail.MessageLabelsBatchResult, len(messageIDs))
	for i, id := range messageIDs {
		results[i].ID = id
		if err := a.labelErrors[id]; err != nil {
			results[i].Err = err
			continue
		}
		msg, ok := a.Messages[id]
		if !ok {
			results[i].Err = &gmail.NotFoundError{Path: "/messages/" + id}
			continue
		}
		results[i].LabelIDs = append([]string(nil), msg.LabelIDs...)
	}
	return results, nil
}

func TestIMAPFilteredRescanPreservesCanonicalIDAndMergesLabels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	env.Syncer = New(env.Mock, env.Store, opts)
	env.Mock.Labels = []*gmail.Label{
		{ID: "[Gmail]/All Mail", Name: "All Mail", Type: labelTypeSystem},
		{ID: "Archive", Name: "Archive", Type: "user"},
		{ID: "INBOX", Name: "INBOX", Type: labelTypeSystem},
	}

	msg := testemail.NewMessage().
		Subject("Filtered rescan").
		Header("Message-ID", "<filtered-rescan@example.com>").
		Body("Same message through a partial mailbox view.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage("[Gmail]/All Mail|42", msg, []string{"[Gmail]/All Mail", "Archive"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})

	delete(env.Mock.Messages, "[Gmail]/All Mail|42")
	env.Mock.AddMessage("INBOX|7", msg, []string{"INBOX"})
	env.Syncer = New(&incompleteLabelSnapshotAPI{
		MockAPI:       env.Mock,
		sourceMatches: true,
	}, env.Store, opts)

	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})

	var sourceMessageID string
	err := env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&sourceMessageID)
	require.NoError(err)
	assert.Equal("[Gmail]/All Mail|42", sourceMessageID,
		"a filtered rescan must preserve the canonical source_message_id")
	assertMessageHasLabel(t, env.Store, sourceMessageID, "[Gmail]/All Mail")
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Archive")
	assertMessageHasLabel(t, env.Store, sourceMessageID, "INBOX")
}

func TestIMAPFilteredRescanExactIDMergesNewLabels(t *testing.T) {
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	env.Syncer = New(env.Mock, env.Store, opts)
	env.Mock.Labels = []*gmail.Label{
		{ID: "[Gmail]/All Mail", Name: "All Mail", Type: labelTypeSystem},
		{ID: "Archive", Name: "Archive", Type: "user"},
	}

	const sourceMessageID = "[Gmail]/All Mail|42"
	msg := testemail.NewMessage().
		Subject("Exact-ID filtered rescan").
		Header("Message-ID", "<exact-id-filtered-rescan@example.com>").
		Body("Same composite ID through a partial mailbox view.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage(sourceMessageID, msg, []string{"[Gmail]/All Mail"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertMessageNotHasLabel(t, env.Store, sourceMessageID, "Archive")

	env.Mock.Messages[sourceMessageID].LabelIDs = []string{"[Gmail]/All Mail", "Archive"}
	callsBeforeRescan := len(env.Mock.GetMessageCalls)
	filteredAPI := &labelMetadataSnapshotAPI{
		MockAPI:  env.Mock,
		filtered: true,
	}
	env.Syncer = New(filteredAPI, env.Store, opts)

	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})
	assert.Len(t, env.Mock.GetMessageCalls, callsBeforeRescan,
		"existing messages should not download raw MIME to refresh labels")
	assert.Equal(t, [][]string{{sourceMessageID}}, filteredAPI.labelCalls)
	assert.Equal(t, [][2]string{{sourceMessageID, ""}}, filteredAPI.seedCalls)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "[Gmail]/All Mail")
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Archive")
}

func TestIMAPCompleteRescanReplacesExactIDLabels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	env.Syncer = New(env.Mock, env.Store, opts)
	env.Mock.Labels = []*gmail.Label{
		{ID: "[Gmail]/All Mail", Name: "All Mail", Type: labelTypeSystem},
		{ID: "Archive", Name: "Archive", Type: "user"},
	}

	const sourceMessageID = "[Gmail]/All Mail|42"
	msg := testemail.NewMessage().
		Subject("Complete exact-ID rescan").
		Header("Message-ID", "<complete-exact-id-rescan@example.com>").
		Body("Labels become authoritative again after an unfiltered rescan.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage(sourceMessageID, msg, []string{"[Gmail]/All Mail"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})

	env.Mock.Messages[sourceMessageID].LabelIDs = []string{"[Gmail]/All Mail", "Archive"}
	filteredAPI := &labelMetadataSnapshotAPI{
		MockAPI:  env.Mock,
		filtered: true,
	}
	env.Syncer = New(filteredAPI, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})
	assertMessageHasLabel(t, env.Store, sourceMessageID, "Archive")

	env.Mock.Messages[sourceMessageID].LabelIDs = []string{"[Gmail]/All Mail"}
	completeAPI := &labelMetadataSnapshotAPI{MockAPI: env.Mock, complete: true}
	env.Syncer = New(completeAPI, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})
	assert.Equal([][]string{{sourceMessageID}}, completeAPI.labelCalls)
	assertMessageHasLabel(t, env.Store, sourceMessageID, "[Gmail]/All Mail")
	assertMessageNotHasLabel(t, env.Store, sourceMessageID, "Archive")

	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err)
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(int64(1), run.MessagesUpdated)

	completeAPI.labelCalls = nil
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
	})
	assert.Equal([][]string{{sourceMessageID}}, completeAPI.labelCalls)
}

func TestIMAPCompleteLimitedRescanReconcilesProcessedExistingLabels(t *testing.T) {
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	env.Syncer = New(env.Mock, env.Store, opts)
	env.Mock.Labels = []*gmail.Label{
		{ID: "[Gmail]/All Mail", Name: "All Mail", Type: labelTypeSystem},
		{ID: "Archive", Name: "Archive", Type: "user"},
	}

	const processedID = "[Gmail]/All Mail|41"
	const truncatedID = "[Gmail]/All Mail|42"
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.MessagePages = [][]string{{processedID, truncatedID}}
	env.Mock.AddMessage(processedID, testMIME(), []string{"[Gmail]/All Mail", "Archive"})
	env.Mock.AddMessage(truncatedID, testMIME(), []string{"[Gmail]/All Mail", "Archive"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
	assertMessageHasLabel(t, env.Store, processedID, "Archive")

	env.Mock.Messages[processedID].LabelIDs = []string{"[Gmail]/All Mail"}
	limitedOpts := DefaultOptions()
	limitedOpts.SourceType = sourceTypeIMAP
	limitedOpts.Limit = 1
	completeAPI := &labelMetadataSnapshotAPI{
		MockAPI:     env.Mock,
		complete:    true,
		deferLabels: true,
	}
	env.Syncer = New(completeAPI, env.Store, limitedOpts)

	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Found:   new(int64(1)),
		Updated: new(int64(1)),
	})
	assert.Equal(t, [][]string{{processedID}}, completeAPI.labelCalls)
	assertMessageNotHasLabel(t, env.Store, processedID, "Archive")
	assertMessageHasLabel(t, env.Store, truncatedID, "Archive")
}

func TestIMAPLabelMetadataFailureDoesNotAbortBatch(t *testing.T) {
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	env.Syncer = New(env.Mock, env.Store, opts)
	env.Mock.Labels = []*gmail.Label{
		{ID: "[Gmail]/All Mail", Name: "All Mail", Type: labelTypeSystem},
		{ID: "Archive", Name: "Archive", Type: "user"},
	}

	const firstID = "[Gmail]/All Mail|41"
	const vanishedID = "[Gmail]/All Mail|42"
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.AddMessage(firstID, testMIME(), []string{"[Gmail]/All Mail"})
	env.Mock.AddMessage(vanishedID, testMIME(), []string{"[Gmail]/All Mail"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})

	env.Mock.Messages[firstID].LabelIDs = []string{"[Gmail]/All Mail", "Archive"}
	filteredAPI := &labelMetadataSnapshotAPI{
		MockAPI:  env.Mock,
		filtered: true,
		labelErrors: map[string]error{
			vanishedID: errors.New("UID vanished before metadata fetch"),
		},
	}
	env.Syncer = New(filteredAPI, env.Store, opts)

	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:  new(int64(0)),
		Errors: new(int64(1)),
	})
	assertMessageHasLabel(t, env.Store, firstID, "Archive")
	assertMessageNotHasLabel(t, env.Store, vanishedID, "Archive")
	assert.Equal(t, [][2]string{{firstID, ""}}, filteredAPI.seedCalls,
		"failed label metadata must not seed dedup state")
}

// TestIncrementalSyncLabelRemovedWithMissingRaw verifies that removing a label
// from a message whose raw MIME data is missing still succeeds. The label-removal
// path operates on the message_labels table directly and never touches raw data.
func TestIncrementalSyncLabelRemovedWithMissingRaw(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX", "STARRED"})

	runFullSync(t, env)

	// Verify starting state
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")
	assertRawDataExists(t, env.Store, "msg1")

	// Delete raw MIME data to simulate missing raw
	_, err := env.Store.DB().Exec(`
		DELETE FROM message_raw WHERE message_id = (
			SELECT id FROM messages WHERE source_message_id = 'msg1'
		)`)
	require.NoError(t, err, "delete raw data")

	// Record raw fetch count before incremental sync
	callsBeforeIncr := len(env.Mock.GetMessageCalls)

	// Now simulate label removal via incremental sync
	env.SetHistory(12350, historyLabelRemoved("msg1", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// No raw fetches should occur for label-only changes
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assert.Equal(t, callsBeforeIncr, callsAfterIncr, "expected 0 GetMessageRaw calls for label removal")

	// Label should be removed despite missing raw data
	assertMessageNotHasLabel(t, env.Store, "msg1", "STARRED")
	assertMessageHasLabel(t, env.Store, "msg1", "INBOX")
}
