package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"slices"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
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

func TestCardDAVOperationsDiscoverNativeContracts(t *testing.T) {
	backend, bookID, _ := cardDAVOperationFixture(t)
	for _, name := range []string{"list_carddav_connections", "list_carddav_books", "list_carddav_runs", "list_carddav_conflicts", "get_carddav_conflict", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"} {
		assert.Contains(t, backend.capabilities(), name)
	}
	result, err := backend.ExecuteOperation(t.Context(), "list_carddav_connections", nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "private-app")
	assert.NotContains(t, string(data), "oauth_app")
	result, err = backend.ExecuteOperation(t.Context(), "list_carddav_books", map[string]any{"connection": "work"})
	require.NoError(t, err)
	books := operationOutput[generated.CardDAVBooksResponse](t, result)
	require.Len(t, books.Books, 1)
	assert.Equal(t, bookID, books.Books[0].ID)
	result, err = backend.ExecuteOperation(t.Context(), "list_carddav_runs", map[string]any{"connection": "work", "limit": 1})
	require.NoError(t, err)
	runs := operationOutput[generated.CardDAVRunsResponse](t, result)
	require.Len(t, runs.Runs, 1)
	data, err = json.Marshal(runs)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "private diagnostic")
	result, err = backend.ExecuteOperation(t.Context(), "get_carddav_conflict", map[string]any{"conflict_id": -1})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestCardDAVRoleApprovalUsesRealStore(t *testing.T) {
	backend, id, _ := cardDAVOperationFixture(t)
	transfer, err := backend.OperationDisclosure(t.Context(), "update_carddav_book_roles", map[string]any{"book_id": id, "write_target": true, "subscribed": true, "lookup_source": true})
	require.NoError(t, err)
	assert.Contains(t, transfer, `"previous_write_target"`)
	assert.Contains(t, transfer, `"connection":"default"`)
	assert.Contains(t, transfer, "remove local CardDAV resource mappings")
	approved := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assert.Contains(t, req.Params.Message, `"connection":"work"`)
		assert.Contains(t, req.Params.Message, `"lookup_source":`)
		approved = true
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: map[string]any{"book_id": id, "write_target": false, "subscribed": false, "lookup_source": true}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.True(t, approved)
	got, err := backend.ExecuteOperation(t.Context(), "list_carddav_books", map[string]any{"connection": "work"})
	require.NoError(t, err)
	books := operationOutput[generated.CardDAVBooksResponse](t, got)
	require.Len(t, books.Books, 1)
	assert.True(t, books.Books[0].LookupSource)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: map[string]any{"book_id": id, "write_target": true, "subscribed": false, "lookup_source": false}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestCardDAVSelectorProofUsesNativeDiscovery(t *testing.T) {
	backend, _, _ := cardDAVOperationFixture(t)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	named := newDaemonMCPCardDAV(backend.client, caps)
	assert.True(t, named.SupportsCardDAVSelector(true))
	assert.True(t, named.SupportsCardDAVSelector(false))
	status, err := named.GetCardDAVConnectionStatus(t.Context(), new("work"))
	require.NoError(t, err)
	require.NotNil(t, status)
	require.NotNil(t, status.Account)
	assert.Equal(t, "work@example.com", status.Account.Username)
	all, err := named.GetCardDAVConnectionStatus(t.Context(), nil)
	require.NoError(t, err)
	assert.Nil(t, all.Account)
	// Both connections are disabled; aggregate sync is a no-op, while selecting
	// work reaches its unavailable runtime instead of silently syncing all.
	_, err = named.SyncCardDAVConnection(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = named.SyncCardDAVConnection(t.Context(), false, new("work"))
	assert.Error(t, err)
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "syncCardDAV" {
			caps.Routes[i].RequestProperties = slices.DeleteFunc(caps.Routes[i].RequestProperties, func(s string) bool { return s == "connection" })
		}
		if caps.Routes[i].OperationID == "getCardDAVStatus" {
			caps.Routes[i].QueryParameters = nil
		}
	}
	old := newDaemonMCPCardDAV(backend.client, caps)
	assert.False(t, old.SupportsCardDAVSelector(true))
	assert.False(t, old.SupportsCardDAVSelector(false))
	_, err = old.SyncCardDAVConnection(t.Context(), false, new("work"))
	assert.Error(t, err)
	_, err = old.GetCardDAVConnectionStatus(t.Context(), new("work"))
	assert.Error(t, err)
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "listCardDAVRuns" {
			caps.Routes[i].QueryParameters = []string{"limit", "before_id"}
		}
		if caps.Routes[i].OperationID == "updateCardDAVBookRoles" {
			caps.Routes[i].RequestProperties = []string{"write_target", "subscribed"}
		}
	}
	limited := newDaemonMCPOperations(backend.client, caps)
	assert.NotContains(t, limited.capabilities(), "list_carddav_runs")
	assert.NotContains(t, limited.capabilities(), "update_carddav_book_roles")
	assert.Contains(t, limited.capabilities(), "list_carddav_books")
}

