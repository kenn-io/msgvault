package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

type archiveProviderFixture struct {
	client       *gmail.Client
	messageID    string
	connects     atomic.Int64
	failReadback atomic.Bool
}

type archiveRewriteTransport struct{ base *url.URL }

func (r archiveRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = r.base.Scheme, r.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func newArchiveProviderFixture(t *testing.T) *archiveProviderFixture {
	t.Helper()
	fixture := &archiveProviderFixture{messageID: "gmail-archive"}
	tags := []string{"INBOX", "UNREAD", "Label_keep"}
	var mutex sync.Mutex
	wrote := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if r.URL.Path == "/gmail/v1/users/me/labels" {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "Inbox", "type": "system"}, {"id": "Label_keep", "name": "Keep", "type": "user"}}}))
			return
		}
		path := "/gmail/v1/users/me/messages/" + fixture.messageID
		switch r.URL.Path {
		case path:
			if wrote && fixture.failReadback.Load() {
				http.Error(w, "synthetic failure", http.StatusBadRequest)
				return
			}
		case path + "/modify":
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&delta)) {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			tags = slices.DeleteFunc(tags, func(tag string) bool { return slices.Contains(delta.Remove, tag) })
			tags = append(tags, delta.Add...)
			wrote = true
		default:
			assert.Fail(t, "unexpected Gmail request", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": fixture.messageID, "labelIds": tags}))
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	require.NoError(t, err)
	fixture.client = gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture"}), gmail.WithHTTPClient(&http.Client{Transport: archiveRewriteTransport{base: base}}), gmail.WithRateLimiter(gmail.NewRateLimiter(1000)))
	return fixture
}

func newArchiveDaemon(t *testing.T) (*store.Store, *store.Source, int64, *storeAPIAdapter, string, *daemonclient.Client, *archiveProviderFixture) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "account@example.test")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant("codes@example.test", "Code Sender", "example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-archive", "Archive")
	require.NoError(t, err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "gmail-archive", MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{"codes@example.test"}, DisplayNames: []string{"Code Sender"}}}})
	require.NoError(t, err)
	cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	cfg.Data.DataDir = cfg.HomeDir
	cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "no-credentials.json")
	provider := newArchiveProviderFixture(t)
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		provider.connects.Add(1)
		return provider.client, nil
	}, draftCacheRefresh: func(context.Context, string) error { return nil }}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	return st, source, id, adapter, server.URL, owner, provider
}

