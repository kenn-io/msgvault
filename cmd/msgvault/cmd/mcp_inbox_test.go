package cmd

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxMCPWiringRequiresCurrentSchema(t *testing.T) {
	for _, version := range []string{"3.14.0", "3.15.0"} {
		t.Run(version, func(t *testing.T) {
			assertions := assert.New(t)

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
				assertions.Nil(opts.Inbox)
				assertions.Empty(opts.InboxOperations)
			} else {
				require.NotNil(t, opts.Inbox)
				assertions.Contains(opts.InboxOperations, inboxcontrol.OpArchive)
				assertions.Contains(opts.InboxOperations, inboxcontrol.OpTags)
				assertions.Contains(opts.InboxOperations, inboxcontrol.OpReceiptGet)
			}
		})
	}
}

func TestInboxMCPDelegatedCatalogUsesCurrentGrant(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
		case "/api/v1/mcp/capabilities":
			_, _ = w.Write([]byte(inboxMCPTestDescriptor))
		case "/api/v1/agent-tokens/self":
			_, _ = w.Write([]byte(`{"id":"synthetic-inbox-grant","label":"Inbox","permissions":["inbox.read","inbox.archive"],"sources":[{"id":1,"source_type":"gmail","identifier":"owner@example.test"}]}`))
		default:
			assertions.Fail("unexpected capability request", r.URL.Path)
		}
	})
	opts, err := delegatedMCPServeOptions(t.Context(), client)
	requirements.NoError(err)
	requirements.NotNil(opts.Inbox)
	assertions.Contains(opts.InboxOperations, inboxcontrol.OpGetState)
	assertions.Contains(opts.InboxOperations, inboxcontrol.OpArchive)
	assertions.Contains(opts.InboxOperations, inboxcontrol.OpReceiptGet)
	assertions.NotContains(opts.InboxOperations, inboxcontrol.OpTags)
	assertions.NotContains(opts.InboxOperations, inboxcontrol.OpSetRead)
	assertions.NotContains(opts.InboxOperations, inboxcontrol.OpMove)
}
