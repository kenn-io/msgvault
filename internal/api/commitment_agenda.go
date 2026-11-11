package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/textutil"
)

const commitmentAgendaBatchSize = 8

var errCommitmentAgendaCitation = errors.New("the earlier filing citation changed and no created issue was found. File the current evidence with msgvault kata create if needed")

type commitmentAgendaArchive interface {
	kataevidence.SourceReader
	kataissues.Archive
	personagenda.IdentityStore
	GetOpenPersonCommitmentContext(ctx context.Context, id int64, now time.Time) (store.PersonCommitment, error)
	ListCommitmentAgendaCandidatesContext(ctx context.Context, now time.Time, limit int) ([]store.PersonCommitment, error)
	GetCommitmentAgendaContext(ctx context.Context, id int64) (store.CommitmentAgendaSuggestion, error)
	EnqueueCommitmentAgendaContext(ctx context.Context, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error)
	ListPendingCommitmentAgendaContext(ctx context.Context, now time.Time, limit int) ([]store.CommitmentAgendaSuggestion, error)
	MarkCommitmentAgendaAttemptContext(ctx context.Context, item store.CommitmentAgendaSuggestion, now time.Time, automatic bool) (bool, error)
	RetireCommitmentAgendaContext(ctx context.Context, key string) error
	DeferCommitmentAgendaContext(ctx context.Context, commitment store.PersonCommitment, now time.Time) error
	LinkCommitmentAgendaContext(ctx context.Context, key, uid, ref string) (store.CommitmentAgendaSuggestion, error)
	ReopenCommitmentAgendaContext(ctx context.Context, key string) (store.CommitmentAgendaSuggestion, error)
}

type commitmentAgendaIssues interface {
	Create(ctx context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error)
	FindCreated(ctx context.Context, key string, input kataissues.CreateInput) (kataissues.Result, bool, error)
}

// CommitmentAgenda prepares local citations and submits durable create intents.
type CommitmentAgenda struct {
	archive  commitmentAgendaArchive
	evidence *kataevidence.Service
	enabled  bool
	origin   string
	issues   func(context.Context) (commitmentAgendaIssues, error)
}

func NewCommitmentAgenda(cfg config.TaskIntegrationConfig, archive *store.Store, origin string) *CommitmentAgenda {
	cfg.ApplyDefaults()
	evidence := kataevidence.New(archive)
	backend := &kataIssueBackend{config: cfg, store: archive, evidence: evidence}
	return &CommitmentAgenda{
		archive: archive, evidence: evidence, enabled: cfg.Enabled, origin: origin,
		issues: func(ctx context.Context) (commitmentAgendaIssues, error) {
			return backend.service(ctx)
		},
	}
}

func (a *CommitmentAgenda) enqueue(ctx context.Context, commitment store.PersonCommitment) (store.CommitmentAgendaSuggestion, error) {
	// The ranges were measured in the original body. Comparing the prepared
	// text also protects against a differently rendered source.
	start, end := int(commitment.Start), int(commitment.End)
	var selectors []kataevidence.Selector
	for offset := start; offset < end; offset += kataevidence.MaxChars {
		from, to := offset, min(offset+kataevidence.MaxChars, end)
		selectors = append(selectors, kataevidence.Selector{
			Kind: "message", MessageID: commitment.AnchorMessageID, StartRune: &from, EndRune: &to,
		})
	}
	evidence, err := a.evidence.Prepare(ctx, selectors)
	if err != nil {
		return store.CommitmentAgendaSuggestion{}, err
	}
	var quote strings.Builder
	refs := make([]kataevidence.Reference, 0, len(evidence))
	for _, citation := range evidence {
		refs = append(refs, citation.Reference)
		quote.WriteString(citation.Excerpt)
	}
	if quote.String() != commitment.Quote {
		return store.CommitmentAgendaSuggestion{}, kataevidence.ErrChanged
	}
	uids, err := personagenda.PersonUIDs(ctx, a.archive, commitment.PersonID)
	if err != nil {
		return store.CommitmentAgendaSuggestion{}, err
	}
	var brief string
	if origin := apiprotocol.BrowserOrigin(ctx, a.origin); origin != "" {
		brief = fmt.Sprintf("[Open the original message](%s/messages/%d)", strings.TrimRight(origin, "/"), commitment.AnchorMessageID)
	}
	return a.archive.EnqueueCommitmentAgendaContext(ctx, store.CommitmentAgendaSuggestion{
		CommitmentID: commitment.ID, TargetKey: commitment.TargetKey,
		Request: kataissues.CreateInput{
			Title: textutil.TruncateRunes(commitment.Summary, 120), List: "commitments", PersonUID: uids[0],
			Brief: brief, Evidence: refs,
		},
		Quote: quote.String(),
	})
}