func TestArchiveMessageDaemonGrantEnforcement(t *testing.T) {
	st, source, id, _, serverURL, owner, provider := newArchiveDaemon(t)
	other, err := st.GetOrCreateSource("gmail", "other@example.test")
	require.NoError(t, err)
	var hidden *emailtags.MessageTagError
	for _, tc := range []struct {
		name    string
		perms   []string
		source  int64
		senders []string
		allowed bool
		missing bool
	}{
		{"wrong source", []string{"message.archive"}, other.ID, nil, false, false},
		{"wrong sender", []string{"message.archive"}, source.ID, []string{"other@example.test"}, false, false},
		{"missing", []string{"message.archive"}, source.ID, []string{"codes@example.test"}, false, true},
		{"unrestricted sender", []string{"message.archive"}, source.ID, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			var selections map[int64][]string
			if tc.senders != nil {
				selections = map[int64][]string{tc.source: tc.senders}
			}
			token, err := owner.IssueAgentToken(t.Context(), tc.name, tc.perms, []int64{tc.source}, nil, selections, time.Time{})
			requirementsForTest.NoError(err)
			client, err := daemonclient.New(daemonclient.Config{URL: serverURL, AgentToken: token.Secret, AllowInsecure: true})
			requirementsForTest.NoError(err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			archived := true
			before := provider.connects.Load()
			targetID := id
			if tc.missing {
				targetID += 1000
			}
			result, err := client.ArchiveMessage(t.Context(), targetID, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
			if !tc.allowed {
				var failure *emailtags.MessageTagError
				requirementsForTest.ErrorAs(err, &failure)
				assertionsForTest.Equal("message_not_found", failure.Code)
				if hidden == nil {
					hidden = failure
				} else {
					assertionsForTest.Equal(hidden, failure)
				}
				assertionsForTest.Equal(before, provider.connects.Load(), "denials must precede provider access")
				return
			}
			requirementsForTest.NoError(err)
			assertionsForTest.True(result.DryRun)
		})
	}
}

func TestArchiveMessageDaemonAgentCLI(t *testing.T) {
	assertionsForTest, requirementsForTest := assert.New(t), require.New(t)
	st, source, id, _, serverURL, owner, _ := newArchiveDaemon(t)
	requirementsForTest.NoError(st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "Your code is 482913", Valid: true}, sql.NullString{}))
	cliGrant, err := owner.IssueAgentToken(t.Context(), "code reader", []string{"message.read", "verification.read", "message.archive"}, []int64{source.ID}, nil, map[int64][]string{source.ID: {"codes@example.test"}}, time.Time{})
	requirementsForTest.NoError(err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/messages/%d", serverURL, id), nil)
	requirementsForTest.NoError(err)
	req.Header.Set(apiprotocol.AgentTokenHeader, cliGrant.Secret)
	resp, err := http.DefaultClient.Do(req)
	requirementsForTest.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	requirementsForTest.NoError(err)
	requirementsForTest.Equal(http.StatusOK, resp.StatusCode, string(body))
	assertionsForTest.Contains(string(body), "482913")

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirementsForTest.NoError(os.WriteFile(tokenFile, []byte(cliGrant.Secret), 0o600))
	cliConfig := config.NewDefaultConfig()
	cliConfig.HomeDir = t.TempDir()
	cliConfig.Data.DataDir = cliConfig.HomeDir
	cliContext := testInvocationContext(t.Context(), cliConfig, invocationOptions{agentURL: serverURL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
	root := newTestRootCmd()
	root.AddCommand(newMessageArchiveCommand(true, nil))
	root.SetArgs([]string{"archive", "--message", strconv.FormatInt(id, 10), "--dry-run", "--json"})
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	requirementsForTest.NoError(root.ExecuteContext(cliContext))
	var result emailtags.ArchiveResult
	requirementsForTest.NoError(json.Unmarshal(output.Bytes(), &result))
	assertionsForTest.True(result.Archived)
	assertionsForTest.True(result.DryRun)
}

func TestArchiveMessageDaemonPreviewDuringSync(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	st, source, id, adapter, _, owner, _ := newArchiveDaemon(t)
	syncExecution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirementsForTest.NoError(err)
	previewArchived := true
	preview, err := owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &previewArchived, DryRun: true})
	requirementsForTest.NoError(err)
	assertionsForTest.True(preview.DryRun)
	assertionsForTest.True(preview.Archived)
	assertionsForTest.True(preview.Changed)
	_, err = adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &previewArchived}, nil, nil)
	var failure *emailtags.MessageTagError
	requirementsForTest.ErrorAs(err, &failure)
	assertionsForTest.Equal("sync_active", failure.Code)
	requirementsForTest.NoError(syncExecution.Release())
}

func TestArchiveMessageDaemonWriteAndReconcile(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	st, _, id, _, _, owner, _ := newArchiveDaemon(t) //nolint:dogsled // This case checks persisted tags.
	archived := true
	result, err := owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
	requirementsForTest.NoError(err)
	assertionsForTest.True(result.Verified)
	assertionsForTest.True(result.Archived)
	assertionsForTest.True(result.Changed)
	msg, err := st.GetMessage(id)
	requirementsForTest.NoError(err)
	assertionsForTest.ElementsMatch([]string{"UNREAD", "Keep"}, msg.Labels)
	// Simulate a stale local INBOX after Gmail has already archived the message.
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.SaveEmailTagsContext(t.Context(), target, &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
	result, err = owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
	requirementsForTest.NoError(err)
	assertionsForTest.True(result.Archived)
	assertionsForTest.False(result.Changed)
	msg, err = st.GetMessage(id)
	requirementsForTest.NoError(err)
	assertionsForTest.ElementsMatch([]string{"UNREAD", "Keep"}, msg.Labels, "satisfied retry must reconcile stale local membership")
}

func TestArchiveMessageUnsupportedProviderAndMissingScope(t *testing.T) {
	for _, provider := range []string{"imap", "gmail"} {
		t.Run(provider, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(provider, scopeEscalationAccount)
			requirementsForTest.NoError(err)
			conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-unsupported", "Archive")
			requirementsForTest.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message-unsupported", MessageType: "email"})
			requirementsForTest.NoError(err)
			cfg := &config.Config{HomeDir: t.TempDir()}
			cfg.Data.DataDir = cfg.HomeDir
			if provider == "gmail" {
				tokenPath, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
				defer restore()
				cfg.HomeDir = filepath.Dir(filepath.Dir(tokenPath))
				cfg.Data.DataDir = cfg.HomeDir
				cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "client_secret.json")
			}
			connects := 0
			adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
				connects++
				return nil, errors.New("provider must not be reached")
			}}
			archived := true
			_, err = adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived}, nil, nil)
			var failure *emailtags.MessageTagError
			requirementsForTest.ErrorAs(err, &failure)
			want := "archive_unsupported"
			if provider == "gmail" {
				want = "insufficient_scope"
			}
			assertionsForTest.Equal(want, failure.Code)
			assertionsForTest.Zero(connects)
		})
	}
}

