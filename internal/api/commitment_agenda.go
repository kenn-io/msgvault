package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
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

var errCommitmentAgendaDestination = errors.New("the configured Kata destination changed after the suggestion was saved")

type commitmentAgendaArchive interface {
	kataevidence.SourceReader
	kataissues.Archive
	personagenda.IdentityStore
	GetOpenPersonCommitmentContext(context.Context, int64, time.Time) (store.PersonCommitment, error)
	ListCommitmentAgendaCandidatesContext(context.Context, time.Time, int) ([]store.PersonCommitment, error)
	GetCommitmentAgendaContext(context.Context, int64) (store.CommitmentAgendaSuggestion, error)
	EnqueueCommitmentAgendaContext(context.Context, store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error)
	ListPendingCommitmentAgendaContext(context.Context, int) ([]store.CommitmentAgendaSuggestion, error)
	MarkCommitmentAgendaAttemptContext(context.Context, int64, time.Time) (bool, error)
	RetireCommitmentAgendaContext(context.Context, int64) error
	LinkCommitmentAgendaContext(context.Context, int64, string, string) (store.CommitmentAgendaSuggestion, error)
	ReopenCommitmentAgendaContext(context.Context, int64) error
}

type commitmentAgendaIssues interface {
	Create(context.Context, string, kataissues.CreateInput) (kataissues.Result, error)
	FindCreated(context.Context, string, kataissues.CreateInput) (kataissues.Result, bool, error)
}

// CommitmentAgenda prepares local citations and submits durable create intents.
type CommitmentAgenda struct {
	archive     commitmentAgendaArchive
	evidence    *kataevidence.Service
	enabled     bool
	destination string
	issues      func(context.Context) (commitmentAgendaIssues, error)
}

