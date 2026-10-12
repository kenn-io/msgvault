package cmd

import (
	"net/http"
	"strings"

	"go.kenn.io/msgvault/internal/inboxcontrol"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestInboxMCPWiringRequiresLiveContract(t *testing.T) {
	assert := Assert.New(t)

	for _, delegated := range []bool{false, true} {
		client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/health":
				_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
			case "/api/v1/agent-tokens/self":
				_, _ = w.Write([]byte(`{"id":"synthetic-inbox-grant","permissions":["inbox.read","inbox.archive"],"sources":[{"id":1,"source_type":"gmail","identifier":"owner@example.test"}]}`))
			default:
				http.NotFound(w, r)
			}
		})
		if delegated {
			opts, err := delegatedMCPServeOptions(t.Context(), client)
			Require.NoError(t, err)
			assert.Nil(opts.Inbox)
			assert.Empty(opts.InboxOperations)
		} else {
			opts := daemonMCPServeOptions(t.Context(), client, nil)
			assert.Nil(opts.Inbox)
			assert.Empty(opts.InboxOperations)
		}
	}
}

// This fixture pins the daemon discovery boundary. The native acceptance tests
// independently obtain this contract from the registered production router.
const inboxMCPTestDescriptor = `{"version":1,"delegated":false,"routes":[{"operation_id":"controlInbox","method":"POST","path":"/api/v1/inbox/control","query_parameters":[],"request_properties":["operation","target","source","expected","preview_token","idempotency_key","receipt_id","dry_run","tags","destination","origin_folder"]}],"inbox_operations":["get-state","archive","tags","receipt-get"]}`

func TestInboxMCPWiringIntersectsLiveOperations(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "delegated"}[delegated], func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
				case "/api/v1/agent-tokens/self":
					_, _ = w.Write([]byte(`{"id":"synthetic-inbox-grant","permissions":["inbox.read","inbox.archive"],"sources":[{"id":1,"source_type":"gmail","identifier":"owner@example.test"}]}`))
				case "/api/v1/mcp/capabilities":
					_, _ = w.Write([]byte(strings.Replace(inboxMCPTestDescriptor, `"get-state","archive","tags","receipt-get"`, `"archive","tags"`, 1)))
				default:
					http.NotFound(w, r)
				}
			})
			opts := daemonMCPServeOptions(t.Context(), client, nil)
			if delegated {
				var err error
				opts, err = delegatedMCPServeOptions(t.Context(), client)
				require.NoError(err)
			}
			require.NotNil(opts.Inbox)
			want := []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpTags}
			if delegated {
				want = []inboxcontrol.Operation{inboxcontrol.OpArchive}
			}
			assert.Equal(want, opts.InboxOperations)
		})
	}
}
