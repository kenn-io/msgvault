package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestDeliveryPolicyHTTPRoutes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "peer@example.test", "Example Person", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	request := func(path string, v any) *httptest.ResponseRecorder {
		body, err := json.Marshal(v)
		requirements.NoError(err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		return response
	}
	q := store.DeliveryPolicyQuery{PersonUID: person.VCardUID}
	response := request("/api/v1/people/delivery-policy/read", q)
	requirements.Equal(200, response.Code, response.Body.String())
	var state store.DeliveryPolicyState
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	w := store.DeliveryPolicyWrite{PersonUID: q.PersonUID, ExpectedRevision: state.Revision, Policy: store.DeliverySendAllowed, ScopeAcknowledgement: "person_all_routes", Reason: "Explicit synthetic approval"}
	response = request("/api/v1/people/delivery-policy/set", w)
	requirements.Equal(200, response.Code, response.Body.String())
	var receipt store.DeliveryPolicyReceipt
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &receipt))
	requirements.NotNil(receipt.Audit)
	assertions.Equal("owner", receipt.Audit.Actor)
	response = request("/api/v1/people/delivery-policy/set", w)
	assertions.Equal(409, response.Code)
	assertions.Contains(response.Body.String(), "revision_conflict")
	response = request("/api/v1/people/delivery-policy/read", q)
	requirements.Equal(200, response.Code, response.Body.String())
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	response = request("/api/v1/people/delivery-policy/set", map[string]any{
		"person_uid": q.PersonUID, "expected_revision": state.Revision,
		"policy": store.DeliveryDraftOnly, "scope_acknowledgement": "person_all_routes",
		"reason": "Explicit synthetic approval", "actor": "impersonated",
	})
	assertions.Equal(400, response.Code, response.Body.String())
	response = request("/api/v1/people/delivery-policy/read", store.DeliveryPolicyQuery{PersonUID: "unknown-synthetic-person"})
	assertions.Equal(404, response.Code)
	assertions.Contains(response.Body.String(), "unknown_person")
	response = request("/api/v1/people/delivery-policy/read", store.DeliveryPolicyQuery{})
	assertions.Equal(400, response.Code)
	assertions.Contains(response.Body.String(), "invalid_policy")
	response = request("/api/v1/people/delivery-policy/clear", store.DeliveryPolicyWrite{PersonUID: q.PersonUID, ExpectedRevision: receipt.After.Revision, Reason: "Restore inheritance"})
	requirements.Equal(200, response.Code, response.Body.String())
	srv.store = nil
	response = request("/api/v1/people/delivery-policy/read", q)
	assertions.Equal(503, response.Code)
	assertions.Contains(response.Body.String(), "delivery_policy_unavailable")
}
