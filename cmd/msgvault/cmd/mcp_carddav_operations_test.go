package cmd

import (
	"context"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPCardDAVActualDaemonDiscoveryAndArgumentRefusals(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	fixture := storetest.New(t)
	controller, err := api.NewCardDAVController(cfg, fixture.Store, slog.New(slog.DiscardHandler))
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: fixture.Store}, CardDAV: controller, Logger: slog.New(slog.DiscardHandler)}))
	status, err := backend.client.GetCardDAVSyncStatus(t.Context())
	requirements.NoError(err)
	requirements.NotNil(status)
	assertions.False(status.Configured)
	assertions.False(status.Available)
	for _, name := range []string{"list_carddav_books", "list_carddav_runs", "get_carddav_connection_status", "sync_carddav_connections", "update_carddav_book_roles", "list_carddav_conflicts", "get_carddav_conflict", "resolve_carddav_conflict", "unpublish_carddav_person"} {
		assertions.Contains(backend.capabilities(), name)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_carddav_connection_status", nil)
	requirements.NoError(err)
	assertions.False(result.IsError)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, dependency := range []struct{ id, tool string }{{"getCardDAVPublication", "unpublish_carddav_person"}, {"getCardDAVConflict", "resolve_carddav_conflict"}, {"listCardDAVBooks", "update_carddav_book_roles"}, {"getCardDAVStatus", "sync_carddav_connections"}} {
		limited := *capabilities
		limited.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == dependency.id })
		assertions.NotContains(newDaemonMCPOperations(backend.client, &limited).capabilities(), dependency.tool, "a write requires its actual disclosure route")
	}
	for _, args := range []map[string]any{{"connection": ""}, {"connection": nil}, {"connection": "unknown"}, {"connection": "../default"}, {"limit": 101}, {"limit": 0}, {"before_id": 0}, {"before_id": nil}, {"env": map[string]any{}}} {
		result, err = backend.ExecuteOperation(t.Context(), "list_carddav_runs", args)
		requirements.NoError(err)
		assertions.True(result.IsError, "%v", args)
	}
	if backend.SupportsCardDAVConnections() {
		return
	}
	for _, name := range []string{"list_carddav_books", "list_carddav_runs", "get_carddav_connection_status"} {
		result, err = backend.ExecuteOperation(t.Context(), name, map[string]any{"connection": "default"})
		requirements.NoError(err)
		assertions.True(result.IsError, "old daemon must not silently ignore connection: %s", name)
	}
}

