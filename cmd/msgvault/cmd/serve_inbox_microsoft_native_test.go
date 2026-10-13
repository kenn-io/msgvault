package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// microsoftDaemonGraph serves native mailbox, selected metadata, delta and MIME
// requests. The daemon factory uses the actual Graph transport, not a provider
// interface stub. Mutable provider state is guarded across HTTP calls.
type microsoftDaemonGraph struct {
	mu                      sync.Mutex
	categories              []string
	read                    bool
	parent, etag            string
	writes, readbacks, mime int
	failAfterWrite          bool
	disconnectAfterWrite    bool
	acknowledgementBody     string
	readbackUnavailable     bool
	readbackBody            string
	writeStatus             int
	factory                 atomic.Int32
	server                  *httptest.Server
}

func newMicrosoftDaemonGraph(t *testing.T) *microsoftDaemonGraph {
	t.Helper()
	g := &microsoftDaemonGraph{categories: []string{"Old", "Keep"}, parent: "inbox-id", etag: `W/"v1"`}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		assert.Equal(t, "Bearer synthetic-native-token", r.Header.Get("Authorization"))
		assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		w.Header().Set("Content-Type", "application/json")
		message := func() map[string]any {
			return map[string]any{"id": "native-message", "categories": g.categories, "isRead": g.read, "parentFolderId": g.parent, "@odata.etag": g.etag, "receivedDateTime": "2024-01-01T10:00:00Z"}
		}
		switch {
		case r.URL.Path == "/me":
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"mail": "owner@example.test", "userPrincipalName": "owner@example.test"}))
		case r.URL.Path == "/me/mailFolders":
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "inbox-id", "displayName": "Inbox"}, {"id": "archive-id", "displayName": "Archive"}}}))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			var values []map[string]any
			if strings.Contains(r.URL.Path, "/inbox-id/") && r.URL.Query().Get("token") == "" {
				values = append(values, message())
			}
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"value": values, "@odata.deltaLink": r.URL.Path + "?token=1"}))
		case r.URL.Path == "/me/mailFolders/inbox":
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "inbox-id"}))
		case r.URL.Path == "/me/mailFolders/archive":
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "archive-id"}))
		case strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/me/messages/native-message/$value":
			g.mime++
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = w.Write([]byte("From: sender@example.test\r\nTo: owner@example.test\r\nSubject: Native Microsoft fixture\r\nMessage-ID: <native-microsoft@example.test>\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000\r\n\r\nSynthetic native body.\r\n"))
		case r.URL.Path == "/me/messages/native-message":
			if r.Method == http.MethodPatch {
				g.writes++
				assert.Equal(t, g.etag, r.Header.Get("If-Match"))
				var body map[string][]string
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					return
				}
				if !assert.Len(t, body, 1) {
					return
				}
				if !assert.NotNil(t, body["categories"]) {
					return
				}
				if g.writeStatus != 0 {
					w.WriteHeader(g.writeStatus)
					return
				}
				g.categories = body["categories"]
				g.etag = fmt.Sprintf(`W/"v%d"`, g.writes+1)
				if g.failAfterWrite {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if g.disconnectAfterWrite {
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(t, ok) {
						return
					}
					connection, _, err := hijacker.Hijack()
					if !assert.NoError(t, err) {
						return
					}
					assert.NoError(t, connection.Close())
					return
				}
				if g.acknowledgementBody != "" {
					_, _ = w.Write([]byte(g.acknowledgementBody))
					return
				}
				// Success still requires an independent complete GET, not this body.
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "native-message"}))
				return
			}
			assert.Equal(t, http.MethodGet, r.Method)
			if g.writes > 0 {
				g.readbacks++
				if g.readbackUnavailable {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if g.readbackBody != "" {
					_, _ = w.Write([]byte(g.readbackBody))
					return
				}
			}
			assert.NoError(t, json.MarshalWrite(w, message()))
		default:
			assert.Fail(t, "unexpected native Microsoft endpoint", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}

func microsoftDaemonFixture(t *testing.T) (*storeAPIAdapter, inboxcontrol.Target, *microsoftDaemonGraph, func([]string)) {
	t.Helper()
	return microsoftDaemonFixtureStore(t, testutil.NewTestStore(t))
}

func microsoftDaemonFixtureStore(t *testing.T, st *store.Store) (*storeAPIAdapter, inboxcontrol.Target, *microsoftDaemonGraph, func([]string)) {
	t.Helper()
	g := newMicrosoftDaemonGraph(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.APIKey = "synthetic-microsoft-owner-key"
	cfg.Server.AgentAccess = true
	client := msmail.NewClient(g.server.URL, func(context.Context) (string, error) { return "synthetic-native-token", nil }, 10000)
	summary, err := msmail.Import(t.Context(), st, client, msmail.Options{Email: "owner@example.test", AttachmentsDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(summary.SourceID, []string{"native-message"})
	require.NoError(t, err)
	require.NotZero(t, ids["native-message"])
	var body string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), ids["native-message"]).Scan(&body))
	assert.Contains(t, body, "Synthetic native body.")
	target := inboxcontrol.Target{SourceID: summary.SourceID, SourceType: sourceTypeMSMail, SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: ids["native-message"], ProviderID: "native-message"}
	observed, err := st.GetInboxProviderState(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, observed)
	require.NotNil(t, observed.Read)
	assert.False(t, *observed.Read)
	assert.Equal(t, "inbox-id", observed.Location)
	key, err := prepareInboxRuntime(t.Context(), cfg, st)
	require.NoError(t, err)
	adapter := &storeAPIAdapter{store: st, config: cfg, inboxKey: key, logger: slog.New(slog.DiscardHandler), microsoftTagClientFactory: func(ctx context.Context, source *store.Source, write bool) (*msmail.Client, error) {
		g.factory.Add(1)
		assert.Equal(t, target.SourceID, source.ID)
		assert.Equal(t, target.AccountID, source.Identifier)
		return msmail.NewClient(g.server.URL, func(context.Context) (string, error) { return "synthetic-native-token", nil }, 10000), nil
	}}
	tokenPath := microsoft.NewGraphMailManager("", "", "", cfg.TokensDir(), nil).TokenPath(target.AccountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	saveScopes := func(scopes []string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"access_token": "synthetic-native-token", "token_type": "Bearer", "scopes": scopes})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(tokenPath, data, 0o600))
	}
	saveScopes(microsoft.GraphMailWriteScopes())
	return adapter, target, g, saveScopes
}

