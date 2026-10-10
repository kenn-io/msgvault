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

type mcpDiscoveryClient interface {
	MCPCapabilities(ctx context.Context) (*apiprotocol.MCPCapabilities, error)
}

func inboxDiscoveryFixture() apiprotocol.MCPCapabilities {
	return apiprotocol.MCPCapabilities{Version: 1, Delegated: true, Commands: []apiprotocol.MCPCommandDescriptor{}, IdentityOperations: []string{}, InboxOperations: []string{"archive", "get-state", "receipt-get"}, Routes: []apiprotocol.MCPRouteDescriptor{{OperationID: "controlInbox", Method: http.MethodPost, Path: "/api/v1/inbox/control", QueryParameters: []string{}, RequestProperties: []string{"operation", "target", "source", "expected", "preview_token", "idempotency_key", "receipt_id", "dry_run", "tags", "destination", "origin_folder"}}}}
}

func TestMCPDiscoveryClientPreservesCallerDescriptor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	want := inboxDiscoveryFixture()
	var controls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-agent-token", r.Header.Get("X-Msgvault-Agent-Token"))
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
		case "/api/v1/mcp/capabilities":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.NoError(t, json.MarshalWrite(w, want))
		default:
			controls.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: "synthetic-agent-token", AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	defer func() { assert.NoError(t, client.Close()) }()
	discovery, ok := any(client).(mcpDiscoveryClient)
	requirements.True(ok)
	got, err := discovery.MCPCapabilities(t.Context())
	requirements.NoError(err)
	assertions.Equal(want, *got)
	assertions.Zero(controls.Load())
}

func TestInboxClientRequiresDiscoveryAdmissionBeforeControl(t *testing.T) {
	for _, mode := range []string{"missing endpoint", "missing route", "wrong route", "missing signed fields", "missing preview field", "missing action", "unsupported version"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
					return
				}
				if r.URL.Path == "/api/v1/mcp/capabilities" {
					descriptor := inboxDiscoveryFixture()
					switch mode {
					case "missing endpoint":
						w.WriteHeader(http.StatusNotFound)
						return
					case "missing route":
						descriptor.Routes = nil
					case "wrong route":
						descriptor.Routes[0].Path = "/api/v1/other"
					case "missing signed fields":
						descriptor.Routes[0].RequestProperties = []string{"operation", "target"}
					case "missing preview field":
						descriptor.Routes[0].RequestProperties = []string{"operation", "target", "source", "expected", "preview_token", "idempotency_key", "receipt_id"}
					case "missing action":
						descriptor.InboxOperations = []string{"get-state"}
					case "unsupported version":
						descriptor.Version = 2
					}
					assert.NoError(t, json.MarshalWrite(w, descriptor))
					return
				}
				posts.Add(1)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assert.NoError(t, client.Close()) }()
			control, ok := any(client).(inboxControlClient)
			requirements.True(ok)
			_, err = control.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: "receipt-a"})
			require.Error(t, err)
			assertions.Zero(posts.Load(), "unadvertised operations must not reach control")
		})
	}
}