func NewCommitmentAgenda(cfg config.TaskIntegrationConfig, archive *store.Store) *CommitmentAgenda {
	cfg.ApplyDefaults()
	evidence := kataevidence.New(archive)
	project := strings.TrimSpace(cfg.DefaultProject)
	return &CommitmentAgenda{
		archive: archive, evidence: evidence, enabled: cfg.Enabled,
		destination: kataevidence.Digest("msgvault.commitment.agenda.destination.v1", strings.TrimSpace(cfg.Endpoint), project),
		issues: func(ctx context.Context) (commitmentAgendaIssues, error) {
			kata, err := connectKata(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &kataissues.Service{Archive: archive, Kata: kata, Evidence: evidence, Project: project}, nil
		},
	}
}

func (a *CommitmentAgenda) enqueue(ctx context.Context, commitment store.PersonCommitment) (store.CommitmentAgendaSuggestion, error) {
	// The ranges were measured in the original body. Comparing the prepared
	// text also protects against a differently rendered source.
	start, end := int(commitment.Start), int(commitment.End)
	evidence, err := a.evidence.Prepare(ctx, []kataevidence.Selector{{
		Kind: "message", MessageID: commitment.AnchorMessageID, StartRune: &start, EndRune: &end,
	}})
	if err != nil {
		return store.CommitmentAgendaSuggestion{}, err
	}
	if len(evidence) != 1 || evidence[0].Excerpt != commitment.Quote {
		return store.CommitmentAgendaSuggestion{}, kataevidence.ErrChanged
	}
	uids, err := personagenda.PersonUIDs(ctx, a.archive, commitment.PersonID)
	if err != nil {
		return store.CommitmentAgendaSuggestion{}, err
	}
	id := commitment.ID
	return a.archive.EnqueueCommitmentAgendaContext(ctx, store.CommitmentAgendaSuggestion{
		CommitmentID: id, TargetKey: commitment.TargetKey,
		IdempotencyKey: "commitment-agenda:" + loops.FormatCommitmentID(id),
		DestinationKey: a.destination,
		Request: kataissues.CreateInput{
			Title: textutil.TruncateRunes(commitment.Summary, 120), List: "commitments", PersonUID: uids[0],
			Evidence: []kataevidence.Reference{evidence[0].Reference},
		},
		Quote: evidence[0].Excerpt,
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
	if item.State == store.CommitmentAgendaRetired {
		commitment, err := a.archive.GetOpenPersonCommitmentContext(ctx, id, now)
		if err != nil {
			return item, err
		}
		if commitment.TargetKey != item.TargetKey || commitment.Quote != item.Quote {
			return item, kataevidence.ErrChanged
		}
		if err := a.archive.ReopenCommitmentAgendaContext(ctx, id); err != nil {
			return item, err
		}
		item.State = store.CommitmentAgendaPending
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
		if _, err := a.enqueue(ctx, commitment); err != nil && !errors.Is(err, sql.ErrNoRows) {
			failures = append(failures, err)
		}
	}
	pending, err := a.archive.ListPendingCommitmentAgendaContext(ctx, commitmentAgendaBatchSize)
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

func (a *CommitmentAgenda) submit(ctx context.Context, item store.CommitmentAgendaSuggestion, now time.Time, automatic bool) (store.CommitmentAgendaSuggestion, error) {
	if item.State == store.CommitmentAgendaLinked {
		return item, nil
	}
	commitment, err := a.archive.GetOpenPersonCommitmentContext(ctx, item.CommitmentID, now)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (commitment.TargetKey != item.TargetKey || commitment.Quote != item.Quote) {
		return a.retire(ctx, item)
	}
	if err != nil {
		return item, err
	}
	if automatic && commitment.SnoozedUntil != nil && now.Before(*commitment.SnoozedUntil) {
		return item, nil
	}
	if item.DestinationKey != a.destination {
		return item, errCommitmentAgendaDestination
	}
	issues, err := a.issues(ctx)
	if err != nil {
		return item, err
	}
	started, err := a.archive.MarkCommitmentAgendaAttemptContext(ctx, item.CommitmentID, now)
	if err != nil {
		return item, err
	}
	if !started {
		current, err := a.archive.GetCommitmentAgendaContext(ctx, item.CommitmentID)
		if err != nil || current.State == store.CommitmentAgendaLinked {
			return current, err
		}
		return a.retire(ctx, current)
	}
	result, err := issues.Create(ctx, item.IdempotencyKey, item.Request)
	if err != nil {
		return item, err
	}
	return a.link(ctx, item, result.Issue)
}

func (a *CommitmentAgenda) retire(ctx context.Context, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error) {
	if item.Attempted && item.DestinationKey == a.destination {
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
	if err := a.archive.RetireCommitmentAgendaContext(ctx, item.CommitmentID); err != nil {
		return item, err
	}
	return item, sql.ErrNoRows
}

func (a *CommitmentAgenda) link(ctx context.Context, item store.CommitmentAgendaSuggestion, issue taskclient.KataTask) (store.CommitmentAgendaSuggestion, error) {
	uid, _ := issue.Metadata[personagenda.PersonMetadataKey].(string)
	list, _ := issue.Metadata[personagenda.ListMetadataKey].(string)
	if uid != item.Request.PersonUID || uid == "" || list != item.Request.List {
		return item, personagenda.ErrUnsafePersonMetadata
	}
	// A successful remote create must be recorded even if the HTTP caller
	// disconnected after Kata committed it.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return a.archive.LinkCommitmentAgendaContext(save, item.CommitmentID, issue.UID, issue.QualifiedRef)
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
	case errors.Is(err, errCommitmentAgendaDestination):
		writeError(w, http.StatusConflict, "kata_destination_changed", err.Error())
	case err != nil:
		s.writeKataIssueError(w, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, CommitmentAgendaResponse{CommitmentID: loops.FormatCommitmentID(id), IssueUID: item.IssueUID, IssueRef: item.IssueRef})
	}
}
