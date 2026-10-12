package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/mcp"
)

// The real router and SDK exercise startup and grant admission. Only the
// external discovery response changes: another daemon can report the same API
// version while implementing different routes, which this router cannot model.
func TestMCPIdentityProductionDiscoveryRequiresRouteContracts(t *testing.T) {
	for _, tc := range []struct {
		name, missing, wrong string
		version              int
		delegated            bool
		read, apply          bool
	}{
		{name: "absent discovery", version: 0},
		{name: "absent preview", version: 1, delegated: true, missing: "previewIdentityOperation"},
		{name: "absent receipt", version: 1, delegated: true, missing: "getIdentityOperationReceipt"},
		{name: "absent apply", version: 1, delegated: true, missing: "applyIdentityOperation", read: true},
		{name: "wrong method", version: 1, delegated: true, wrong: "method"},
		{name: "wrong path", version: 1, delegated: true, wrong: "path"},
		{name: "missing fields", version: 1, delegated: true, wrong: "fields"},
		{name: "unknown descriptor", version: 2, delegated: true},
		{name: "different caller", version: 1},
		{name: "matching contract", version: 1, delegated: true, read: true, apply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			prior := mcpAllowIdentityDecisions
			mcpAllowIdentityDecisions = true
			t.Cleanup(func() { mcpAllowIdentityDecisions = prior })
			f := newDraftReplyFixture(t)
			native := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/mcp/capabilities" {
					native.Config.Handler.ServeHTTP(w, r)
					return
				}
				if tc.version == 0 {
					http.NotFound(w, r)
					return
				}
				routes := []map[string]any{}
				for _, entry := range []struct {
					id, method, suffix string
					fields             []string
				}{
					{"previewIdentityOperation", "POST", "preview", []string{"operation", "target"}},
					{"applyIdentityOperation", "POST", "apply", []string{"operation", "target", "expected_fingerprint", "preview_token", "idempotency_key"}},
					{"getIdentityOperationReceipt", "GET", "receipt", []string{"idempotency_key", "receipt_id", "principal"}},
				} {
					if entry.id == tc.missing {
						continue
					}
					query, body := []string{}, entry.fields
					if entry.method == "GET" {
						query, body = entry.fields, []string{}
					}
					route := map[string]any{"operation_id": entry.id, "method": entry.method, "path": "/api/v1/identity/operations/" + entry.suffix, "query_parameters": query, "request_properties": body}
					if entry.suffix == "preview" {
						switch tc.wrong {
						case "method":
							route["method"] = "GET"
						case "path":
							route["path"] = "/api/v1/unrelated"
						case "fields":
							route["request_properties"] = []string{"operation"}
						}
					}
					routes = append(routes, route)
				}
				w.Header().Set("Content-Type", "application/json")
				data, err := json.Marshal(map[string]any{"version": tc.version, "delegated": tc.delegated, "routes": routes, "identity_operations": []string{"graph-link"}})
				assertions.NoError(err)
				_, err = w.Write(data)
				assertions.NoError(err)
			}))
			t.Cleanup(server.Close)
			session := mcpDraftTestSession(mcpDraftAgentContext(t, server, f.source.ID, []string{"identity.read", "identity.link", "identity.unlink"}), t)
			names := mcpDraftToolNames(t, session)
			for _, entry := range []struct {
				name string
				want bool
			}{{mcp.ToolPreviewIdentityOperation, tc.read}, {mcp.ToolGetIdentityReceipt, tc.read}, {mcp.ToolLinkParticipantIdentity, tc.apply}} {
				assertions.Equal(entry.want, slices.Contains(names, entry.name), entry.name)
			}
			assertions.NotContains(names, mcp.ToolLinkParticipantToPerson, "only the descriptor's admitted graph-link action is available")
			assertions.NotContains(names, mcp.ToolUnlinkParticipantIdentity)
			assertions.NotContains(names, mcp.ToolUnlinkParticipantFromPerson)
		})
	}
}
