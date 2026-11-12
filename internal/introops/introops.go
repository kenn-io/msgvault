package introops

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
)

var ErrForbidden = errors.New("not permitted")

type Service struct{ Store *store.Store }
type Record struct {
	store.PersonIntroduction

	Members  []store.IntroductionMember  `json:"members"`
	Blockers []store.IntroductionBlocker `json:"blockers"`
}
type Page struct {
	Items       []Record `json:"items"`
	HasMore     bool     `json:"has_more"`
	NextAfterID string   `json:"next_after_id"`
}

func ref(row store.PersonIntroduction) agentgrant.SourceRef {
	return agentgrant.SourceRef{Type: row.SourceType, Identifier: row.SourceIdentifier}
}
func allowed(g *agentgrant.Grant, row store.PersonIntroduction) bool {
	return g == nil || g.Allows(agentgrant.PermissionDraftCreate, ref(row))
}
func (s Service) project(ctx context.Context, g *agentgrant.Grant, row store.PersonIntroduction) (Record, error) {
	if !allowed(g, row) {
		return Record{}, ErrForbidden
	}
	p, err := s.Store.GetPersonIntroductionPreflightContext(ctx, row.SourceID, row.PersonAID, row.PersonBID)
	if err != nil {
		if g != nil && (errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrIntroductionSource)) {
			return Record{}, ErrForbidden
		}
		return Record{}, err
	}
	if g != nil && !visible(p) {
		return Record{}, ErrForbidden
	}
	return projectedRecord(g, row, p), nil
}
func visible(p store.PersonIntroductionPreflight) bool {
	for _, blocker := range p.Blockers {
		if blocker.Code == "owner_selected" || blocker.Code == "no_identity_on_account" {
			return false
		}
	}
	return true
}
func projectedRecord(g *agentgrant.Grant, row store.PersonIntroduction, p store.PersonIntroductionPreflight) Record {
	if g != nil {
		if !g.Allows(agentgrant.PermissionDraftEdit, ref(row)) {
			row.Body = ""
		}
		p.Members = append([]store.IntroductionMember(nil), p.Members...)
		for i := range p.Members {
			p.Members[i].UserID = ""
		}
	}
	return Record{row, p.Members, p.Blockers}
}
func (s Service) Prepare(ctx context.Context, g *agentgrant.Grant, in store.PersonIntroductionPrepare) (Record, error) {
	if in.SourceID <= 0 {
		return Record{}, store.ErrIntroductionInvalid
	}
	src, err := s.Store.GetSourceByIDContext(ctx, in.SourceID)
	if err != nil {
		if g != nil && errors.Is(err, store.ErrSourceNotFound) {
			return Record{}, ErrForbidden
		}
		return Record{}, err
	}
	if g != nil && !g.Allows(agentgrant.PermissionDraftCreate, agentgrant.SourceRef{Type: src.SourceType, Identifier: src.Identifier}) {
		return Record{}, ErrForbidden
	}
	p, err := s.Store.GetPersonIntroductionPreflightContext(ctx, in.SourceID, in.PersonAID, in.PersonBID)
	if err != nil {
		if g != nil && (errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrIntroductionSource)) {
			return Record{}, ErrForbidden
		}
		return Record{}, err
	}
	if g != nil && !visible(p) {
		return Record{}, ErrForbidden
	}
	actor := "owner"
	if g != nil {
		actor = g.ID
	}
	row, err := s.Store.PreparePersonIntroductionContext(ctx, in, actor)
	if err != nil {
		return Record{}, err
	}
	return projectedRecord(g, row, p), nil
}
func (s Service) Get(ctx context.Context, g *agentgrant.Grant, id string) (Record, error) {
	row, err := s.Store.GetPersonIntroductionContext(ctx, id)
	if err != nil {
		if g != nil && errors.Is(err, store.ErrIntroductionNotFound) {
			return Record{}, ErrForbidden
		}
		return Record{}, err
	}
	return s.project(ctx, g, row)
}
func (s Service) List(ctx context.Context, g *agentgrant.Grant, q store.PersonIntroductionQuery) (Page, error) {
	var ids []int64
	if g != nil {
		if !g.HasPermission(agentgrant.PermissionDraftCreate) {
			return Page{}, ErrForbidden
		}
		ids = []int64{}
		sources, err := s.Store.ListSourcesContext(ctx, "")
		if err != nil {
			return Page{}, err
		}
		for _, src := range sources {
			if g.MatchesSource(agentgrant.SourceRef{Type: src.SourceType, Identifier: src.Identifier}) {
				ids = append(ids, src.ID)
			}
		}
	}
	if g != nil {
		return s.listDelegated(ctx, g, q, ids)
	}
	rows, err := s.Store.ListPersonIntroductionsContext(ctx, q, ids)
	if err != nil {
		return Page{}, err
	}
	inputs := make([]store.PersonIntroductionPrepare, 0, len(rows.Items))
	for _, row := range rows.Items {
		if !allowed(g, row) {
			return Page{}, ErrForbidden
		}
		inputs = append(inputs, store.PersonIntroductionPrepare{SourceID: row.SourceID, PersonAID: row.PersonAID, PersonBID: row.PersonBID})
	}
	preflights, err := s.Store.GetPersonIntroductionPreflightsContext(ctx, inputs)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: []Record{}, HasMore: rows.HasMore, NextAfterID: rows.NextAfterID}
	for i, row := range rows.Items {
		out.Items = append(out.Items, projectedRecord(g, row, preflights[i]))
	}
	return out, nil
}