func TestDaemonInboxMicrosoftNativeSDKReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter, target, graph, _ := microsoftDaemonFixture(t)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: adapter.config, Store: adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: adapter.config.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() {
		requirements.NoError(client.Close())
	})
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}, Remove: []string{"OLD"}}, DryRun: true}
	preview, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(preview.Before)
	requirements.NotNil(preview.Projected)
	assertions.ElementsMatch([]string{"Keep", "Next"}, preview.Projected.Tags)
	graph.mu.Lock()
	assertions.Zero(graph.writes)
	graph.mu.Unlock()
	request.DryRun = false
	request.Expected = preview.Before
	request.PreviewToken = preview.PreviewToken
	request.IdempotencyKey = "native-microsoft-once"
	result, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	requirements.NotNil(result.After)
	requirements.NotNil(result.After.Read)
	assertions.False(*result.After.Read)
	assertions.Equal("inbox-id", result.After.Location)
	graph.mu.Lock()
	assertions.Equal(1, graph.writes)
	assertions.Positive(graph.readbacks)
	graph.mu.Unlock()
	replay, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(replay.Receipt)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	graph.mu.Lock()
	assertions.Equal(1, graph.writes)
	graph.mu.Unlock()
	message, err := adapter.store.GetMessage(target.ItemID)
	requirements.NoError(err)
	assertions.ElementsMatch([]string{"Inbox", "Category: Keep", "Category: Next"}, message.Labels)
	beforeFactory := graph.factory.Load()
	conflicting := request
	conflicting.Tags = &emailtags.MessageTagChange{Add: []string{"Different"}}
	_, err = client.ControlInbox(t.Context(), conflicting)
	requirements.ErrorIs(err, inboxcontrol.ErrConflict)
	assertions.Equal(beforeFactory, graph.factory.Load(), "a used key cannot acquire a different native intent")
	noop := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Add: []string{"Next"}}, "microsoft-native-noop")
	unchanged, err := client.ControlInbox(t.Context(), noop)
	requirements.NoError(err)
	requirements.NotNil(unchanged.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, unchanged.Receipt.Status)
	graph.mu.Lock()
	assertions.Equal(1, graph.writes, "a native no-op must not PATCH")
	graph.mu.Unlock()
}

func microsoftDaemonSDK(t *testing.T, adapter *storeAPIAdapter) (*daemonclient.Client, string) {
	t.Helper()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: adapter.config, Store: adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: adapter.config.Server.APIKey, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client, server.URL
}

func microsoftSignedTags(t *testing.T, client *daemonclient.Client, target inboxcontrol.Target, change emailtags.MessageTagChange, key string) inboxcontrol.Request {
	t.Helper()
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &change, DryRun: true}
	preview, err := client.ControlInbox(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, preview.Before)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, key
	return request
}

