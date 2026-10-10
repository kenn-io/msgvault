package daemonclient_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxControlClient interface {
	ControlInbox(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Result, error)
}

func TestInboxClientRejectsOldDaemonBeforeControl(t *testing.T) {
	for _, version := range []string{"", "3.2.0", "invalid"} {
		t.Run(version, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				assert.Equal(t, "/api/v1/health", r.URL.Path)
				_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + version + `"}`))
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assert.NoError(t, client.Close()) }()
			control, ok := any(client).(inboxControlClient)
			requirements.True(ok)
			_, err = control.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: "receipt-a"})
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			assertions.Equal(int64(0), posts.Load())
		})
	}
}

func TestInboxClientPreservesSignedRequestAndUnknownReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 2, ProviderID: "chat-a"}
	before := inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, Expected: &before, PreviewToken: "signed-preview", IdempotencyKey: "operation-a"}
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-agent-token", r.Header.Get("X-Msgvault-Agent-Token"))
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
			return
		}
		if r.URL.Path == "/api/v1/mcp/capabilities" {
			assert.NoError(t, json.MarshalWrite(w, inboxDiscoveryFixture()))
			return
		}
		posts.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/inbox/control", r.URL.Path)
		var decoded inboxcontrol.Request
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &decoded)) {
			return
		}
		assert.Equal(t, request, decoded)
		w.WriteHeader(http.StatusBadGateway)
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"error": "inbox_outcome_unknown", "message": "untrusted synthetic response", "result": inboxcontrol.Result{Receipt: &inboxcontrol.Receipt{ID: "receipt-a", Status: inboxcontrol.StatusUnknown}}}))
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: "synthetic-agent-token", AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	defer func() { assert.NoError(t, client.Close()) }()
	control, ok := any(client).(inboxControlClient)
	requirements.True(ok)
	result, err := control.ControlInbox(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
	requirements.NotNil(result)
	requirements.NotNil(result.Receipt)
	assertions.Equal("receipt-a", result.Receipt.ID)
	assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
	assertions.NotContains(err.Error(), "untrusted synthetic response")
	assertions.Equal(int64(1), posts.Load())
}

func TestInboxClientNeverRetriesUncertainMutationOrFollowsRedirect(t *testing.T) {
	for _, mode := range []string{"server error", "empty result", "redirect", "lost response"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var posts, followed atomic.Int64
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
					return
				}
				if r.URL.Path == "/api/v1/mcp/capabilities" {
					assert.NoError(t, json.MarshalWrite(w, inboxDiscoveryFixture()))
					return
				}
				posts.Add(1)
				switch mode {
				case "server error":
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte("untrusted synthetic response"))
				case "empty result":
					_, _ = w.Write([]byte(`{}`))
				case "redirect":
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
				case "lost response":
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(t, ok) {
						return
					}
					connection, _, err := hijacker.Hijack()
					if assert.NoError(t, err) {
						assert.NoError(t, connection.Close())
					}
				}
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assert.NoError(t, client.Close()) }()
			control, ok := any(client).(inboxControlClient)
			requirements.True(ok)
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 2, ProviderID: "chat-a"}
			before := inboxcontrol.State{Target: target, Inbox: new(true), ObservedAt: time.Now().UTC()}
			_, err = control.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, Expected: &before, PreviewToken: "signed-preview", IdempotencyKey: "operation-a"})
			require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
			assertions.Equal(int64(1), posts.Load())
			assertions.Equal(int64(0), followed.Load())
		})
	}
}
