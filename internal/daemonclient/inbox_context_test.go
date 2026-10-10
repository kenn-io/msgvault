package daemonclient_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxContextClient interface {
	InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error)
}

// Exercise the real client against hostile HTTP responses. The native Store,
// router and issued content grants are covered by the API context regression.
func TestInboxContextClientContract(t *testing.T) {
	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "synthetic-context"}
	for _, mode := range []string{"success", "chat-success", "chat-foreign-message", "empty", "unavailable", "missing-discovery", "partial-discovery", "wrong-version", "wrong-method", "wrong-path", "foreign-target", "foreign-message", "missing-text", "null-text", "missing-truncated", "missing-unavailable", "unavailable-with-text", "unavailable-truncated", "over-budget", "invalid-json", "oversized", "redirect", "denied", "unauthorized", "invalid", "server-error"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			target := target
			request := inboxcontrol.ContextRequest{Target: target, MaxBytes: 9}
			messageID := target.ItemID
			if mode == "chat-success" || mode == "chat-foreign-message" {
				target.SourceType, target.SourceIdentifier, target.AccountID = "beeper", "synthetic-account", "synthetic-account"
				target.Scope = inboxcontrol.ScopeChat
				request.Target, request.MessageID = target, 19
				messageID = request.MessageID
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "synthetic-context-token", r.Header.Get(apiprotocol.AgentTokenHeader))
				switch r.URL.Path {
				case "/api/v1/health":
					assert.Equal(t, http.MethodGet, r.Method)
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
				case "/api/v1/mcp/capabilities":
					assert.Equal(t, http.MethodGet, r.Method)
					descriptor := apiprotocol.MCPCapabilities{Version: 1, Delegated: true, Routes: []apiprotocol.MCPRouteDescriptor{}}
					if mode != "missing-discovery" {
						descriptor.Routes = append(descriptor.Routes, apiprotocol.MCPRouteDescriptor{OperationID: "getInboxContext", Method: http.MethodPost, Path: "/api/v1/inbox/context", RequestProperties: []string{"target", "message_id", "max_bytes"}})
					}
					if mode == "partial-discovery" {
						descriptor.Routes[0].RequestProperties = []string{"target"}
					}
					switch mode {
					case "wrong-version":
						descriptor.Version = 2
					case "wrong-method":
						descriptor.Routes[0].Method = http.MethodGet
					case "wrong-path":
						descriptor.Routes[0].Path = "/api/v1/context"
					}
					assert.NoError(t, json.MarshalWrite(w, descriptor))
				case "/api/v1/inbox/context":
					calls.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
					var received inboxcontrol.ContextRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &received)) {
						return
					}
					assert.Equal(t, request, received)
					switch mode {
					case "redirect":
						http.Redirect(w, r, "/unexpected-redirect", http.StatusTemporaryRedirect)
						return
					case "denied", "unauthorized", "invalid", "server-error":
						status := map[string]int{"denied": http.StatusForbidden, "unauthorized": http.StatusUnauthorized, "invalid": http.StatusBadRequest, "server-error": http.StatusServiceUnavailable}[mode]
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"error":"untrusted-code","message":"untrusted detail"}`))
						return
					case "oversized":
						_, _ = w.Write(make([]byte, (1<<20)+1))
						return
					case "invalid-json":
						_, _ = w.Write([]byte(`{"text":`))
						return
					}
					body := map[string]any{"target": target, "message_id": messageID, "text": "Synthetic", "truncated": false, "unavailable": false}
					switch mode {
					case "empty", "unavailable":
						body["text"] = ""
						body["unavailable"] = mode == "unavailable"
					case "foreign-target":
						foreign := target
						foreign.AccountID = "foreign@example.test"
						body["target"] = foreign
					case "foreign-message", "chat-foreign-message":
						body["message_id"] = messageID + 1
					case "missing-text":
						delete(body, "text")
					case "null-text":
						body["text"] = nil
					case "missing-truncated":
						delete(body, "truncated")
					case "missing-unavailable":
						delete(body, "unavailable")
					case "unavailable-with-text":
						body["unavailable"] = true
					case "unavailable-truncated":
						body["text"], body["unavailable"], body["truncated"] = "", true, true
					case "over-budget":
						body["text"] = "Synthetic!"
					}
					assert.NoError(t, json.MarshalWrite(w, body))
				default:
					assert.Fail(t, "unexpected native context path")
				}
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, AgentToken: "synthetic-context-token", HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assert.NoError(t, client.Close()) }()
			reader, ok := any(client).(inboxContextClient)
			requirements.True(ok, "daemon client must expose scoped context reads")
			result, err := reader.InboxContext(t.Context(), request)
			switch mode {
			case "success", "chat-success", "empty", "unavailable":
				requirements.NoError(err)
				requirements.NotNil(result)
				assertions.Equal(target, result.Target)
				assertions.Equal(messageID, result.MessageID)
				if mode == "success" || mode == "chat-success" {
					assertions.Equal("Synthetic", result.Text)
				} else {
					assertions.Empty(result.Text)
				}
				assertions.Equal(mode == "unavailable", result.Unavailable)
			case "denied", "unauthorized":
				require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			case "invalid":
				require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
			default:
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			}
			if err != nil {
				assertions.Nil(result)
				assertions.NotContains(err.Error(), "untrusted detail")
			}
			wantCalls := int64(1)
			if mode == "missing-discovery" || mode == "partial-discovery" || mode == "wrong-version" || mode == "wrong-method" || mode == "wrong-path" {
				wantCalls = 0
			}
			assertions.Equal(wantCalls, calls.Load())
		})
	}
}

func TestInboxContextClientRejectsInvalidBeforeHTTP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	defer func() { assert.NoError(t, client.Close()) }()
	reader, ok := any(client).(inboxContextClient)
	requirements.True(ok)
	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "synthetic-context"}
	for _, request := range []inboxcontrol.ContextRequest{
		{},
		{Target: target, MaxBytes: -1},
		{Target: target, MaxBytes: inboxcontrol.MaxContextBytes + 1},
		{Target: target, MessageID: 7},
	} {
		result, err := reader.InboxContext(t.Context(), request)
		requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
		assertions.Nil(result)
	}
	chat := target
	chat.SourceType, chat.SourceIdentifier, chat.AccountID = "beeper", "synthetic-account", "synthetic-account"
	chat.Scope = inboxcontrol.ScopeChat
	for _, messageID := range []int64{0, -1} {
		result, err := reader.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: chat, MessageID: messageID})
		requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
		assertions.Nil(result)
	}
	assertions.Equal(int64(0), calls.Load())
}
