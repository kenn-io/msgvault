package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

func TestDeliveryPolicyReadBypassesHeldOperationGate(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	assertions, requirements := assert.New(t), require.New(t)

	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	base, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "policy-read-peer@example.test", "Example Person", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)

	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "synthetic archive operation")
	requirements.True(ok, "hold the operation gate")
	defer release()

	srv := NewServerWithOptions(ServerOptions{
		Config:        base.cfg,
		Store:         st,
		Logger:        base.logger,
		OperationGate: gate,
	})
	srv.allowDeliveryPolicyWrites = true
	request := func(path string, value any, policyWrite bool) *httptest.ResponseRecorder {
		body, err := json.Marshal(value)
		requirements.NoError(err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		if policyWrite {
			req.Header.Set(DeliveryPolicyWriteHeader, "true")
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		return response
	}

	query := store.DeliveryPolicyQuery{PersonUID: person.VCardUID}
	readResponse := request("/api/v1/people/delivery-policy/read", query, false)
	assertions.Equal(http.StatusOK, readResponse.Code, readResponse.Body.String())

	write := store.DeliveryPolicyWrite{
		Query:                query,
		Policy:               store.DeliverySendAllowed,
		ScopeAcknowledgement: "person_all_routes",
		Reason:               "Synthetic explicit approval",
	}
	writeResponse := request("/api/v1/people/delivery-policy/set", write, true)
	requirements.Equal(http.StatusServiceUnavailable, writeResponse.Code, writeResponse.Body.String())
	var gateError ErrorResponse
	requirements.NoError(json.Unmarshal(writeResponse.Body.Bytes(), &gateError))
	assertions.Equal("operation_in_progress", gateError.Error)
}

func TestDeliveryPolicyHTTPSeparateOwnerOptInAndCAS(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "peer@example.test", "Example Person", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	request := func(path string, v any, header bool) *httptest.ResponseRecorder {
		body, err := json.Marshal(v)
		requirements.NoError(err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		if header {
			req.Header.Set(DeliveryPolicyWriteHeader, "true")
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		return response
	}
	q := store.DeliveryPolicyQuery{PersonUID: person.VCardUID}
	response := request("/api/v1/people/delivery-policy/read", q, false)
	requirements.Equal(200, response.Code, response.Body.String())
	var state store.DeliveryPolicyState
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	w := store.DeliveryPolicyWrite{Query: q, ExpectedRevision: state.PolicyRevision, ExpectedPersonRevision: state.PersonRevision, BindingDigest: state.BindingDigest, Policy: store.DeliverySendAllowed, ScopeAcknowledgement: "person_all_routes", Reason: "Explicit synthetic approval"}
	assertions.Equal(403, request("/api/v1/people/delivery-policy/set", w, true).Code)
	srv.allowDeliveryPolicyWrites = true
	assertions.Equal(403, request("/api/v1/people/delivery-policy/set", w, false).Code)
	response = request("/api/v1/people/delivery-policy/set", w, true)
	requirements.Equal(200, response.Code, response.Body.String())
	var receipt store.DeliveryPolicyReceipt
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &receipt))
	assertions.Equal(store.DeliverySendAllowed, receipt.After.EffectivePolicy)
	assertions.Equal("owner:loopback", receipt.Audit.Actor)
	response = request("/api/v1/people/delivery-policy/set", w, true)
	assertions.Equal(409, response.Code)
	assertions.Contains(response.Body.String(), "revision_conflict")
	response = request("/api/v1/people/delivery-policy/set", map[string]any{"actor": "impersonated"}, true)
	assertions.Equal(400, response.Code)
	assertions.False(delegatedOperationAllowed("setDeliveryPolicy"))
	assertions.False(delegatedOperationAllowed("clearDeliveryPolicy"))
	assertions.False(delegatedOperationAllowed("getDeliveryPolicy"))
}

func TestDeliveryPolicyHTTPDraftCredentialCannotSelfElevate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, registry := newTestServerWithAgentGrants(t)
	srv.allowDeliveryPolicyWrites = true
	_, secret, _, err := registry.Issue("synthetic-policy-denial", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "account@example.test"}})
	requirements.NoError(err)
	for _, op := range []string{"read", "set", "clear"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/people/delivery-policy/"+op, bytes.NewBufferString(`{"person_uid":"synthetic-uid"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		req.Header.Set(DeliveryPolicyWriteHeader, "true")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assertions.Equal(http.StatusUnauthorized, response.Code, "valid draft token cannot acquire owner policy authority")
	}
}

