package cmd

import (
	"context"
	"encoding/json"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

func TestDaemonInboxLegacyTagsUseNativeSignedReceipt(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	adapter, target := inboxGmailFixture(t)
	adapter.config = &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "native-tags-owner", AgentAccess: true}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	var err error
	adapter.inboxKey, err = prepareInboxRuntime(t.Context(), adapter.config, adapter.store)
	require.NoError(err)
	labels := []string{"INBOX", "UNREAD"}
	writes, readbacks, lastReadbackWrite := 0, 0, 0
	var mu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": target.AccountID})
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"Label_next","name":"Next","type":"user"}]}`))
		case "/gmail/v1/users/me/messages/mail-fixture":
			if writes > lastReadbackWrite {
				readbacks++
				lastReadbackWrite = writes
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": target.ProviderID, "labelIds": labels, "historyId": "12"})
		case "/gmail/v1/users/me/messages/mail-fixture/modify":
			writes++
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&delta)) {
				return
			}
			if writes == 1 {
				assert.Equal([]string{"Label_next"}, delta.Add)
				assert.Empty(delta.Remove)
				labels = append(labels, "Label_next")
			} else {
				assert.Equal(2, writes)
				assert.Empty(delta.Add)
				assert.Equal([]string{"Label_next"}, delta.Remove)
				labels = slices.DeleteFunc(labels, func(label string) bool { return label == "Label_next" })
			}
			_, _ = w.Write([]byte(`{"id":"mail-fixture"}`))
		default:
			assert.Fail("unexpected Gmail request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	endpoint, err := url.Parse(provider.URL)
	require.NoError(err)
	adapter.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			u := *r.URL
			clone.URL = &u
			clone.URL.Scheme, clone.URL.Host = endpoint.Scheme, endpoint.Host
			return http.DefaultTransport.RoundTrip(clone)
		})
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "native-tag-token"}), gmail.WithTransport(transport), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: adapter.config, Store: adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: adapter.config.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	defer client.Close()
	result, err := client.MessageTags(t.Context(), target.ItemID, &emailtags.MessageTagChange{Add: []string{"Label_next"}}, "")
	require.NoError(err)
	require.NotNil(result)
	assert.True(result.Verified)
	assert.NotEmpty(result.ReceiptID)
	assert.NotEmpty(result.IdempotencyKey)
	assert.Equal("verified", result.ReceiptStatus)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Label_next"}, result.Tags)
	mu.Lock()
	assert.Equal(1, writes)
	assert.Equal(1, readbacks, "legacy command success also requires independent native readback")
	mu.Unlock()
	receipt, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.ReceiptID})
	require.NoError(err)
	require.NotNil(receipt.Receipt)
	assert.Equal(result.IdempotencyKey, receipt.Receipt.IdempotencyKey)
	message, err := adapter.store.GetMessage(target.ItemID)
	require.NoError(err)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Next"}, message.Labels)
	grant, err := client.IssueAgentToken(t.Context(), "native-tags", []string{"inbox.read", "inbox.tag"}, []int64{target.SourceID}, nil, time.Time{})
	require.NoError(err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	require.NoError(err)
	defer delegated.Close()
	session := nativeInboxMCPSession(t, delegated)
	targetData, err := json.Marshal(target)
	require.NoError(err)
	var targetArgs map[string]any
	require.NoError(json.Unmarshal(targetData, &targetArgs))
	args := map[string]any{"target": targetArgs, "remove": []any{"Label_next"}}
	preview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	require.NoError(err)
	require.False(preview.IsError)
	previewData, err := json.Marshal(preview.StructuredContent)
	require.NoError(err)
	var envelope struct {
		Result *inboxcontrol.Result `json:"result"`
	}
	require.NoError(json.Unmarshal(previewData, &envelope))
	require.NotNil(envelope.Result)
	args["dry_run"], args["expected"], args["preview_token"], args["idempotency_key"] = false, envelope.Result.Before, envelope.Result.PreviewToken, "native-mcp-tag-remove"
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	require.NoError(err)
	require.True(pending.NeedsInput())
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	require.NoError(err)
	require.False(confirmed.IsError)
	confirmedData, err := json.Marshal(confirmed.StructuredContent)
	require.NoError(err)
	require.NoError(json.Unmarshal(confirmedData, &envelope))
	require.NotNil(envelope.Result.Receipt)
	assert.Equal(inboxcontrol.StatusVerified, envelope.Result.Receipt.Status)
	mu.Lock()
	assert.Equal(2, writes, "each of two independently authorized intents dispatches once")
	assert.Equal(2, readbacks)
	mu.Unlock()

}