// Walk at most 100 candidates and share the owner lookup across their preflights.
func (s Service) listDelegated(ctx context.Context, g *agentgrant.Grant, q store.PersonIntroductionQuery, ids []int64) (Page, error) {
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return Page{}, store.ErrIntroductionInvalid
	}
	q.Limit = 1
	var candidates []store.PersonIntroduction
	var cursors []string
	var inputs []store.PersonIntroductionPrepare
	more := false
	for range 100 {
		rows, err := s.Store.ListPersonIntroductionsContext(ctx, q, ids)
		if err != nil {
			return Page{}, err
		}
		if len(rows.Items) == 0 {
			break
		}
		row := rows.Items[0]
		if !allowed(g, row) {
			return Page{}, ErrForbidden
		}
		if row.SourceType == "beeper" {
			candidates = append(candidates, row)
			cursors = append(cursors, rows.NextAfterID)
			inputs = append(inputs, store.PersonIntroductionPrepare{SourceID: row.SourceID, PersonAID: row.PersonAID, PersonBID: row.PersonBID})
		}
		more = rows.HasMore
		if !more {
			break
		}
		q.AfterID = rows.NextAfterID
	}
	preflights, err := s.Store.GetPersonIntroductionPreflightsContext(ctx, inputs)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: []Record{}}
	var afterVisible string
	for i, row := range candidates {
		if !visible(preflights[i]) {
			continue
		}
		if len(out.Items) == limit {
			out.HasMore, out.NextAfterID = true, afterVisible
			return out, nil
		}
		out.Items = append(out.Items, projectedRecord(g, row, preflights[i]))
		afterVisible = cursors[i]
	}
	if more {
		out.HasMore, out.NextAfterID = true, q.AfterID
	}
	return out, nil
}
func (s Service) Edit(ctx context.Context, g *agentgrant.Grant, id string, in store.PersonIntroductionEdit) (Record, error) {
	if g != nil {
		return Record{}, ErrForbidden
	}
	pre, err := s.Get(ctx, g, id)
	if err != nil {
		return Record{}, err
	}
	row, err := s.Store.EditPersonIntroductionContext(ctx, id, in)
	if err != nil {
		return Record{}, err
	}
	return Record{row, pre.Members, pre.Blockers}, nil
}
func (s Service) Cancel(ctx context.Context, g *agentgrant.Grant, id string, revision int64) (Record, error) {
	pre, err := s.Get(ctx, g, id)
	if err != nil {
		return Record{}, err
	}
	row, err := s.Store.CancelPersonIntroductionContext(ctx, id, revision)
	if err != nil {
		return Record{}, err
	}
	return projectedRecord(g, row, store.PersonIntroductionPreflight{Members: pre.Members, Blockers: pre.Blockers}), nil
}

// ErrorCode maps failures to stable, content-free codes shared by transports.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrForbidden):
		return "not_permitted"
	case errors.Is(err, store.ErrIntroductionNotFound):
		return "intro_not_found"
	case errors.Is(err, store.ErrIntroductionInvalid), errors.Is(err, store.ErrPersonNotFound):
		return "invalid_args"
	case errors.Is(err, store.ErrIntroductionSamePerson):
		return "same_person"
	case errors.Is(err, store.ErrIntroductionSource), errors.Is(err, store.ErrSourceNotFound):
		return "unsupported_source"
	case errors.Is(err, store.ErrSourceRetired):
		return "source_retired"
	case errors.Is(err, store.ErrIntroductionRevision):
		return "revision_mismatch"
	case errors.Is(err, store.ErrIntroductionState):
		return "state_conflict"
	default:
		return "local_store_failed"
	}
}
