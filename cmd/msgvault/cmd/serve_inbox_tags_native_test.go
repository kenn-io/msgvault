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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assertions := assert.New(t)

	requirements := require.New(t)

	adapter, target := inboxGmailFixture(t)
	adapter.config = &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "native-tags-owner", AgentAccess: true}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	var err error
	adapter.inboxKey, err = prepareInboxRuntime(t.Context(), adapter.config, adapter.store)
	requirements.NoError(err)
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
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&delta)) {
				return
			}
			if writes == 1 {
				assertions.Equal([]string{"Label_next"}, delta.Add)
				assertions.Empty(delta.Remove)
				labels = append(labels, "Label_next")
			} else {
				assertions.Equal(2, writes)
				assertions.Empty(delta.Add)
				assertions.Equal([]string{"Label_next"}, delta.Remove)
				labels = slices.DeleteFunc(labels, func(label string) bool { return label == "Label_next" })
			}
			_, _ = w.Write([]byte(`{"id":"mail-fixture"}`))
		default:
			assertions.Fail("unexpected Gmail request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	endpoint, err := url.Parse(provider.URL)
	requirements.NoError(err)
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
	requirements.NoError(err)
	defer func() { assertions.NoError(client.Close()) }()
	result, err := client.MessageTags(t.Context(), target.ItemID, &emailtags.MessageTagChange{Add: []string{"Label_next"}}, "")
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.Verified)
	assertions.NotEmpty(result.ReceiptID)
	assertions.NotEmpty(result.IdempotencyKey)
	assertions.Equal("verified", result.ReceiptStatus)
	assertions.ElementsMatch([]string{"INBOX", "UNREAD", "Label_next"}, result.Tags)
	mu.Lock()
	assertions.Equal(1, writes)
	assertions.Equal(1, readbacks, "legacy command success also requires independent native readback")
	mu.Unlock()
	receipt, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.ReceiptID})
	requirements.NoError(err)
	requirements.NotNil(receipt.Receipt)
	assertions.Equal(result.IdempotencyKey, receipt.Receipt.IdempotencyKey)
	message, err := adapter.store.GetMessage(target.ItemID)
	requirements.NoError(err)
	assertions.ElementsMatch([]string{"INBOX", "UNREAD", "Next"}, message.Labels)
	grant, err := client.IssueAgentToken(t.Context(), "native-tags", []string{"inbox.read", "inbox.tag"}, []int64{target.SourceID}, nil, time.Time{})
	requirements.NoError(err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	defer func() { assertions.NoError(delegated.Close()) }()
	session := nativeInboxMCPSession(t, delegated)
	targetData, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(targetData, &targetArgs))
	args := map[string]any{"target": targetArgs, "remove": []any{"Label_next"}}
	preview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	requirements.NoError(err)
	requirements.False(preview.IsError)
	previewData, err := json.Marshal(preview.StructuredContent)
	requirements.NoError(err)
	var envelope struct {
		Result *inboxcontrol.Result `json:"result"`
	}
	requirements.NoError(json.Unmarshal(previewData, &envelope))
	requirements.NotNil(envelope.Result)
	args["dry_run"], args["expected"], args["preview_token"], args["idempotency_key"] = false, envelope.Result.Before, envelope.Result.PreviewToken, "native-mcp-tag-remove"
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_message_tags", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	requirements.False(confirmed.IsError)
	confirmedData, err := json.Marshal(confirmed.StructuredContent)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(confirmedData, &envelope))
	requirements.NotNil(envelope.Result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, envelope.Result.Receipt.Status)
	mu.Lock()
	assertions.Equal(2, writes, "each of two independently authorized intents dispatches once")
	assertions.Equal(2, readbacks)
	mu.Unlock()
}
