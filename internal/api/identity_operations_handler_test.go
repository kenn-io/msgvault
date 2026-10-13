package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeIdentityOperationsHandlerOwnsAuthentication(t *testing.T) {
	for _, credential := range []string{"", "invalid-daemon-token"} {
		t.Run("agent-header="+credential, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
			requirements.NoError(err)
			b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
			requirements.NoError(err)
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
			t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
			allowed := true
			var operations []string
			handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, operation Operation) bool {
				operations = append(operations, operation.ID)
				if !allowed {
					w.WriteHeader(http.StatusForbidden)
				}
				return allowed
			})
			request := func(method, path string, body any) *httptest.ResponseRecorder {
				data, marshalErr := json.Marshal(body)
				requirements.NoError(marshalErr)
				r := httptest.NewRequest(method, path, bytes.NewReader(data))
				r.Header.Set("Content-Type", "application/json")
				if credential != "" {
					r.Header.Set("X-Msgvault-Agent-Token", credential)
				}
				if !allowed {
					r.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			discovery := request(http.MethodGet, "/api/v1/mcp/capabilities", nil)
			requirements.Equal(http.StatusOK, discovery.Code, discovery.Body.String())
			var descriptor apiprotocol.MCPCapabilities
			requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &descriptor))
			assertions.False(descriptor.Delegated)
			assertions.True(descriptor.HasIdentityPreviewContract())
			assertions.True(descriptor.HasIdentityApplyContract())
			assertions.True(descriptor.HasIdentityReceiptContract())
			assertions.Equal([]string{"graph-link", "graph-unlink", "person-link", "person-unlink"}, descriptor.IdentityOperations)

			intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
			w := request(http.MethodPost, identityOperationsPath+"/preview", intent)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var preview identityOperationPreviewResponse
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &preview))
			requirements.NotNil(preview.Snapshot)
			requirements.NotEmpty(preview.PreviewToken)
			body := identityOperationApplyRequest{Operation: intent.Operation, Target: intent.Target, ExpectedFingerprint: preview.Snapshot.Fingerprint, PreviewToken: preview.PreviewToken, IdempotencyKey: "synthetic-caller-link"}
			w = request(http.MethodPost, identityOperationsPath+"/apply", body)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var receipt store.IdentityReceipt
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &receipt))
			assertions.True(receipt.Changed)
			body.PreviewToken = "expired-synthetic-preview"
			w = request(http.MethodPost, identityOperationsPath+"/apply", body)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var replay store.IdentityReceipt
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &replay))
			assertions.Equal(receipt, replay)
			w = request(http.MethodGet, identityOperationsPath+"/receipt?idempotency_key="+body.IdempotencyKey, nil)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &replay))
			assertions.Equal(receipt, replay)
			assertions.Equal([]string{"getMCPCapabilities", "previewIdentityOperation", "applyIdentityOperation", "applyIdentityOperation", "getIdentityOperationReceipt"}, operations)

			// Caller rejection must precede preview disclosure, replay, and recovery.
			allowed = false
			for _, denied := range []struct {
				method string
				path   string
				body   any
			}{
				{http.MethodGet, "/api/v1/mcp/capabilities", nil},
				{http.MethodPost, identityOperationsPath + "/preview", intent},
				{http.MethodPost, identityOperationsPath + "/apply", body},
				{http.MethodGet, identityOperationsPath + "/receipt?idempotency_key=" + body.IdempotencyKey, nil},
			} {
				w = request(denied.method, denied.path, denied.body)
				assertions.Equal(http.StatusForbidden, w.Code)
				assertions.Empty(w.Body.String())
			}
			current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
			requirements.NoError(err)
			assertions.Equal(receipt.AfterFingerprint, current.Fingerprint)
		})
	}
}
