package daemonclient

import (
	"go.kenn.io/msgvault/internal/whoknows"
	"go.kenn.io/msgvault/pkg/client/generated"
	"time"
)

// WhoKnowsRequestOptions keeps zero values explicit for the daemon's defaults.
func WhoKnowsRequestOptions(topic string, p whoknows.Params) *generated.FindPeopleWhoKnowRequestOptions {
	q := &generated.FindPeopleWhoKnowQuery{Q: topic, Limit: new(int64(p.Limit)), Evidence: new(int64(p.Evidence)), Candidates: new(int64(p.Candidates)), HalfLifeDays: new(int64(p.HalfLifeDays)), MinScore: new(p.MinScore), MinProfileScore: new(p.MinProfileScore), IncludeOwner: new(p.IncludeOwner), IncludeUnsaved: new(p.IncludeUnsaved), IncludeOrganizations: new(p.IncludeOrganizations), Excerpts: new(p.Excerpts), Tiebreak: new(generated.FindPeopleWhoKnowQueryTiebreak(p.Tiebreak))}
	if p.After != nil {
		q.After = new(p.After.Format(time.RFC3339Nano))
	}
	if p.Before != nil {
		q.Before = new(p.Before.Format(time.RFC3339Nano))
	}
	return &generated.FindPeopleWhoKnowRequestOptions{Query: q}
}
