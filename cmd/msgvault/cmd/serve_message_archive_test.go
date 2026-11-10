package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

type archiveProviderFixture struct {
	connects     atomic.Int64
	failReadback atomic.Bool
}

func newArchiveProviderFixture(t *testing.T) *archiveProviderFixture {
	t.Helper()
	fixture := &archiveProviderFixture{}
	tags := []string{"INBOX", "UNREAD", "Label_keep"}
	var mutex sync.Mutex
	failRead := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gmail/v1/users/me/labels":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "Label_keep", "name": "Keep", "type": "user"}}}))
			return
		case "/gmail/v1/users/me/messages/gmail-archive/modify":
			var change struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&change)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			failRead = fixture.failReadback.Load()
			tags = slices.DeleteFunc(tags, func(tag string) bool { return slices.Contains(change.Remove, tag) })
			tags = append(tags, change.Add...)
		case "/gmail/v1/users/me/messages/gmail-archive":
			if failRead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		default:
			assert.Fail(t, "unexpected Gmail request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "gmail-archive", "labelIds": tags}))
	}))
	t.Cleanup(provider.Close)
	base, err := url.Parse(provider.URL)
	require.NoError(t, err)
	original := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "gmail.googleapis.com" {
			clone := r.Clone(r.Context())
			clone.URL.Scheme, clone.URL.Host = base.Scheme, base.Host
			clone.Host = base.Host
			return original.RoundTrip(clone)
		}
		return original.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = original })
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
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-fixture-token"}), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}, draftCacheRefresh: func(context.Context, string) error { return nil }}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	return st, source, id, adapter, server.URL, owner, provider
}

func TestArchiveMessageDaemonEndToEnd(t *testing.T) {
	st, source, id, adapter, serverURL, owner, provider := newArchiveDaemon(t)
	other, err := st.GetOrCreateSource("gmail", "other@example.test")
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		perms   []string
		source  int64
		senders []string
		allowed bool
	}{
		{"read permission alone", []string{"message.read"}, source.ID, nil, false},
		{"wrong source", []string{"message.archive"}, other.ID, nil, false},
		{"wrong sender", []string{"message.archive"}, source.ID, []string{"other@example.test"}, false},
		{"allowed sender", []string{"message.read", "message.archive"}, source.ID, []string{"codes@example.test"}, true},
		{"unrestricted sender", []string{"message.archive"}, source.ID, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selections map[int64][]string
			if tc.senders != nil {
				selections = map[int64][]string{tc.source: tc.senders}
			}
			token, err := owner.IssueAgentTokenWithArchiveSenders(t.Context(), tc.name, tc.perms, []int64{tc.source}, nil, selections, time.Time{})
			require.NoError(t, err)
			client, err := daemonclient.New(daemonclient.Config{URL: serverURL, AgentToken: token.Secret, AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			archived := true
			before := provider.connects.Load()
			result, err := client.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
			if !tc.allowed {
				var failure *emailtags.MessageTagError
				require.ErrorAs(t, err, &failure)
				want := "message_not_found"
				if !slices.Contains(tc.perms, "message.archive") {
					want = "permission_denied"
				}
				assert.Equal(t, want, failure.Code)
				assert.Equal(t, before, provider.connects.Load(), "denials must precede provider access")
				return
			}
			require.NoError(t, err)
			assert.True(t, result.DryRun)
		})
	}
	cliGrant, err := owner.IssueAgentTokenWithArchiveSenders(t.Context(), "cli archive", []string{"message.archive"}, []int64{source.ID}, nil, map[int64][]string{source.ID: {"codes@example.test"}}, time.Time{})
	require.NoError(t, err)
	client, err := daemonclient.New(daemonclient.Config{URL: serverURL, AgentToken: cliGrant.Secret, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(cliGrant.Secret), 0o600))
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
	require.NoError(t, root.ExecuteContext(cliContext))
	assert.Contains(t, output.String(), `"dry_run":true`)
	_, err = os.Stat(filepath.Join(cliConfig.HomeDir, "config.toml"))
	assert.True(t, os.IsNotExist(err), "delegated CLI must not initialize a local archive")
	syncExecution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err)
	previewArchived := true
	preview, err := owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &previewArchived, DryRun: true})
	require.NoError(t, err)
	assert.True(t, preview.DryRun)
	localPreview, err := st.GetMessage(id)
	require.NoError(t, err)
	assert.Empty(t, localPreview.Labels, "preview must not save even authoritative labels")
	require.NoError(t, syncExecution.Release())
	archived := true
	result, err := owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
	require.NoError(t, err)
	assert.True(t, result.Verified)
	msg, err := st.GetMessage(id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"UNREAD", "Keep"}, msg.Labels)
	// Simulate a stale local INBOX after Gmail has already archived the message.
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	require.NoError(t, st.SaveEmailTagsContext(t.Context(), target, &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
	_, err = owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
	require.NoError(t, err)
	msg, err = st.GetMessage(id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"UNREAD", "Keep"}, msg.Labels, "satisfied retry must reconcile stale local membership")
	var failure *emailtags.MessageTagError
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err)
	_, err = adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived}, nil, nil)
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "sync_active", failure.Code)
	require.NoError(t, execution.Release())
}

