package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func cardDAVOperationFixture(t *testing.T) (*daemonMCPOperations, int64, *store.Store) {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.CardDAV = config.CardDAVConfig{BaseURL: "https://contacts.example/default", Username: "default@example.com"}
	cfg.CardDAVConnections = map[string]config.CardDAVConfig{"work": {BaseURL: "https://contacts.example/dav", Username: "work@example.com", OAuthApp: "private-app"}}
	st := testutil.NewSQLiteTestStore(t)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "default", BaseURL: "https://contacts.example/default", Username: "default@example.com", PrincipalURL: "https://contacts.example/default/principal/", HomeURL: "https://contacts.example/default/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/default/books/one/", DisplayName: "Default", CanCreate: new(true)}}})
	require.NoError(t, err)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "work", BaseURL: "https://contacts.example/dav", Username: "work@example.com", PrincipalURL: "https://contacts.example/principal/", HomeURL: "https://contacts.example/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/one/", DisplayName: "Example", CanCreate: new(true)}}})
	require.NoError(t, err)
	require.Len(t, books, 1)
	run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: account.ID, Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(t, err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunFailed, ErrorCode: "synthetic", ErrorMessage: "private diagnostic"})
	require.NoError(t, err)
	controller, err := api.NewCardDAVController(cfg, st, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, CardDAV: controller, Logger: slog.New(slog.DiscardHandler)}))
	return backend, books[0].ID, st
}

func TestCardDAVRoleApprovalUsesRealStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend, id, st := cardDAVOperationFixture(t)
	transfer, err := backend.OperationDisclosure(t.Context(), "update_carddav_book_roles", map[string]any{"book_id": id, "write_target": true, "subscribed": true, "lookup_source": true})
	require.NoError(err)
	assert.Contains(transfer, `"previous_write_target"`)
	assert.Contains(transfer, "removing subscription can delete untouched imported local people")
	approved := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assert.Contains(req.Params.Message, `"connection":"work"`)
		assert.Contains(req.Params.Message, `"lookup_source":`)
		approved = true
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: map[string]any{"book_id": id, "write_target": false, "subscribed": false, "lookup_source": true}})
	require.NoError(err)
	require.False(result.IsError)
	assert.True(approved)
	got, err := backend.ExecuteOperation(t.Context(), "list_carddav_books", map[string]any{"connection": "work"})
	require.NoError(err)
	books := operationOutput[generated.CardDAVBooksResponse](t, got)
	require.Len(books.Books, 1)
	assert.True(books.Books[0].LookupSource)
	changedDisclosure, err := backend.OperationDisclosure(t.Context(), "update_carddav_book_roles", map[string]any{"book_id": id, "write_target": false, "subscribed": false, "lookup_source": true})
	require.NoError(err)
	assert.Contains(changedDisclosure, `"lookup_source":true`)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), id, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	selfDisclosure, err := backend.OperationDisclosure(t.Context(), "update_carddav_book_roles", map[string]any{"book_id": id, "write_target": true, "subscribed": true, "lookup_source": true})
	require.NoError(err)
	assert.NotContains(selfDisclosure, `"previous_write_target"`)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: map[string]any{"book_id": id, "write_target": true, "subscribed": false, "lookup_source": false}})
	require.NoError(err)
	failure := loopToolFailure(t, result)
	assert.Equal("conflict", failure["error"])
	assert.Equal(false, failure["operation_may_have_completed"])
}

func TestCardDAVSelectorProofUsesNativeDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend, bookID, _ := cardDAVOperationFixture(t)
	for _, name := range []string{"list_carddav_connections", "list_carddav_books", "list_carddav_runs", "list_carddav_conflicts", "get_carddav_conflict", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"} {
		assert.Contains(backend.capabilities(), name)
	}
	result, err := backend.ExecuteOperation(t.Context(), "list_carddav_connections", nil)
	require.NoError(err)
	require.NotNil(result)
	assert.False(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.NotContains(string(data), "private-app")
	assert.NotContains(string(data), "oauth_app")
	result, err = backend.ExecuteOperation(t.Context(), "list_carddav_books", map[string]any{"connection": "work"})
	require.NoError(err)
	books := operationOutput[generated.CardDAVBooksResponse](t, result)
	require.Len(books.Books, 1)
	assert.Equal(bookID, books.Books[0].ID)
	result, err = backend.ExecuteOperation(t.Context(), "list_carddav_runs", map[string]any{"connection": "work", "limit": 1})
	require.NoError(err)
	runs := operationOutput[generated.CardDAVRunsResponse](t, result)
	require.Len(runs.Runs, 1)
	data, err = json.Marshal(runs)
	require.NoError(err)
	assert.NotContains(string(data), "private diagnostic")
	result, err = backend.ExecuteOperation(t.Context(), "get_carddav_conflict", map[string]any{"conflict_id": -1})
	require.NoError(err)
	assert.True(result.IsError)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(err)
	named := newDaemonMCPCardDAV(backend.client, caps)
	assert.True(named.SupportsCardDAVSelector())
	var prompts []string
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		prompts = append(prompts, req.Params.Message)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	selectedStatus, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_carddav_sync_status", Arguments: map[string]any{"connection": "work"}})
	require.NoError(err)
	require.False(selectedStatus.IsError)
	encoded, err := json.Marshal(selectedStatus.StructuredContent)
	require.NoError(err)
	assert.Contains(string(encoded), "work@example.com")
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_carddav", Arguments: map[string]any{"connection": "work"}})
	require.NoError(err)
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_carddav", Arguments: map[string]any{}})
	require.NoError(err)
	require.Len(prompts, 2)
	assert.Contains(prompts[0], `connection "work"`)
	assert.Contains(prompts[1], "Account details unavailable.")
	assert.NotContains(prompts[1], "private-app")
	assert.NotContains(prompts[1], "contacts.example")
	status, err := named.GetCardDAVConnectionStatus(t.Context(), new("work"))
	require.NoError(err)
	require.NotNil(status)
	require.NotNil(status.Account)
	assert.Equal("work@example.com", status.Account.Username)
	all, err := named.GetCardDAVConnectionStatus(t.Context(), nil)
	require.NoError(err)
	assert.Nil(all.Account)
	// Neither disabled connection has a sync runtime; aggregate sync has no
	// eligible connections and the named sync also returns unavailable.
	_, err = named.SyncCardDAVConnection(t.Context(), false, nil)
	require.ErrorContains(err, "API error (503)")
	_, err = named.SyncCardDAVConnection(t.Context(), false, new("work"))
	require.ErrorContains(err, "API error (503)")
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "syncCardDAV" {
			caps.Routes[i].RequestProperties = slices.DeleteFunc(caps.Routes[i].RequestProperties, func(s string) bool { return s == "connection" })
		}
	}
	old := newDaemonMCPCardDAV(backend.client, caps)
	assert.False(old.SupportsCardDAVSelector())
	prompted := false
	legacySession := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		prompted = true
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}}, old)
	for _, tool := range []string{"sync_carddav", "get_carddav_sync_status"} {
		refused, callErr := legacySession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: map[string]any{"connection": "work"}})
		require.NoError(callErr)
		assert.True(refused.IsError)
		require.Len(refused.Content, 1)
		content, ok := refused.Content[0].(*sdkmcp.TextContent)
		require.True(ok)
		assert.Contains(content.Text, "named CardDAV connection is not supported")
	}
	assert.False(prompted)
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "syncCardDAV" {
			caps.Routes[i].RequestProperties = append(caps.Routes[i].RequestProperties, "connection")
		}
		if caps.Routes[i].OperationID == "getCardDAVStatus" {
			caps.Routes[i].QueryParameters = nil
		}
	}
	assert.False(newDaemonMCPCardDAV(backend.client, caps).SupportsCardDAVSelector())
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "listCardDAVRuns" {
			caps.Routes[i].QueryParameters = []string{"limit", "before_id"}
		}
		if caps.Routes[i].OperationID == "updateCardDAVBookRoles" {
			caps.Routes[i].RequestProperties = []string{"write_target", "subscribed"}
		}
	}
	limited := newDaemonMCPOperations(backend.client, caps)
	assert.NotContains(limited.capabilities(), "list_carddav_runs")
	assert.NotContains(limited.capabilities(), "update_carddav_book_roles")
	assert.Contains(limited.capabilities(), "list_carddav_books")
	for _, tc := range []struct {
		missing string
		dropped []string
	}{
		{"listCardDAVBooks", []string{"list_carddav_books", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"}},
		{"getCardDAVConflict", []string{"get_carddav_conflict", "resolve_carddav_conflict"}},
		{"getCardDAVPublication", []string{"unpublish_carddav_person"}},
	} {
		copyCaps := *caps
		copyCaps.Routes = slices.DeleteFunc(slices.Clone(caps.Routes), func(r apiprotocol.MCPRouteDescriptor) bool { return r.OperationID == tc.missing })
		pruned := newDaemonMCPOperations(backend.client, &copyCaps).capabilities()
		for _, name := range tc.dropped {
			assert.NotContains(pruned, name, tc.missing)
		}
		assert.Contains(pruned, "list_carddav_connections")
	}

	// A fixed daemon error is an external contract: exercise the adapter's
	// classification without needing a race after a native commit.
	var daemonCode string
	fake := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, daemonCode)
	})
	adapter := &daemonMCPOperations{client: fake, supported: []string{"update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"}}
	for _, tc := range []struct {
		name, code, want string
		args             map[string]any
		uncertain        bool
	}{
		{"update_carddav_book_roles", "not_found", "not_found", map[string]any{"book_id": 1, "write_target": true, "subscribed": true, "lookup_source": true}, true},
		{"update_carddav_book_roles", "conflict", "conflict", map[string]any{"book_id": 1, "write_target": true, "subscribed": true, "lookup_source": true}, false},
		{"resolve_carddav_conflict", "not_found", "not_found", map[string]any{"conflict_id": 1, "choice": "keep_remote"}, false},
		{"resolve_carddav_conflict", "not_found", "not_found", map[string]any{"conflict_id": 1, "choice": "keep_local"}, true},
		{"resolve_carddav_conflict", "carddav_conflict_stale", "carddav_conflict_stale", map[string]any{"conflict_id": 1, "choice": "keep_remote"}, false},
		{"resolve_carddav_conflict", "carddav_conflict_stale", "carddav_conflict_stale", map[string]any{"conflict_id": 1, "choice": "keep_local"}, true},
		{"resolve_carddav_conflict", "carddav_remote_protected", "carddav_remote_protected", map[string]any{"conflict_id": 1, "choice": "keep_remote"}, false},
		{"resolve_carddav_conflict", "carddav_publication_pending", "carddav_publication_pending", map[string]any{"conflict_id": 1, "choice": "keep_remote"}, false},
		{"unpublish_carddav_person", "carddav_publication_pending", "carddav_publication_pending", map[string]any{"person_id": 1}, true},
		{"unpublish_carddav_person", "conflict", "conflict", map[string]any{"person_id": 1}, true},
		{"unpublish_carddav_person", "server_busy", "server_busy", map[string]any{"person_id": 1}, false},
		{"resolve_carddav_conflict", "server_busy", "server_busy", map[string]any{"conflict_id": 1, "choice": "keep_local"}, false},
	} {
		daemonCode = tc.code
		got, callErr := adapter.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(callErr)
		assertLoopRefusal(t, got, tc.want, tc.uncertain)
	}
}

func TestCardDAVConflictReadsKeepDurableIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend, bookID, st := cardDAVOperationFixture(t)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), bookID, store.CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true}))
	account, err := st.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	require.NotNil(account)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
	require.NoError(err)
	require.Len(books, 1)
	base := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic\r\nFN:Base Example\r\nEND:VCARD\r\n")
	href := "https://contacts.example/books/one/synthetic.vcf"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: bookID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, NextSyncToken: "synthetic-token", Upserts: []store.CardDAVRemoteResource{{Href: href, RemoteUID: "synthetic", RemoteETag: `"one"`, RemoteBody: base, SemanticHash: "synthetic-hash", DisplayName: "Base Example"}}})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), bookID, href)
	require.NoError(err)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{AddressBookID: bookID, Href: href, ExpectedMappingRevision: mapping.MappingRevision, BaseLocalHash: mapping.LocalHash, LocalHash: mapping.LocalHash, BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag, RemoteETag: `"two"`, LocalBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic\r\nFN:Local Example\r\nEND:VCARD\r\n"), RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic\r\nFN:Remote Example\r\nEND:VCARD\r\n")})
	require.NoError(err)
	listed, err := backend.ExecuteOperation(t.Context(), "list_carddav_conflicts", nil)
	require.NoError(err)
	conflicts := operationOutput[generated.CardDAVConflictsResponse](t, listed)
	require.Len(conflicts.Conflicts, 1)
	assert.Equal(conflict.ID, conflicts.Conflicts[0].ID)
	detail, err := backend.ExecuteOperation(t.Context(), "get_carddav_conflict", map[string]any{"conflict_id": conflict.ID})
	require.NoError(err)
	view := operationOutput[generated.CardDAVConflictDetailResponse](t, detail)
	assert.Equal(conflict.ID, view.ID)
	assert.Equal(bookID, view.AddressBook.ID)
	require.NotNil(view.Local.DisplayName)
	assert.Equal("Local Example", *view.Local.DisplayName)
	disclosure, err := backend.OperationDisclosure(t.Context(), "resolve_carddav_conflict", map[string]any{"conflict_id": conflict.ID, "choice": "keep_remote"})
	require.NoError(err)
	assert.Contains(disclosure, `"choice":"keep_remote"`)
	assert.Contains(disclosure, "keeping the remote version; this can change the local person")
	assert.NotContains(disclosure, "remove a remote contact")
	localDisclosure, err := backend.OperationDisclosure(t.Context(), "resolve_carddav_conflict", map[string]any{"conflict_id": conflict.ID, "choice": "keep_local"})
	require.NoError(err)
	assert.Contains(localDisclosure, "keeping the local version; this can create, update or remove a remote contact")
	result, err := backend.ExecuteOperation(t.Context(), "resolve_carddav_conflict", map[string]any{"conflict_id": conflict.ID, "choice": "keep_remote"})
	require.NoError(err)
	assertLoopRefusal(t, result, "carddav_unavailable", false)
}

func TestCardDAVUnpublishDisclosesOwnerAndKeepsPersonOnUnavailableConnection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend, bookID, st := cardDAVOperationFixture(t)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), bookID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	participant, err := st.EnsureParticipant("person@example.com", "Synthetic Person", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	args := map[string]any{"person_id": person.ID}
	approved := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approved = true
		assert.Contains(request.Params.Message, `"connection":"work"`)
		assert.Contains(request.Params.Message, "unpublish the person; this can remove a remote contact while retaining the local person")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "unpublish_carddav_person", Arguments: args})
	require.NoError(err)
	assert.True(approved)
	failure := loopToolFailure(t, result)
	assert.Equal("carddav_unavailable", failure["error"])
	assert.Equal(false, failure["operation_may_have_completed"])
}
