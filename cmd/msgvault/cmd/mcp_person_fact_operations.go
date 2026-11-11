package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personFactMCPCapabilities(has mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path string
		query                  []string
		body                   string
	}{
		{"list_person_fact_targets", "listPersonFactTargets", "GET", "/api/v1/person-fact-targets", []string{"include_sensitive"}, ""},
		{"list_person_fact_evidence", "listPersonFactEvidence", "GET", "/api/v1/people/{id}/fact-evidence", []string{"target", "limit", "offset"}, ""},
		{"list_person_fact_status_events", "listPersonFactEvidenceStatusEvents", "GET", "/api/v1/people/{id}/fact-evidence-status-events", []string{"evidence_key", "supported", "limit", "offset"}, ""},
		{"list_person_fact_claims", "listPersonFactClaims", "GET", "/api/v1/people/{id}/fact-claims", []string{"target", "limit", "offset"}, ""},
		{"list_person_fact_decisions", "listPersonFactDecisions", "GET", "/api/v1/people/{id}/fact-decisions", []string{"target", "limit", "offset"}, ""},
		{"list_person_fact_pins", "listPersonFactPins", "GET", "/api/v1/people/{id}/fact-pins", nil, ""},
		{"get_person_tracking", "getPersonTracking", "GET", "/api/v1/people/{id}/tracking", nil, ""},
		{"set_person_fact_pin", "setPersonFactPin", "PUT", "/api/v1/people/{id}/fact-pins/{kind}/{key}", nil, "pinned"},
		{"set_person_tracking", "setPersonTracking", "PUT", "/api/v1/people/{id}/tracking", nil, "tracked"},
	} {
		if has(r.id, r.method, r.path, r.query...) && (r.body == "" || mcpRequestPropertiesPresent(caps, r.id, r.body)) {
			names = append(names, r.name)
		}
	}
	return names
}

