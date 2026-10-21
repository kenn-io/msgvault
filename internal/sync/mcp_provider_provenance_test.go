package sync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"golang.org/x/oauth2"
)

// These provenance controls explicitly model a granted native credential. The
// generic MockAPI remains unknown; real native HTTP/SDK tests verify admission.
type grantedGmailProvenanceAPI struct{ *gmail.MockAPI }

func (grantedGmailProvenanceAPI) NativeMessageReadPermission() (bool, bool) { return true, true }

type refreshOnProfileGmailAPI struct {
	gmail.API

	permission   nativeMessageReadPermission
	refresh      func(context.Context) error
	profileEmail string
}

func (c refreshOnProfileGmailAPI) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return &gmail.Profile{EmailAddress: c.profileEmail}, nil
}

func (c refreshOnProfileGmailAPI) NativeMessageReadPermission() (bool, bool) {
	return c.permission.NativeMessageReadPermission()
}

func TestGmailRefreshScopeReductionRevokesEventsAuthority(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	email := source.Identifier

	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{
			Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"},
		}},
	})
	require.NoError(err)

	var refreshCalls atomic.Int32
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"narrowed-access","refresh_token":"narrowed-refresh","token_type":"Bearer","expires_in":3600,"scope":%q}`, oauth.ScopeGmailMetadata)
	}))
	t.Cleanup(tokenEndpoint.Close)
	clientSecrets := filepath.Join(t.TempDir(), "oauth-client.json")
	secrets := fmt.Sprintf(`{"installed":{"client_id":"synthetic-client","client_secret":"synthetic-secret","auth_uri":"https://accounts.example.net/o/oauth2/auth","token_uri":%q,"redirect_uris":["http://localhost"]}}`, tokenEndpoint.URL)
	require.NoError(os.WriteFile(clientSecrets, []byte(secrets), 0o600))
	tokensDir := t.TempDir()
	manager, err := oauth.NewManagerWithCredentials(t.Context(), config.OAuthApp{ClientSecrets: clientSecrets}, tokensDir, config.OAuthTokenCommands{}, nil, []string{oauth.ScopeGmailReadonly})
	require.NoError(err)
	tokenJSON := fmt.Sprintf(`{"access_token":"old-access","token_type":"Bearer","refresh_token":"old-refresh","expiry":%q,"scopes":[%q],"client_id":"synthetic-client"}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), oauth.ScopeGmailReadonly)
	require.NoError(os.WriteFile(manager.TokenPath(email), []byte(tokenJSON), 0o600))
	assert.Equal([]string{oauth.ScopeGmailReadonly}, manager.GrantedScopes(t.Context(), email))

	initial, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	permission := store.NativeSourcePermission{SourceID: source.ID, SourceReference: initial.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
	require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), permission))
	_, binding, err := env.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, initial.Reference)
	require.NoError(err)

	client := gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused"}), gmail.WithGrantedScopesReader(func() []string {
		return manager.GrantedScopes(t.Context(), email)
	}))
	refreshing := refreshOnProfileGmailAPI{
		API: client, permission: client,
		refresh:      func(ctx context.Context) error { return manager.ForceRefresh(ctx, email) },
		profileEmail: email,
	}
	worker := New(refreshing, env.Store.ScopedToSync(source.ID, startSyncRun(t, env, source.ID)), nil)
	profile, err := worker.nativeProfileWithPermission(t.Context(), source)
	require.NoError(err)
	assert.Equal(email, profile.EmailAddress)
	assert.Equal(int32(1), refreshCalls.Load(), "profile verification must exercise the real refresh-token endpoint")
	assert.Equal([]string{oauth.ScopeGmailMetadata}, manager.GrantedScopes(t.Context(), email), "explicit refresh scope replaces the previous grant")

	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "metadata-only refresh must retire message Events authority")
	current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.NotEqual(initial.Reference, current.Reference, "scope reduction retires the old source lifetime")
	require.Error(env.Store.CheckMCPResourceBinding(t.Context(), binding), "retained Events binding must not outlive body-read permission")
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

