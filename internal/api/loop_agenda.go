package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type loopAgendaArchive interface {
	loops.Store
	GetPersonCommitmentContext(context.Context, int64, time.Time) (store.PersonCommitment, error)
	GetPersonContext(context.Context, int64) (*store.Person, error)
	WalkOpenPersonCommitments(context.Context, time.Time, func(store.PersonCommitment) error) error
}

var errAgendaPersonRequired = errors.New("link the contact to a person first")

// LoopAgenda files open obligations with their archive citations.
type LoopAgenda struct {
	archive loopAgendaArchive
	backend *kataIssueBackend
	origin  string
}

func NewLoopAgenda(cfg config.TaskIntegrationConfig, archive *store.Store, origin string) *LoopAgenda {
	cfg.ApplyDefaults()
	return &LoopAgenda{archive: archive, backend: &kataIssueBackend{config: cfg, store: archive, evidence: kataevidence.New(archive)}, origin: origin}
}

func (a *LoopAgenda) File(ctx context.Context, ref string, now time.Time) (kataissues.Result, error) {
	return a.file(ctx, ref, now, nil)
}

func (a *LoopAgenda) file(ctx context.Context, ref string, now time.Time, service *kataissues.Service) (kataissues.Result, error) {
	var input kataissues.CreateInput
	var messageID, conversationID int64
	var selectors []kataevidence.Selector
	var expected string
	var key string
	if strings.HasPrefix(ref, "c") {
		id, err := strconv.ParseInt(strings.TrimPrefix(ref, "c"), 10, 64)
		if err != nil || id < 1 {
			return kataissues.Result{}, sql.ErrNoRows
		}
		c, err := a.archive.GetPersonCommitmentContext(ctx, id, now)
		if err != nil {
			return kataissues.Result{}, err
		}
		messageID = c.AnchorMessageID
		conversation, err := a.archive.MessageConversation(ctx, messageID)
		if err != nil {
			return kataissues.Result{}, err
		}
		conversationID = conversation.ConversationID
		input.Title, input.List, input.PersonID, input.DeadlineOn = c.Summary, "promises", &c.PersonID, c.DueOn
		input.SourceTime = c.PromisedAt
		if c.Direction == "owed_to" {
			person, err := a.archive.GetPersonContext(ctx, c.PersonID)
			if err != nil {
				return kataissues.Result{}, err
			}
			name := fmt.Sprintf("person %d", c.PersonID)
			if person.DisplayName != nil && *person.DisplayName != "" {
				name = *person.DisplayName
			}
			input.Title, input.List = fmt.Sprintf("Waiting on %s: %s", name, c.Summary), "waiting"
		}
		expected = c.Quote
		for offset := int(c.Start); offset < int(c.End); offset += kataevidence.MaxChars {
			start, end := offset, min(offset+kataevidence.MaxChars, int(c.End))
			selectors = append(selectors, kataevidence.Selector{Kind: "message", MessageID: messageID, StartRune: &start, EndRune: &end})
		}
		key = fmt.Sprintf("commitment:c%d", id)
	} else {
		id, err := strconv.ParseInt(ref, 10, 64)
		if err != nil || id < 1 {
			return kataissues.Result{}, loops.ErrNotOpen
		}
		loop, err := loops.Open(ctx, a.archive, id, now)
		if err != nil {
			return kataissues.Result{}, err
		}
		if loop.Kind != loops.ReplyOwed {
			return kataissues.Result{}, loops.ErrNotOpen
		}
		messageID, conversationID = id, loop.ConversationID
		input.SourceTime = loop.Anchor.OccurredAt
		input.Title = fmt.Sprintf("Reply to %s: %s", loop.Counterparty.DisplayName, loop.Summary)
		if loop.Counterparty.PersonID == 0 {
			return kataissues.Result{}, errAgendaPersonRequired
		}
		input.PersonID, input.List = &loop.Counterparty.PersonID, "replies"
		selectors = []kataevidence.Selector{{Kind: "message", MessageID: id, MaxChars: new(kataevidence.MaxChars)}}
		key = fmt.Sprintf("loop:%d", id)
	}
	input.Title = textutil.TruncateRunes(input.Title, 512)
	evidence, err := a.backend.Prepare(ctx, selectors)
	if err != nil {
		return kataissues.Result{}, err
	}
	var quote strings.Builder
	for _, e := range evidence {
		input.Evidence = append(input.Evidence, e.Reference)
		quote.WriteString(e.Excerpt)
	}
	if expected != "" && quote.String() != expected {
		return kataissues.Result{}, kataevidence.ErrChanged
	}
	input.Thread = strconv.FormatInt(conversationID, 10)
	origin := apiprotocol.BrowserOrigin(ctx, a.origin)
	if origin != "" {
		input.OriginLink = fmt.Sprintf("[Open the original message](%s/messages/%d)", strings.TrimRight(origin, "/"), messageID)
	}
	if service == nil {
		service, err = a.backend.service(ctx)
		if err != nil {
			return kataissues.Result{}, err
		}
	}
	result, err := service.Create(ctx, key, input)
	if errors.Is(err, kataissues.ErrIdempotencyConflict) && result.Issue.UID != "" {
		result.Replayed = true
		return result, nil
	}
	return result, err
}

