package api

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEmailFamilyHTTPReadsAndErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	source, err := st.GetOrCreateSource("imap", "family@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	for _, tc := range []struct {
		path   string
		status int
	}{{fmt.Sprintf("/api/v1/messages/%d/family", id), 200}, {"/api/v1/messages/999999/family", 404}, {"/api/v1/messages/invalid/family", 400}, {fmt.Sprintf("/api/v1/messages/%d/family?source_id=99999", id), 404}, {fmt.Sprintf("/api/v1/messages/%d/family?source_id=bad", id), 400}} {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		assert.Equal(tc.status, w.Code, w.Body.String())
		if tc.status == 200 {
			assert.Contains(w.Body.String(), "anchor_message_id")
			assert.Equal("no-store", w.Header().Get("Cache-Control"))
		}
	}
}

func TestEmailFamilyHTTPRequiresOwner(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	_, secret, _, err := reg.Issue("family-read", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "owner@example.test"}}, time.Time{})
	require.NoError(t, err)
	for i, route := range []struct{ method, path string }{{"GET", "/api/v1/messages/1/family"}, {"POST", "/api/v1/messages/family/decisions/preview"}, {"POST", "/api/v1/messages/family/decisions"}, {"POST", "/api/v1/messages/family/decisions/1/undo"}, {"GET", "/api/v1/messages/family/decisions?message_id=1"}, {"GET", "/api/v1/messages/family/decisions/1"}} {
		for _, tc := range []struct {
			header, value string
			status        int
		}{{"", "", 401}, {apiprotocol.AgentTokenHeader, secret, 401}, {"Authorization", "Bearer owner-key", 503}} {
			request := httptest.NewRequest(route.method, route.path, nil)
			request.RemoteAddr = fmt.Sprintf("192.0.2.%d:4242", i+2)
			if tc.header != "" {
				request.Header.Set(tc.header, tc.value)
			}
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
		}
	}
}

// newEmailFamilyOperationGateTestServer exercises the same outer gate and
// handler-owned gate used by the daemon. The short wait bounds busy responses;
// successful requests have no elapsed-time assertion.
func newEmailFamilyOperationGateTestServer(t *testing.T) (*Server, *stubIdentityCacheStore, *SerialOperationGate) {
	t.Helper()
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })
	st := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Logger: testLogger(), OperationGate: gate,
	})
	return srv, st, gate
}

func TestEmailFamilyDecisionHTTPPreviewApplyHistoryUndo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st, gate := newEmailFamilyOperationGateTestServer(t)
	source, err := st.GetOrCreateSource("imap", "decisions@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	ids := []int64{}
	for i := range 2 {
		id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(i), MessageType: "email"}, RawMIME: []byte(fmt.Sprintf("Message-ID: <m%d@example.test>\r\n\r\n", i))})
		require.NoError(err)
		ids = append(ids, id)
	}
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		var reader *bytes.Reader
		if body == nil {
			reader = bytes.NewReader(nil)
		} else {
			encoded, err := json.Marshal(body)
			require.NoError(err)
			reader = bytes.NewReader(encoded)
		}
		req := httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("a", 32), Kind: "link", MessageIDs: ids, ActorKind: "cli"}
	emptyScope := input
	emptyScope.SourceIDs = []int64{}
	denied := request("POST", "/api/v1/messages/family/decisions/preview", emptyScope)
	assert.Equal(404, denied.Code, denied.Body.String())
	response := request("POST", "/api/v1/messages/family/decisions/preview", input)
	require.Equal(200, response.Code, response.Body.String())
	var preview store.EmailFamilyDecisionPreview
	require.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
	assert.Len(preview.Family.Members, 2)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	func() {
		release, ok := gate.BeginWorkContext(t.Context())
		require.True(ok)
		defer release()
		busy := request(http.MethodPost, "/api/v1/messages/family/decisions", input)
		assert.Equal(http.StatusServiceUnavailable, busy.Code, busy.Body.String())
	}()
	response = request("POST", "/api/v1/messages/family/decisions", input)
	require.Equal(200, response.Code, response.Body.String())
	var applied store.EmailFamilyDecisionResult
	require.NoError(json.Unmarshal(response.Body.Bytes(), &applied))
	stale := input
	stale.RequestKey = strings.Repeat("b", 32)
	response = request("POST", "/api/v1/messages/family/decisions", stale)
	assert.Equal(409, response.Code, response.Body.String())
	response = request("GET", fmt.Sprintf("/api/v1/messages/family/decisions?message_id=%d", ids[0]), nil)
	assert.Equal(200, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "request_key")

	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"), ids[1])
	require.NoError(err)
	detailPath := fmt.Sprintf("/api/v1/messages/family/decisions/%d", applied.Decision.ID)
	for _, tc := range []struct {
		path   string
		status int
	}{{detailPath, 200}, {detailPath + "?source_id=", 404}, {detailPath + "?source_id=bad", 400}, {"/api/v1/messages/family/decisions/0", 400}, {"/api/v1/messages/family/decisions/999999", 404}} {
		response = request("GET", tc.path, nil)
		assert.Equal(tc.status, response.Code, response.Body.String())
	}
	response = request("GET", detailPath, nil)
	require.Equal(200, response.Code, response.Body.String())
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	var current store.EmailFamilyDecisionResult
	require.NoError(json.Unmarshal(response.Body.Bytes(), &current))
	assert.Len(current.Family.Members, 1)
	undoPath := fmt.Sprintf("/api/v1/messages/family/decisions/%d/undo", applied.Decision.ID)
	undoInput := map[string]any{"expected_revision": applied.Decision.Revision, "expected_family_revision": current.Family.Revision}
	func() {
		release, ok := gate.BeginWorkContext(t.Context())
		require.True(ok)
		defer release()
		busy := request(http.MethodPost, undoPath, undoInput)
		assert.Equal(http.StatusServiceUnavailable, busy.Code, busy.Body.String())
	}()
	response = request(http.MethodPost, undoPath, undoInput)
	assert.Equal(200, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), `"undone"`)
}

func TestEmailFamilyPreviewHTTPWhileArchiveOperationRuns(t *testing.T) {
	require := require.New(t)
	srv, st, gate := newEmailFamilyOperationGateTestServer(t)
	source, err := st.GetOrCreateSource("imap", "preview@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <preview@example.test>\r\n\r\n")})
	require.NoError(err)
	body, err := json.Marshal(store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("c", 32), Kind: "handled", MessageIDs: []int64{id}, ActorKind: "cli"})
	require.NoError(err)
	release, ok := gate.BeginWorkContext(t.Context())
	require.True(ok)
	defer release()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/messages/family/decisions/preview", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var preview store.EmailFamilyDecisionPreview
	require.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
	require.Len(preview.Family.Members, 1)
}