func TestDaemonInboxMicrosoftNativeKnownEmptyReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter, target, graph, _ := microsoftDaemonFixture(t)
	client, _ := microsoftDaemonSDK(t, adapter)
	request := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Remove: []string{"Old", "Keep"}}, "microsoft-empty-categories")
	result, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	requirements.NotNil(result.After)
	assertions.NotNil(result.After.Tags, "known-empty categories must survive the native SDK wire")
	assertions.Empty(result.After.Tags)
	requirements.NotNil(result.Receipt.Projected)
	assertions.NotNil(result.Receipt.Projected.Tags)
	requirements.NotNil(result.Receipt.After)
	assertions.NotNil(result.Receipt.After.Tags, "durable receipt serialization must preserve empty categories")
	replayed, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(replayed.After)
	assertions.NotNil(replayed.After.Tags)
	assertions.Empty(replayed.After.Tags)
	persisted, err := adapter.store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(persisted)
	assertions.NotNil(persisted.Tags)
	assertions.Empty(persisted.Tags)
	message, err := adapter.store.GetMessage(target.ItemID)
	requirements.NoError(err)
	assertions.Equal([]string{"Inbox"}, message.Labels)
	fromEmpty := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Add: []string{"Fresh"}}, "microsoft-after-empty-categories")
	assertions.NotNil(fromEmpty.Expected.Tags, "known-empty expected state must survive preview decoding")
	added, err := client.ControlInbox(t.Context(), fromEmpty)
	requirements.NoError(err, "generated SDK request serialization must preserve the signed empty expected state")
	requirements.NotNil(added.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, added.Receipt.Status)
	assertions.Equal([]string{"Fresh"}, added.After.Tags)
	graph.mu.Lock()
	assertions.Equal(2, graph.writes)
	graph.mu.Unlock()
}

func TestDaemonInboxMicrosoftNativeUnknownRestartReconcile(t *testing.T) {
	for _, kind := range []string{"ack-lost", "readback-lost", "empty-readback-lost", "disconnect-after-write", "oversized-ack", "invalid-readback", "oversized-readback"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			path := filepath.Join(t.TempDir(), "native-microsoft.db")
			st, err := store.OpenForTest(path)
			requirements.NoError(err)
			requirements.NoError(st.InitSchema())
			t.Cleanup(func() { _ = st.Close() })
			adapter, target, graph, _ := microsoftDaemonFixtureStore(t, st)
			client, _ := microsoftDaemonSDK(t, adapter)
			change := emailtags.MessageTagChange{Add: []string{"Next"}, Remove: []string{"Old"}}
			if kind == "empty-readback-lost" {
				change = emailtags.MessageTagChange{Remove: []string{"Old", "Keep"}}
			}
			request := microsoftSignedTags(t, client, target, change, "microsoft-ambiguous-once")
			graph.mu.Lock()
			switch kind {
			case "ack-lost":
				graph.failAfterWrite = true
			case "disconnect-after-write":
				graph.disconnectAfterWrite = true
			case "oversized-ack":
				graph.acknowledgementBody = strings.Repeat("x", 2<<20)
			case "invalid-readback":
				graph.readbackBody = "{"
			case "oversized-readback":
				graph.readbackBody = strings.Repeat("x", 2<<20)
			default:
				graph.readbackUnavailable = true
			}
			graph.mu.Unlock()
			unknown, err := client.ControlInbox(t.Context(), request)
			requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
			requirements.NotNil(unknown)
			requirements.NotNil(unknown.Receipt)
			assertions.Equal(inboxcontrol.StatusUnknown, unknown.Receipt.Status)
			replay, err := client.ControlInbox(t.Context(), request)
			requirements.NoError(err)
			requirements.NotNil(replay.Receipt)
			assertions.Equal(unknown.Receipt.ID, replay.Receipt.ID)
			assertions.Equal(inboxcontrol.StatusUnknown, replay.Receipt.Status)
			requirements.NoError(st.Close())
			st, err = store.OpenForTest(path)
			requirements.NoError(err)
			adapter.store = st
			adapter.inboxKey, err = prepareInboxRuntime(t.Context(), adapter.config, st)
			requirements.NoError(err)
			loaded, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: unknown.Receipt.ID})
			requirements.NoError(err)
			requirements.NotNil(loaded.Receipt)
			assertions.Equal(inboxcontrol.StatusUnknown, loaded.Receipt.Status)
			if kind == "empty-readback-lost" {
				requirements.NotNil(loaded.Receipt.Projected)
				assertions.NotNil(loaded.Receipt.Projected.Tags, "known-empty projection survives receipt serialization and reopening")
				assertions.Empty(loaded.Receipt.Projected.Tags)
			}
			graph.mu.Lock()
			graph.readbackUnavailable = false
			graph.readbackBody = ""
			graph.mu.Unlock()
			reconciled, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: unknown.Receipt.ID})
			requirements.NoError(err)
			requirements.NotNil(reconciled.Receipt)
			assertions.Equal(inboxcontrol.StatusVerified, reconciled.Receipt.Status)
			if kind == "empty-readback-lost" {
				assertions.NotNil(reconciled.After.Tags)
				assertions.Empty(reconciled.After.Tags)
				requirements.NotNil(reconciled.Receipt.After)
				assertions.NotNil(reconciled.Receipt.After.Tags)
			} else {
				assertions.ElementsMatch([]string{"Keep", "Next"}, reconciled.After.Tags)
			}
			graph.mu.Lock()
			assertions.Equal(1, graph.writes, "restart and reconciliation must never redispatch an uncertain PATCH")
			assertions.Positive(graph.readbacks)
			graph.mu.Unlock()
			message, err := st.GetMessage(target.ItemID)
			requirements.NoError(err)
			if kind == "empty-readback-lost" {
				assertions.Equal([]string{"Inbox"}, message.Labels)
			} else {
				assertions.ElementsMatch([]string{"Inbox", "Category: Keep", "Category: Next"}, message.Labels)
			}
		})
	}
}