// An isolated test process reads the synthetic vendor CA before Go caches
// system roots. Production TLS verification and credential loading stay active.
func TestMCPCardDAVProtocolUsesRealTLSAndDurableHistory(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	if os.Getenv("MSGVAULT_MCP_CARDDAV_PROTOCOL_CHILD") != "1" {
		vendor := httptest.NewTLSServer(http.NotFoundHandler())
		t.Cleanup(vendor.Close)
		certificate := filepath.Join(t.TempDir(), "vendor-ca.pem")
		requirements.NoError(os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: vendor.Certificate().Raw}), 0o600))
		executable, err := os.Executable()
		requirements.NoError(err)
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestMCPCardDAVProtocolUsesRealTLSAndDurableHistory$", "-test.v")
		command.Env = append(os.Environ(), "MSGVAULT_MCP_CARDDAV_PROTOCOL_CHILD=1", "SSL_CERT_FILE="+certificate)
		output, err := command.CombinedOutput()
		requirements.NoError(err, "%s", output)
		if strings.Contains(string(output), "--- SKIP:") {
			t.Skip("the isolated TLS fixture requires a private interface for its explicit destination pin")
		}
		return
	}
	var calls atomic.Int32
	var failDelete atomic.Bool
	addresses, err := net.InterfaceAddrs()
	requirements.NoError(err)
	var privateAddress string
	for _, address := range addresses {
		prefix, parseErr := netip.ParsePrefix(address.String())
		if parseErr == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() {
			privateAddress = prefix.Addr().String()
			break
		}
	}
	if privateAddress == "" {
		t.Skip("real configured CardDAV TLS protocol requires a private interface for the explicit destination pin")
	}
	vendor := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		user, password, ok := r.BasicAuth()
		assertions.True(ok)
		assertions.Equal("synthetic", user)
		assertions.Equal("synthetic-carddav-secret", password)
		if failDelete.Load() && r.Method == http.MethodDelete {
			http.Error(w, "synthetic-carddav-private-body", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		_, err := fmt.Fprint(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"></D:multistatus>`)
		assertions.NoError(err)
	}))
	requirements.NoError(vendor.Listener.Close())
	vendor.Listener, err = net.Listen("tcp", net.JoinHostPort(privateAddress, "0"))
	requirements.NoError(err)
	vendor.StartTLS()
	t.Cleanup(vendor.Close)
	vendorURL, err := url.Parse(vendor.URL)
	requirements.NoError(err)
	origin := "https://example.com:" + vendorURL.Port()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = t.TempDir(), t.TempDir()
	cfg.CardDAV = config.CardDAVConfig{BaseURL: origin + "/dav", Username: "synthetic", Enabled: false, TrustedOrigin: origin, TrustedAddresses: []string{privateAddress}}
	fixture := storetest.New(t)
	account, books, err := fixture.Store.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, PrincipalURL: origin + "/principal/", HomeURL: origin + "/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: origin + "/books/synthetic/", DisplayName: "Synthetic book", CanCreate: new(true)}}})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, Password: "synthetic-carddav-secret", ConnectionGeneration: account.ConnectionGeneration}))
	requirements.NoError(cfg.Save())
	controller, err := api.NewCardDAVController(cfg, fixture.Store, slog.New(slog.DiscardHandler))
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: fixture.Store}, CardDAV: controller, Logger: slog.New(slog.DiscardHandler)}))
	accept := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		action := "decline"
		if accept {
			action = "accept"
		}
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": accept}}, nil
	}})
	roles := map[string]any{"book_id": books[0].ID, "write_target": false, "subscribed": true, "lookup_source": false}
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: roles})
	requirements.NoError(err)
	assertions.True(called.IsError)
	current, err := fixture.Store.ListCardDAVAddressBooksContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(books[0].IsSubscribed, current[0].IsSubscribed)
	accept = true
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: roles})
	requirements.NoError(err)
	assertions.False(called.IsError)
	assertions.Equal(int32(0), calls.Load())
	syncArgs := map[string]any{"full": true}
	if backend.SupportsCardDAVConnections() {
		syncArgs["connection"] = "default"
	}
	accept = false
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_carddav_connections", Arguments: syncArgs})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(int32(0), calls.Load())
	accept = true
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_carddav_connections", Arguments: syncArgs})
	requirements.NoError(err)
	requirements.False(called.IsError, "%+v", called)
	assertions.Positive(calls.Load(), "explicit manual sync must work while disabled")
	result, err := backend.ExecuteOperation(t.Context(), "get_carddav_connection_status", nil)
	requirements.NoError(err)
	status := operationOutput[mcpserver.CardDAVStatus](t, result)
	requirements.NotNil(status.LatestSuccessful)
	assertions.Equal("succeeded", status.LatestSuccessful.State)
	assertions.Equal(int64(1), status.LatestSuccessful.Books)
	assertions.False(status.Enabled)
	for range 26 {
		run, runErr := fixture.Store.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
		requirements.NoError(runErr)
		_, runErr = fixture.Store.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunSucceeded})
		requirements.NoError(runErr)
	}
	result, err = backend.ExecuteOperation(t.Context(), "list_carddav_runs", nil)
	requirements.NoError(err)
	history := operationOutput[mcpserver.CardDAVRuns](t, result)
	assertions.Len(history.Runs, 25)
	requirements.NotNil(history.NextBeforeID)
	result, err = backend.ExecuteOperation(t.Context(), "list_carddav_runs", map[string]any{"before_id": *history.NextBeforeID})
	requirements.NoError(err)
	assertions.Len(operationOutput[mcpserver.CardDAVRuns](t, result).Runs, 2)
	active, err := fixture.Store.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	requirements.NoError(err)
	before := calls.Load()
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_carddav_connections", Arguments: syncArgs})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(before, calls.Load())
	_, err = fixture.Store.FinishCardDAVSyncRunContext(t.Context(), active.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunCancelled, ErrorCode: "cancelled", ErrorMessage: "Synthetic cancellation"})
	requirements.NoError(err)
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	assertions.NotContains(string(data), "run_id")
	assertions.NotContains(string(data), "synthetic-carddav-secret")
	// A lost remote delete leaves durable queued work. The MCP failure must
	// include the owning publication state instead of erasing that receipt.
	requirements.NoError(fixture.Store.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	participant, err := fixture.Store.EnsureParticipant("synthetic@example.com", "Synthetic Published", "example.com")
	requirements.NoError(err)
	person, _, err := fixture.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	snapshot, err := fixture.Store.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Synthetic Published\r\nEND:VCARD\r\n")
	remote := store.CardDAVRemoteResource{Href: books[0].CanonicalURL + "synthetic.vcf", RemoteUID: person.VCardUID, RemoteETag: `"synthetic"`, RemoteBody: body, SemanticHash: "synthetic-published-hash", DisplayName: "Synthetic Published"}
	pending, err := fixture.Store.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: person.ID, Desired: true, AddressBookID: books[0].ID, Href: remote.Href, OutgoingBody: body, OutgoingSemanticHash: remote.SemanticHash, LocalHash: snapshot.Fingerprint})
	requirements.NoError(err)
	requirements.NoError(fixture.Store.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *pending, Remote: remote}))
	failDelete.Store(true)
	before = calls.Load()
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "unpublish_carddav_person", Arguments: map[string]any{"person_id": person.ID}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(before+1, calls.Load(), "MCP must not retry an uncertain remote delete")
	publication, err := fixture.Store.GetCardDAVPublicationContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.False(publication.Desired)
	assertions.Equal(store.CardDAVMutationDelete, publication.PendingOperation)
	data, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var failure struct {
		Publication *struct {
			Desired          bool   `json:"desired"`
			PendingOperation string `json:"pending_operation"`
		} `json:"publication"`
	}
	requirements.NoError(json.Unmarshal(data, &failure))
	requirements.NotNil(failure.Publication, "%s", data)
	assertions.False(failure.Publication.Desired)
	assertions.Equal("delete", failure.Publication.PendingOperation)
	assertions.NotContains(string(data), "synthetic-carddav-private-body")
}

func FuzzMCPCardDAVConnectionSelectorMatchesDeclaredContract(f *testing.F) {
	for _, value := range []string{"default", "work", "a_b-1", "", "../default", "a b", "A", "a" + strings.Repeat("b", 64)} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		requirements := require.New(t)
		assertions := assert.New(t)
		backend := &daemonMCPOperations{cardDAVNamed: true}
		valid := len(value) > 0 && len(value) <= 64 && value[0] >= 'a' && value[0] <= 'z'
		for i := 1; i < len(value); i++ {
			c := value[i]
			valid = valid && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-')
		}
		query, err := backend.cardDAVQuery(&value)
		if valid {
			requirements.NoError(err)
			assertions.Equal(value, query["connection"])
		} else {
			requirements.Error(err)
			assertions.Nil(query)
		}
	})
}