func TestCardDAVApprovalRejectsChangedBook(t *testing.T) {
	backend, id, _ := cardDAVOperationFixture(t)
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	args := map[string]any{"book_id": id, "write_target": false, "subscribed": false, "lookup_source": true}
	first, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, first.RequestState)
	changed, err := backend.ExecuteOperation(t.Context(), "update_carddav_book_roles", map[string]any{"book_id": id, "write_target": false, "subscribed": true, "lookup_source": false})
	require.NoError(t, err)
	require.False(t, changed.IsError)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_carddav_book_roles", Arguments: args, RequestState: first.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	listed, err := backend.ExecuteOperation(t.Context(), "list_carddav_books", map[string]any{"connection": "work"})
	require.NoError(t, err)
	books := operationOutput[generated.CardDAVBooksResponse](t, listed)
	require.Len(t, books.Books, 1)
	assert.True(t, books.Books[0].Subscribed)
	assert.False(t, books.Books[0].LookupSource)
}

func TestCardDAVConflictReadsKeepDurableIdentity(t *testing.T) {
	backend, bookID, st := cardDAVOperationFixture(t)
	account, err := st.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(t, err)
	require.NotNil(t, account)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
	require.NoError(t, err)
	require.Len(t, books, 1)
	base := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic\r\nFN:Base Example\r\nEND:VCARD\r\n")
	href := "https://contacts.example/books/one/synthetic.vcf"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: bookID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, NextSyncToken: "synthetic-token", Upserts: []store.CardDAVRemoteResource{{Href: href, RemoteUID: "synthetic", RemoteETag: `"one"`, RemoteBody: base, SemanticHash: "synthetic-hash", DisplayName: "Base Example"}}})
	require.NoError(t, err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), bookID, href)
	require.NoError(t, err)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{AddressBookID: bookID, Href: href, ExpectedMappingRevision: mapping.MappingRevision, BaseLocalHash: mapping.LocalHash, LocalHash: mapping.LocalHash, BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag, RemoteETag: `"two"`, LocalBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic\r\nFN:Local Example\r\nEND:VCARD\r\n"), RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic\r\nFN:Remote Example\r\nEND:VCARD\r\n")})
	require.NoError(t, err)
	listed, err := backend.ExecuteOperation(t.Context(), "list_carddav_conflicts", nil)
	require.NoError(t, err)
	conflicts := operationOutput[generated.CardDAVConflictsResponse](t, listed)
	require.Len(t, conflicts.Conflicts, 1)
	assert.Equal(t, conflict.ID, conflicts.Conflicts[0].ID)
	detail, err := backend.ExecuteOperation(t.Context(), "get_carddav_conflict", map[string]any{"conflict_id": conflict.ID})
	require.NoError(t, err)
	view := operationOutput[generated.CardDAVConflictDetailResponse](t, detail)
	assert.Equal(t, conflict.ID, view.ID)
	assert.Equal(t, bookID, view.AddressBook.ID)
	require.NotNil(t, view.Local.DisplayName)
	assert.Equal(t, "Local Example", *view.Local.DisplayName)
	disclosure, err := backend.OperationDisclosure(t.Context(), "resolve_carddav_conflict", map[string]any{"conflict_id": conflict.ID, "choice": "keep_remote"})
	require.NoError(t, err)
	assert.Contains(t, disclosure, `"connection":"work"`)
	assert.Contains(t, disclosure, `"choice":"keep_remote"`)
	result, err := backend.ExecuteOperation(t.Context(), "resolve_carddav_conflict", map[string]any{"conflict_id": conflict.ID, "choice": "keep_remote"})
	require.NoError(t, err)
	require.True(t, result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "https://")
	assert.NotContains(t, string(encoded), "Local Example")
}

func TestCardDAVUnpublishDisclosesOwnerAndKeepsPersonOnUnavailableConnection(t *testing.T) {
	backend, bookID, st := cardDAVOperationFixture(t)
	require.NoError(t, st.SetCardDAVBookRolesContext(t.Context(), bookID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	participant, err := st.EnsureParticipant("person@example.com", "Synthetic Person", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	args := map[string]any{"person_id": person.ID}
	disclosure, err := backend.OperationDisclosure(t.Context(), "unpublish_carddav_person", args)
	require.NoError(t, err)
	assert.Contains(t, disclosure, `"connection":"work"`)
	assert.Contains(t, disclosure, "remove the published remote contact")
	approved := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCardDAV}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approved = true
		assert.Contains(t, request.Params.Message, `"connection":"work"`)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "unpublish_carddav_person", Arguments: args})
	require.NoError(t, err)
	assert.True(t, approved)
	assert.True(t, result.IsError)
	retained, err := st.GetPersonContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.Equal(t, person.ID, retained.ID)
}