func TestDaemonInboxMicrosoftNativeScopeAndIdentityFences(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter, target, graph, saveScopes := microsoftDaemonFixture(t)
	client, _ := microsoftDaemonSDK(t, adapter)
	for _, tc := range []struct {
		name   string
		change func(*inboxcontrol.Target)
	}{
		{"account", func(v *inboxcontrol.Target) { v.AccountID = "foreign@example.test" }},
		{"source-identifier", func(v *inboxcontrol.Target) { v.SourceIdentifier = "foreign@example.test" }},
		{"provider-kind", func(v *inboxcontrol.Target) { v.SourceType = "gmail" }},
		{"provider-message", func(v *inboxcontrol.Target) { v.ProviderID = "foreign-message" }},
		{"archive-item", func(v *inboxcontrol.Target) { v.ItemID++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			foreign := target
			tc.change(&foreign)
			_, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &foreign})
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
		})
	}
	assertions.Zero(graph.factory.Load(), "invalid archive authority must not construct a provider client")
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpSetRead} {
		_, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: operation, Target: &target, DryRun: true})
		requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	}
	assertions.Zero(graph.factory.Load(), "unsupported native operations must fail before provider access")
	request := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Add: []string{"Next"}}, "microsoft-scope-revoked")
	beforeFactory := graph.factory.Load()
	saveScopes(microsoft.GraphMailScopes())
	_, err := client.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Equal(beforeFactory, graph.factory.Load(), "scope loss after preview must prevent client creation")
	read, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	requirements.NoError(err)
	requirements.NotNil(read.Before)
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	capabilities, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
	requirements.NoError(err)
	requirements.NotNil(capabilities.Capabilities)
	assertions.False(capabilities.Capabilities.ConditionalWrite)
	assertions.Contains(capabilities.Capabilities.Operations, inboxcontrol.Capability{Operation: inboxcontrol.OpTags, Status: inboxcontrol.CapabilityPermissionRequired, Reason: "Mail.ReadWrite scope required"})
	tokenPath := microsoft.NewGraphMailManager("", "", "", adapter.config.TokensDir(), nil).TokenPath(target.AccountID)
	requirements.NoError(os.WriteFile(tokenPath, []byte("not-json"), 0o600))
	beforeFactory = graph.factory.Load()
	_, err = client.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assertions.Equal(beforeFactory, graph.factory.Load(), "unreadable scope evidence must fail closed")
	saveScopes(microsoft.GraphMailWriteScopes())
	for _, column := range []string{"deleted_at", "deleted_from_source_at"} {
		_, err := adapter.store.DB().Exec(adapter.store.Rebind("UPDATE messages SET "+column+"=CURRENT_TIMESTAMP WHERE id=?"), target.ItemID)
		requirements.NoError(err)
		_, err = client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		requirements.ErrorIs(err, inboxcontrol.ErrDenied)
		assertions.Equal(beforeFactory, graph.factory.Load(), "deleted item authority must fail before native access")
		_, err = adapter.store.DB().Exec(adapter.store.Rebind("UPDATE messages SET "+column+"=NULL WHERE id=?"), target.ItemID)
		requirements.NoError(err)
	}
	graph.mu.Lock()
	assertions.Zero(graph.writes)
	graph.mu.Unlock()
}