// The fake replaces provider IO; permission persistence and both sync paths stay real.
type deniedAfterProfileGmailAPI struct {
	grantedGmailProvenanceAPI

	operation    string
	denial       error
	beforeDenial func()
	batchWide    bool
}

func (c deniedAfterProfileGmailAPI) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	if c.operation == "labels" {
		c.beforeDenial()
		return nil, c.denial
	}
	return c.MockAPI.ListLabels(ctx)
}
func (c deniedAfterProfileGmailAPI) ListHistory(ctx context.Context, start uint64, page string) (*gmail.HistoryResponse, error) {
	if c.operation == "history" {
		c.beforeDenial()
		return nil, c.denial
	}
	return c.MockAPI.ListHistory(ctx, start, page)
}
func (c deniedAfterProfileGmailAPI) ListMessages(ctx context.Context, query, page string) (*gmail.MessageListResponse, error) {
	if c.operation == "list" {
		c.beforeDenial()
		return nil, c.denial
	}
	return c.MockAPI.ListMessages(ctx, query, page)
}
func (c deniedAfterProfileGmailAPI) GetMessageRaw(ctx context.Context, id string) (*gmail.RawMessage, error) {
	if c.operation == "raw" {
		c.beforeDenial()
		return nil, c.denial
	}
	return c.MockAPI.GetMessageRaw(ctx, id)
}
func (c deniedAfterProfileGmailAPI) GetMessagesRawBatchWithErrors(ctx context.Context, ids []string) ([]gmail.RawMessageBatchResult, error) {
	if c.operation == "batch" {
		c.beforeDenial()
		if c.batchWide {
			return nil, c.denial
		}
		results := make([]gmail.RawMessageBatchResult, len(ids))
		for i, id := range ids {
			results[i] = gmail.RawMessageBatchResult{ID: id, Err: c.denial}
		}
		return results, nil
	}
	return c.MockAPI.GetMessagesRawBatchWithErrors(ctx, ids)
}
func testGmailDenialRetiresEventsAuthority(t *testing.T, operation string, batchWide bool) {
	t.Helper()
	env := newTestEnv(t)
	enableProviderEvents(t, env, "gmail")
	source := env.CreateSourceWithHistory(t, "1000")
	pinned, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	Require.NoError(t, err)
	env.SetHistory(1001, historyAdded("synthetic-denied-message"))
	observed := false
	var denial error = &gmail.StatusError{StatusCode: http.StatusForbidden}
	if operation == "labels" {
		denial = &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	}
	client := deniedAfterProfileGmailAPI{
		grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}, operation: operation,
		denial: denial, batchWide: batchWide,
		beforeDenial: func() {
			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			Require.NoError(t, err)
			Require.True(t, allowed, "startup establishes authority before the provider denies a later request")
			observed = true
		},
	}
	env.Syncer = New(client, env.Store, nil)
	if operation == "list" {
		_, err = env.Syncer.Full(t.Context(), testEmail)
	} else {
		_, err = env.Syncer.Incremental(t.Context(), source)
	}
	if operation != "batch" {
		Require.Error(t, err)
	} else {
		Require.ErrorIs(t, err, errNativeSourcePermissionReduction, "a reduced provider denial must stop incremental sync so history remains retryable")
	}
	Require.True(t, observed)
	allowed, readErr := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	Require.NoError(t, readErr)
	Assert.False(t, allowed, "the later provider denial must reduce authority before sync returns")
	current, readErr := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	Require.NoError(t, readErr)
	Assert.NotEqual(t, pinned.Reference, current.Reference, "denial retires the pinned account lifetime")
}
func TestIncrementalGmailHistoryPermissionDenialRetiresEventsAuthority(t *testing.T) {
	testGmailDenialRetiresEventsAuthority(t, "history", false)
}
func TestIncrementalGmailBatchFetchPermissionDenialRetiresEventsAuthority(t *testing.T) {
	t.Run("individual", func(t *testing.T) { testGmailDenialRetiresEventsAuthority(t, "batch", false) })
	t.Run("whole_batch", func(t *testing.T) { testGmailDenialRetiresEventsAuthority(t, "batch", true) })
}
func TestFullSyncGmailPermissionDenialRetiresEventsAuthority(t *testing.T) {
	testGmailDenialRetiresEventsAuthority(t, "list", false)
}

