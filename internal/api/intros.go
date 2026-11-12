package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
)

// IntroductionOperations provides the shared introduction read projection.
type IntroductionOperations interface {
	ListIntroductionsContext(context.Context, *agentgrant.Grant, store.PersonIntroductionQuery) (introops.Page, error)
	GetIntroductionContext(context.Context, *agentgrant.Grant, string) (introops.Record, error)
}

func (s *Server) registerIntroductionRoutes(api huma.API) {
	list := rawAPIV1Operation("listIntroductions", http.MethodGet, "/intros", "List local introductions")
	list.Parameters = []*huma.Param{contactIntegerParam("source_id", 1, 0), contactIntegerParam("limit", 1, 100), queryStringParam("after_id", "Opaque continuation cursor", false)}
	list.Responses = jsonResponsesFor[introops.Page](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable)
	addIntroductionNoStoreHeader(list.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, list, s.handleListIntroductions)

	get := rawAPIV1Operation("getIntroduction", http.MethodGet, "/intros/{id}", "Get a local introduction")
	get.Parameters = []*huma.Param{{Name: "id", In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	get.Responses = jsonResponsesFor[introops.Record](api)
	addErrorResponses(api, get.Responses, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	addIntroductionNoStoreHeader(get.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, get, s.handleGetIntroduction)
}

func addIntroductionNoStoreHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{"Cache-Control": {Description: "Always no-store", Schema: &huma.Schema{Type: huma.TypeString}}}
}

func (s *Server) handleListIntroductions(w http.ResponseWriter, r *http.Request) {
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	source, err := contactQueryInteger(r, "source_id", 1, 0)
	if err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	limit, err := contactQueryInteger(r, "limit", 1, 100)
	if err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	after, err := contactQueryString(r, "after_id")
	if err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	ops, ok := s.store.(IntroductionOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Introduction reads are unavailable")
		return
	}
	grant, release, ok := s.admitIntroductionRead(w, r)
	if !ok {
		return
	}
	defer release()
	page, err := ops.ListIntroductionsContext(r.Context(), grant, store.PersonIntroductionQuery{SourceID: source, Limit: int(limit), AfterID: after})
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetIntroduction(w http.ResponseWriter, r *http.Request) {
	ops, ok := s.store.(IntroductionOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Introduction reads are unavailable")
		return
	}
	grant, release, ok := s.admitIntroductionRead(w, r)
	if !ok {
		return
	}
	defer release()
	row, err := ops.GetIntroductionContext(r.Context(), grant, r.PathValue("id"))
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// admitIntroductionRead reserves the shared snapshot budget only for authorized agents.
func (s *Server) admitIntroductionRead(w http.ResponseWriter, r *http.Request) (*agentgrant.Grant, func(), bool) {
	grant := s.requestAuthentication(r).Grant
	if grant == nil {
		return nil, func() {}, true
	}
	if !grant.HasPermission(agentgrant.PermissionDraftCreate) {
		s.writeIntroductionError(w, introops.ErrForbidden)
		return nil, nil, false
	}
	release, apiErr := s.acquireReadSnapshotSlot(r.Context(), "agent_read_busy", "Agent reads are at capacity; retry later")
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return nil, nil, false
	}
	return grant, release, true
}

func (s *Server) writeIntroductionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrIntroductionInvalid):
		writeError(w, http.StatusBadRequest, "invalid_args", "Invalid introduction parameters")
	case errors.Is(err, introops.ErrForbidden):
		writeError(w, http.StatusForbidden, "not_permitted", "Introduction access is not permitted")
	case errors.Is(err, store.ErrIntroductionNotFound):
		writeError(w, http.StatusNotFound, "intro_not_found", "Introduction not found")
	default:
		writeError(w, http.StatusServiceUnavailable, "local_store_failed", "Introduction storage is unavailable")
	}
}