func TestDaemonInboxMicrosoftNativeGrantAdmission(t *testing.T) {
	adapter, target, graph, _ := microsoftDaemonFixture(t)
	owner, daemonURL := microsoftDaemonSDK(t, adapter)
	foreign, err := adapter.store.GetOrCreateSource("msmail", "foreign@example.test")
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		permissions []string
		sources     []int64
		expiring    bool
	}{
		{"read-only", []string{"inbox.read"}, []int64{target.SourceID}, false},
		{"content-only", []string{"message.read", "search.read"}, []int64{target.SourceID}, false},
		{"wrong-action", []string{"inbox.read", "inbox.archive"}, []int64{target.SourceID}, false},
		{"wrong-source", []string{"inbox.read", "inbox.tag"}, []int64{foreign.ID}, false},
		{"expired", []string{"inbox.read", "inbox.tag"}, []int64{target.SourceID}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			var expires time.Time
			if tc.expiring {
				expires = time.Now().Add(2 * time.Second)
			}
			grant, err := owner.IssueAgentToken(t.Context(), tc.name, tc.permissions, tc.sources, nil, expires)
			requirements.NoError(err)
			client, err := daemonclient.New(daemonclient.Config{URL: daemonURL, AgentToken: grant.Secret, AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() {
				requirements.NoError(client.Close())
			})
			if tc.expiring {
				time.Sleep(time.Until(expires)) //nolint:kennlint // Wait for the synthetic grant expiry before asserting refusal.
			}
			_, err = client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}}, DryRun: true})
			requirements.Error(err)
			assert.Zero(t, graph.factory.Load(), "grant rejection must precede native client construction")
		})
	}
	graph.mu.Lock()
	assert.Zero(t, graph.writes)
	graph.mu.Unlock()
}

func TestDaemonInboxMicrosoftNativeRemovedSourceRetiresReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter, target, graph, _ := microsoftDaemonFixture(t)
	client, _ := microsoftDaemonSDK(t, adapter)
	request := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Add: []string{"Next"}}, "microsoft-retired-key")
	result, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	requirements.NoError(adapter.store.RemoveSource(target.SourceID))
	beforeFactory := graph.factory.Load()
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
		_, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: operation, ReceiptID: result.Receipt.ID})
		requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	}
	_, err = client.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Equal(beforeFactory, graph.factory.Load(), "retired authority must not reach the native client")
	provider := msmail.NewClient(graph.server.URL, func(context.Context) (string, error) { return "synthetic-native-token", nil }, 10000)
	_, err = msmail.Import(t.Context(), adapter.store, provider, msmail.Options{Email: target.AccountID, AttachmentsDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	requirements.NoError(err)
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
		_, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: operation, ReceiptID: result.Receipt.ID})
		requirements.ErrorIs(err, inboxcontrol.ErrDenied, "re-import must not revive an old receipt")
	}
	_, err = client.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Equal(beforeFactory, graph.factory.Load())
	audit, err := adapter.store.GetInboxReceipt(t.Context(), result.Receipt.ID)
	requirements.NoError(err)
	requirements.NotNil(audit)
	assertions.Equal(inboxcontrol.StatusVerified, audit.Status)
	graph.mu.Lock()
	assertions.Equal(1, graph.writes)
	graph.mu.Unlock()
}

func TestDaemonInboxMicrosoftNativeReadOnlyMCPDiscoveryAndRevocation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter, target, graph, _ := microsoftDaemonFixture(t)
	owner, daemonURL := microsoftDaemonSDK(t, adapter)
	grant, err := owner.IssueAgentToken(t.Context(), "native-microsoft-read-only", []string{"inbox.read"}, []int64{target.SourceID}, nil, time.Time{})
	requirements.NoError(err)
	client, err := daemonclient.New(daemonclient.Config{URL: daemonURL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(client.Close()) })

	descriptor, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	requirements.True(descriptor.HasInboxContract())
	assertions.True(descriptor.Delegated)
	assertions.Contains(descriptor.InboxOperations, string(inboxcontrol.OpGetState))
	assertions.NotContains(descriptor.InboxOperations, string(inboxcontrol.OpTags))
	assertions.Zero(graph.factory.Load(), "discovery must not construct a native provider")
	session := nativeInboxMCPSession(t, client)
	tools, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assertions.Contains(names, "inbox_get_state")
	assertions.NotContains(names, "update_message_tags")
	read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_get_state", Arguments: map[string]any{"target": target}})
	requirements.NoError(err)
	requirements.False(read.IsError)
	data, err := json.Marshal(read.StructuredContent)
	requirements.NoError(err)
	var envelope struct {
		Result *inboxcontrol.Result `json:"result"`
	}
	requirements.NoError(json.Unmarshal(data, &envelope))
	requirements.NotNil(envelope.Result)
	requirements.NotNil(envelope.Result.Before)
	assertions.Equal(target, envelope.Result.Before.Target)
	assertions.ElementsMatch([]string{"Old", "Keep"}, envelope.Result.Before.Tags)

	beforeFactory := graph.factory.Load()
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}}, DryRun: true}
	_, err = client.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable, "the live descriptor excludes tag operations for a read-only grant")
	typed, err := client.GeneratedClient()
	requirements.NoError(err)
	data, err = json.Marshal(request)
	requirements.NoError(err)
	var body generated.ControlInboxBody
	requirements.NoError(json.Unmarshal(data, &body))
	denied, err := typed.ControlInboxWithResponse(t.Context(), &generated.ControlInboxRequestOptions{Body: &body})
	requirements.Error(err)
	requirements.NotNil(denied)
	assertions.Equal(http.StatusForbidden, denied.StatusCode, "the actual control route also refuses tag previews without inbox.tag")
	assertions.Equal(beforeFactory, graph.factory.Load())

	requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
	_, err = client.MCPCapabilities(t.Context())
	requirements.ErrorIs(err, daemonclient.ErrMCPCapabilitiesUnavailable)
	read, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_get_state", Arguments: map[string]any{"target": target}})
	requirements.NoError(err)
	assertions.True(read.IsError, "an already connected MCP session must refuse a revoked grant")
	assertions.Equal(beforeFactory, graph.factory.Load(), "revocation must prevent further native access")
	graph.mu.Lock()
	assertions.Zero(graph.writes)
	graph.mu.Unlock()
}

