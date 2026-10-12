package cmd

import (
	"fmt"
	"net/http"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxMCPWiringRequiresCurrentSchema(t *testing.T) {
	for _, version := range []string{"3.14.0", "3.15.0"} {
		t.Run(version, func(t *testing.T) {
			assert := Assert.New(t)

			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
				case "/api/v1/mcp/capabilities":
					_, _ = w.Write([]byte(inboxMCPTestDescriptor))
				default:
					http.NotFound(w, r)
				}
			})
			opts := daemonMCPServeOptions(t.Context(), client, nil)
			if version == "3.14.0" {
				assert.Nil(opts.Inbox)
				assert.Empty(opts.InboxOperations)
			} else {
				Require.NotNil(t, opts.Inbox)
				assert.Contains(opts.InboxOperations, inboxcontrol.OpArchive)
				assert.Contains(opts.InboxOperations, inboxcontrol.OpTags)
				assert.Contains(opts.InboxOperations, inboxcontrol.OpReceiptGet)
			}
		})
	}
}

func TestInboxMCPDelegatedCatalogUsesCurrentGrant(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
		case "/api/v1/mcp/capabilities":
			_, _ = w.Write([]byte(inboxMCPTestDescriptor))
		case "/api/v1/agent-tokens/self":
			_, _ = w.Write([]byte(`{"id":"synthetic-inbox-grant","label":"Inbox","permissions":["inbox.read","inbox.archive"],"sources":[{"id":1,"source_type":"gmail","identifier":"owner@example.test"}]}`))
		default:
			assert.Fail("unexpected capability request", r.URL.Path)
		}
	})
	opts, err := delegatedMCPServeOptions(t.Context(), client)
	require.NoError(err)
	require.NotNil(opts.Inbox)
	assert.Contains(opts.InboxOperations, inboxcontrol.OpGetState)
	assert.Contains(opts.InboxOperations, inboxcontrol.OpArchive)
	assert.Contains(opts.InboxOperations, inboxcontrol.OpReceiptGet)
	assert.NotContains(opts.InboxOperations, inboxcontrol.OpTags)
	assert.NotContains(opts.InboxOperations, inboxcontrol.OpSetRead)
	assert.NotContains(opts.InboxOperations, inboxcontrol.OpMove)
}