func TestArchiveIncomingSenderPreservesLiteralFromAndRechecksUnderLock(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirementsForTest.NoError(err)
	participant, err := st.EnsureParticipant("allowed@example.test", "Allowed", "example.test")
	requirementsForTest.NoError(err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "envelope-thread", "Synthetic Envelope")
	requirementsForTest.NoError(err)
	archived := true
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageArchive}, Sources: []agentgrant.SourceRef{{Type: source.SourceType, Identifier: source.Identifier, IncomingSenderKeys: []string{"allowed@example.test", "first last@example.test"}}}}
	adapter := &storeAPIAdapter{store: st}
	// Address.String encodes literal quotes inside the addr-spec.
	literal := `"allowed"@example.test`
	provider := newArchiveProviderFixture(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = cfg.HomeDir
	adapter.config = cfg
	adapter.logger = slog.New(slog.DiscardHandler)
	adapter.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return provider.client, nil
	}
	for _, tc := range []struct {
		name, header, literal string
		allowed               bool
	}{
		{"literal quote characters", (&mail.Address{Address: literal}).String(), literal, false},
		{"quoted local part", `<"first last"@example.test>`, "first last@example.test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			parsed, err := mime.Parse([]byte("From: " + tc.header + "\r\n\r\nsynthetic\r\n"))
			requirementsForTest.NoError(err)
			requirementsForTest.Len(parsed.From, 1)
			requirementsForTest.Equal(tc.literal, parsed.From[0].Email)
			provider.messageID = "gmail-archive-" + tc.name
			id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message:    &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: provider.messageID, MessageType: "email"},
				Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, EmailAddresses: []string{parsed.From[0].Email}}},
			})
			requirementsForTest.NoError(err)
			result, err := adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: true}, grant, nil)
			if tc.allowed {
				requirementsForTest.NoError(err, "an allowed sender parsed and stored by production MIME code must remain allowed")
				assertionsForTest.True(result.Archived)
				return
			}
			var failure *emailtags.MessageTagError
			requirementsForTest.ErrorAs(err, &failure)
			assertionsForTest.Equal("message_not_found", failure.Code)
		})
	}
	data := &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "changing", MessageType: "email"}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, EmailAddresses: []string{"allowed@example.test"}}}}
	id, err := st.PersistMessageContext(t.Context(), data)
	requirementsForTest.NoError(err)
	entered := false
	_, err = adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived}, grant, func(ctx context.Context) (func(), error) {
		entered = true
		data.Recipients[0].EmailAddresses[0] = "denied@example.test"
		_, err := st.PersistMessageContext(ctx, data)
		return func() {}, err
	})
	assertionsForTest.True(entered)
	var failure *emailtags.MessageTagError
	requirementsForTest.ErrorAs(err, &failure)
	assertionsForTest.Equal("message_not_found", failure.Code)
}

// Failed writes disclose label snapshots only to callers with read authority.
func TestArchiveOnlyGrantFailureBodyHidesLabels(t *testing.T) {
	for _, caller := range []string{"archive-only", "archive-reader"} {
		t.Run(caller, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			_, source, id, _, serverURL, owner, provider := newArchiveDaemon(t)
			permissions := []string{"message.archive"}
			if caller == "archive-reader" {
				permissions = append(permissions, "message.read")
			}
			token, err := owner.IssueAgentToken(t.Context(), caller, permissions, []int64{source.ID}, nil, nil, time.Time{})
			requirementsForTest.NoError(err)
			client, err := daemonclient.New(daemonclient.Config{URL: serverURL, AgentToken: token.Secret, AllowInsecure: true})
			requirementsForTest.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			provider.failReadback.Store(true)
			archived := true
			_, err = client.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
			var failure *emailtags.MessageTagError
			requirementsForTest.ErrorAs(err, &failure)
			requirementsForTest.Equal("remote_unknown", failure.Code)
			if caller == "archive-only" {
				assertionsForTest.Nil(failure.Result, "archive-only grant received message label state: %+v", failure.Result)
			} else {
				requirementsForTest.NotNil(failure.Result)
				assertionsForTest.Contains(failure.Result.Tags, "Label_keep")
				assertionsForTest.Contains(failure.Result.Before, "INBOX")
			}
		})
	}
}

