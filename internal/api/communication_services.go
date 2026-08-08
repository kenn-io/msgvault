package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

type CommunicationServiceStore interface {
	ListCommunicationServicesContext(
		ctx context.Context, includeInactive bool,
	) ([]store.CommunicationService, error)
	EnsureCommunicationServiceContext(
		ctx context.Context, input store.CommunicationServiceInput,
	) (*store.CommunicationService, bool, error)
}

type CommunicationServicesResponse struct {
	Services []store.CommunicationService `json:"services"`
}

type CreateCommunicationServiceRequest struct {
	Slug                 string   `json:"slug"`
	DisplayLabel         string   `json:"display_label"`
	Aliases              []string `json:"aliases,omitempty"`
	ScopePolicy          string   `json:"scope_policy" enum:"none,optional,required"`
	DefaultScopeKind     *string  `json:"default_scope_kind,omitempty"`
	Normalization        string   `json:"normalization" enum:"none,lower,email,phone_e164,strip_at_lower,by_address_kind"`
	NormalizationVersion int      `json:"normalization_version,omitempty"`
	URIScheme            *string  `json:"uri_scheme,omitempty"`
	ProfileURLTemplate   *string  `json:"profile_url_template,omitempty"`
}

func (s *Server) registerCommunicationServiceRoutes(api huma.API) {
	list := rawAPIV1Operation(
		"listCommunicationServices", http.MethodGet, "/communication-services",
		"List communication services",
	)
	list.Description = "Lists the small open service catalog without pagination, including aliases and normalization policy."
	list.Parameters = append(list.Parameters, &huma.Param{
		Name: "include_inactive", In: "query",
		Description: "Include inactive catalog entries",
		Schema:      &huma.Schema{Type: huma.TypeBoolean, Default: false},
	})
	list.Responses = jsonResponsesFor[CommunicationServicesResponse](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListCommunicationServices)

	create := rawAPIV1Operation(
		"createCommunicationService", http.MethodPost, "/communication-services",
		"Register a communication service",
	)
	create.Description = "Registers an unknown or custom service without a schema migration. Re-registering a slug is idempotent."
	create.RequestBody = jsonRequestBodyFor[CreateCommunicationServiceRequest](api)
	create.Responses = jsonResponsesFor[store.CommunicationService](
		api, http.StatusOK, http.StatusCreated,
	)
	addErrorResponses(api, create.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreateCommunicationService)
}

func (s *Server) handleListCommunicationServices(w http.ResponseWriter, r *http.Request) {
	servicesStore, ok := s.communicationServiceStore(w)
	if !ok {
		return
	}
	includeInactive, _, err := queryBool(r, "include_inactive")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	services, err := servicesStore.ListCommunicationServicesContext(
		r.Context(), includeInactive,
	)
	if err != nil {
		s.writeCommunicationServiceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, CommunicationServicesResponse{Services: services})
}

func (s *Server) handleCreateCommunicationService(w http.ResponseWriter, r *http.Request) {
	servicesStore, ok := s.communicationServiceStore(w)
	if !ok {
		return
	}
	var request CreateCommunicationServiceRequest
	if !decodePersonRequest(w, r, &request) {
		return
	}
	if request.NormalizationVersion == 0 {
		request.NormalizationVersion = 1
	}
	service, created, err := servicesStore.EnsureCommunicationServiceContext(
		r.Context(), store.CommunicationServiceInput{
			Slug: request.Slug, DisplayLabel: request.DisplayLabel,
			Aliases: request.Aliases, ScopePolicy: request.ScopePolicy,
			DefaultScopeKind:     request.DefaultScopeKind,
			Normalization:        request.Normalization,
			NormalizationVersion: request.NormalizationVersion,
			URIScheme:            request.URIScheme,
			ProfileURLTemplate:   request.ProfileURLTemplate,
		},
	)
	if err != nil {
		s.writeCommunicationServiceError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set(
			"Location", "/api/v1/communication-services/"+strconv.FormatInt(service.ID, 10),
		)
	}
	writeJSON(w, status, service)
}

func (s *Server) communicationServiceStore(
	w http.ResponseWriter,
) (CommunicationServiceStore, bool) {
	services, ok := s.store.(CommunicationServiceStore)
	if !ok {
		writeError(
			w, http.StatusServiceUnavailable, "communication_services_unavailable",
			"Communication services are unavailable",
		)
	}
	return services, ok
}

func (s *Server) writeCommunicationServiceError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrInvalidServiceSlug),
		errors.Is(err, store.ErrInvalidScopePolicy),
		errors.Is(err, store.ErrInvalidNormalization):
		writeError(w, http.StatusBadRequest, "invalid_communication_service", err.Error())
	case errors.Is(err, store.ErrServiceAliasConflict):
		writeError(w, http.StatusConflict, "service_alias_conflict", err.Error())
	case errors.Is(err, store.ErrServiceNotFound):
		writeError(w, http.StatusNotFound, "communication_service_not_found", err.Error())
	default:
		s.logger.Error("communication service operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "communication_service_failed", "Communication service operation failed")
	}
}