func TestGmailBatchFetchPermissionReductionFailureIsFatal(t *testing.T) {
	require, assert := Require.New(t), Assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	pin, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	replacementReference := ""
	client := deniedAfterProfileGmailAPI{
		grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}, operation: "batch",
		denial: &gmail.StatusError{StatusCode: http.StatusForbidden},
		beforeDenial: func() {
			// A different authority observer retires and replaces the source while
			// this run's provider request is in flight. Its old pin must not be adopted.
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: pin.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true}
			require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), observation))
			replacement, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			replacementReference = replacement.Reference
			observation.SourceReference, observation.MessageRead = replacement.Reference, true
			require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), observation))
		},
	}
	worker := New(client, env.Store.ScopedToSync(source.ID, startSyncRun(t, env, source.ID)), nil)
	_, err = worker.nativeProfileWithPermission(t.Context(), source)
	require.NoError(err)
	_, err = worker.getMessagesRawBatchWithDiagnostics(t.Context(), []string{"synthetic-stale-fetch"})
	require.ErrorContains(err, "resource_unavailable", "a failed permission reduction must abort the batch")
	allowed, readErr := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(readErr)
	assert.True(allowed, "a stale denial cannot retire the replacement authority")
	current, readErr := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(readErr)
	assert.Equal(replacementReference, current.Reference)
}

func TestIncrementalGmailPermissionReductionFailureStopsSync(t *testing.T) {
	for _, mode := range []string{"history", "replay", "label_change"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := Require.New(t), Assert.New(t)
			env := newTestEnv(t)
			enableProviderEvents(t, env, "gmail")
			source := env.CreateSourceWithHistory(t, "1000")
			pin, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			env.SetHistory(1001, historyAdded("synthetic-stale-fetch"))
			if mode == "replay" {
				env.SetHistory(1000)
				recordIncrementalSyncRunItems(t, env, source.ID, store.SyncRunItem{SourceMessageID: "synthetic-stale-fetch", Phase: syncItemPhaseFetch, Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError})
			}
			replacementReference := ""
			client := deniedAfterProfileGmailAPI{
				grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}, operation: "batch",
				denial: &gmail.StatusError{StatusCode: http.StatusForbidden},
				beforeDenial: func() {
					observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: pin.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true}
					require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), observation))
					replacement, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
					require.NoError(err)
					replacementReference = replacement.Reference
					observation.SourceReference, observation.MessageRead = replacement.Reference, true
					require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), observation))
				},
			}
			if mode == "label_change" {
				client.operation = "raw"
				env.SetHistory(1001, historyLabelAdded("synthetic-stale-fetch", "INBOX"))
			}
			_, err = New(client, env.Store, nil).Incremental(t.Context(), source)
			require.ErrorContains(err, "resource_unavailable", "a rejected authority write must fail the sync")
			allowed, readErr := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(readErr)
			assert.True(allowed, "the stale sync cannot revoke replacement authority")
			current, readErr := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(readErr)
			assert.Equal(replacementReference, current.Reference)
		})
	}
}

func TestIncrementalGmailLabelPermissionDenialRetiresEventsAuthority(t *testing.T) {
	testGmailDenialRetiresEventsAuthority(t, "labels", false)
}

type deniedIdentityValidationGmailAPI struct{ deniedAfterProfileGmailAPI }

