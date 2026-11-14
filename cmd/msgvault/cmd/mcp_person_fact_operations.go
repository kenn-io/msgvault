package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"slices"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/personfacts"
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
		{"set_person_fact_pin", "setPersonFactPin", "PUT", "/api/v1/people/{id}/fact-pins/{kind}/{key}", []string{"visible_only"}},
		{"set_person_tracking", "setPersonTracking", "PUT", "/api/v1/people/{id}/tracking", nil},
	} {
		if has(r.id, r.method, r.path, r.query...) &&
			(r.name == "set_person_tracking" || r.name == "set_person_fact_pin" || has("listPersonFactTargets", "GET", "/api/v1/person-fact-targets")) &&
			(r.name != "list_person_fact_claims" || (has("listPersonFactEvidence", "GET", "/api/v1/people/{id}/fact-evidence", "target", "limit", "offset", "visible_only") &&
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
	a, err := decodeMCPOperationArguments[factArgs](args)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil
	}
	switch name {
	case "list_person_fact_claims":
		r, e := b.listPersonFactClaims(ctx, a)
		return r, true, e
	case "list_person_fact_pins":
		visible, failure, err := b.personFactVisibleTargets(ctx)
		if failure != nil || err != nil {
			return failure, true, err
		}
		r, e := readMCPOperationJSONWithClassifier[generated.PersonFactPinsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-pins", &generated.ListPersonFactPinsRequestOptions{PathParams: &generated.ListPersonFactPinsPath{ID: a.PersonID}}, factClassify, http.StatusOK)
		if e == nil && !r.IsError {
			out, _ := r.Output.(generated.PersonFactPinsResponse)
			out.Pins = slices.DeleteFunc(out.Pins, func(p generated.PinState) bool { return !visible[[2]string{p.Target.Kind, p.Target.Key}] })
			r.Output = out
		}
		return r, true, e
	case "set_person_fact_pin":
		r, e := readMCPOperationJSONWithClassifier[generated.PersonFactPinWrite](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/fact-pins/{kind}/{key}", &generated.SetPersonFactPinRequestOptions{PathParams: &generated.SetPersonFactPinPath{ID: a.PersonID, Kind: generated.SetPersonFactPinPathKind(a.Kind), Key: url.PathEscape(a.Key)}, Query: &generated.SetPersonFactPinQuery{VisibleOnly: new(true)}, Body: &generated.SetPersonFactPinBody{Pinned: a.Pinned}}, factClassify, http.StatusOK)
		return r, true, e
	case "set_person_tracking":
		r, e := readMCPOperationJSONWithClassifier[generated.PersonTracking](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/tracking", &generated.SetPersonTrackingRequestOptions{PathParams: &generated.SetPersonTrackingPath{ID: a.PersonID}, Body: &generated.SetPersonTrackingBody{Tracked: a.Tracked}}, factClassify, http.StatusOK)
		return r, true, e
	}
	return nil, false, nil
}

func (b *daemonMCPOperations) personFactVisibleTargets(ctx context.Context) (map[[2]string]bool, *mcpserver.OperationResult, error) {
	r, err := readMCPOperationJSONWithClassifier[generated.Catalog](ctx, b.client, http.MethodGet, "/api/v1/person-fact-targets", &generated.ListPersonFactTargetsRequestOptions{}, factClassify, http.StatusOK)
	if err != nil || r.IsError {
		return nil, r, err
	}
	catalog, _ := r.Output.(generated.Catalog)
	visible := map[[2]string]bool{}
	for _, target := range catalog.Targets {
		visible[[2]string{target.Kind, target.Key}] = true
	}
	return visible, nil, nil
}

func (b *daemonMCPOperations) listPersonFactClaims(ctx context.Context, a factArgs) (*mcpserver.OperationResult, error) {
	if a.Target == nil && (a.IncludeEvidence || a.IncludeDecisions) {
		return operationFailure("invalid_arguments", false), nil
	}
	var target personfacts.TargetRef
	if a.Target != nil {
		decoded, err := personfacts.DecodeTargetRef(*a.Target)
		if err != nil {
			return operationFailure("invalid_target", false), nil //nolint:nilerr // Invalid targets are operation refusals.
		}
		target = decoded
	}
	r, err := readMCPOperationJSONWithClassifier[generated.PersonFactClaimsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-claims", &generated.ListPersonFactClaimsRequestOptions{PathParams: &generated.ListPersonFactClaimsPath{ID: a.PersonID}, Query: &generated.ListPersonFactClaimsQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, factClassify, http.StatusOK)
	if err != nil || r.IsError {
		return r, err
	}
	claims, _ := r.Output.(generated.PersonFactClaimsResponse)
	limit, offset := int64(50), int64(0)
	if a.Limit != nil {
		limit = *a.Limit
	}
	if a.Offset != nil {
		offset = *a.Offset
	}
	out := mcpserver.PersonFactClaimsOutput{}
	more := int64(len(claims.Claims)) == limit
	if a.IncludeEvidence {
		r, err := readMCPOperationJSONWithClassifier[generated.PersonFactEvidenceResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-evidence", &generated.ListPersonFactEvidenceRequestOptions{PathParams: &generated.ListPersonFactEvidencePath{ID: a.PersonID}, Query: &generated.ListPersonFactEvidenceQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset, VisibleOnly: new(true)}}, factClassify, http.StatusOK)
		if err != nil || r.IsError {
			return r, err
		}
		rows, _ := r.Output.(generated.PersonFactEvidenceResponse)
		out.Evidence = rows.Evidence
		more = more || int64(len(rows.Evidence)) == limit
	}
	if a.IncludeDecisions {
		r, err := readMCPOperationJSONWithClassifier[generated.PersonFactDecisionsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/fact-decisions", &generated.ListPersonFactDecisionsRequestOptions{PathParams: &generated.ListPersonFactDecisionsPath{ID: a.PersonID}, Query: &generated.ListPersonFactDecisionsQuery{Target: a.Target, Limit: a.Limit, Offset: a.Offset}}, factClassify, http.StatusOK)
		if err != nil || r.IsError {
			return r, err
		}
		rows, _ := r.Output.(generated.PersonFactDecisionsResponse)
		out.Decisions = rows.Decisions
		more = more || int64(len(rows.Decisions)) == limit
	}
	visible, failure, err := b.personFactVisibleTargets(ctx)
	if failure != nil || err != nil {
		return failure, err
	}
	if a.Target != nil && !visible[[2]string{string(target.Kind), target.Key}] {
		return operationFailure("invalid_fact_target", false), nil
	}
	if more {
		out.NextOffset = new(offset + limit)
	}
	return &mcpserver.OperationResult{Output: mcpserver.PersonFactClaimsOutput{Claims: slices.DeleteFunc(claims.Claims, func(c generated.PersonFactClaim) bool { return !visible[[2]string{c.Target.Kind, c.Target.Key}] }), Evidence: out.Evidence, Decisions: out.Decisions, NextOffset: out.NextOffset}}, nil
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
