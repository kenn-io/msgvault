package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxTriageMappingReader reads configuration without contacting a provider.
type InboxTriageMappingReader interface {
	InboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity) (map[string]string, int64, error)
}

// InboxTriageMappingUpdater validates native existence under controller gates.
type InboxTriageMappingUpdater interface {
	UpdateInboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expectedRevision int64, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) (int64, error)
}

type inboxTriageMappingConfiguration struct {
	Source   inboxcontrol.SourceIdentity `json:"source"`
	Entries  map[string]string           `json:"entries" nullable:"false"`
	Revision int64                       `json:"revision"`
}

type inboxTriageMappingUpdate struct {
	Source           inboxcontrol.SourceIdentity `json:"source"`
	Entries          map[string]string           `json:"entries" nullable:"false"`
	ExpectedRevision *int64                      `json:"expected_revision" nullable:"false" minimum:"0"`
}

type inboxTriageMappingRevision struct {
	Revision int64 `json:"revision"`
}

func (s *Server) registerInboxTriageMappingRoutes(api huma.API) {
	read := rawAPIV1Operation("getInboxTriageMappings", http.MethodGet, "/inbox/triage/mappings", "Read configured category mappings for one exact source")
	read.Tags = []string{"Inbox"}
	id := queryIntegerParam("source_id", "Exact archive source ID")
	id.Required = true
	read.Parameters = []*huma.Param{id, queryStringParam("source_type", "Exact provider type", true), queryStringParam("source_identifier", "Exact source identifier", true), queryStringParam("account_id", "Exact provider account", true)}
	read.Responses = jsonResponsesFor[inboxTriageMappingConfiguration](api)
	for code, response := range jsonResponsesFor[ErrorResponse](api, 400, 401, 403, 409, 500, 503) {
		if code != defaultErrorResponse {
			read.Responses[code] = response
		}
	}
	registerRawHumaRoute(api, read, s.handleInboxTriageMappings)
	update := rawAPIV1Operation("updateInboxTriageMappings", http.MethodPut, "/inbox/triage/mappings", "Replace owner category mappings after native catalog validation")
	update.Tags = []string{"Inbox"}
	update.RequestBody = jsonRequestBodyFor[inboxTriageMappingUpdate](api)
	update.Responses = jsonResponsesFor[inboxTriageMappingRevision](api)
	for code, response := range jsonResponsesFor[ErrorResponse](api, 400, 401, 403, 409, 413, 415, 500, 503) {
		if code != defaultErrorResponse {
			update.Responses[code] = response
		}
	}
	registerRawHumaRoute(api, update, s.handleUpdateInboxTriageMappings)
}

func parseInboxMappingSource(r *http.Request) (inboxcontrol.SourceIdentity, error) {
	var source inboxcontrol.SourceIdentity
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return source, inboxcontrol.ErrInvalid
	}
	for name, entries := range values {
		switch name {
		case "source_id", "source_type", "source_identifier", "account_id":
		default:
			return source, inboxcontrol.ErrInvalid
		}
		if len(entries) != 1 {
			return source, inboxcontrol.ErrInvalid
		}
	}
	source.SourceID, err = strconv.ParseInt(values.Get("source_id"), 10, 64)
	if err != nil {
		return source, inboxcontrol.ErrInvalid
	}
	source.SourceType = values.Get("source_type")
	source.SourceIdentifier = values.Get("source_identifier")
	source.AccountID = values.Get("account_id")
	return source, source.Validate()
}

func (s *Server) handleInboxTriageMappings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	source, err := parseInboxMappingSource(r)
	if err != nil {
		writeInboxMappingError(w, err)
		return
	}
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode == AuthModeRequired {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return
	}
	if auth.Mode == AuthModeDelegated && (auth.Grant == nil || !auth.Grant.Allows(agentgrant.PermissionInboxRead, agentgrant.SourceRef{ID: source.SourceID, Type: source.SourceType, Identifier: source.SourceIdentifier})) {
		writeInboxMappingError(w, inboxcontrol.ErrDenied)
		return
	}
	reader, ok := s.store.(InboxTriageMappingReader)
	if !ok {
		writeInboxMappingError(w, inboxcontrol.ErrUnavailable)
		return
	}
	entries, revision, err := reader.InboxTriageMappings(r.Context(), source)
	if err != nil {
		writeInboxMappingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inboxTriageMappingConfiguration{Source: source, Entries: entries, Revision: revision})
}

func (s *Server) handleUpdateInboxTriageMappings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request inboxTriageMappingUpdate
	if !decodeInboxControlRequest(w, r, &request) {
		return
	}
	principal := inboxcontrol.Principal{ID: "owner", Owner: true}
	authorize := func(_ context.Context, p inboxcontrol.Principal, intent inboxcontrol.Request) error {
		current := s.classifyAPIRequestDirect(r)
		if !p.Owner || p.ID != "owner" || current.Mode == AuthModeRequired || current.Mode == AuthModeDelegated || intent.Source == nil || *intent.Source != request.Source {
			return inboxcontrol.ErrDenied
		}
		return nil
	}
	if err := authorize(r.Context(), principal, inboxcontrol.Request{Source: &request.Source}); err != nil {
		writeInboxMappingError(w, err)
		return
	}
	if request.ExpectedRevision == nil || request.Entries == nil {
		writeInboxMappingError(w, inboxcontrol.ErrInvalid)
		return
	}
	if err := inboxcontrol.ValidateTriageMappingUpdate(request.Source, request.Entries, *request.ExpectedRevision, principal); err != nil {
		writeInboxMappingError(w, err)
		return
	}
	updater, ok := s.store.(InboxTriageMappingUpdater)
	if !ok {
		writeInboxMappingError(w, inboxcontrol.ErrUnavailable)
		return
	}
	revision, err := updater.UpdateInboxTriageMappings(r.Context(), request.Source, request.Entries, *request.ExpectedRevision, principal, authorize, s.beginInboxMutation)
	if errors.Is(err, errCalendarGateBusy) {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	if err != nil {
		writeInboxMappingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inboxTriageMappingRevision{Revision: revision})
}

func writeInboxMappingError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "inbox_failed", "Unable to access inbox category mappings"
	switch {
	case errors.Is(err, inboxcontrol.ErrInvalid):
		status, code, message = http.StatusBadRequest, "invalid_inbox_request", inboxcontrol.ErrInvalid.Error()
	case errors.Is(err, inboxcontrol.ErrDenied):
		status, code, message = http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error()
	case errors.Is(err, inboxcontrol.ErrConflict), errors.Is(err, inboxcontrol.ErrPlanChanged):
		status, code, message = http.StatusConflict, "inbox_conflict", "Inbox mapping configuration conflicts with the expected revision"
	case errors.Is(err, inboxcontrol.ErrUnavailable):
		status, code, message = http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error()
	case errors.Is(err, inboxcontrol.ErrInternal):
		status, code, message = http.StatusInternalServerError, "inbox_internal", inboxcontrol.ErrInternal.Error()
	}
	writeError(w, status, code, message)
}
