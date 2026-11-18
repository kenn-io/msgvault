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
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type loopAgendaArchive interface {
	loops.Store
	GetPersonCommitmentContext(ctx context.Context, id int64, now time.Time) (store.PersonCommitment, error)
	GetPersonContext(ctx context.Context, id int64) (*store.Person, error)
	WalkOpenPersonCommitments(ctx context.Context, now time.Time, visit func(store.PersonCommitment) error) error
}

var errAgendaPersonRequired = errors.New("link the contact to a person first")

// LoopAgenda files open obligations with their archive citations.
type LoopAgenda struct {
	archive loopAgendaArchive
	backend *kataIssueBackend
	origin  string
	browser atomic.Pointer[string]
}

// noteBrowserOrigin records the origin a signed-in browser reached the server
// at, so issues filed without a request link to an address browsers reach.
func (a *LoopAgenda) noteBrowserOrigin(origin string) {
	if a == nil {
		return
	}
	if seen := a.browser.Load(); seen == nil || *seen != origin {
		a.browser.Store(&origin)
	}
}

// linkOrigin prefers the request's own origin, then the last signed-in
// browser's, then the configured server address.
func (a *LoopAgenda) linkOrigin(ctx context.Context) string {
	fallback := a.origin
	if seen := a.browser.Load(); seen != nil {
		fallback = *seen
	}
	return apiprotocol.BrowserOrigin(ctx, fallback)
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
	if commitmentRef, ok := strings.CutPrefix(ref, "c"); ok {
		id, err := strconv.ParseInt(commitmentRef, 10, 64)
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
		key = commitmentAgendaKey(c)
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
			name := loop.Counterparty.DisplayName
			if name == "" {
				name = loop.Counterparty.Address
			}
			return kataissues.Result{}, fmt.Errorf("%w: %s", errAgendaPersonRequired, name)
		}
		input.PersonID, input.List = &loop.Counterparty.PersonID, "replies"
		selectors = []kataevidence.Selector{{Kind: "message", MessageID: id, MaxChars: new(kataevidence.MaxChars)}}
		key = fmt.Sprintf("loop:%d", id)
	}
	if service == nil {
		var err error
		service, err = a.backend.service(ctx)
		if err != nil {
			return kataissues.Result{}, err
		}
	}
	issue, found, err := service.FindFiled(ctx, key)
	if err != nil {
		return kataissues.Result{}, err
	}
	if found {
		return kataissues.Result{Issue: issue, Replayed: true}, nil
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
	origin := a.linkOrigin(ctx)
	if origin != "" {
		input.OriginLink = fmt.Sprintf("[Open the original message](%s/messages/%d)", strings.TrimRight(origin, "/"), messageID)
	}
	result, err := service.Create(ctx, key, input)
	if errors.Is(err, kataissues.ErrIdempotencyConflict) && result.Issue.UID != "" {
		result.Replayed = true
		return result, nil
	}
	return result, err
}

func commitmentAgendaKey(c store.PersonCommitment) string {
	return "commitment:" + c.SourceKey
}

// Run files open obligations in one pass.
func (a *LoopAgenda) Run(ctx context.Context, now time.Time) error {
	service, err := a.backend.service(ctx)
	if err != nil {
		return err
	}
	var failures []error
	file := func(ref, key string) {
		_, found, err := service.FindFiled(ctx, key)
		if err == nil && found {
			return
		}
		if err == nil {
			_, err = a.file(ctx, ref, now, service)
		}
		if err != nil && !errors.Is(err, errAgendaPersonRequired) && !errors.Is(err, kataissues.ErrIssueDeleted) && !errors.Is(err, kataevidence.ErrUnsupported) && !errors.Is(err, kataevidence.ErrChanged) && !errors.Is(err, kataissues.ErrUnsupportedEvidence) && !errors.Is(err, kataissues.ErrEvidenceLimit) && !errors.Is(err, personagenda.ErrPersonIdentity) && !errors.Is(err, loops.ErrNotOpen) {
			failures = append(failures, err)
		}
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
		candidates = append(candidates, candidate{ref, commitmentAgendaKey(c), c.PromisedAt})
		return nil
	})
	if err != nil {
		failures = append(failures, err)
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return a.at.Compare(b.at) })
	for _, item := range candidates {
		file(item.ref, item.key)
	}
	return errors.Join(failures...)
}

// LoopAgendaResponse identifies the issue that tracks an obligation.
type LoopAgendaResponse struct {
	Issue    KataIssueReceipt `json:"issue"`
	Replayed bool             `json:"replayed"`
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
		writeError(w, http.StatusServiceUnavailable, "kata_unavailable", "Kata issues are unavailable")
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
		writeError(w, http.StatusConflict, "person_required", err.Error())
	case errors.Is(err, loops.ErrNotOpen) || !commitment && errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "loop_not_open", "Loop not open")
	case commitment && errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "commitment_not_found", "Commitment not found")
	case err != nil:
		s.writeKataIssueError(w, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, LoopAgendaResponse{Issue: kataIssueReceipt(result.Issue), Replayed: result.Replayed})
	}
}