func (a *CommitmentAgenda) Manual(ctx context.Context, id int64, now time.Time) (store.CommitmentAgendaSuggestion, error) {
	item, err := a.archive.GetCommitmentAgendaContext(ctx, id)
	if err == nil && item.State == store.CommitmentAgendaLinked {
		return item, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		commitment, err := a.archive.GetOpenPersonCommitmentContext(ctx, id, now)
		if err != nil {
			return item, err
		}
		item, err = a.enqueue(ctx, commitment)
		if err != nil {
			return item, err
		}
	} else if err != nil {
		return item, err
	}

	return a.submit(ctx, item, now, false)
}

// AutoRun enqueues and drains a bounded batch. The caller checks the explicit
// setting and exact profile grant before calling it.
func (a *CommitmentAgenda) AutoRun(ctx context.Context, now time.Time) error {
	if a == nil || !a.enabled {
		return nil
	}
	commitments, err := a.archive.ListCommitmentAgendaCandidatesContext(ctx, now, commitmentAgendaBatchSize)
	if err != nil {
		return err
	}
	var failures []error
	for _, commitment := range commitments {
		item, err := a.archive.GetCommitmentAgendaContext(ctx, commitment.ID)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			_, err = a.refresh(ctx, commitment, item)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			failures = append(failures, err, a.archive.DeferCommitmentAgendaContext(ctx, commitment, now))
		}
	}
	pending, err := a.archive.ListPendingCommitmentAgendaContext(ctx, now, commitmentAgendaBatchSize)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, item := range pending {
		if _, err := a.submit(ctx, item, now, true); err != nil && !errors.Is(err, sql.ErrNoRows) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (a *CommitmentAgenda) refresh(ctx context.Context, commitment store.PersonCommitment, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error) {
	if !item.Attempted {
		return a.enqueue(ctx, commitment)
	}
	if commitment.TargetKey != item.TargetKey || commitment.Quote != item.Quote {
		return a.citationChanged(ctx, item)
	}
	if item.State == store.CommitmentAgendaRetired {
		return a.archive.ReopenCommitmentAgendaContext(ctx, item.IdempotencyKey)
	}
	return item, nil
}

func (a *CommitmentAgenda) submit(ctx context.Context, item store.CommitmentAgendaSuggestion, now time.Time, automatic bool) (store.CommitmentAgendaSuggestion, error) {
	if item.State == store.CommitmentAgendaLinked {
		return item, nil
	}
	commitment, err := a.archive.GetOpenPersonCommitmentContext(ctx, item.CommitmentID, now)
	if errors.Is(err, sql.ErrNoRows) {
		retired, retireErr := a.retire(ctx, item)
		if !errors.Is(retireErr, sql.ErrNoRows) || !item.Attempted {
			return retired, retireErr
		}
		// A valid citation can belong to an owner-closed promise. A changed
		// citation instead needs the same conflict as other attempted retries.
		if _, citationErr := a.refreshCitation(ctx, item); citationErr != nil {
			if permanentCommitmentCitationError(citationErr) {
				citationErr = errCommitmentAgendaCitation
			}
			return retired, citationErr
		}
		return retired, retireErr
	}
	if err != nil {
		return item, err
	}
	if automatic && commitment.SnoozedUntil != nil && now.Before(*commitment.SnoozedUntil) {
		return item, nil
	}
	if item.State == store.CommitmentAgendaRetired ||
		commitment.TargetKey != item.TargetKey || commitment.Quote != item.Quote {
		refreshed, err := a.refresh(ctx, commitment, item)
		if err != nil {
			if automatic && !item.Attempted && !errors.Is(err, sql.ErrNoRows) {
				err = errors.Join(err, a.archive.DeferCommitmentAgendaContext(ctx, commitment, now))
			}
			return item, err
		}
		item = refreshed
		if item.State == store.CommitmentAgendaLinked {
			return item, nil
		}
	}
	if commitment.TargetKey != item.TargetKey || commitment.Quote != item.Quote {
		return item, kataevidence.ErrChanged
	}
	issues, err := a.issues(ctx)
	if err != nil {
		return item, err
	}
	started, err := a.archive.MarkCommitmentAgendaAttemptContext(ctx, item, now, automatic)
	if err != nil {
		return item, err
	}
	if !started {
		return item, kataevidence.ErrChanged
	}
	item.Attempted = true
	result, err := issues.Create(ctx, item.IdempotencyKey, item.Request)
	if errors.Is(err, kataevidence.ErrChanged) {
		request, refreshErr := a.refreshCitation(ctx, item)
		if refreshErr != nil {
			if permanentCommitmentCitationError(refreshErr) {
				return a.citationChanged(ctx, item)
			}
			return item, refreshErr
		}
		result, err = issues.Create(ctx, item.IdempotencyKey, request)
	}
	if err != nil {
		return item, err
	}
	return a.link(ctx, item, result.Issue)
}

// Refresh only content hashes when the same original passages still quote the
// frozen promise. The create key and all other request fields remain unchanged.
func (a *CommitmentAgenda) refreshCitation(ctx context.Context, item store.CommitmentAgendaSuggestion) (kataissues.CreateInput, error) {
	selectors := make([]kataevidence.Selector, 0, len(item.Request.Evidence))
	for _, ref := range item.Request.Evidence {
		if ref.Kind != "message" {
			return item.Request, kataevidence.ErrChanged
		}
		start, end := ref.Range()
		selectors = append(selectors, kataevidence.Selector{Kind: ref.Kind, MessageID: ref.MessageID, StartRune: &start, EndRune: &end})
	}
	evidence, err := a.evidence.Prepare(ctx, selectors)
	if err != nil {
		return item.Request, err
	}
	if len(evidence) != len(item.Request.Evidence) {
		return item.Request, kataevidence.ErrChanged
	}
	request := item.Request
	request.Evidence = make([]kataevidence.Reference, len(evidence))
	var quote strings.Builder
	for i, citation := range evidence {
		if kataevidence.PassageLocation(citation.Reference) != kataevidence.PassageLocation(item.Request.Evidence[i]) {
			return item.Request, kataevidence.ErrChanged
		}
		request.Evidence[i] = citation.Reference
		quote.WriteString(citation.Excerpt)
	}
	if quote.String() != item.Quote {
		return item.Request, kataevidence.ErrChanged
	}
	return request, nil
}

func permanentCommitmentCitationError(err error) bool {
	return errors.Is(err, kataevidence.ErrChanged) || errors.Is(err, kataevidence.ErrUnavailable) ||
		errors.Is(err, kataevidence.ErrUnsupported) || errors.Is(err, kataevidence.ErrInvalidReference)
}

func (a *CommitmentAgenda) citationChanged(ctx context.Context, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error) {
	retired, err := a.retire(ctx, item)
	if errors.Is(err, sql.ErrNoRows) {
		err = errCommitmentAgendaCitation
	}
	return retired, err
}

func (a *CommitmentAgenda) retire(ctx context.Context, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error) {
	if item.Attempted {
		issues, err := a.issues(ctx)
		if err != nil {
			return item, err
		}
		result, found, err := issues.FindCreated(ctx, item.IdempotencyKey, item.Request)
		if err != nil {
			return item, err
		}
		if found {
			return a.link(ctx, item, result.Issue)
		}
	}
	if err := a.archive.RetireCommitmentAgendaContext(ctx, item.IdempotencyKey); err != nil {
		return item, err
	}
	return item, sql.ErrNoRows
}

func (a *CommitmentAgenda) link(ctx context.Context, item store.CommitmentAgendaSuggestion, issue taskclient.KataTask) (store.CommitmentAgendaSuggestion, error) {
	// kataissues verifies the original create marker and request hash. Current
	// agenda metadata may reflect later owner edits and must stay untouched.
	// A successful remote create must be recorded even if the HTTP caller
	// disconnected after Kata committed it.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	linked, err := a.archive.LinkCommitmentAgendaContext(save, item.IdempotencyKey, issue.UID, issue.QualifiedRef)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent purge or losing merge may remove the receipt after Kata
		// succeeds. Return the created issue without recreating archive state.
		item.State, item.IssueUID, item.IssueRef = "linked", issue.UID, issue.QualifiedRef
		return item, nil
	}
	return linked, err
}