// SQL row-close logging runs after the envelope read releases its database cursor.
// Use that existing seam to commit a real owner merge before provider access.
type archiveMergeAfterEnvelopeHandler struct {
	slog.Handler

	merged atomic.Bool
	merge  func()
}

func (h *archiveMergeAfterEnvelopeHandler) Handle(_ context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "stmt" && strings.Contains(attr.Value.String(), "FROM message_recipients scope_from") && h.merged.CompareAndSwap(false, true) {
			h.merge()
		}
		return true
	})
	return nil
}

func TestArchivePreviewAuthorizesResolvedSourceDuringMerge(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	st, source, id, adapter, serverURL, owner, _ := newArchiveDaemon(t)
	destination, err := st.GetOrCreateSource("gmail", "destination@example.test")
	requirementsForTest.NoError(err)
	token, err := owner.IssueAgentToken(t.Context(), "source-bound preview", []string{"message.archive"}, []int64{source.ID}, nil, map[int64][]string{source.ID: {"codes@example.test"}}, time.Time{})
	requirementsForTest.NoError(err)
	client, err := daemonclient.New(daemonclient.Config{URL: serverURL, AgentToken: token.Secret, AllowInsecure: true})
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	var destinationCalls atomic.Int64
	factory := adapter.emailTagClientFactory
	adapter.emailTagClientFactory = func(ctx context.Context, captured *store.Source) (gmail.API, error) {
		if captured.ID == destination.ID {
			destinationCalls.Add(1)
			return nil, errors.New("ungranted destination credentials must not be opened")
		}
		return factory(ctx, captured)
	}
	var mergeResult store.SourceMergeResult
	var mergeErr error
	handler := &archiveMergeAfterEnvelopeHandler{
		Handler: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
		merge: func() {
			mergeResult, mergeErr = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: destination.ID})
		},
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)
	archived := true
	result, err := client.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
	requirementsForTest.True(handler.merged.Load(), "merge must interleave the envelope read and provider access")
	requirementsForTest.NoError(mergeErr)
	assertionsForTest.EqualValues(1, mergeResult.MessagesMoved)
	moved, lookupErr := st.GetMessageSourceContext(t.Context(), id)
	requirementsForTest.NoError(lookupErr)
	assertionsForTest.Equal(destination.ID, moved.ID)
	assertionsForTest.Zero(destinationCalls.Load(), "preview must never open credentials outside its grant")
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(source.ID, result.SourceID, "provider access remains bound to the authorized captured target")
	assertionsForTest.True(result.DryRun)
}

func TestArchiveCodeReaderGrant(t *testing.T) {
	st, source, id, _, serverURL, owner, _ := newArchiveDaemon(t)
	require.NoError(t, st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "Your code is 482913", Valid: true}, sql.NullString{}))
	require.NoError(t, st.UpsertFTSContext(t.Context(), id, "", "Your code is 482913", "codes@example.test", "", ""))
	for _, tc := range []struct {
		name        string
		permissions []string
		wantCode    bool
	}{
		{"documented grant", []string{"search.read", "message.read", "verification.read", "message.archive"}, true},
		{"masked", []string{"message.read", "message.archive"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			token, err := owner.IssueAgentToken(t.Context(), tc.name, tc.permissions, []int64{source.ID}, nil, map[int64][]string{source.ID: {"codes@example.test"}}, time.Time{})
			requirementsForTest.NoError(err)
			if tc.name == "documented grant" {
				searchRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serverURL+"/api/v1/cli/search?q=code", nil)
				requirementsForTest.NoError(err)
				searchRequest.Header.Set(apiprotocol.AgentTokenHeader, token.Secret)
				searchResponse, err := http.DefaultClient.Do(searchRequest)
				requirementsForTest.NoError(err)
				defer func() { _ = searchResponse.Body.Close() }()
				searchBody, err := io.ReadAll(searchResponse.Body)
				requirementsForTest.NoError(err)
				requirementsForTest.Equal(http.StatusOK, searchResponse.StatusCode, string(searchBody))
				assertionsForTest.Contains(string(searchBody), fmt.Sprintf(`"id":%d`, id))
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/messages/%d", serverURL, id), nil)
			requirementsForTest.NoError(err)
			req.Header.Set(apiprotocol.AgentTokenHeader, token.Secret)
			resp, err := http.DefaultClient.Do(req)
			requirementsForTest.NoError(err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			requirementsForTest.NoError(err)
			requirementsForTest.Equal(http.StatusOK, resp.StatusCode, string(body))
			if tc.wantCode {
				assertionsForTest.Contains(string(body), "482913")
			} else {
				assertionsForTest.NotContains(string(body), "482913")
				assertionsForTest.Contains(string(body), "[verification content masked]")
			}
		})
	}
}