func TestDaemonInboxMicrosoftNativeMCPConfirmationAndCLIRecovery(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter, target, graph, _ := microsoftDaemonFixture(t)
	owner, daemonURL := microsoftDaemonSDK(t, adapter)
	grant, err := owner.IssueAgentToken(t.Context(), "native-microsoft", []string{"inbox.read", "inbox.tag"}, []int64{target.SourceID}, nil, time.Time{})
	requirements.NoError(err)
	client, err := daemonclient.New(daemonclient.Config{URL: daemonURL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() {
		requirements.NoError(client.Close())
	})
	session := nativeInboxMCPSession(t, client)
	targetData, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(targetData, &targetArgs))
	args := map[string]any{"target": targetArgs, "add": []any{"Next"}, "remove": []any{"Old"}}
	preview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	requirements.NoError(err)
	requirements.False(preview.IsError)
	data, err := json.Marshal(preview.StructuredContent)
	requirements.NoError(err)
	var envelope struct {
		Result *inboxcontrol.Result `json:"result"`
	}
	requirements.NoError(json.Unmarshal(data, &envelope))
	requirements.NotNil(envelope.Result)
	args["dry_run"], args["expected"], args["preview_token"], args["idempotency_key"] = false, envelope.Result.Before, envelope.Result.PreviewToken, "microsoft-mcp-confirmed"
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	graph.mu.Lock()
	assertions.Zero(graph.writes, "MCP must obtain confirmation before mutation")
	graph.mu.Unlock()
	changed := make(map[string]any, len(args))
	maps.Copy(changed, args)
	changed["add"] = []any{"Unconfirmed"}
	refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: changed, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	requirements.True(refused.IsError)
	graph.mu.Lock()
	assertions.Zero(graph.writes, "confirmation for different arguments cannot authorize the native write")
	graph.mu.Unlock()
	// A mismatched confirmation consumes its one-use challenge. Request a new
	// challenge before confirming the original exact arguments.
	pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	requirements.False(confirmed.IsError)
	data, err = json.Marshal(confirmed.StructuredContent)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(data, &envelope))
	requirements.NotNil(envelope.Result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, envelope.Result.Receipt.Status)

	foreignGrant, err := owner.IssueAgentToken(t.Context(), "other-native-microsoft", []string{"inbox.read", "inbox.tag"}, []int64{target.SourceID}, nil, time.Time{})
	requirements.NoError(err)
	foreignClient, err := daemonclient.New(daemonclient.Config{URL: daemonURL, AgentToken: foreignGrant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() {
		requirements.NoError(foreignClient.Close())
	})
	beforeForeign := graph.factory.Load()
	_, err = foreignClient.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: envelope.Result.Receipt.ID})
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Equal(beforeForeign, graph.factory.Load(), "a grant for the same source/action cannot read another principal's receipt")
	// CLI recovery gets its own server so earlier MCP calls cannot exhaust the
	// delegated client's per-IP rate-limit burst before the child process runs.
	cliOwner, cliDaemonURL := microsoftDaemonSDK(t, adapter)
	cliGrant, err := cliOwner.IssueAgentToken(t.Context(), "native-microsoft-cli", []string{"inbox.read", "inbox.tag"}, []int64{target.SourceID}, nil, time.Time{})
	requirements.NoError(err)
	cliClient, err := daemonclient.New(daemonclient.Config{URL: cliDaemonURL, AgentToken: cliGrant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() {
		requirements.NoError(cliClient.Close())
	})
	request := microsoftSignedTags(t, cliClient, target, emailtags.MessageTagChange{Add: []string{"Last"}}, "microsoft-cli-lost-stdout")
	requestData, err := json.Marshal(request)
	requirements.NoError(err)
	requestFile := filepath.Join(t.TempDir(), "request.json")
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirements.NoError(os.WriteFile(requestFile, requestData, 0o600))
	requirements.NoError(os.WriteFile(tokenFile, []byte(cliGrant.Secret), 0o600))
	reader, writer, err := os.Pipe()
	requirements.NoError(err)
	requirements.NoError(reader.Close())
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInboxCLIMicrosoftClosedStdoutHelper$") //nolint:gosec // Re-executes this test binary with a fixed test selector.
	child.Env = append(os.Environ(), "MSGVAULT_MICROSOFT_CLOSED_STDOUT=1", "MSGVAULT_MICROSOFT_DAEMON_URL="+cliDaemonURL, "MSGVAULT_MICROSOFT_REQUEST="+requestFile, "MSGVAULT_MICROSOFT_AGENT_TOKEN_FILE="+tokenFile)
	child.Stdout = writer
	var stderr bytes.Buffer
	child.Stderr = &stderr
	requirements.NoError(child.Run(), stderr.String())
	recovered, err := cliClient.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(recovered.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, recovered.Receipt.Status)
	assertions.Equal(request.IdempotencyKey, recovered.Receipt.IdempotencyKey)
	requirements.NoError(cliOwner.RevokeAgentToken(t.Context(), cliGrant.ID))
	beforeFactory := graph.factory.Load()
	_, err = cliClient.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable, "the schema-version request also refuses a revoked grant")
	typed, err := cliClient.GeneratedClient()
	requirements.NoError(err)
	var body generated.ControlInboxBody
	requirements.NoError(json.Unmarshal(requestData, &body))
	denied, err := typed.ControlInboxWithResponse(t.Context(), &generated.ControlInboxRequestOptions{Body: &body})
	requirements.Error(err)
	requirements.NotNil(denied)
	assertions.Contains([]int{http.StatusUnauthorized, http.StatusForbidden}, denied.StatusCode, "the actual control route must also refuse replay after revocation")
	assertions.Equal(beforeFactory, graph.factory.Load())
	graph.mu.Lock()
	assertions.Equal(2, graph.writes, "one MCP intent and one CLI intent, without duplicate writes after lost stdout")
	assertions.Positive(graph.readbacks)
	graph.mu.Unlock()
}

