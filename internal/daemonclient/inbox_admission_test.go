package daemonclient_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This descriptor pins the external peer contract in transport-only tests.
// Native positive admission separately uses the registered production router.
const syntheticInboxDescriptor = `{"version":1,"delegated":false,"routes":[{"operation_id":"controlInbox","method":"POST","path":"/api/v1/inbox/control","query_parameters":[],"request_properties":["operation","target","source","expected","preview_token","idempotency_key","receipt_id","dry_run","tags","destination","origin_folder"]}],"inbox_operations":["archive","tags","receipt-get"]}`

// A synthetic daemon is appropriate here: this pins refusal at the external
// descriptor transport boundary. Native positive admission uses the real
// registered daemon/router in the command acceptance tests.
func TestInboxClientRequiresLiveContractBeforeControl(t *testing.T) {
	const route = `{"operation_id":"controlInbox","method":"POST","path":"/api/v1/inbox/control","query_parameters":[],"request_properties":["operation","target","source","expected","preview_token","idempotency_key","receipt_id","dry_run","tags","destination","origin_folder"]}`
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "missing", status: http.StatusNotFound},
		{name: "malformed", body: `{`},
		{name: "unknown version", body: `{"version":2,"routes":[` + route + `],"inbox_operations":["receipt-get"]}`},
		{name: "missing route", body: `{"version":1,"routes":[],"inbox_operations":["receipt-get"]}`},
		{name: "wrong path", body: `{"version":1,"routes":[` + strings.ReplaceAll(route, "/inbox/control", "/inbox/other") + `],"inbox_operations":["receipt-get"]}`},
		{name: "missing signed field", body: `{"version":1,"routes":[` + strings.ReplaceAll(route, `,"origin_folder"`, "") + `],"inbox_operations":["receipt-get"]}`},
		{name: "operation not admitted", body: `{"version":1,"routes":[` + route + `],"inbox_operations":["get-state"]}`},
		{name: "oversized", body: strings.Repeat(" ", (1<<20)+1) + `{"version":1,"routes":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
				case "/api/v1/mcp/capabilities":
					if tc.status != 0 {
						w.WriteHeader(tc.status)
					}
					_, _ = w.Write([]byte(tc.body))
				case "/api/v1/inbox/control":
					posts.Add(1)
					_, _ = w.Write([]byte(`{}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			Require.NoError(t, err)
			defer func() { Assert.NoError(t, client.Close()) }()
			_, err = client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: "synthetic-receipt"})
			Assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			Assert.Zero(t, posts.Load(), "schema3.14 alone cannot admit an Inbox control POST")
		})
	}
}

func TestInboxClientDiscoveryDoesNotFollowRedirect(t *testing.T) {
	assert := Assert.New(t)

	var posts, followed atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
		case "/api/v1/mcp/capabilities":
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
		default:
			posts.Add(1)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	Require.NoError(t, err)
	defer func() { assert.NoError(client.Close()) }()
	_, err = client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: "synthetic-receipt"})
	assert.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assert.Zero(posts.Load())
	assert.Zero(followed.Load())
}
