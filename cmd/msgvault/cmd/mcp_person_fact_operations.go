package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personFactMCPCapabilities(has mcpRouteCheck) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path string
		query                  []string
	}{
		{"list_person_fact_claims", "listPersonFactClaims", "GET", "/api/v1/people/{id}/fact-claims", []string{"target", "limit", "offset"}},
		{"list_person_fact_pins", "listPersonFactPins", "GET", "/api/v1/people/{id}/fact-pins", nil},
		{"set_person_fact_pin", "setPersonFactPin", "PUT", "/api/v1/people/{id}/fact-pins/{kind}/{key}", nil},
		{"set_person_tracking", "setPersonTracking", "PUT", "/api/v1/people/{id}/tracking", nil},
	} {
		if has(r.id, r.method, r.path, r.query...) &&
			(r.name != "list_person_fact_claims" || (has("listPersonFactEvidence", "GET", "/api/v1/people/{id}/fact-evidence", "target", "limit", "offset") &&
				has("listPersonFactDecisions", "GET", "/api/v1/people/{id}/fact-decisions", "target", "limit", "offset"))) {
			names = append(names, r.name)
		}
	}
	return names
}

type factArgs struct {
	PersonID         int64   `json:"person_id"`
	Limit            *int64  `json:"limit,omitempty"`
	Offset           *int64  `json:"offset,omitempty"`
	Target           *string `json:"target,omitempty"`
	IncludeEvidence  bool    `json:"include_evidence,omitempty"`
	IncludeDecisions bool    `json:"include_decisions,omitempty"`
	Kind             string  `json:"kind"`
	Key              string  `json:"key"`
	Pinned           bool    `json:"pinned"`
	Tracked          bool    `json:"tracked"`
}

func factClassify(code string) bool {
	switch code {
	case "invalid_target", "invalid_target_key", "invalid_fact_target", "person_fact_conflict", "person_facts_unavailable", "tracking_unavailable":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executePersonFactOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "list_person_fact_claims", "list_person_fact_pins", "set_person_fact_pin", "set_person_tracking":
	default:
		return nil, false, nil
	}
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	a, err := decodeMCPOperationArguments[factArgs](args)
	if err != nil {
		return invalid()
	}
	switch name {
	case "list_person_fact_claims":
		r, e := readMCPOperationJSONWithClassifier[generated.PersonFactClaimsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-claims", &generated.ListPersonFactClaimsRequestOptions{PathParams: &generated.ListPersonFactClaimsPath{ID: a.PersonID}, Query: &generated.ListPersonFactClaimsQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, factClassify, http.StatusOK)
		if e != nil || r.IsError {
			return r, true, e
		}
		claims, _ := r.Output.(generated.PersonFactClaimsResponse)
		out := mcpserver.PersonFactClaimsOutput{Claims: claims.Claims}
		if a.IncludeEvidence {
			r, e = readMCPOperationJSONWithClassifier[generated.PersonFactEvidenceResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-evidence", &generated.ListPersonFactEvidenceRequestOptions{PathParams: &generated.ListPersonFactEvidencePath{ID: a.PersonID}, Query: &generated.ListPersonFactEvidenceQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, factClassify, http.StatusOK)
			if e != nil || r.IsError {
				return r, true, e
			}
			page, _ := r.Output.(generated.PersonFactEvidenceResponse)
			out.Evidence = page.Evidence
		}
		if a.IncludeDecisions {
			r, e = readMCPOperationJSONWithClassifier[generated.PersonFactDecisionsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-decisions", &generated.ListPersonFactDecisionsRequestOptions{PathParams: &generated.ListPersonFactDecisionsPath{ID: a.PersonID}, Query: &generated.ListPersonFactDecisionsQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, factClassify, http.StatusOK)
			if e != nil || r.IsError {
				return r, true, e
			}
			page, _ := r.Output.(generated.PersonFactDecisionsResponse)
			out.Decisions = page.Decisions
		}
		return &mcpserver.OperationResult{Output: out}, true, nil
	case "list_person_fact_pins":
		r, e := readMCPOperationJSONWithClassifier[generated.PersonFactPinsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-pins", &generated.ListPersonFactPinsRequestOptions{PathParams: &generated.ListPersonFactPinsPath{ID: a.PersonID}}, factClassify, http.StatusOK)
		return r, true, e
	case "set_person_fact_pin":
		r, e := readMCPOperationJSONWithClassifier[generated.PersonFactPinWrite](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/fact-pins/{kind}/{key}", &generated.SetPersonFactPinRequestOptions{PathParams: &generated.SetPersonFactPinPath{ID: a.PersonID, Kind: generated.SetPersonFactPinPathKind(a.Kind), Key: url.PathEscape(a.Key)}, Body: &generated.SetPersonFactPinBody{Pinned: a.Pinned}}, factClassify, http.StatusOK)
		return r, true, e
	case "set_person_tracking":
		r, e := readMCPOperationJSONWithClassifier[generated.PersonTracking](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/tracking", &generated.SetPersonTrackingRequestOptions{PathParams: &generated.SetPersonTrackingPath{ID: a.PersonID}, Body: &generated.SetPersonTrackingBody{Tracked: a.Tracked}}, factClassify, http.StatusOK)
		return r, true, e
	}
	return nil, false, nil
}
func (b *daemonMCPOperations) personFactOperationDisclosure(name string, args map[string]any) (string, bool, error) {
	switch name {
	case "set_person_fact_pin", "set_person_tracking":
	default:
		return "", false, nil
	}
	a, err := decodeMCPOperationArguments[factArgs](args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	var effect string
	switch name {
	case "set_person_fact_pin":
		effect = "Re-resolves the effective value and may change profile and vCard projections; the person must be tracked. The native operation has no revision guard."
	case "set_person_tracking":
		if a.Tracked {
			effect = "Enrolls the person in people sweeps, which send limited archived text to the configured sweep model when enabled and may update profile facts; with enrichment consent it also queues external enrichment lookups. The native operation has no revision guard."
		} else {
			effect = "Stops automatic fact maintenance and removes queued sweep work and pending enrichment. The native operation has no revision guard."
		}
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