func TestInboxCLIMicrosoftClosedStdoutHelper(t *testing.T) {
	if os.Getenv("MSGVAULT_MICROSOFT_CLOSED_STDOUT") != "1" {
		return
	}
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{agentURL: os.Getenv("MSGVAULT_MICROSOFT_DAEMON_URL"), agentTokenFile: os.Getenv("MSGVAULT_MICROSOFT_AGENT_TOKEN_FILE"), agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
	command := newInboxCmd()
	ensureSilenceUsageWrapped(command)
	command.SetContext(ctx)
	command.SetArgs([]string{"tags", "--request", os.Getenv("MSGVAULT_MICROSOFT_REQUEST"), "--apply"})
	command.SetOut(os.Stdout)
	command.SetErr(io.Discard)
	if !errors.Is(command.Execute(), inboxcontrol.ErrOutcomeUnknown) {
		os.Exit(23)
	}
	os.Exit(0)
}

func TestDaemonInboxMicrosoftNativeSignedPreviewFences(t *testing.T) {
	for _, kind := range []string{"categories", "etag", "read", "folder", "intent", "tampered", "expired"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			adapter, target, graph, _ := microsoftDaemonFixture(t)
			client, _ := microsoftDaemonSDK(t, adapter)
			request := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Add: []string{"Next"}}, "microsoft-preview-"+kind)
			graph.mu.Lock()
			switch kind {
			case "categories":
				graph.categories = append(graph.categories, "Concurrent")
			case "etag":
				graph.etag = `W/"concurrent"`
			case "read":
				graph.read = true
			case "folder":
				graph.parent = "archive-id"
			}
			graph.mu.Unlock()
			switch kind {
			case "intent":
				request.Tags.Add = []string{"Different"}
			case "tampered":
				request.PreviewToken += "A"
			case "expired":
				payload, err := base64.RawURLEncoding.DecodeString(strings.Split(request.PreviewToken, ".")[0])
				requirements.NoError(err)
				var claims inboxcontrol.PreviewClaims
				requirements.NoError(json.Unmarshal(payload, &claims))
				now := time.Now()
				claims.IssuedAt, claims.ExpiresAt = now.Add(-6*time.Minute), now.Add(-time.Minute)
				request.PreviewToken, err = inboxcontrol.SignPreview(adapter.inboxKey, claims)
				requirements.NoError(err)
			}
			_, err := client.ControlInbox(t.Context(), request)
			requirements.Error(err)
			graph.mu.Lock()
			assert.Zero(t, graph.writes, "changed or unauthenticated signed plans must never dispatch")
			graph.mu.Unlock()
		})
	}
}