func TestDeliveryPolicyGeneratedClientCanRevokeBrokenEndpoint(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	srv.allowDeliveryPolicyWrites = true
	source, err := st.GetOrCreateSource("gmail", "account@example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(st.mustParticipant(t, "origin@example.test", "Example Person", "example.test"))
	requirements.NoError(err)
	point, err := st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "peer@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	q := store.DeliveryPolicyQuery{PersonUID: person.VCardUID, Target: &store.DeliveryTarget{SourceID: source.ID, SourceType: "gmail", AccountID: source.Identifier, Network: "email", Endpoint: point.NormalizedValue, ContactPointID: point.Envelope.ID}}
	state, err := client.GetDeliveryPolicy(t.Context(), q)
	requirements.NoError(err)
	allowed, err := client.SetDeliveryPolicy(t.Context(), store.DeliveryPolicyWrite{Query: q, ExpectedRevision: state.PolicyRevision, ExpectedPersonRevision: state.PersonRevision, BindingDigest: state.BindingDigest, Policy: store.DeliverySendAllowed, Reason: "Explicit synthetic approval"})
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, allowed.After.EffectivePolicy)
	requirements.NoError(st.SupersedePersonContactPointContext(t.Context(), person.ID, point.Envelope.ID, nil))
	broken, err := client.GetDeliveryPolicy(t.Context(), q)
	requirements.NoError(err)
	assertions.Empty(broken.BindingDigest)
	denied, err := client.SetDeliveryPolicy(t.Context(), store.DeliveryPolicyWrite{Query: q, ExpectedRevision: broken.PolicyRevision, Policy: store.DeliveryDraftOnly, Reason: "Explicit synthetic restriction"})
	requirements.NoError(err)
	cleared, err := client.ClearDeliveryPolicy(t.Context(), store.DeliveryPolicyWrite{Query: q, ExpectedRevision: denied.After.PolicyRevision, Reason: "Restore inheritance"})
	requirements.NoError(err)
	assertions.Nil(cleared.After.StoredPolicy)
	assertions.Equal(store.DeliveryDraftOnly, cleared.After.EffectivePolicy)
}

func TestDeliveryPolicyGeneratedClientPreservesRefusalCodesWithoutPrivateProse(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	srv.allowDeliveryPolicyWrites = true
	person, _, err := st.CreatePersonFromParticipant(st.mustParticipant(t, "policy-error@example.test", "Example Person", "example.test"))
	requirements.NoError(err)
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	q := store.DeliveryPolicyQuery{PersonUID: person.VCardUID}
	state, err := client.GetDeliveryPolicy(t.Context(), q)
	requirements.NoError(err)
	write := store.DeliveryPolicyWrite{Query: q, ExpectedRevision: state.PolicyRevision, ExpectedPersonRevision: state.PersonRevision, BindingDigest: state.BindingDigest, Policy: store.DeliverySendAllowed, ScopeAcknowledgement: "person_all_routes", Reason: "Synthetic approval detail"}
	allowed, err := client.SetDeliveryPolicy(t.Context(), write)
	requirements.NoError(err)
	check := func(err error, status int, code string) {
		var refusal *daemonclient.DeliveryPolicyError
		requirements.ErrorAs(err, &refusal)
		assertions.Equal(status, refusal.Status)
		assertions.Equal(code, refusal.Code)
		safe := daemonclient.SafeMCPError(err).Error()
		assertions.Contains(safe, code)
		assertions.NotContains(safe, refusal.Message)
	}
	_, err = client.SetDeliveryPolicy(t.Context(), write)
	check(err, http.StatusConflict, "revision_conflict")
	write.ExpectedRevision = allowed.After.PolicyRevision
	write.BindingDigest = "stale-synthetic-binding"
	_, err = client.SetDeliveryPolicy(t.Context(), write)
	check(err, http.StatusConflict, "target_changed")
	_, err = client.GetDeliveryPolicy(t.Context(), store.DeliveryPolicyQuery{PersonUID: "unknown-synthetic-person"})
	check(err, http.StatusNotFound, "unknown_person")
	_, err = client.GetDeliveryPolicy(t.Context(), store.DeliveryPolicyQuery{})
	check(err, http.StatusBadRequest, "invalid_policy")
	srv.allowDeliveryPolicyWrites = false
	_, err = client.SetDeliveryPolicy(t.Context(), write)
	check(err, http.StatusForbidden, "policy_write_forbidden")
	srv.store = nil
	_, err = client.GetDeliveryPolicy(t.Context(), q)
	check(err, http.StatusServiceUnavailable, "delivery_policy_unavailable")
}