type factPageArgs struct {
	PersonID int64   `json:"person_id"`
	Limit    *int64  `json:"limit,omitempty"`
	Offset   *int64  `json:"offset,omitempty"`
	Target   *string `json:"target,omitempty"`
}
type factStatusArgs struct {
	PersonID    int64   `json:"person_id"`
	Limit       *int64  `json:"limit,omitempty"`
	Offset      *int64  `json:"offset,omitempty"`
	EvidenceKey *string `json:"evidence_key,omitempty"`
	Supported   *bool   `json:"supported,omitempty"`
}
type factPersonArgs struct {
	PersonID int64 `json:"person_id"`
}
type factPinArgs struct {
	PersonID int64  `json:"person_id"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Pinned   *bool  `json:"pinned"`
}
type factTrackingArgs struct {
	PersonID int64 `json:"person_id"`
	Tracked  *bool `json:"tracked"`
}

func factPageValid(id int64, limit, offset *int64) bool {
	return validLoopID(id) && (limit == nil || *limit >= 1 && *limit <= 200) && (offset == nil || *offset >= 0 && *offset <= 1<<53-1)
}
func factPinValid(a factPinArgs) bool {
	return validLoopID(a.PersonID) && (a.Kind == "attribute" || a.Kind == "employment") && a.Key != "" && a.Key == strings.TrimSpace(a.Key) && a.Pinned != nil
}
func factClassify(code string) bool {
	switch code {
	case "invalid_target", "invalid_limit", "invalid_offset", "invalid_target_kind", "invalid_target_key", "invalid_fact_target", "person_fact_conflict", "person_facts_unavailable", "tracking_unavailable":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executePersonFactOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	switch name {
	case "list_person_fact_targets":
		a, e := decodeMCPOperationArguments[struct {
			IncludeSensitive *bool `json:"include_sensitive,omitempty"`
		}](args)
		if e != nil {
			return invalid()
		}
		r, e := readMCPOperationJSON[generated.Catalog](ctx, b.client, http.MethodGet, "/api/v1/person-fact-targets", &generated.ListPersonFactTargetsRequestOptions{Query: &generated.ListPersonFactTargetsQuery{IncludeSensitive: a.IncludeSensitive}}, http.StatusOK)
		return r, true, e
	case "list_person_fact_evidence", "list_person_fact_claims", "list_person_fact_decisions":
		a, e := decodeMCPOperationArguments[factPageArgs](args)
		if e != nil || !factPageValid(a.PersonID, a.Limit, a.Offset) {
			return invalid()
		}
		if a.Limit == nil {
			a.Limit = new(int64(50))
		}
		switch name {
		case "list_person_fact_evidence":
			r, e := readMCPOperationJSON[generated.PersonFactEvidenceResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-evidence", &generated.ListPersonFactEvidenceRequestOptions{PathParams: &generated.ListPersonFactEvidencePath{ID: a.PersonID}, Query: &generated.ListPersonFactEvidenceQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
			return r, true, e
		case "list_person_fact_claims":
			r, e := readMCPOperationJSON[generated.PersonFactClaimsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-claims", &generated.ListPersonFactClaimsRequestOptions{PathParams: &generated.ListPersonFactClaimsPath{ID: a.PersonID}, Query: &generated.ListPersonFactClaimsQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
			return r, true, e
		default:
			r, e := readMCPOperationJSON[generated.PersonFactDecisionsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-decisions", &generated.ListPersonFactDecisionsRequestOptions{PathParams: &generated.ListPersonFactDecisionsPath{ID: a.PersonID}, Query: &generated.ListPersonFactDecisionsQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
			return r, true, e
		}
	case "list_person_fact_status_events":
		a, e := decodeMCPOperationArguments[factStatusArgs](args)
		if e != nil || !factPageValid(a.PersonID, a.Limit, a.Offset) {
			return invalid()
		}
		if a.Limit == nil {
			a.Limit = new(int64(50))
		}
		r, e := readMCPOperationJSON[generated.PersonFactEvidenceStatusEventsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-evidence-status-events", &generated.ListPersonFactEvidenceStatusEventsRequestOptions{PathParams: &generated.ListPersonFactEvidenceStatusEventsPath{ID: a.PersonID}, Query: &generated.ListPersonFactEvidenceStatusEventsQuery{EvidenceKey: a.EvidenceKey, Supported: a.Supported, Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
		return r, true, e
	case "list_person_fact_pins", "get_person_tracking":
		a, e := decodeMCPOperationArguments[factPersonArgs](args)
		if e != nil || !validLoopID(a.PersonID) {
			return invalid()
		}
		if name == "list_person_fact_pins" {
			r, e := readMCPOperationJSON[generated.PersonFactPinsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-pins", &generated.ListPersonFactPinsRequestOptions{PathParams: &generated.ListPersonFactPinsPath{ID: a.PersonID}}, http.StatusOK)
			return r, true, e
		}
		r, e := readMCPOperationJSON[generated.PersonTracking](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/tracking", &generated.GetPersonTrackingRequestOptions{PathParams: &generated.GetPersonTrackingPath{ID: a.PersonID}}, http.StatusOK)
		return r, true, e
	case "set_person_fact_pin":
		a, e := decodeMCPOperationArguments[factPinArgs](args)
		if e != nil || !factPinValid(a) {
			return invalid()
		}
		r, e := readMCPOperationJSONWithClassifier[generated.PersonFactPinWrite](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/fact-pins/{kind}/{key}", &generated.SetPersonFactPinRequestOptions{PathParams: &generated.SetPersonFactPinPath{ID: a.PersonID, Kind: generated.SetPersonFactPinPathKind(a.Kind), Key: url.PathEscape(a.Key)}, Body: &generated.SetPersonFactPinBody{Pinned: *a.Pinned}}, factClassify, http.StatusOK)
		return r, true, e
	case "set_person_tracking":
		a, e := decodeMCPOperationArguments[factTrackingArgs](args)
		if e != nil || !validLoopID(a.PersonID) || a.Tracked == nil {
			return invalid()
		}
		r, e := readMCPOperationJSONWithClassifier[generated.PersonTracking](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/tracking", &generated.SetPersonTrackingRequestOptions{PathParams: &generated.SetPersonTrackingPath{ID: a.PersonID}, Body: &generated.SetPersonTrackingBody{Tracked: *a.Tracked}}, factClassify, http.StatusOK)
		return r, true, e
	}
	return nil, false, nil
}
func (b *daemonMCPOperations) personFactOperationDisclosure(name string, args map[string]any) (string, bool, error) {
	switch name {
	case "set_person_fact_pin":
		a, e := decodeMCPOperationArguments[factPinArgs](args)
		if e != nil || !factPinValid(a) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
	case "set_person_tracking":
		a, e := decodeMCPOperationArguments[factTrackingArgs](args)
		if e != nil || !validLoopID(a.PersonID) || a.Tracked == nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
	default:
		return "", false, nil
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, args, "Replace the requested state; the native operation has no revision guard."}, json.Deterministic(true))
	return string(data), true, err
}
