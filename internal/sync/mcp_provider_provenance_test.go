package sync

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/gmail"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// These provenance controls explicitly model a granted native credential. The
// generic MockAPI remains unknown; real native HTTP/SDK tests verify admission.
type grantedGmailProvenanceAPI struct{ *gmail.MockAPI }

func (grantedGmailProvenanceAPI) NativeMessageReadPermission() (bool, bool) { return true, true }

type refreshingGmailGrantAPI struct {
	*gmail.MockAPI

	messageRead     bool
	narrowAfter     string
	batchError      error
	messageError    error
	labelBatchCalls int
}

func (a *refreshingGmailGrantAPI) NativeMessageReadPermission() (bool, bool) {
	return a.messageRead, true
}

func (a *refreshingGmailGrantAPI) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	labels, err := a.MockAPI.ListLabels(ctx)
	if err == nil && a.narrowAfter == "labels" {
		a.messageRead = false
	}
	return labels, err
}

func (a *refreshingGmailGrantAPI) ListHistory(ctx context.Context, historyID uint64, pageToken string) (*gmail.HistoryResponse, error) {
	response, err := a.MockAPI.ListHistory(ctx, historyID, pageToken)
	if err == nil && a.narrowAfter == "history" {
		a.messageRead = false
	}
	return response, err
}

func (a *refreshingGmailGrantAPI) GetMessageLabelsBatch(_ context.Context, ids []string) ([]gmail.MessageLabelsBatchResult, error) {
	a.labelBatchCalls++
	if a.batchError != nil {
		return nil, a.batchError
	}
	results := make([]gmail.MessageLabelsBatchResult, len(ids))
	for i, id := range ids {
		results[i].ID = id
		if a.messageError != nil {
			results[i].Err = a.messageError
			continue
		}
		message, ok := a.Messages[id]
		if !ok {
			results[i].Err = &gmail.NotFoundError{Path: "/messages/" + id}
			continue
		}
		results[i].LabelIDs = append([]string(nil), message.LabelIDs...)
		results[i].HistoryID = message.HistoryID
	}
	if a.narrowAfter == "metadata" {
		a.messageRead = false
	}
	return results, nil
}

func newGmailMetadataPermissionEnv(t *testing.T) (*TestEnv, *store.Source, *refreshingGmailGrantAPI) {
	t.Helper()
	require := Require.New(t)

	env := newTestEnv(t)
	seedMessages(env, 1, 1000, "existing-message")
	source := env.CreateSourceWithHistory(t, "1000")
	api := &refreshingGmailGrantAPI{MockAPI: env.Mock, messageRead: true}
	env.Syncer = New(api, env.Store, nil)
	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled:   true,
		Principal: "synthetic-owner",
		Capabilities: []store.MCPEventCapability{{
			Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"},
		}},
	})
	require.NoError(err)
	_, err = env.Syncer.Full(env.Context, testEmail)
	require.NoError(err, "initial full sync with a granted native credential")
	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	require.True(allowed, "initial full sync stores the current message-read grant")
	return env, source, api
}

func enableProviderEvents(t *testing.T, env *TestEnv, sourceType string) {
	t.Helper()
	require := Require.New(t)
	if sourceType == "gmail" {
		scoped := *env.Syncer
		scoped.client = grantedGmailProvenanceAPI{env.Mock}
		env.Syncer = &scoped
	}

	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: sourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
}