type CommitmentAgendaResponse struct {
	CommitmentID string `json:"commitment_id"`
	IssueUID     string `json:"issue_uid"`
	IssueRef     string `json:"issue_ref"`
}

func (s *Server) registerCommitmentAgenda(api huma.API) {
	op := rawAPIV1Operation("createCommitmentAgendaIssue", http.MethodPost, "/loops/commitments/{commitment_id}/agenda", "Create a Kata agenda issue with the cited promise")
	op.Errors = []int{400, 401, 403, 404, 409, 422, 500, 503}
	op.Parameters = []*huma.Param{pathStringParam("commitment_id", "The commitment ID, for example c12")}
	op.Responses = jsonResponsesFor[CommitmentAgendaResponse](api)
	addActivityErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleCommitmentAgenda)
}

func (s *Server) handleCommitmentAgenda(w http.ResponseWriter, r *http.Request) {
	id, err := loops.ParseCommitmentID(r.PathValue("commitment_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if s.commitmentAgenda == nil || !s.commitmentAgenda.enabled {
		writeError(w, http.StatusServiceUnavailable, "kata_unavailable", "Kata agenda integration is unavailable")
		return
	}
	item, err := s.commitmentAgenda.Manual(r.Context(), id, s.clockNow().UTC())
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "commitment_not_found", "Open commitment not found")
	case errors.Is(err, errCommitmentAgendaCitation):
		writeError(w, http.StatusConflict, "kata_citation_changed", err.Error())
	case errors.Is(err, kataevidence.ErrChanged):
		writeError(w, http.StatusConflict, "evidence_changed", "The commitment or filing changed. Refresh and try again.")
	case err != nil:
		s.writeKataIssueError(w, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, CommitmentAgendaResponse{CommitmentID: loops.FormatCommitmentID(id), IssueUID: item.IssueUID, IssueRef: item.IssueRef})
	}
}
