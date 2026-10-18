package api

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

// PersonCardStore supplies bounded profile resolution. Optional card inputs use
// the same local feature interfaces as their existing HTTP readers.
type PersonCardStore interface {
	personCardMaintenanceStore
	GetPersonContext(ctx context.Context, personID int64) (*store.Person, error)
	ResolvePersonByEmailContext(ctx context.Context, email string) ([]int64, error)
	EmailParticipantContext(ctx context.Context, email string) (int64, error)
	GetParticipantIdentityContext(ctx context.Context, participantIDs []int64) (*store.ParticipantIdentityContext, error)
	ListPersonContactPointsContext(ctx context.Context, personID int64, currentOnly bool) ([]store.PersonContactPoint, error)
	ListPersonCategoriesContext(ctx context.Context, personID int64, currentOnly bool) ([]store.PersonCategory, error)
}

func (s *Server) registerPersonCardRoutes(api huma.API) {
	op := rawAPIV1Operation("getPersonCard", http.MethodGet, "/people/{ref}/card", "Prepare a bounded person card (owner only)")
	op.Description = "Read-only current profile projection with dated citations, gaps and freshness. as_of is an evaluation time, not a historical snapshot."
	op.Parameters = []*huma.Param{{Name: "ref", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}, Description: "Numeric person ID, person:<id>, participant:<id>, or email:<address>"}, queryStringParam("as_of", "RFC3339 evaluation time; defaults to collection time", false), queryIntegerParam("max_items", "Ranked items (default 3, range 1–10)"), queryIntegerParam("max_bytes", "Content byte budget (default 16384, range 2048–65536)"), queryStringParam("format", "json (default) or markdown; both return a JSON envelope", false), queryStringParam("affiliation", "Optional internal or external Markdown label", false)}
	op.Responses = jsonResponsesFor[personcard.PersonCardResult](api)
	addErrorResponses(api, op.Responses, 400, 401, 403, 404, 500, 503)
	registerRawHumaRoute(api, op, s.handleGetPersonCard)
}

func (s *Server) handleGetPersonCard(w http.ResponseWriter, r *http.Request) {
	if !s.ownerAccessAuthorized(r) {
		writeError(w, 403, "ref_not_permitted", "Person cards require owner access")
		return
	}
	now := s.clockNow().UTC()
	opts, code := parsePersonCardOptions(r.URL.Query(), now)
	if code != "" {
		writeError(w, 400, code, "Invalid person card parameter")
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	kind, id, email, err := parsePersonCardRef(ref)
	if err != nil {
		writeError(w, 400, "invalid_ref", "Invalid person reference")
		return
	}
	if kind == "email" {
		ref = "email:" + email
	}
	backend, ok := s.store.(PersonCardStore)
	if !ok {
		writeError(w, 503, "person_cards_unavailable", "Person cards are unavailable")
		return
	}
	person, status, participantID, candidates, err := resolvePersonCard(r.Context(), backend, kind, id, email)
	if err != nil {
		if errors.Is(err, store.ErrPersonNotFound) || errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "person_not_found", "Person not found")
		} else if !s.writeIfContextError(w, err) {
			s.logger.Error("resolve person card", "error", err)
			writeError(w, 500, "person_card_failed", "Could not read person card")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if status != "ok" {
		result := personcard.ResultForStatus(status, ref, now, opts, candidates, participantID)
		writePersonCardJSON(w, result)
		return
	}
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: person.ID, VCardUID: person.VCardUID}}
	if person.DisplayName != nil {
		in.Person.UntrustedText.DisplayName = *person.DisplayName
	}
	points, err := backend.ListPersonContactPointsContext(r.Context(), person.ID, true)
	if err != nil {
		s.writePersonCardRequiredError(w, err)
		return
	}
	for _, point := range points {
		if point.AddressKind == store.ContactAddressEmail {
			in.Person.Emails = append(in.Person.Emails, strings.ToLower(strings.TrimSpace(point.OriginalValue)))
		}
	}
	categories, err := backend.ListPersonCategoriesContext(r.Context(), person.ID, true)
	if err != nil {
		s.writePersonCardRequiredError(w, err)
		return
	}
	for _, category := range categories {
		in.Person.UntrustedText.Categories = append(in.Person.UntrustedText.Categories, category.OriginalValue)
	}
	if err := fillPersonCardProfile(r.Context(), backend, person.ID, &in); err != nil {
		s.writePersonCardRequiredError(w, err)
		return
	}
	s.gatherPersonCard(r.Context(), person, &in, opts)
	card := personcard.Assemble(in, opts)
	result, err := personcard.BuildResult(ref, card, opts)
	if err != nil {
		if !errors.Is(err, personcard.ErrBudgetTooSmall) {
			s.logger.Error("encode person card", "error", err)
			writeError(w, 500, "person_card_failed", "Could not encode person card")
			return
		}
		writeError(w, 400, "budget_too_small", "Card cannot fit this byte budget; increase max_bytes")
		return
	}
	writePersonCardJSON(w, result)
}

