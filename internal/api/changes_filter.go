package api

import (
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/store"
)

type changesFilter struct {
	PersonID                   int64
	SourceIDs, ConversationIDs []int64
	IncomingOnly               bool
	participants               []int64
	grantRefs                  []changeGrantRef
}

func (f changesFilter) hasSelectors() bool { return f.shared().HasSelectors() }

func parseChangesFilter(r *http.Request) (changesFilter, error) {
	var f changesFilter
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

type changeGrantRef struct{ Type, Identifier string }

func (f changesFilter) bindGrant(grant *agentgrant.Grant) changesFilter {
	// A non-nil slice distinguishes even an empty grant from an owner.
	f.grantRefs = make([]changeGrantRef, 0, len(grant.Sources))
	for _, ref := range grant.Sources {
		f.grantRefs = append(f.grantRefs, changeGrantRef{ref.Type, ref.Identifier})
	}
	slices.SortFunc(f.grantRefs, func(a, b changeGrantRef) int {
		if n := strings.Compare(a.Type, b.Type); n != 0 {
			return n
		}
		return strings.Compare(a.Identifier, b.Identifier)
	})
	return f
}

func (f changesFilter) shared() changefeed.Filter {
	var refs []changefeed.GrantRef
	if f.grantRefs != nil {
		refs = make([]changefeed.GrantRef, 0, len(f.grantRefs))
		for _, r := range f.grantRefs {
			refs = append(refs, changefeed.GrantRef{Type: r.Type, Identifier: r.Identifier})
		}
	}
	return changefeed.Filter{PersonID: f.PersonID, SourceIDs: f.SourceIDs, ConversationIDs: f.ConversationIDs, IncomingOnly: f.IncomingOnly, Participants: f.participants, GrantRefs: refs}
}
func (f changesFilter) digest() string                      { return f.shared().Digest() }
func (f changesFilter) matches(m store.ChangedMessage) bool { return f.shared().Matches(m) }