func TestDaemonInboxMicrosoftNativeRejectedWriteReceipt(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusPreconditionFailed, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			adapter, target, graph, _ := microsoftDaemonFixture(t)
			client, _ := microsoftDaemonSDK(t, adapter)
			request := microsoftSignedTags(t, client, target, emailtags.MessageTagChange{Add: []string{"Next"}}, "microsoft-rejected-once")
			graph.mu.Lock()
			graph.writeStatus = status
			graph.mu.Unlock()
			failed, err := client.ControlInbox(t.Context(), request)
			requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
			requirements.NotNil(failed)
			requirements.NotNil(failed.Receipt)
			assertions.Equal(inboxcontrol.StatusFailed, failed.Receipt.Status)
			replay, err := client.ControlInbox(t.Context(), request)
			requirements.NoError(err)
			requirements.NotNil(replay.Receipt)
			assertions.Equal(failed.Receipt.ID, replay.Receipt.ID)
			assertions.Equal(inboxcontrol.StatusFailed, replay.Receipt.Status)
			graph.mu.Lock()
			assertions.Equal(1, graph.writes, "a rejected write must not retry or replay")
			assertions.ElementsMatch([]string{"Old", "Keep"}, graph.categories)
			graph.mu.Unlock()
			message, err := adapter.store.GetMessage(target.ItemID)
			requirements.NoError(err)
			assertions.ElementsMatch([]string{"Inbox", "Category: Old", "Category: Keep"}, message.Labels)
		})
	}
}

func TestDaemonInboxMicrosoftNativeReimportDoesNotReviveGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "source-lifetime.db"))
	require.NoError(err)
	require.NoError(st.InitSchema())
	t.Cleanup(func() { _ = st.Close() })
	adapter, original, graph, _ := microsoftDaemonFixtureStore(t, st)
	owner, daemonURL := microsoftDaemonSDK(t, adapter)
	grant, err := owner.IssueAgentToken(t.Context(), "native-retired-source", []string{"inbox.read", "inbox.tag"}, []int64{original.SourceID}, nil, time.Time{})
	require.NoError(err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: daemonURL, AgentToken: grant.Secret, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(delegated.Close()) })
	_, err = delegated.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &original})
	require.NoError(err, "the old grant initially admits its actual native source")
	require.NoError(st.RemoveSource(original.SourceID))
	provider := msmail.NewClient(graph.server.URL, func(context.Context) (string, error) { return "synthetic-native-token", nil }, 10000)
	summary, err := msmail.Import(t.Context(), st, provider, msmail.Options{Email: original.AccountID, AttachmentsDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	require.NoError(err)
	ids, err := st.MessageExistsBatch(summary.SourceID, []string{original.ProviderID})
	require.NoError(err)
	require.NotZero(ids[original.ProviderID])
	current := original
	current.SourceID = summary.SourceID
	current.ItemID = ids[original.ProviderID]
	t.Logf("synthetic source reimport reused numeric ID: %t", current.SourceID == original.SourceID)
	adapter.microsoftTagClientFactory = func(_ context.Context, source *store.Source, _ bool) (*msmail.Client, error) {
		graph.factory.Add(1)
		assert.Equal(current.SourceID, source.ID)
		assert.Equal(current.AccountID, source.Identifier)
		return provider, nil
	}
	_, err = owner.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &current})
	require.NoError(err, "owner authority can observe the new native source lifetime")
	beforeFactory := graph.factory.Load()
	for _, request := range []inboxcontrol.Request{
		{Operation: inboxcontrol.OpGetState, Target: &current},
		{Operation: inboxcontrol.OpTags, Target: &current, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}}, DryRun: true},
	} {
		_, err := delegated.ControlInbox(t.Context(), request)
		require.ErrorIs(err, inboxcontrol.ErrDenied, "an old grant cannot authorize a new native source lifetime")
	}
	assert.Equal(beforeFactory, graph.factory.Load(), "retired source grants are refused before any native access")
	freshGrant, err := owner.IssueAgentToken(t.Context(), "native-current-source", []string{"inbox.read", "inbox.tag"}, []int64{current.SourceID}, nil, time.Time{})
	require.NoError(err)
	fresh, err := daemonclient.New(daemonclient.Config{URL: daemonURL, AgentToken: freshGrant.Secret, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(fresh.Close()) })
	for _, request := range []inboxcontrol.Request{
		{Operation: inboxcontrol.OpGetState, Target: &current},
		{Operation: inboxcontrol.OpTags, Target: &current, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}}, DryRun: true},
	} {
		_, err := fresh.ControlInbox(t.Context(), request)
		require.NoError(err, "a fresh grant authorizes the current native source lifetime")
	}
	graph.mu.Lock()
	assert.Zero(graph.writes)
	graph.mu.Unlock()
}