// Run files at most 25 new obligations in one pass.
func (a *LoopAgenda) Run(ctx context.Context, now time.Time) error {
	service, err := a.backend.service(ctx)
	if err != nil {
		return err
	}
	var failures []error
	created := 0
	stop := errors.New("agenda pass limit reached")
	file := func(ref, key string) error {
		found, err := service.Filed(ctx, key)
		if err == nil && found {
			return nil
		}
		if err == nil {
			var result kataissues.Result
			result, err = a.file(ctx, ref, now, service)
			if err == nil && !result.Replayed {
				created++
			}
		}
		if err != nil && !errors.Is(err, errAgendaPersonRequired) && !errors.Is(err, kataissues.ErrIssueDeleted) && !errors.Is(err, kataevidence.ErrUnsupported) && !errors.Is(err, kataevidence.ErrChanged) && !errors.Is(err, kataissues.ErrUnsupportedEvidence) {
			failures = append(failures, err)
		}
		if created == 25 {
			return stop
		}
		return nil
	}
	type candidate struct {
		ref, key string
		at       time.Time
	}
	var candidates []candidate
	err = loops.WalkOpen(ctx, a.archive, now, func(l loops.Loop) error {
		if l.Kind == loops.ReplyOwed && l.Counterparty.PersonID != 0 {
			candidates = append(candidates, candidate{strconv.FormatInt(l.Anchor.ID, 10), fmt.Sprintf("loop:%d", l.Anchor.ID), l.Anchor.OccurredAt})
		}
		return nil
	})
	if err != nil {
		failures = append(failures, err)
	}
	err = a.archive.WalkOpenPersonCommitments(ctx, now, func(c store.PersonCommitment) error {
		ref := fmt.Sprintf("c%d", c.ID)
		candidates = append(candidates, candidate{ref, "commitment:" + ref, c.PromisedAt})
		return nil
	})
	if err != nil {
		failures = append(failures, err)
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return a.at.Compare(b.at) })
	for _, item := range candidates {
		if errors.Is(file(item.ref, item.key), stop) {
			break
		}
	}
	return errors.Join(failures...)
}

// LoopAgendaResponse identifies the issue that tracks an obligation.
type LoopAgendaResponse struct {
	Issue     KataIssueReceipt `json:"issue"`
	Replayed  bool             `json:"replayed"`
	ParentRef string           `json:"parent_ref,omitempty"`
}

func (s *Server) registerLoopAgenda(api huma.API) {
	for _, entry := range []struct{ id, path, param string }{
		{"fileLoopAgenda", "/loops/{message_id}/agenda", "message_id"},
		{"fileCommitmentAgenda", "/loops/commitments/{commitment_id}/agenda", "commitment_id"},
	} {
		op := rawAPIV1Operation(entry.id, http.MethodPost, entry.path, "File an open obligation in Kata")
		op.Parameters = []*huma.Param{pathStringParam(entry.param, "The obligation ID")}
		op.Responses = jsonResponsesFor[LoopAgendaResponse](api)
		addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
		registerRawHumaRoute(api, op, s.handleLoopAgenda)
	}
}

func (s *Server) handleLoopAgenda(w http.ResponseWriter, r *http.Request) {
	if s.loopAgenda == nil {
		writeError(w, http.StatusServiceUnavailable, "kata_unavailable", "Kata agenda is unavailable")
		return
	}
	ref := r.PathValue("message_id")
	commitment := ref == ""
	if commitment {
		ref = r.PathValue("commitment_id")
		if _, err := loops.ParseCommitmentID(ref); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	} else if id, err := strconv.ParseInt(ref, 10, 64); err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid message ID")
		return
	}
	result, err := s.loopAgenda.File(r.Context(), ref, s.clockNow().UTC())
	switch {
	case errors.Is(err, errAgendaPersonRequired):
		writeError(w, http.StatusConflict, "person_required", errAgendaPersonRequired.Error())
	case errors.Is(err, loops.ErrNotOpen) || !commitment && errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "loop_not_open", "Loop not open")
	case commitment && errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "commitment_not_found", "Commitment not found")
	case err != nil:
		s.writeKataIssueError(w, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, LoopAgendaResponse{Issue: kataIssueReceipt(result.Issue), Replayed: result.Replayed, ParentRef: result.Issue.ParentRef})
	}
}
