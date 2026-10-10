package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScopedCardDAVAPIUsesCurrentExactGrantAndReceipt(t *testing.T) {
	for _, mode := range []string{"allowed", "owner", "dispatching", "transport_removed", "wrong_book", "missing_key", "changed_token", "revoked_after_put", "receipt_not_found", "mcp_allowed", "mcp_dispatching", "mcp_transport_removed", "owner_restart"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			restart := mode == "owner_restart"
			useMCP := restart || strings.HasPrefix(mode, "mcp_")
			mode := strings.TrimPrefix(mode, "mcp_")
			if restart {
				mode = "owner"
			}
			st := testutil.NewTestStore(t)
			var mu sync.Mutex
			var body []byte
			var scoped atomic.Bool
			var puts, gets atomic.Int32
			var revoke atomic.Value
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPut:
					data, err := io.ReadAll(r.Body)
					assert.NoError(err)
					mu.Lock()
					body = data
					mu.Unlock()
					w.Header().Set("ETag", `"synthetic-api-receipt"`)
					w.WriteHeader(http.StatusCreated)
					if scoped.Load() {
						puts.Add(1)
						if fn, ok := revoke.Load().(func()); ok {
							fn()
						}
					}
				case http.MethodGet:
					if scoped.Load() {
						gets.Add(1)
					}
					mu.Lock()
					data := append([]byte{}, body...)
					mu.Unlock()
					w.Header().Set("ETag", `"synthetic-api-receipt"`)
					w.Header().Set("Content-Type", "text/vcard")
					_, err := w.Write(data)
					assert.NoError(err)
				default:
					assert.Fail("native publication fixture supports only mapped PUT and GET")
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			t.Cleanup(upstream.Close)
			target, err := url.Parse(upstream.URL)
			require.NoError(err)
			base := "http://contacts.example.test:" + target.Port()
			configured := config.CardDAVConfig{BaseURL: base + "/dav", Username: "synthetic-api-owner"}
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: configured.BaseURL, Username: configured.Username, PrincipalURL: base + "/principal/", HomeURL: base + "/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: base + "/books/synthetic/", DisplayName: "Synthetic Receipt Book", CanCreate: new(true), CanUpdate: new(true), CanDelete: new(true)}}})
			require.NoError(err)
			require.Len(books, 1)
			require.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
			participant, err := st.EnsureParticipant("receipt-api@example.test", "Synthetic API Person", "example.test")
			require.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participant)
			require.NoError(err)
			candidate, err := fixtureCardDAVFactory(t, target)(st, configured, "synthetic-api-password")
			require.NoError(err)
			require.NoError(candidate.PublishPerson(t.Context(), person.ID))
			current, err := st.GetPerson(person.ID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, current.Revision, new("Synthetic API Correction"))
			require.NoError(err)
			cfg := &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}, CardDAV: configured}
			controller := &CardDAVController{cfg: cfg, store: st, service: candidate}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), CardDAV: controller, OperationGate: NewSerialOperationGate()})
			grantBody := map[string]any{"label": "Synthetic receipt publisher", "permissions": []string{"person.read", "carddav.write"}, "person_ids": []int64{person.ID}}
			if mode != "wrong_book" {
				grantBody["address_book_ids"] = []int64{books[0].ID}
			}
			issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, grantBody)
			require.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
			var issued agentTokenIssueResponse
			require.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
			callRequest := func(method, path string, body any) *httptest.ResponseRecorder {
				if mode == "owner" {
					return identityNativeRequest(t, srv, method, path, agentTokenTestAPIKey, false, body)
				}
				return delegatedPersonNativeRequest(t, srv, method, path, issued.Secret, "", body)
			}
			discovery := callRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
			require.Equal(http.StatusOK, discovery.Code, "%s", discovery.Body.String())
			var capabilities apiprotocol.MCPCapabilities
			require.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
			var operationIDs []string
			for _, route := range capabilities.Routes {
				operationIDs = append(operationIDs, route.OperationID)
			}
			for _, operation := range []string{"previewScopedCardDAVPublication", "approveScopedCardDAVPublication", "reconcileScopedCardDAVPublication"} {
				if mode == "wrong_book" {
					assert.NotContains(operationIDs, operation)
				} else {
					assert.Contains(operationIDs, operation)
				}
			}
			var nativeClient *daemonclient.Client
			var daemonHTTP *httptest.Server
			if mode == "allowed" || mode == "dispatching" || useMCP {
				daemonHTTP = httptest.NewServer(srv.Router())
				t.Cleanup(daemonHTTP.Close)
				clientConfig := daemonclient.Config{URL: daemonHTTP.URL, AgentToken: issued.Secret, AllowInsecure: true, HTTPClient: daemonHTTP.Client()}
				if mode == "owner" {
					clientConfig.AgentToken = ""
					clientConfig.APIKey = agentTokenTestAPIKey
				}
				nativeClient, err = daemonclient.New(clientConfig)
				require.NoError(err)
				originalClient := nativeClient
				t.Cleanup(func() { assert.NoError(originalClient.Close()) })
			}
			var mcpSession *sdkmcp.ClientSession
			if useMCP {
				mcpSession = scopedCardDAVMCPSession(t, nativeClient)
			}
			path := fmt.Sprintf("/api/v1/carddav/scoped/publications/%d", person.ID)
			scoped.Store(true)
			previewResponse := callRequest(http.MethodGet, path+"/preview", nil)
			if mode == "wrong_book" {
				require.Equal(http.StatusForbidden, previewResponse.Code, "%s", previewResponse.Body.String())
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
				return
			}
			require.Equal(http.StatusOK, previewResponse.Code, "%s", previewResponse.Body.String())
			var preview CardDAVPublicationPreviewResponse
			require.NoError(json.Unmarshal(previewResponse.Body.Bytes(), &preview))
			if nativeClient != nil {
				nativePreview, err := nativeClient.PreviewScopedCardDAVPublication(t.Context(), person.ID)
				require.NoError(err)
				require.NotNil(nativePreview)
				assert.NotEmpty(nativePreview.ApprovalToken)
				assert.Equal(preview.VCard, nativePreview.Vcard)
			}
			assert.Zero(puts.Load())
			assert.Zero(gets.Load())
			if useMCP {
				result, err := mcpSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "preview_scoped_carddav_publication", Arguments: map[string]any{"person_id": person.ID}})
				require.NoError(err)
				require.False(result.IsError)
				data, err := json.Marshal(result.StructuredContent)
				require.NoError(err)
				require.NoError(json.Unmarshal(data, &preview))
				require.NotEmpty(preview.ApprovalToken)
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
			}
			request := map[string]any{"approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-api-key"}
			if mode == "receipt_not_found" {
				recovery := callRequest(http.MethodPost, path+"/reconcile", request)
				require.Equal(http.StatusNotFound, recovery.Code, "%s", recovery.Body.String())
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
				return
			}
			if mode == "missing_key" {
				delete(request, "idempotency_key")
			}
			if mode == "revoked_after_put" {
				revoke.Store(func() { assert.True(srv.agentGrants.Revoke(issued.ID)) })
			}
			if mode == "dispatching" {
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_api_receipt_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic API receipt failure'; END; $$`)
					require.NoError(err)
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_api_receipt_failure BEFORE UPDATE ON carddav_publication_receipts FOR EACH ROW EXECUTE FUNCTION synthetic_api_receipt_failure()`)
					require.NoError(err)
					t.Cleanup(func() {
						_, err := st.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS synthetic_api_receipt_failure ON carddav_publication_receipts`)
						assert.NoError(err)
						_, err = st.DB().ExecContext(context.Background(), `DROP FUNCTION IF EXISTS synthetic_api_receipt_failure()`)
						assert.NoError(err)
					})
				} else {
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_api_receipt_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic API receipt failure'); END`)
					require.NoError(err)
				}
			}
			if useMCP {
				args := map[string]any{"person_id": person.ID, "approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-api-key"}
				missing := scopedCardDAVMCPCall(t, mcpSession, "reconcile_scoped_carddav_publication", map[string]any{"person_id": person.ID, "approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-missing-key"}, true)
				require.True(missing.IsError)
				require.NotEmpty(missing.Content)
				missingText, ok := missing.Content[0].(*sdkmcp.TextContent)
				require.True(ok)
				assert.Contains(missingText.Text, "carddav_receipt_not_found")
				pending, err := mcpSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "approve_scoped_carddav_publication", Arguments: args})
				require.NoError(err)
				require.True(pending.NeedsInput())
				changedArgs := map[string]any{"person_id": person.ID, "approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-changed-key"}
				changed, err := mcpSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "approve_scoped_carddav_publication", Arguments: changedArgs, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
				require.NoError(err)
				require.True(changed.IsError, "confirmation must bind the original key and token")
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
				declined := scopedCardDAVMCPCall(t, mcpSession, "approve_scoped_carddav_publication", args, false)
				require.True(declined.IsError)
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
				applied := scopedCardDAVMCPCall(t, mcpSession, "approve_scoped_carddav_publication", args, true)
				require.False(applied.IsError)
			}
			response := callRequest(http.MethodPost, path+"/approve", request)
			if mode == "missing_key" {
				require.Equal(http.StatusBadRequest, response.Code, "%s", response.Body.String())
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
				return
			}
			if mode == "revoked_after_put" {
				require.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
				recovery := callRequest(http.MethodPost, path+"/reconcile", request)
				require.Equal(http.StatusUnauthorized, recovery.Code, "%s", recovery.Body.String())
				assert.Equal(int32(1), puts.Load())
				assert.Zero(gets.Load())
				pending, err := st.GetCardDAVPublicationContext(t.Context(), person.ID)
				require.NoError(err)
				assert.NotEmpty(pending.PendingIntentID)
				receipt, err := st.CardDAVPublicationReceiptContext(t.Context(), issued.ID, "synthetic-api-key", func(_ context.Context, _ *store.IdentityGrantSelection) error { return nil })
				require.NoError(err)
				assert.Equal("dispatching", receipt.State)
				return
			}
			wantStatus, wantState := http.StatusOK, "verified"
			if mode == "dispatching" {
				wantStatus, wantState = http.StatusAccepted, "dispatching"
			}
			require.Equal(wantStatus, response.Code, "%s", response.Body.String())
			var first struct {
				Receipt *store.CardDAVPublicationReceipt `json:"receipt"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &first))
			require.NotNil(first.Receipt)
			assert.Equal(wantState, first.Receipt.State)
			if useMCP {
				repeated := scopedCardDAVMCPCall(t, mcpSession, "approve_scoped_carddav_publication", map[string]any{"person_id": person.ID, "approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-api-key"}, true)
				require.False(repeated.IsError)
				data, err := json.Marshal(repeated.StructuredContent)
				require.NoError(err)
				var receipt CardDAVScopedPublicationReceiptResponse
				require.NoError(json.Unmarshal(data, &receipt))
				require.NotNil(receipt.Receipt)
				assert.Equal(first.Receipt.ID, receipt.Receipt.ID)
				assert.Equal(wantState, receipt.Receipt.State)
			}
			if nativeClient != nil {
				saved, err := nativeClient.ApproveScopedCardDAVPublication(t.Context(), person.ID, preview.ApprovalToken, "synthetic-api-key")
				require.NoError(err)
				require.NotNil(saved)
				assert.Equal(first.Receipt.ID, saved.Receipt.ID)
				assert.Equal(wantState, saved.Receipt.State)
			}
			assert.Equal(int32(1), puts.Load())
			assert.Equal(int32(1), gets.Load())
			if mode == "changed_token" {
				request["approval_token"] = "different-original-token"
			}
			if mode == "transport_removed" {
				srv.cardDAV = nil
			}
			if restart {
				require.NoError(mcpSession.Close())
				daemonHTTP.Close()
				require.NoError(nativeClient.Close())
				require.NoError(srv.Shutdown(t.Context()))
				st = reopenScopedCardDAVReceiptStore(t, st)
				// A new daemon owns only the durable archive. No CardDAV
				// controller, provider client or old grant registry survives.
				srv = NewServerWithOptions(ServerOptions{Config: &config.Config{Server: cfg.Server}, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
				t.Cleanup(func() { assert.NoError(srv.Shutdown(context.Background())) })
				expired := delegatedPersonNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", issued.Secret, "", nil)
				assert.Equal(http.StatusUnauthorized, expired.Code)
				daemonHTTP = httptest.NewServer(srv.Router())
				t.Cleanup(daemonHTTP.Close)
				nativeClient, err = daemonclient.New(daemonclient.Config{URL: daemonHTTP.URL, APIKey: agentTokenTestAPIKey, AllowInsecure: true, HTTPClient: daemonHTTP.Client()})
				require.NoError(err)
				reopenedClient := nativeClient
				t.Cleanup(func() { assert.NoError(reopenedClient.Close()) })
			}
			if useMCP && (mode == "transport_removed" || restart) {
				descriptor, err := nativeClient.MCPCapabilities(t.Context())
				require.NoError(err)
				var recoveryAdmitted bool
				for _, route := range descriptor.Routes {
					switch route.OperationID {
					case "previewScopedCardDAVPublication", "approveScopedCardDAVPublication":
						assert.Fail("provider publication routes require their controller")
					case "reconcileScopedCardDAVPublication":
						recoveryAdmitted = true
						assert.Equal(http.MethodPost, route.Method)
						assert.Equal("/api/v1/carddav/scoped/publications/{person_id}/reconcile", route.Path)
						assert.Contains(route.RequestProperties, "approval_token")
						assert.Contains(route.RequestProperties, "idempotency_key")
					}
				}
				require.True(recoveryAdmitted, "settled receipt recovery must remain discoverable without a CardDAV controller")
				// Construct a new SDK session only from the admitted contract.
				mcpSession = scopedCardDAVMCPSession(t, nativeClient, mcpserver.ServeOptions{ScopedCardDAVReconcile: nativeClient, AllowCardDAVWrites: true, CalendarOnly: true})
				tools, err := mcpSession.ListTools(t.Context(), nil)
				require.NoError(err)
				require.Len(tools.Tools, 1)
				assert.Equal("reconcile_scoped_carddav_publication", tools.Tools[0].Name)
				assert.Equal(int32(1), puts.Load())
				assert.Equal(int32(1), gets.Load(), "restart and discovery must perform no provider work")
			}

			repeat := callRequest(http.MethodPost, path+"/approve", request)
			if mode == "changed_token" {
				require.Equal(http.StatusConflict, repeat.Code, "%s", repeat.Body.String())
			} else {
				require.Equal(wantStatus, repeat.Code, "%s", repeat.Body.String())
				var saved struct {
					Receipt *store.CardDAVPublicationReceipt `json:"receipt"`
				}
				require.NoError(json.Unmarshal(repeat.Body.Bytes(), &saved))
				require.NotNil(saved.Receipt)
				assert.Equal(first.Receipt.ID, saved.Receipt.ID)
			}
			assert.Equal(int32(1), puts.Load())
			assert.Equal(int32(1), gets.Load())
			if mode == "dispatching" {
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_api_receipt_failure ON carddav_publication_receipts`)
				} else {
					_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_api_receipt_failure`)
				}
				require.NoError(err)
			}
			if useMCP {
				result := scopedCardDAVMCPCall(t, mcpSession, "reconcile_scoped_carddav_publication", map[string]any{"person_id": person.ID, "approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-api-key"}, true)
				require.False(result.IsError)
				data, err := json.Marshal(result.StructuredContent)
				require.NoError(err)
				var receipt CardDAVScopedPublicationReceiptResponse
				require.NoError(json.Unmarshal(data, &receipt))
				require.NotNil(receipt.Receipt)
				assert.Equal(first.Receipt.ID, receipt.Receipt.ID)
				assert.Equal("verified", receipt.Receipt.State)
			}
			if nativeClient != nil {
				recovered, err := nativeClient.ReconcileScopedCardDAVPublication(t.Context(), person.ID, preview.ApprovalToken, "synthetic-api-key")
				require.NoError(err)
				require.NotNil(recovered)
				assert.Equal(first.Receipt.ID, recovered.Receipt.ID)
				assert.Equal("verified", recovered.Receipt.State)
			}
			recovery := callRequest(http.MethodPost, path+"/reconcile", request)
			if mode == "changed_token" {
				require.Equal(http.StatusConflict, recovery.Code, "%s", recovery.Body.String())
			} else {
				require.Equal(http.StatusOK, recovery.Code, "%s", recovery.Body.String())
				var recovered CardDAVScopedPublicationReceiptResponse
				require.NoError(json.Unmarshal(recovery.Body.Bytes(), &recovered))
				require.NotNil(recovered.Receipt)
				assert.Equal(first.Receipt.ID, recovered.Receipt.ID)
				assert.Equal("verified", recovered.Receipt.State)
			}
			wantGets := int32(1)
			if mode == "dispatching" {
				wantGets++
			}
			assert.Equal(int32(1), puts.Load(), "recovery never replays PUT")
			assert.Equal(wantGets, gets.Load())
			if useMCP && !restart {
				require.True(srv.agentGrants.Revoke(issued.ID))
				denied := scopedCardDAVMCPCall(t, mcpSession, "reconcile_scoped_carddav_publication", map[string]any{"person_id": person.ID, "approval_token": preview.ApprovalToken, "idempotency_key": "synthetic-api-key"}, true)
				require.True(denied.IsError)
				assert.Equal(int32(1), puts.Load())
				assert.Equal(wantGets, gets.Load(), "revocation denies even saved receipts before provider work")
			}
		})
	}
}

// The MCP SDK client drives production tool dispatch and confirmation; the
// backend is the real authenticated daemon HTTP client, Store and DAV service.
func scopedCardDAVMCPSession(t *testing.T, backend *daemonclient.Client, admitted ...mcpserver.ServeOptions) *sdkmcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	opts := mcpserver.ServeOptions{ScopedCardDAVPreview: backend, ScopedCardDAVApprove: backend, ScopedCardDAVReconcile: backend, AllowCardDAVWrites: true, CalendarOnly: true}
	if len(admitted) > 0 {
		opts = admitted[0]
	}
	go func() {
		done <- mcpserver.ServeTransport(ctx, opts, serverTransport)
	}()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-carddav-client", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, session.Close()); cancel(); <-done })
	return session
}

func scopedCardDAVMCPCall(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any, confirm bool) *sdkmcp.CallToolResult {
	t.Helper()
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.True(t, pending.NeedsInput())
	require.NotEmpty(t, pending.RequestState)
	response := &sdkmcp.ElicitResult{Action: "decline"}
	if confirm {
		response = &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}
	}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": response}})
	require.NoError(t, err)
	return result
}

// Reopen the fixture's real database, preserving only committed native data.
// This covers both a disk SQLite archive and the owned PostgreSQL test database.
func reopenScopedCardDAVReceiptStore(t *testing.T, original *store.Store) *store.Store {
	t.Helper()
	var databasePath string
	if original.IsPostgreSQL() {
		databaseURL, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		var database, schema string
		require.NoError(t, original.DB().QueryRowContext(t.Context(), "SELECT current_database(), current_schema()").Scan(&database, &schema))
		databaseURL.Path = "/" + database
		query := databaseURL.Query()
		query.Set("search_path", schema)
		databaseURL.RawQuery = query.Encode()
		databasePath = databaseURL.String()
	} else {
		var sequence int
		var name string
		require.NoError(t, original.DB().QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&sequence, &name, &databasePath))
		require.Equal(t, "main", name)
		require.NotEmpty(t, databasePath)
	}
	require.NoError(t, original.Close())
	reopened, err := store.Open(databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	return reopened
}