func providerEventCount(t *testing.T, env *TestEnv) int {
	t.Helper()
	require := Require.New(t)

	var count int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family = 'msgvault.message_archived'`).Scan(&count))
	return count
}

func TestMCPProviderGmailRefreshNarrowsMessageReadPermission(t *testing.T) {
	for _, tc := range []struct {
		name            string
		refreshEndpoint string
		finalHistoryID  uint64
		wantHistoryCall int
	}{
		{name: "idle labels refresh", refreshEndpoint: "labels", finalHistoryID: 1000},
		{name: "history refresh", refreshEndpoint: "history", finalHistoryID: 1001, wantHistoryCall: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "1000")
			api := &refreshingGmailGrantAPI{MockAPI: env.Mock, messageRead: true}
			scoped := *env.Syncer
			scoped.client = api
			env.Syncer = &scoped

			_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
				Enabled:   true,
				Principal: "synthetic-owner",
				Capabilities: []store.MCPEventCapability{{
					Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"},
				}},
			})
			require.NoError(err)

			_, err = env.Syncer.Incremental(env.Context, source)
			require.NoError(err, "initial granted sync")
			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			require.True(allowed, "initial sync stores the current message-read grant")

			api.narrowAfter = tc.refreshEndpoint
			env.SetHistory(tc.finalHistoryID)
			summary, err := env.Syncer.Incremental(env.Context, source)
			require.NoError(err, "a successful refresh-capable request may still complete the archive sync")
			require.NotNil(summary)
			assert.Equal(tc.finalHistoryID, summary.FinalHistoryID)
			assert.Len(env.Mock.HistoryCalls, tc.wantHistoryCall)

			allowed, err = env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a successful request that narrows the saved grant must revoke stored Events authority")
		})
	}
}

func TestMCPProviderGmailMetadataBatchRefreshNarrowsMessageReadPermission(t *testing.T) {
	for _, syncKind := range []string{"full", "incremental"} {
		t.Run(syncKind, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			env, source, api := newGmailMetadataPermissionEnv(t)
			api.narrowAfter = "metadata"

			var err error
			if syncKind == "incremental" {
				env.Mock.Messages["existing-message"].LabelIDs = []string{"INBOX", "STARRED"}
				env.Mock.Messages["existing-message"].HistoryID = 1001
				env.SetHistory(1001, historyLabelAdded("existing-message", "STARRED"))
				current, getErr := env.Store.GetOrCreateSource("gmail", testEmail)
				require.NoError(getErr)
				_, err = env.Syncer.Incremental(env.Context, current)
			} else {
				_, err = env.Syncer.Full(env.Context, testEmail)
			}
			require.NoError(err)
			assert.Equal(1, api.labelBatchCalls)
			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a successful metadata refresh that narrows the saved grant must revoke Events authority")
		})
	}
}

func TestMCPProviderGmailMetadataBatchFailuresRevokeMessageReadPermission(t *testing.T) {
	for _, tc := range []struct {
		name     string
		syncKind string
		perItem  bool
	}{
		{name: "full batch error", syncKind: "full"},
		{name: "full message error", syncKind: "full", perItem: true},
		{name: "incremental batch error", syncKind: "incremental"},
		{name: "incremental message error", syncKind: "incremental", perItem: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			env, source, api := newGmailMetadataPermissionEnv(t)
			statusErr := &gmail.StatusError{StatusCode: http.StatusForbidden}
			if tc.perItem {
				api.messageError = statusErr
			} else {
				api.batchError = statusErr
			}

			var err error
			if tc.syncKind == "incremental" {
				env.Mock.Messages["existing-message"].HistoryID = 1001
				env.SetHistory(1001, historyLabelAdded("existing-message", "STARRED"))
				current, getErr := env.Store.GetOrCreateSource("gmail", testEmail)
				require.NoError(getErr)
				_, err = env.Syncer.Incremental(env.Context, current)
			} else {
				_, err = env.Syncer.Full(env.Context, testEmail)
			}
			assert.Equal(1, api.labelBatchCalls)
			if tc.syncKind == "incremental" {
				assert.Error(err, "failed history metadata refresh retains the source cursor")
			}
			allowed, permissionErr := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(permissionErr)
			assert.False(allowed, "an observed 403 during metadata refresh must revoke Events authority")
		})
	}
}

func TestMCPProviderGmailFullMutedIncrementalLive(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	env := newTestEnv(t)
	enableProviderEvents(t, env, "gmail")
	seedMessages(env, 1, 1000, "historical")
	runFullSync(t, env)
	assert.Zero(providerEventCount(t, env))
	env.Mock.AddMessage("live-delta", testemail.NewMessage().From("sender@example.test").To("receiver@example.test").Body("Live synthetic body").Bytes(), []string{"INBOX"})
	env.SetHistory(1001, historyAdded("live-delta"))
	runIncrementalSync(t, env)
	require.Equal(1, providerEventCount(t, env), "exactly one event is required before reading its archived identity")
	var providerID string
	require.NoError(env.Store.DB().QueryRow(`SELECT source_message_id FROM messages WHERE id = (SELECT message_id FROM mcp_event_log WHERE family = 'msgvault.message_archived')`).Scan(&providerID))
	assert.Equal("live-delta", providerID)
	assertBodyContains(t, env.Store, "live-delta", "Live synthetic body")
}

func TestMCPProviderGmailReplayDebtMutedWithinLiveRun(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")
	recordIncrementalSyncRunItems(t, env, source.ID, store.SyncRunItem{SourceMessageID: "old-fetch-debt", Phase: syncItemPhaseFetch, Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError})
	env.Mock.AddMessage("old-fetch-debt", testemail.NewMessage().Body("Recovered historical body").Bytes(), []string{"INBOX"})
	env.Mock.AddMessage("live-delta", testemail.NewMessage().Body("New live body").Bytes(), []string{"INBOX"})
	env.SetHistory(1001, historyAdded("live-delta"))
	enableProviderEvents(t, env, "gmail")
	runIncrementalSync(t, env)
	assertMessageCount(t, env.Store, 2)
	require.Equal(1, providerEventCount(t, env), "exactly one event is required before reading its archived identity")
	var providerID string
	require.NoError(env.Store.DB().QueryRow(`SELECT source_message_id FROM messages WHERE id = (SELECT message_id FROM mcp_event_log WHERE family = 'msgvault.message_archived')`).Scan(&providerID))
	assert.Equal("live-delta", providerID)
}

func TestMCPProviderGmailLabelChangeRecoveryMuted(t *testing.T) {
	assert := Assert.New(t)

	env := newTestEnv(t)
	enableProviderEvents(t, env, "gmail")
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 1000
	env.Mock.MessagePages = [][]string{{"historical", "missed-in-full"}}
	env.Mock.AddMessage("historical", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("missed-in-full", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["missed-in-full"] = errors.New("temporary fetch failure")
	runFullSync(t, env)
	assertMessageCount(t, env.Store, 1)
	delete(env.Mock.GetMessageError, "missed-in-full")
	env.SetHistory(1001, historyLabelAdded("missed-in-full", "STARRED"))

	runIncrementalSync(t, env)

	assertMessageCount(t, env.Store, 2)
	assert.Zero(providerEventCount(t, env), "a label change on an old message is not a live arrival")
}

func TestMCPProviderIMAPMailboxDeltaLiveRecoveryMuted(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	env := newTestEnv(t)
	enableProviderEvents(t, env, "imap")
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	route := (&imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}).Identifier()
	runNative := func() { _, err := env.Syncer.Full(env.Context, route); require.NoError(err) }
	appendMessage := func(id string) {
		testutil.AppendIMAPRawMessage(t, user, "INBOX", testemail.NewMessage().Header("Message-ID", "<"+id+"@example.test>").Body(id+" body").CRLF().Bytes())
	}
	appendMessage("historical")
	first := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(first.Client, env.Store, opts)
	runNative()
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())
	assert.Zero(providerEventCount(t, env))
	appendMessage("live-delta")
	second := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(second.Client, env.Store, opts)
	runNative()
	assert.Equal(store.IngestLive, second.MessageIngestContext("INBOX|2").Mode, "saved=%+v deltas=%+v", saved, second.ObservedMailboxDeltas())
	assert.Equal(1, providerEventCount(t, env))
	saved = second.ObservedFolderStates()
	require.NoError(second.Close())
	appendMessage("recovery-found")
	third := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStates(saved), imapclient.WithForceFullEnumeration())
	env.Syncer = New(third.Client, env.Store, opts)
	runNative()
	assert.Equal(1, providerEventCount(t, env), "forced recovery discovers historical rows without live attribution")
	assertMessageCount(t, env.Store, 3)
}
