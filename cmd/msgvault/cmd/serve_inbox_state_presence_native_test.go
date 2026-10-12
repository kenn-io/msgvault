package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	archivesync "go.kenn.io/msgvault/internal/sync"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
	"golang.org/x/oauth2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// Real MIME receive, OAuth/Gmail HTTP, daemon resolution and generated SDK
// requests prove that known-empty labels remain usable after receipt restart.
func TestDaemonInboxNativeSDKKnownEmptyReceiptRestart(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	const account = "empty-owner@example.test"
	const ownerKey = "synthetic-known-empty-owner"
	var mu sync.Mutex
	labels := []string{"UNREAD"}
	posts, readbacks, rawGets := 0, 0, 0
	raw := []byte("From: Synthetic Sender <sender@example.test>\r\nTo: empty-owner@example.test\r\nSubject: Empty labels fixture\r\nMessage-ID: <empty-labels@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSynthetic body.\r\n")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		assert.Equal("Bearer synthetic-known-empty-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": account, "historyId": "17", "messagesTotal": 1})
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"UNREAD","name":"UNREAD","type":"system"}]}`))
		case "/gmail/v1/users/me/messages":
			_, _ = w.Write([]byte(`{"messages":[{"id":"empty-message","threadId":"empty-thread"}],"resultSizeEstimate":1}`))
		case "/gmail/v1/users/me/messages/empty-message":
			body := map[string]any{"id": "empty-message", "threadId": "empty-thread", "labelIds": labels, "historyId": fmt.Sprint(17 + posts)}
			if r.URL.Query().Get("format") == "raw" {
				rawGets++
				body["raw"] = base64.RawURLEncoding.EncodeToString(raw)
			} else if posts > 0 {
				readbacks++
			}
			_ = json.NewEncoder(w).Encode(body)
		case "/gmail/v1/users/me/messages/empty-message/modify":
			assert.Equal(http.MethodPost, r.Method)
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&delta)) {
				return
			}
			if posts == 0 {
				assert.Empty(delta.Add)
				assert.Equal([]string{"UNREAD"}, delta.Remove)
			} else {
				assert.Equal([]string{"UNREAD"}, delta.Add)
				assert.Empty(delta.Remove)
			}
			labels = slices.DeleteFunc(labels, func(id string) bool { return slices.Contains(delta.Remove, id) })
			labels = append(labels, delta.Add...)
			posts++
			_, _ = w.Write([]byte(`{"id":"empty-message"}`))
		default:
			assert.Fail("unexpected native Gmail request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	endpoint, err := url.Parse(provider.URL)
	require.NoError(err)
	factory := func(context.Context, *store.Source) (gmail.API, error) {
		transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			u := *r.URL
			clone.URL = &u
			clone.URL.Scheme, clone.URL.Host = endpoint.Scheme, endpoint.Host
			return http.DefaultTransport.RoundTrip(clone)
		})
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-known-empty-token"}), gmail.WithTransport(transport), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	native, err := factory(t.Context(), nil)
	require.NoError(err)
	_, err = archivesync.New(native, st, nil).Full(t.Context(), account)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("gmail", account)
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"empty-message"})
	require.NoError(err)
	require.Positive(ids["empty-message"])
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: account, AccountID: account, Scope: inboxcontrol.ScopeMessage, ItemID: ids["empty-message"], ProviderID: "empty-message"}
	cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: ownerKey}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	key, err := prepareInboxRuntime(t.Context(), cfg, st)
	require.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: cfg, inboxKey: key, emailTagClientFactory: factory}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.Default(), OperationGate: api.NewSerialOperationGate()})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { server.Router().ServeHTTP(w, r) }))
	t.Cleanup(daemon.Close)
	sdk, err := apiclient.New(daemon.URL)
	require.NoError(err)
	edit := func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+ownerKey)
		return nil
	}
	encoded, err := json.Marshal(target)
	require.NoError(err)
	var sdkTarget generated.InboxTarget
	require.NoError(json.Unmarshal(encoded, &sdkTarget))
	preview, err := sdk.ControlInbox(t.Context(), &generated.ControlInboxRequestOptions{Body: &generated.ControlInboxBody{Operation: "set-read", Target: &sdkTarget, DryRun: new(true)}}, edit)
	require.NoError(err)
	require.NotNil(preview.Before)
	mu.Lock()
	assert.Zero(posts)
	assert.Equal(1, rawGets)
	mu.Unlock()
	body := generated.ControlInboxBody{Operation: "set-read", Target: &sdkTarget, Expected: preview.Before, PreviewToken: preview.PreviewToken, IdempotencyKey: new("known-empty-read")}
	result, err := sdk.ControlInbox(t.Context(), &generated.ControlInboxRequestOptions{Body: &body}, edit)
	require.NoError(err)
	require.NotNil(result.Receipt)
	assert.Equal("verified", result.Receipt.Status)
	require.NotNil(result.After)
	assert.Equal([]string{}, result.After.Tags)
	receiptID := result.Receipt.ID
	mu.Lock()
	assert.Equal(1, posts)
	assert.Equal(1, readbacks)
	mu.Unlock()
	replay, err := sdk.ControlInbox(t.Context(), &generated.ControlInboxRequestOptions{Body: &body}, edit)
	require.NoError(err)
	require.NotNil(replay.After)
	assert.Equal([]string{}, replay.After.Tags)
	require.NoError(st.Close())
	st, err = store.OpenForTest(dbPath)
	require.NoError(err)
	adapter.store = st
	adapter.inboxKey, err = prepareInboxRuntime(t.Context(), cfg, st)
	require.NoError(err)
	server = api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.Default()})
	recovered, err := sdk.ControlInbox(t.Context(), &generated.ControlInboxRequestOptions{Body: &generated.ControlInboxBody{Operation: "receipt-get", ReceiptID: &receiptID}}, edit)
	require.NoError(err)
	require.NotNil(recovered.Receipt)
	assert.Equal("verified", recovered.Receipt.Status)
	require.NotNil(recovered.Receipt.After)
	assert.Equal([]string{}, recovered.Receipt.After.Tags)
	next, err := sdk.ControlInbox(t.Context(), &generated.ControlInboxRequestOptions{Body: &generated.ControlInboxBody{Operation: "set-unread", Target: &sdkTarget, DryRun: new(true)}}, edit)
	require.NoError(err)
	require.NotNil(next.Before)
	assert.Equal([]string{}, next.Before.Tags)
	after, err := sdk.ControlInbox(t.Context(), &generated.ControlInboxRequestOptions{Body: &generated.ControlInboxBody{Operation: "set-unread", Target: &sdkTarget, Expected: next.Before, PreviewToken: next.PreviewToken, IdempotencyKey: new("known-empty-unread")}}, edit)
	require.NoError(err)
	require.NotNil(after.Receipt)
	assert.Equal("verified", after.Receipt.Status)
	require.NotNil(after.After)
	assert.Equal([]string{"UNREAD"}, after.After.Tags)
	mu.Lock()
	assert.Equal(2, posts, "exactly one native write per distinct signed operation; replay/restart add none")
	assert.GreaterOrEqual(readbacks, 2, "each write requires independent native readback")
	mu.Unlock()
}
