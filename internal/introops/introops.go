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
	if g != nil && !g.Allows(agentgrant.PermissionDraftEdit, ref(row)) {
		row.Body = ""
	}
	p, err := s.Store.GetPersonIntroductionPreflightContext(ctx, row.SourceID, row.PersonAID, row.PersonBID)
	if err != nil {
		return Record{}, err
	}
	return Record{row, p.Members, p.Blockers}, nil
}
func (s Service) Prepare(ctx context.Context, g *agentgrant.Grant, in store.PersonIntroductionPrepare) (Record, error) {
	src, err := s.Store.GetSourceByIDContext(ctx, in.SourceID)
	if err != nil {
		if g != nil {
			return Record{}, ErrForbidden
		}
		return Record{}, err
	}
	if g != nil && !g.Allows(agentgrant.PermissionDraftCreate, agentgrant.SourceRef{Type: src.SourceType, Identifier: src.Identifier}) {
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
	return s.project(ctx, g, row)
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
	rows, err := s.Store.ListPersonIntroductionsContext(ctx, q, ids)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: []Record{}, HasMore: rows.HasMore, NextAfterID: rows.NextAfterID}
	for _, row := range rows.Items {
		record, err := s.project(ctx, g, row)
		if err != nil {
			return Page{}, err
		}
		out.Items = append(out.Items, record)
	}
	return out, nil
}
func (s Service) Edit(ctx context.Context, g *agentgrant.Grant, id string, in store.PersonIntroductionEdit) (Record, error) {
	if g != nil {
		return Record{}, ErrForbidden
	}
	row, err := s.Store.EditPersonIntroductionContext(ctx, id, in)
	if err != nil {
		return Record{}, err
	}
	return s.project(ctx, g, row)
}
func (s Service) Cancel(ctx context.Context, g *agentgrant.Grant, id string, revision int64) (Record, error) {
	if _, err := s.Get(ctx, g, id); err != nil {
		return Record{}, err
	}
	row, err := s.Store.CancelPersonIntroductionContext(ctx, id, revision)
	if err != nil {
		return Record{}, err
	}
	return s.project(ctx, g, row)
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
