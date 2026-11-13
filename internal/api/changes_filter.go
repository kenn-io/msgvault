package api

import (
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/changefeed"
)

func parseChangesFilter(r *http.Request) (changefeed.Filter, error) {
	var f changefeed.Filter
	var err error
	f.PersonID, _, err = queryInt64(r, "person_id")
	if err != nil {
		return f, err
	}
	if r.URL.Query().Has("person_id") && f.PersonID <= 0 {
		return f, newParamError("person_id", "person_id must be positive")
	}
	for _, item := range []struct {
		name   string
		target *[]int64
	}{{"source_id", &f.SourceIDs}, {"conversation_id", &f.ConversationIDs}} {
		values, _, e := queryInt64s(r, item.name)
		if e != nil {
			return f, e
		}
		if len(values) > 50 {
			return f, newParamError(item.name, item.name+" accepts at most 50 values")
		}
		for _, id := range values {
			if id <= 0 {
				return f, newParamError(item.name, item.name+" values must be positive")
			}
		}
		slices.Sort(values)
		*item.target = slices.Compact(values)
	}
	f.IncomingOnly, _, err = queryBool(r, "incoming_only")
	if err != nil {
		return f, err
	}
	if r.URL.Query().Has("incoming_only") && r.URL.Query().Get("incoming_only") == "" {
		return f, newParamError("incoming_only", "incoming_only must be a boolean")
	}
	return f, nil
}

func bindChangesGrant(f changefeed.Filter, grant *agentgrant.Grant) changefeed.Filter {
	f.GrantRefs = make([]changefeed.GrantRef, 0, len(grant.Sources))
	for _, ref := range grant.Sources {
		f.GrantRefs = append(f.GrantRefs, changefeed.GrantRef{Type: ref.Type, Identifier: ref.Identifier})
	}
	slices.SortFunc(f.GrantRefs, func(a, b changefeed.GrantRef) int {
		if n := strings.Compare(a.Type, b.Type); n != 0 {
			return n
		}
		return strings.Compare(a.Identifier, b.Identifier)
	})
	return f
}