func (s *Server) writePersonCardRequiredError(w http.ResponseWriter, err error) {
	if !s.writeIfContextError(w, err) {
		s.logger.Error("read person card profile", "error", err)
		writeError(w, 500, "person_card_failed", "Could not read required person profile")
	}
}

func parsePersonCardRef(ref string) (kind string, id int64, email string, err error) {
	if value, ok := strings.CutPrefix(ref, "email:"); ok {
		email = strings.ToLower(strings.TrimSpace(value))
		var address *mail.Address
		address, err = mail.ParseAddress(email)
		if err == nil && (address.Address != email || address.Name != "" || strings.Count(email, "@") != 1 || strings.ContainsAny(email, "\r\n\t ")) {
			err = store.ErrInvalidArgument
		}
		if len(email) > 320 {
			err = store.ErrInvalidArgument
		}
		return "email", 0, email, err
	}
	kind, id, err = store.ParsePersonMeetingRef(ref)
	return
}

func resolvePersonCard(ctx context.Context, b PersonCardStore, kind string, id int64, email string) (*store.Person, string, *int64, []personcard.Candidate, error) {
	if kind == "email" {
		ids, err := b.ResolvePersonByEmailContext(ctx, email)
		if err != nil {
			return nil, "", nil, nil, err
		}
		if len(ids) > 1 {
			candidates := make([]personcard.Candidate, 0, len(ids))
			for _, pid := range ids {
				p, err := b.GetPersonContext(ctx, pid)
				if err != nil {
					return nil, "", nil, nil, err
				}
				c := personcard.Candidate{PersonID: pid}
				if p.DisplayName != nil {
					c.UntrustedText.DisplayName = *p.DisplayName
				}
				candidates = append(candidates, c)
			}
			return nil, "ambiguous", nil, candidates, nil
		}
		if len(ids) == 1 {
			id = ids[0]
			kind = "person"
		} else {
			pid, err := b.EmailParticipantContext(ctx, email)
			if err != nil {
				return nil, "", nil, nil, err
			}
			if pid > 0 {
				return nil, "observed_only", &pid, nil, nil
			}
			return nil, "unknown", nil, nil, nil
		}
	}
	if kind == "participant" {
		identity, err := b.GetParticipantIdentityContext(ctx, []int64{id})
		if err != nil {
			return nil, "", nil, nil, err
		}
		if identity == nil || len(identity.Members) == 0 {
			return nil, "unknown", nil, nil, nil
		}
		resolved, err := resolver.Resolve(ctx, b, resolver.Reference{Kind: resolver.ReferenceParticipant, ID: id}, nil)
		if err != nil && !errors.Is(err, resolver.ErrEmptyPopulation) {
			return nil, "", nil, nil, err
		}
		if resolved.PersonID == 0 {
			return nil, "observed_only", &id, nil, nil
		}
		id = resolved.PersonID
	}
	p, err := b.GetPersonContext(ctx, id)
	return p, "ok", nil, nil, err
}

func writePersonCardJSON(w http.ResponseWriter, result personcard.PersonCardResult) {
	data, err := json.Marshal(result,
		json.FormatNilSliceAsNull(false),
		jsontext.AllowInvalidUTF8(true),
		json.Deterministic(true),
	)
	if err != nil {
		writeError(w, 500, "person_card_failed", "Could not encode person card")
		return
	}
	w.Header().Set("Content-Type", applicationJSONMediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- The body is JSON-encoded, served as application/json, and marked nosniff.
	_, _ = w.Write(data)
}
