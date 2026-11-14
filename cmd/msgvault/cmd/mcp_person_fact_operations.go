package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"slices"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
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
			(r.name != "list_person_fact_claims" || (has("listPersonFactTargets", "GET", "/api/v1/person-fact-targets") &&
				has("listPersonFactEvidence", "GET", "/api/v1/people/{id}/fact-evidence", "target", "limit", "offset") &&
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
		r, e := b.listPersonFactClaims(ctx, a)
		return r, true, e
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

const (
	personFactScanPageSize = 50
	personFactScanMaxPages = 100
)

// scanPersonFactRows reads every row of one list in fixed pages so rows can be
// filtered before the caller's limit and offset apply. It fails closed when the
// list exceeds the scan cap instead of returning unfiltered or partial data.
func scanPersonFactRows[R, T any](ctx context.Context, b *daemonMCPOperations, rows func(R) []T, options func(limit, offset *int64) runtime.RequestOptions, path string) ([]T, *mcpserver.OperationResult, error) {
	var all []T
	limit := int64(personFactScanPageSize)
	for page := range personFactScanMaxPages {
		offset := int64(page) * personFactScanPageSize
		r, e := readMCPOperationJSONWithClassifier[R](ctx, b.client, http.MethodGet, path, options(&limit, &offset), factClassify, http.StatusOK)
		if e != nil || r.IsError {
			return nil, r, e
		}
		output, _ := r.Output.(R)
		got := rows(output)
		all = append(all, got...)
		if len(got) < personFactScanPageSize {
			return all, nil, nil
		}
	}
	return nil, operationFailure("person_fact_scan_limit", false), nil
}

func personFactPage[T any](rows []T, limit, offset *int64) []T {
	from, count := int64(0), int64(personFactScanPageSize)
	if offset != nil {
		from = *offset
	}
	if limit != nil {
		count = *limit
	}
	if from >= int64(len(rows)) {
		return []T{}
	}
	return rows[from:min(from+count, int64(len(rows)))]
}

// listPersonFactClaims omits every row tied to a sensitive, retired, or unknown
// target. Evidence has no target and decisions reference claims by key, so both
// are filtered through an index of all of the person's claims.
func (b *daemonMCPOperations) listPersonFactClaims(ctx context.Context, a factArgs) (*mcpserver.OperationResult, error) {
	r, e := readMCPOperationJSONWithClassifier[generated.Catalog](ctx, b.client, http.MethodGet, "/api/v1/person-fact-targets", &generated.ListPersonFactTargetsRequestOptions{}, factClassify, http.StatusOK)
	if e != nil || r.IsError {
		return r, e
	}
	catalog, _ := r.Output.(generated.Catalog)
	visible := map[[2]string]bool{}
	for _, target := range catalog.Targets {
		visible[[2]string{target.Kind, target.Key}] = true
	}
	claimRows := func(r generated.PersonFactClaimsResponse) []generated.PersonFactClaim { return r.Claims }
	claimOptions := func(target *string) func(limit, offset *int64) runtime.RequestOptions {
		return func(limit, offset *int64) runtime.RequestOptions {
			return &generated.ListPersonFactClaimsRequestOptions{PathParams: &generated.ListPersonFactClaimsPath{ID: a.PersonID}, Query: &generated.ListPersonFactClaimsQuery{Target: target, Limit: limit, Offset: offset}}
		}
	}
	const claimsPath = "/api/v1/people/{id}/fact-claims"
	all, failed, e := scanPersonFactRows(ctx, b, claimRows, claimOptions(nil), claimsPath)
	if failed != nil || e != nil {
		return failed, e
	}
	allowedClaims, shown, hidden := map[string]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, claim := range all {
		allowed := visible[[2]string{claim.Target.Kind, claim.Target.Key}]
		allowedClaims[claim.ClaimKey] = allowed
		for _, id := range claim.EvidenceIds {
			if allowed {
				shown[id] = true
			} else {
				hidden[id] = true
			}
		}
	}
	selected := all
	if a.Target != nil {
		if selected, failed, e = scanPersonFactRows(ctx, b, claimRows, claimOptions(a.Target), claimsPath); failed != nil || e != nil {
			return failed, e
		}
	}
	out := mcpserver.PersonFactClaimsOutput{Claims: personFactPage(slices.DeleteFunc(slices.Clone(selected), func(c generated.PersonFactClaim) bool { return !allowedClaims[c.ClaimKey] }), a.Limit, a.Offset)}
	if a.IncludeEvidence {
		rows, failed, e := scanPersonFactRows(ctx, b, func(r generated.PersonFactEvidenceResponse) []generated.PersonFactEvidence { return r.Evidence }, func(limit, offset *int64) runtime.RequestOptions {
			return &generated.ListPersonFactEvidenceRequestOptions{PathParams: &generated.ListPersonFactEvidencePath{ID: a.PersonID}, Query: &generated.ListPersonFactEvidenceQuery{Target: a.Target, Limit: limit, Offset: offset}}
		}, "/api/v1/people/{id}/fact-evidence")
		if failed != nil || e != nil {
			return failed, e
		}
		out.Evidence = personFactPage(slices.DeleteFunc(rows, func(v generated.PersonFactEvidence) bool { return !shown[v.ID] || hidden[v.ID] }), a.Limit, a.Offset)
	}
	if a.IncludeDecisions {
		rows, failed, e := scanPersonFactRows(ctx, b, func(r generated.PersonFactDecisionsResponse) []generated.PersonFactDecision { return r.Decisions }, func(limit, offset *int64) runtime.RequestOptions {
			return &generated.ListPersonFactDecisionsRequestOptions{PathParams: &generated.ListPersonFactDecisionsPath{ID: a.PersonID}, Query: &generated.ListPersonFactDecisionsQuery{Target: a.Target, Limit: limit, Offset: offset}}
		}, "/api/v1/people/{id}/fact-decisions")
		if failed != nil || e != nil {
			return failed, e
		}
		out.Decisions = personFactPage(slices.DeleteFunc(rows, func(v generated.PersonFactDecision) bool {
			return !allowedClaims[v.ClaimKey] || v.CompetingClaimKey != nil && !allowedClaims[*v.CompetingClaimKey]
		}), a.Limit, a.Offset)
	}
	return &mcpserver.OperationResult{Output: out}, nil
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
