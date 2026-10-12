package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// Owner credentials carry no label of their own, so audit rows name the role.
const deliveryPolicyActor = "owner"

func (s *Server) registerDeliveryPolicyRoutes(api huma.API) {
	read := rawAPIV1Operation("getDeliveryPolicy", http.MethodPost, "/people/delivery-policy/read", "Inspect an exact delivery policy")
	read.Description = "Returns stored and effective policy, inheritance, canonical UID, the saved address, and the delivery policy revision. Pass that revision as expected_revision on a write; it is not the person profile revision. Policy is one app gate; it is not live reachability or provider authorization. An omitted endpoint reads the person-wide default."
	read.RequestBody = jsonRequestBodyFor[store.DeliveryPolicyQuery](api)
	read.Responses = jsonResponsesFor[store.DeliveryPolicyState](api)
	addErrorResponses(api, read.Responses, 400, 401, 403, 404, 503)
	registerRawHumaRoute(api, read, s.handleDeliveryPolicyRead)
	for _, reset := range []bool{false, true} {
		id, path, title := "setDeliveryPolicy", "/people/delivery-policy/set", "Explicitly approve or restrict a delivery policy"
		if reset {
			id, path, title = "clearDeliveryPolicy", "/people/delivery-policy/clear", "Clear an address delivery policy"
		}
		op := rawAPIV1Operation(id, http.MethodPost, path, title)
		op.Description = "Owner only. No drafting, sending or delegated credential grants policy-write. Writes require expected_revision, the delivery policy revision from the read route; send_allowed on an address needs an active declared email contact point of the person that no other person holds. Approving the person-wide default requires scope_acknowledgement person_all_routes. send_allowed never sends or disables drafting. Clear restores inheritance; clearing the default restores draft_only."
		op.RequestBody = jsonRequestBodyFor[store.DeliveryPolicyWrite](api)
		op.Responses = jsonResponsesFor[store.DeliveryPolicyReceipt](api)
		addErrorResponses(api, op.Responses, 400, 401, 403, 404, 409, 503)
		registerRawHumaRoute(api, op, func(w http.ResponseWriter, r *http.Request) { s.handleDeliveryPolicyWrite(w, r, reset) })
	}
}

type deliveryPolicyStore interface {
	GetDeliveryPolicyContext(ctx context.Context, query store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error)
	SetDeliveryPolicyContext(ctx context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error)
	ClearDeliveryPolicyContext(ctx context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error)
}

func (s *Server) handleDeliveryPolicyRead(w http.ResponseWriter, r *http.Request) {
	policies, ok := s.store.(deliveryPolicyStore)
	if !ok {
		writeError(w, 503, "delivery_policy_unavailable", "Delivery policy storage unavailable")
		return
	}
	var query store.DeliveryPolicyQuery
	if !decodeEntityRequest(w, r, &query, "delivery policy") {
		return
	}
	state, err := policies.GetDeliveryPolicyContext(r.Context(), query)
	if err != nil {
		s.writeDeliveryPolicyError(w, err)
		return
	}
	writeJSON(w, 200, state)
}

func (s *Server) handleDeliveryPolicyWrite(w http.ResponseWriter, r *http.Request, reset bool) {
	policies, ok := s.store.(deliveryPolicyStore)
	if !ok {
		writeError(w, 503, "delivery_policy_unavailable", "Delivery policy storage unavailable")
		return
	}
	var request store.DeliveryPolicyWrite
	if !decodeEntityRequest(w, r, &request, "delivery policy") {
		return
	}
	// Authentication is owner-scoped; actor cannot be impersonated in JSON.
	request.Actor = deliveryPolicyActor
	var receipt *store.DeliveryPolicyReceipt
	var err error
	if reset {
		receipt, err = policies.ClearDeliveryPolicyContext(r.Context(), request)
	} else {
		receipt, err = policies.SetDeliveryPolicyContext(r.Context(), request)
	}
	if err != nil {
		s.writeDeliveryPolicyError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}

func (s *Server) writeDeliveryPolicyError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, store.ErrDeliveryPolicyInvalid):
		status = 400
	case errors.Is(err, store.ErrDeliveryPersonUnknown):
		status = 404
	case errors.Is(err, store.ErrDeliveryPolicyRevisionConflict):
		status = 409
	}
	// Internal SQL errors may contain identifiers or database details.
	message := "Delivery policy unavailable"
	if status != 503 {
		message = err.Error()
	}
	writeError(w, status, store.DeliveryErrorCode(err), message)
}