func TestArchiveMessageUnsupportedProviderAndMissingScope(t *testing.T) {
	for _, provider := range []string{"imap", "msmail", "beeper", "gmail"} {
		t.Run(provider, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(provider, scopeEscalationAccount)
			require.NoError(t, err)
			conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-unsupported", "Archive")
			require.NoError(t, err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message-unsupported", MessageType: "email"})
			require.NoError(t, err)
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
			require.ErrorAs(t, err, &failure)
			want := "archive_unsupported"
			if provider == "gmail" {
				want = "insufficient_scope"
			}
			assert.Equal(t, want, failure.Code)
			assert.Zero(t, connects)
		})
	}
}

func TestArchiveSenderRequiresEveryLiteralFromAndRechecksUnderLock(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	participant, err := st.EnsureParticipant("allowed@example.test", "Allowed", "example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "envelope-thread", "Synthetic Envelope")
	require.NoError(t, err)
	archived := true
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageArchive}, Sources: []agentgrant.SourceRef{{Type: source.SourceType, Identifier: source.Identifier, ArchiveSenderKeys: []string{"allowed@example.test"}}}}
	adapter := &storeAPIAdapter{store: st}
	bare := `"allowed"@example.test`
	parsed, err := mime.Parse([]byte("From: " + (&mail.Address{Address: bare}).String() + "\r\n\r\nsynthetic\r\n"))
	require.NoError(t, err)
	require.Len(t, parsed.From, 1)
	require.Equal(t, bare, parsed.From[0].Email)
	quoted, err := mime.Parse([]byte("From: <\"first last\"@example.test>\r\nTo: owner@example.test\r\nSubject: code\r\n\r\nsynthetic\r\n"))
	require.NoError(t, err)
	require.Len(t, quoted.From, 1)
	require.Equal(t, "first last@example.test", quoted.From[0].Email)
	grant.Sources[0].ArchiveSenderKeys = append(grant.Sources[0].ArchiveSenderKeys, "first last@example.test")
	newArchiveProviderFixture(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = cfg.HomeDir
	adapter.config = cfg
	adapter.logger = slog.New(slog.DiscardHandler)
	adapter.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}
	for _, tc := range []struct {
		name      string
		addresses []string
		allowed   bool
	}{
		{"quoted local part", []string{parsed.From[0].Email}, false},
		{"missing", nil, false},
		{"legacy null", []string{""}, false},
		{"mixed", []string{"allowed@example.test", "denied@example.test"}, false},
		{"stored quoted mailbox", []string{quoted.From[0].Email}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]int64, len(tc.addresses))
			for i := range ids {
				ids[i] = participant
			}
			id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "gmail-archive", MessageType: "email"}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: ids, EmailAddresses: tc.addresses}}})
			require.NoError(t, err)
			result, err := adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: true}, grant, nil)
			if tc.allowed {
				require.NoError(t, err, "an allowed sender parsed and stored by production MIME code must remain allowed")
				assert.True(t, result.Archived)
				return
			}
			var failure *emailtags.MessageTagError
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, "message_not_found", failure.Code)
		})
	}
	data := &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "changing", MessageType: "email"}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, EmailAddresses: []string{"allowed@example.test"}}}}
	id, err := st.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	entered := false
	_, err = adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived}, grant, func(ctx context.Context) (func(), error) {
		entered = true
		data.Recipients[0].EmailAddresses[0] = "denied@example.test"
		_, err := st.PersistMessageContext(ctx, data)
		return func() {}, err
	})
	assert.True(t, entered)
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "message_not_found", failure.Code)
}

// Failed writes disclose label snapshots only to callers with read authority.
func TestArchiveOnlyGrantFailureBodyHidesLabels(t *testing.T) {
	for _, caller := range []string{"archive-only", "archive-reader"} {
		t.Run(caller, func(t *testing.T) {
			st, source, id, _, serverURL, owner, provider := newArchiveDaemon(t)
			permissions := []string{"message.archive"}
			if caller == "archive-reader" {
				permissions = append(permissions, "message.read")
			}
			token, err := owner.IssueAgentTokenWithArchiveSenders(t.Context(), caller, permissions, []int64{source.ID}, nil, nil, time.Time{})
			require.NoError(t, err)
			client, err := daemonclient.New(daemonclient.Config{URL: serverURL, AgentToken: token.Secret, AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			target, err := st.EmailTagTargetContext(t.Context(), id, "")
			require.NoError(t, err)
			before := []string{"INBOX", "UNREAD"}
			require.NoError(t, st.SaveEmailTagsContext(t.Context(), target, &emailtags.MessageTagResult{Provider: "gmail", Tags: before, Verified: true}))
			provider.failReadback.Store(true)
			archived := true
			_, err = client.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
			var failure *emailtags.MessageTagError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "remote_unknown", failure.Code)
			if caller == "archive-only" {
				assert.Nil(t, failure.Result, "archive-only grant received message label state: %+v", failure.Result)
			} else {
				require.NotNil(t, failure.Result)
				assert.Contains(t, failure.Result.Tags, "Label_keep")
				assert.Contains(t, failure.Result.Before, "INBOX")
			}
			msg, err := st.GetMessage(id)
			require.NoError(t, err)
			assert.ElementsMatch(t, before, msg.Labels, "uncertain readback must not be persisted")
		})
	}
}
