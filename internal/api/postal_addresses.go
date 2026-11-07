package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/postaladdress"
)

// PostalAddressValidator is an explicit, transient external assessment. Store
// has no provider capability, so no provider call can run under its locks.
type PostalAddressValidator interface {
	Check(ctx context.Context, input postaladdress.Input) (postaladdress.Assessment, error)
}

type PostalAddressAssessmentRequest struct {
	Address       postaladdress.Input `json:"address"`
	AllowExternal bool                `json:"allow_external,omitempty"`
}

// SetPostalAddressValidator wires the operator's configured provider. It does
// not enable the provider or confer per-request consent.
func (s *Server) SetPostalAddressValidator(validator PostalAddressValidator) {
	s.postalAddressValidator = validator
}

func (s *Server) registerPostalAddressRoutes(api huma.API) {
	operation := rawAPIV1Operation("assessPersonPostalAddress", http.MethodPost, "/people/{id}/addresses/assess", "Assess a draft postal address without saving it")
	operation.Description = "Local assessment never contacts a provider. An explicit allow_external request uses an enabled provider for the owner only; results are transient and must be reviewed before saving."
	addPersonIDParameter(&operation)
	operation.RequestBody = jsonRequestBodyFor[PostalAddressAssessmentRequest](api)
	operation.Responses = jsonResponsesFor[postaladdress.Assessment](api)
	addErrorResponses(api, operation.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, operation, s.handleAssessPersonPostalAddress)
}

func (s *Server) handleAssessPersonPostalAddress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Postal address assessment requires owner access")
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	profiles, ok := s.personProfileValueStore(w)
	if !ok {
		return
	}
	if _, err := profiles.GetPersonProfileContext(r.Context(), id); err != nil {
		s.writePersonProfileValueError(w, err)
		return
	}
	// Keep wire presence separate from the published non-nullable value schema.
	var input struct {
		Address       *postaladdress.Input `json:"address"`
		AllowExternal bool                 `json:"allow_external,omitempty"`
	}
	decoder := jsontext.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20), json.RejectUnknownMembers(true))
	if err := json.UnmarshalDecode(decoder, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid postal address assessment request")
		return
	}
	if !requireSingleJSONValue(w, decoder, "bad_request") {
		return
	}
	if input.Address == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "A postal address assessment requires an address")
		return
	}
	if input.Address.AddressKind == "" {
		input.Address.AddressKind = "postal"
	}
	if input.Address.AddressKind != "postal" {
		writeError(w, http.StatusBadRequest, "bad_request", "Only postal addresses can be assessed")
		return
	}
	assessment := postaladdress.Assess(*input.Address)
	if input.AllowExternal {
		reason := ""
		cfg := s.cfg.People.AddressValidation
		switch {
		case !cfg.Enabled:
			reason = "provider_disabled"
		case cfg.Provider != "google" || s.postalAddressValidator == nil:
			reason = "provider_not_configured"
		default:
			// Deliberate single call; never retry billed work or persist its result.
			checked, err := s.postalAddressValidator.Check(r.Context(), *input.Address)
			assessment = checked
			if err != nil && assessment.Status != postaladdress.StatusUnavailable {
				assessment = postaladdress.Assess(*input.Address)
				reason = "provider_unavailable"
			}
		}
		if reason != "" {
			assessment.Status = postaladdress.StatusUnavailable
			assessment.ReasonCodes = append(assessment.ReasonCodes, reason)
		}
	}
	writeJSON(w, http.StatusOK, assessment)
}
