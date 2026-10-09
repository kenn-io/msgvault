package api

import (
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/delivery"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
)

const DeliveryPolicyWriteHeader = "X-Msgvault-Delivery-Policy-Write"

func (s *Server) registerDeliveryPolicyRoutes(api huma.API) {
	read := rawAPIV1Operation("getDeliveryPolicy", http.MethodPost, "/people/delivery-policy/read", "Inspect an exact delivery policy")
	read.Description = "Returns stored and effective policy, inheritance, canonical UID, exact native route identifiers and revisions. Policy is one app gate; archive evidence is not live reachability or provider authorization. No target inspects the person-wide default."
	read.RequestBody = jsonRequestBodyFor[store.DeliveryPolicyQuery](api)
	read.Responses = jsonResponsesFor[store.DeliveryPolicyState](api)
	addErrorResponses(api, read.Responses, 400, 401, 403, 404, 503)
	registerRawHumaRoute(api, read, s.handleDeliveryPolicyRead)
	for _, reset := range []bool{false, true} {
		id, path, title := "setDeliveryPolicy", "/people/delivery-policy/set", "Explicitly approve or restrict a delivery policy"
		if reset {
			id, path, title = "clearDeliveryPolicy", "/people/delivery-policy/clear", "Clear an exact delivery policy override"
		}
		op := rawAPIV1Operation(id, http.MethodPost, path, title)
		op.Description = "Owner only. Requires daemon policy-write opt-in and X-Msgvault-Delivery-Policy-Write: true. No drafting, sending or delegated credential grants policy-write. Writes require expected_revision; send_allowed additionally requires the current person revision and reviewed binding digest. Setting the person-wide default requires scope_acknowledgement person_all_routes. send_allowed never sends or disables drafting. Clear restores inheritance; clearing the default restores draft_only."
		op.Parameters = []*huma.Param{{Name: DeliveryPolicyWriteHeader, In: "header", Required: true, Schema: &huma.Schema{Type: "string", Enum: []any{"true"}}}}
		op.RequestBody = jsonRequestBodyFor[store.DeliveryPolicyWrite](api)
		op.Responses = jsonResponsesFor[store.DeliveryPolicyReceipt](api)
		for _, status := range []int{400, 401, 403, 404, 409, 503} {
			op.Responses[httpStatusKey(status)] = jsonResponsesFor[ErrorResponse](api, status)[httpStatusKey(status)]
		}
		registerRawHumaRoute(api, op, func(w http.ResponseWriter, r *http.Request) { s.handleDeliveryPolicyWrite(w, r, reset) })
	}
}
func (s *Server) deliveryPolicyService() (delivery.Service, bool) {
	st, ok := s.store.(delivery.PolicyStore)
	return delivery.Service{Store: st}, ok
}
func (s *Server) handleDeliveryPolicyRead(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, 403, "policy_read_forbidden", "Owner policy-read capability required")
		return
	}
	service, ok := s.deliveryPolicyService()
	if !ok {
		writeError(w, 503, "delivery_policy_unavailable", "Delivery policy storage unavailable")
		return
	}
	var query store.DeliveryPolicyQuery
	if !decodeEntityRequest(w, r, &query, "delivery policy") {
		return
	}
	state, err := service.Read(r.Context(), delivery.Authority{PolicyRead: true}, query)
	if err != nil {
		s.writeDeliveryPolicyError(w, err)
		return
	}
	writeJSON(w, 200, state)
}
func (s *Server) handleDeliveryPolicyWrite(w http.ResponseWriter, r *http.Request, reset bool) {
	if !s.apiRequestAuthorized(r) || !s.allowDeliveryPolicyWrites || r.Header.Get(DeliveryPolicyWriteHeader) != "true" {
		writeError(w, 403, "policy_write_forbidden", "Separate owner policy-write opt-in required")
		return
	}
	service, ok := s.deliveryPolicyService()
	if !ok {
		writeError(w, 503, "delivery_policy_unavailable", "Delivery policy storage unavailable")
		return
	}
	var request store.DeliveryPolicyWrite
	if !decodeEntityRequest(w, r, &request, "delivery policy") {
		return
	}
	// Authentication is owner-scoped; actor cannot be impersonated in JSON.
	auth := s.requestAuthentication(r)
	actor := "owner:" + string(auth.Mode)
	receipt, err := service.Write(r.Context(), delivery.Authority{Actor: actor, PolicyWrite: true}, request, reset)
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
	case errors.Is(err, store.ErrDeliveryPolicyRevisionConflict) || errors.Is(err, store.ErrDeliveryTargetChanged):
		status = 409
	}
	// Internal SQL errors may contain identifiers or database details.
	message := "Delivery policy unavailable"
	if status != 503 {
		message = err.Error()
	}
	writeError(w, status, store.DeliveryErrorCode(err), message)
}
