package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const restrictionsNotice = "Staged configuration only; rules do not yet restrict reads."

type restrictionStore interface {
	AddAgentRestriction(ctx context.Context, rule store.AgentRestriction) (store.AgentRestriction, error)
	ListAgentRestrictions(ctx context.Context) ([]store.AgentRestriction, error)
	RemoveAgentRestriction(ctx context.Context, id int64) error
	CheckAgentRestrictions(ctx context.Context, messageID int64) ([]store.AgentRestriction, error)
	AgentRestrictionTaint(ctx context.Context) (bool, error)
	SetAgentRestrictionTaint(ctx context.Context, enabled bool) error
}
type RestrictionRequest struct {
	Kind     string `json:"kind"`
	SourceID int64  `json:"source_id,omitempty"`
	LabelID  int64  `json:"label_id,omitempty"`
	PersonID int64  `json:"person_id,omitempty"`
	Reveal   string `json:"reveal,omitempty"`
	Note     string `json:"note,omitempty"`
}
type RestrictionTaintRequest struct {
	Enabled bool `json:"enabled"`
}
type RestrictionResponse struct {
	Staged bool                     `json:"staged"`
	Notice string                   `json:"notice"`
	Rules  []store.AgentRestriction `json:"rules"`
	Taint  bool                     `json:"taint"`
}

func (s *Server) registerRestrictionRoutes(api huma.API) {
	for _, route := range []struct {
		id, method, path, summary string
		handler                   http.HandlerFunc
		body                      int
		idParam                   bool
	}{
		{"listRestrictions", http.MethodGet, "/restrictions", "List staged restriction rules", s.handleRestrictions, 0, false},
		{"addRestriction", http.MethodPost, "/restrictions", "Add a staged restriction rule", s.handleRestrictions, 1, false},
		{"removeRestriction", http.MethodDelete, "/restrictions/{id}", "Remove a staged restriction rule", s.handleRestrictions, 0, true},
		{"checkRestrictions", http.MethodGet, "/restrictions/check/{id}", "Check staged rules for a message", s.handleRestrictions, 0, true},
		{"setRestrictionTaint", http.MethodPut, "/restrictions/taint", "Set staged thread taint", s.handleRestrictions, 2, false},
	} {
		op := rawAPIV1Operation(route.id, route.method, route.path, route.summary)
		op.Description = restrictionsNotice
		if route.idParam {
			description := "Restriction rule ID"
			if route.id == "checkRestrictions" {
				description = "Message ID"
			}
			op.Parameters = append(op.Parameters, &huma.Param{Name: "id", In: "path", Required: true, Description: description, Schema: &huma.Schema{Type: "integer", Format: "int64", Minimum: new(float64(1))}})
		}
		switch route.body {
		case 1:
			op.RequestBody = jsonRequestBodyFor[RestrictionRequest](api)
		case 2:
			op.RequestBody = jsonRequestBodyFor[RestrictionTaintRequest](api)
		}
		op.Responses = jsonResponsesFor[RestrictionResponse](api)
		addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
		registerRawHumaRoute(api, op, route.handler)
	}
}
func (s *Server) handleRestrictions(w http.ResponseWriter, r *http.Request) {
	mode := s.requestAuthentication(r).Mode
	if mode != AuthModeLoopback && mode != AuthModeAPIKey && mode != AuthModeSession && mode != AuthModeCaller {
		writeError(w, http.StatusForbidden, "forbidden", "Owner access required")
		return
	}
	st, ok := s.store.(restrictionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Restrictions are unavailable")
		return
	}
	ctx := r.Context()
	var err error
	var rules []store.AgentRestriction
	switch {
	case r.Method == http.MethodPost:
		var req RestrictionRequest
		if !decodeEntityRequest(w, r, &req, "restriction") {
			return
		}
		var rule store.AgentRestriction
		rule, err = st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: req.Kind, SourceID: req.SourceID, LabelID: req.LabelID, PersonID: req.PersonID, Reveal: req.Reveal, Note: req.Note})
		if err == nil {
			rules = []store.AgentRestriction{rule}
		}
	case r.Method == http.MethodPut:
		var req RestrictionTaintRequest
		fields, valid := decodeEntityRequestFields(w, r, &req, "restriction taint")
		if !valid {
			return
		}
		if value, ok := fields["enabled"]; !ok || isJSONNull(value) {
			writeError(w, http.StatusBadRequest, "bad_request", "enabled is required")
			return
		}
		err = st.SetAgentRestrictionTaint(ctx, req.Enabled)
	case r.Method == http.MethodDelete:
		var id int64
		id, err = strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err == nil && id > 0 {
			err = st.RemoveAgentRestriction(ctx, id)
		} else {
			err = errors.New("invalid rule ID")
		}
	case r.PathValue("id") != "":
		var id int64
		id, err = strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err == nil && id > 0 {
			rules, err = st.CheckAgentRestrictions(ctx, id)
		} else {
			err = errors.New("invalid message ID")
		}
	default:
		rules, err = st.ListAgentRestrictions(ctx)
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeError(w, status, "restriction_error", err.Error())
		return
	}
	taint, err := st.AgentRestrictionTaint(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Restriction setting is unavailable")
		return
	}
	if rules == nil {
		rules = []store.AgentRestriction{}
	}
	writeJSON(w, http.StatusOK, RestrictionResponse{Staged: true, Notice: restrictionsNotice, Rules: rules, Taint: taint})
}