func (c deniedIdentityValidationGmailAPI) GetMessagesRawBatchWithIdentityValidation(ctx context.Context, ids []string) ([]gmail.RawMessageBatchResult, error) {
	return c.GetMessagesRawBatchWithErrors(ctx, ids)
}
func TestGmailIdentityValidationPermissionDenialRetiresEventsAuthority(t *testing.T) {
	require, assert := Require.New(t), Assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	pin, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	client := deniedIdentityValidationGmailAPI{deniedAfterProfileGmailAPI{
		grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}, operation: "batch",
		denial: &gmail.StatusError{StatusCode: http.StatusForbidden},
		beforeDenial: func() {
			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			require.True(allowed, "startup permission succeeds before identity-validation fetch denial")
		},
	}}
	worker := New(client, env.Store.ScopedToSync(source.ID, startSyncRun(t, env, source.ID)), nil)
	_, err = worker.nativeProfileWithPermission(t.Context(), source)
	require.NoError(err)
	_, err = worker.getMessagesRawBatchWithIdentityValidation(t.Context(), []string{"synthetic-denied-validation"}, map[string]inconclusiveLabelRefresh{"synthetic-denied-validation": {}})
	require.ErrorIs(err, errNativeSourcePermissionReduction, "a reduced provider denial must stop further batch work")
	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
	current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.NotEqual(pin.Reference, current.Reference)
}

type deniedLabelMetadataGmailAPI struct{ deniedAfterProfileGmailAPI }

func (c deniedLabelMetadataGmailAPI) GetMessageLabelsBatch(_ context.Context, ids []string) ([]gmail.MessageLabelsBatchResult, error) {
	c.beforeDenial()
	if c.batchWide {
		return nil, c.denial
	}
	results := make([]gmail.MessageLabelsBatchResult, len(ids))
	for i, id := range ids {
		results[i] = gmail.MessageLabelsBatchResult{ID: id, Err: c.denial}
	}
	return results, nil
}

func TestFullSyncGmailLabelMetadataPermissionDenial(t *testing.T) {
	for _, batchWide := range []bool{false, true} {
		for _, replaced := range []bool{false, true} {
			name := "individual"
			if batchWide {
				name = "whole_batch"
			}
			if replaced {
				name += "/stale_source"
			}
			t.Run(name, func(t *testing.T) {
				require, assert := Require.New(t), Assert.New(t)
				env := newTestEnv(t)
				enableProviderEvents(t, env, "gmail")
				seedMessages(env, 1, 1000, "synthetic-label-denial")
				runFullSync(t, env)
				source, err := env.Store.GetOrCreateSource(sourceTypeGmail, testEmail)
				require.NoError(err)
				pin, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(err)
				replacementReference := ""
				client := deniedLabelMetadataGmailAPI{deniedAfterProfileGmailAPI{
					grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock},
					denial:                    &gmail.StatusError{StatusCode: http.StatusForbidden}, batchWide: batchWide,
					beforeDenial: func() {
						allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
						require.NoError(err)
						require.True(allowed)
						if !replaced {
							return
						}
						observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: pin.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true}
						require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), observation))
						replacement, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
						require.NoError(err)
						replacementReference = replacement.Reference
						observation.SourceReference, observation.MessageRead = replacement.Reference, true
						require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), observation))
					},
				}}
				_, err = New(client, env.Store, nil).Full(t.Context(), testEmail)
				if replaced {
					require.ErrorContains(err, "resource_unavailable", "the rejected authority write must stop the sync")
				} else if batchWide {
					require.Error(err)
				} else {
					require.ErrorIs(err, errNativeSourcePermissionReduction, "a reduced provider denial must stop further sync work")
				}
				allowed, readErr := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
				require.NoError(readErr)
				assert.Equal(replaced, allowed, "a current denial revokes authority; a stale denial preserves its replacement")
				current, readErr := env.Store.GetMCPResource(t.Context(), "source", source.ID)
				require.NoError(readErr)
				if replaced {
					assert.Equal(replacementReference, current.Reference)
				} else {
					assert.NotEqual(pin.Reference, current.Reference)
				}
			})
		}
	}
}
