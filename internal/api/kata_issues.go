package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

const kataEvidencePreparePath = "/api/v1/integrations/kata/evidence/prepare"

// KataIssueOperations quotes archive evidence and files it in Kata.
type KataIssueOperations interface {
	Prepare(ctx context.Context, selectors []kataevidence.Selector) ([]kataevidence.Evidence, error)
	Create(ctx context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error)
	Link(ctx context.Context, ref string, evidence []kataevidence.Reference) (taskclient.KataTask, error)
	// Find lists issues citing a message, or one of its attachments when
	// attachmentID is set, and whether more do.
	Find(ctx context.Context, messageID, attachmentID int64) ([]taskclient.KataTask, bool, error)
	// Context reads one page of the passages an issue cites.
	Context(ctx context.Context, ref string, offset int) (kataissues.Context, error)
}

// kataIssueFindLimit bounds the issues one lookup returns.
const kataIssueFindLimit = 10

type KataEvidencePrepareRequest struct {
	Selectors []kataevidence.Selector `json:"selectors" minItems:"1" maxItems:"32"`
}

type KataEvidencePrepareResponse struct {
	Evidence []kataevidence.Evidence `json:"evidence"`
}

type KataIssueCreateRequest struct {
	Title    string                   `json:"title" minLength:"1" maxLength:"512"`
	Brief    string                   `json:"brief,omitempty" maxLength:"2000"`
	List     string                   `json:"list,omitempty" maxLength:"80"`
	PersonID *int64                   `json:"person_id,omitempty" minimum:"1"`
	Evidence []kataevidence.Reference `json:"evidence" minItems:"1" maxItems:"32"`
}

type KataEvidenceLinkRequest struct {
	Evidence []kataevidence.Reference `json:"evidence" minItems:"1" maxItems:"32"`
}

type KataIssueReceipt struct {
	UID          string `json:"uid"`
	Ref          string `json:"ref"`
	QualifiedRef string `json:"qualified_ref"`
	Project      string `json:"project"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Revision     string `json:"revision"`
	WebURL       string `json:"web_url,omitempty"`
}

// KataIssueConflictResponse names the issue an Idempotency-Key already filed
// when a retry under it carries different details.
type KataIssueConflictResponse struct {
	Error   string            `json:"error"`
	Message string            `json:"message,omitempty"`
	Issue   *KataIssueReceipt `json:"issue,omitempty"`
}

type KataIssueListResponse struct {
	Issues    []KataIssueReceipt `json:"issues"`
	Truncated bool               `json:"truncated" doc:"More issues cite this source than were returned"`
}

type KataIssueContextResponse struct {
	Issue      KataIssueReceipt            `json:"issue"`
	Passages   []kataissues.ContextPassage `json:"passages"`
	NextOffset int                         `json:"next_offset,omitzero" doc:"Offset of the next page of passages; absent on the last page"`
}

type KataIssueResponse struct {
	Issue    KataIssueReceipt `json:"issue"`
	Replayed bool             `json:"replayed" doc:"An earlier request with this Idempotency-Key already created the issue"`
}

func (s *Server) registerKataIssueRoutes(api huma.API) {
	prepare := rawAPIV1Operation("prepareKataEvidence", http.MethodPost, "/integrations/kata/evidence/prepare", "Prepare exact message and file evidence for a Kata issue")
	prepare.RequestBody = jsonRequestBodyFor[KataEvidencePrepareRequest](api)
	prepare.Responses = jsonResponsesFor[KataEvidencePrepareResponse](api)
	addErrorResponses(api, prepare.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, prepare, s.handlePrepareKataEvidence)

	create := rawAPIV1Operation("createKataIssue", http.MethodPost, "/integrations/kata/issues", "Create a Kata issue that quotes exact archive evidence")
	addIdempotencyKeyParameter(&create)
	create.RequestBody = jsonRequestBodyFor[KataIssueCreateRequest](api)
	create.Description = "Returns 201 both when it files the issue and when an earlier request under the same Idempotency-Key already did; `replayed` tells them apart."
	create.Responses = jsonResponsesFor[KataIssueResponse](api, http.StatusCreated)
	addErrorResponses(api, create.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	create.Responses[httpStatusKey(http.StatusConflict)] = jsonResponsesFor[KataIssueConflictResponse](api, http.StatusConflict)[httpStatusKey(http.StatusConflict)]
	registerRawHumaRoute(api, create, s.handleCreateKataIssue)

	find := rawAPIV1Operation("findKataIssues", http.MethodGet, "/integrations/kata/issues", "Find Kata issues, open or closed, that cite a message or one of its files")
	messageID := queryIntegerParam("message_id", "Message the issues cite")
	messageID.Required = true
	find.Parameters = append(find.Parameters, messageID, queryIntegerParam("attachment_id", "Only issues citing this attachment of the message"))
	find.Responses = jsonResponsesFor[KataIssueListResponse](api)
	addErrorResponses(api, find.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, find, s.handleFindKataIssues)

	link := rawAPIV1Operation("linkKataEvidence", http.MethodPost, "/integrations/kata/issues/{ref}/evidence", "Add exact archive evidence to an existing Kata issue")
	link.Parameters = append(link.Parameters, &huma.Param{Name: "ref", In: pathKey, Required: true, Description: "Kata issue ref, qualified ref or UID", Schema: &huma.Schema{Type: huma.TypeString}})
	link.RequestBody = jsonRequestBodyFor[KataEvidenceLinkRequest](api)
	link.Responses = jsonResponsesFor[KataIssueResponse](api)
	addErrorResponses(api, link.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, link, s.handleLinkKataEvidence)

	issueContext := rawAPIV1Operation("getKataIssueContext", http.MethodGet, "/integrations/kata/issues/{ref}/context", "Read each passage a Kata issue cites with its state in the archive today")
	issueContext.Parameters = append(issueContext.Parameters, &huma.Param{Name: "ref", In: pathKey, Required: true, Description: "Kata issue ref, qualified ref or UID", Schema: &huma.Schema{Type: huma.TypeString}},
		queryIntegerParam("offset", "Index of the first passage to return; pass next_offset from the previous page"))
	issueContext.Responses = jsonResponsesFor[KataIssueContextResponse](api)
	addErrorResponses(api, issueContext.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, issueContext, s.handleKataIssueContext)
}

func (s *Server) handlePrepareKataEvidence(w http.ResponseWriter, r *http.Request) {
	var request KataEvidencePrepareRequest
	if !s.requireKataIssues(w) || !decodeBoundedRequest(w, r, &request, "Invalid evidence request", kataevidence.MaxRequestBytes) {
		return
	}
	evidence, err := s.kataIssueOperations.Prepare(r.Context(), request.Selectors)
	if err != nil {
		s.writeKataIssueError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, KataEvidencePrepareResponse{Evidence: evidence})
}

func (s *Server) handleCreateKataIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireKataIssues(w) {
		return
	}
	key, ok := personOperationIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request KataIssueCreateRequest
	if !decodeBoundedRequest(w, r, &request, "Invalid Kata issue request", kataevidence.MaxRequestBytes) {
		return
	}
	result, err := s.kataIssueOperations.Create(r.Context(), key, kataissues.CreateInput{Title: request.Title, Brief: request.Brief, List: request.List, PersonID: request.PersonID, Evidence: request.Evidence})
	if errors.Is(err, kataissues.ErrIdempotencyConflict) && result.Issue.UID != "" {
		receipt := kataIssueReceipt(result.Issue)
		writeJSON(w, http.StatusConflict, KataIssueConflictResponse{Error: "idempotency_conflict", Message: "This Idempotency-Key already filed an issue with different details", Issue: &receipt})
		return
	}
	if err != nil {
		s.writeKataIssueError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, KataIssueResponse{Issue: kataIssueReceipt(result.Issue), Replayed: result.Replayed})
}

func (s *Server) handleLinkKataEvidence(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.PathValue("ref"))
	var request KataEvidenceLinkRequest
	if !s.requireKataIssues(w) || !decodeBoundedRequest(w, r, &request, "Invalid Kata evidence request", kataevidence.MaxRequestBytes) {
		return
	}
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref_required", "Kata issue ref is required")
		return
	}
	issue, err := s.kataIssueOperations.Link(r.Context(), ref, request.Evidence)
	if err != nil {
		s.writeKataIssueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, KataIssueResponse{Issue: kataIssueReceipt(issue)})
}

func (s *Server) handleKataIssueContext(w http.ResponseWriter, r *http.Request) {
	if !s.requireKataIssues(w) {
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref_required", "Kata issue ref is required")
		return
	}
	offset, _, err := queryInt(r, "offset")
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
		return
	}
	result, err := s.kataIssueOperations.Context(r.Context(), ref, offset)
	if err != nil {
		s.writeKataIssueError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, KataIssueContextResponse{Issue: kataIssueReceipt(result.Issue), Passages: result.Passages, NextOffset: result.NextOffset})
}

func (s *Server) handleFindKataIssues(w http.ResponseWriter, r *http.Request) {
	if !s.requireKataIssues(w) {
		return
	}
	messageID, err := parseRequiredInt64Query(r, "message_id")
	var attachmentID int64
	if err == nil && r.URL.Query().Has("attachment_id") {
		attachmentID, err = parseRequiredInt64Query(r, "attachment_id")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}
	issues, truncated, err := s.kataIssueOperations.Find(r.Context(), messageID, attachmentID)
	if err != nil {
		s.writeKataIssueError(w, err)
		return
	}
	response := KataIssueListResponse{Issues: make([]KataIssueReceipt, len(issues)), Truncated: truncated}
	for i, issue := range issues {
		response.Issues[i] = kataIssueReceipt(issue)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) requireKataIssues(w http.ResponseWriter) bool {
	if s.kataIssueOperations != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "kata_unavailable", "Kata issues are unavailable")
	return false
}

func kataIssueReceipt(issue taskclient.KataTask) KataIssueReceipt {
	return KataIssueReceipt{UID: issue.UID, Ref: issue.Ref, QualifiedRef: issue.QualifiedRef, Project: issue.Project, Title: issue.Title, Status: issue.Status, Revision: issue.Revision, WebURL: personagenda.SafeWebURL(issue.WebURL)}
}

func (s *Server) writeKataIssueError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	for _, entry := range []struct {
		cause         error
		status        int
		code, message string
	}{
		{kataevidence.ErrDocbankLimit, http.StatusBadRequest, "invalid_evidence", kataevidence.ErrDocbankLimit.Error()},
		{kataevidence.ErrInvalidReference, http.StatusBadRequest, "invalid_evidence", "Evidence identity or range is invalid"},
		{taskclient.ErrInvalidRef, http.StatusBadRequest, "invalid_ref", "Kata issue ref is malformed"},
		{kataissues.ErrInvalidRequest, http.StatusUnprocessableEntity, "invalid_request", "Title, brief, list or person is invalid"},
		{kataevidence.ErrArchiveUnavailable, http.StatusServiceUnavailable, "archive_unavailable", "The archive could not be read"},
		{kataevidence.ErrUnavailable, http.StatusNotFound, "evidence_unavailable", "The cited source is unavailable"},
		{kataevidence.ErrChanged, http.StatusConflict, "evidence_changed", "The cited source has changed; prepare it again"},
		{kataevidence.ErrUnprocessed, http.StatusUnprocessableEntity, "evidence_unprocessed", "The file has no extracted text or transcript"},
		{kataevidence.ErrQuoteNotFound, http.StatusUnprocessableEntity, "quote_not_found", "The quoted text does not appear in the source"},
		{kataevidence.ErrQuoteAmbiguous, http.StatusUnprocessableEntity, "quote_ambiguous", "The quoted text appears more than once; quote more of it"},
		{kataevidence.ErrUnsupported, http.StatusUnprocessableEntity, "evidence_unsupported", "This source cannot be cited"},
		{kataissues.ErrEvidenceLimit, http.StatusUnprocessableEntity, "evidence_limit", "A request can cite at most 32 passages"},
		{kataissues.ErrIssueDeleted, http.StatusConflict, "kata_issue_deleted", "The issue this key filed was deleted in Kata or is not visible to this credential; use a new idempotency key, or restore the issue in Kata"},
		{kataissues.ErrIdempotencyConflict, http.StatusConflict, "idempotency_conflict", "This Idempotency-Key was used with a different request"},
		{kataissues.ErrUnsupportedEvidence, http.StatusConflict, "unsupported_issue_evidence", "msgvault can't read this issue's evidence metadata"},
		{kataissues.ErrIssueFull, http.StatusUnprocessableEntity, "issue_evidence_full", "This issue already holds as much evidence as it can; file a new issue"},
		{kataissues.ErrIssueChanged, http.StatusConflict, "kata_issue_changed", "The issue kept changing; try again"},
		{store.ErrPersonNotFound, http.StatusNotFound, "person_profile_not_found", "Person profile not found"},
		{personagenda.ErrPersonIdentity, http.StatusConflict, "person_identity_required", "Person has no stable vCard UID for agenda linking"},
		{personagenda.ErrIdentityLookup, http.StatusServiceUnavailable, "person_identity_unavailable", "Person identity lookup failed"},
		{taskclient.ErrIdempotencyKeyRequired, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 1 to 128 characters"},
		{taskclient.ErrNotFound, http.StatusNotFound, "kata_issue_not_found", "Kata issue not found"},
		{taskclient.ErrConflict, http.StatusConflict, "kata_conflict", "The Kata issue changed before it could be updated"},
		{taskclient.ErrRequestRejected, http.StatusUnprocessableEntity, "kata_request_rejected", "Kata rejected this request"},
		{taskclient.ErrAuthenticationRequired, http.StatusUnauthorized, "authentication_required", "Kata authentication is required"},
		{taskclient.ErrWrongProject, http.StatusServiceUnavailable, "wrong_project", "The configured Kata project is unavailable"},
	} {
		if errors.Is(err, entry.cause) {
			writeError(w, entry.status, entry.code, entry.message)
			return
		}
	}
	writeError(w, http.StatusServiceUnavailable, "kata_unavailable", "Kata is unavailable")
}

type kataIssueStore interface {
	kataevidence.SourceReader
	KataCitationSource(ctx context.Context, messageID, attachmentID int64) ([]kataevidence.Reference, error)
	kataevidence.DocbankBindingStore
	kataissues.Archive
	personagenda.IdentityStore
}

type kataIssueBackend struct {
	config   config.TaskIntegrationConfig
	store    kataIssueStore
	evidence *kataevidence.Service
}

func newKataIssueBackend(cfg *config.Config, messageStore MessageStore) KataIssueOperations {
	store, ok := messageStore.(kataIssueStore)
	if !ok || cfg == nil {
		return nil
	}
	evidence := kataevidence.New(store)
	if docbank := cfg.Integrations.Docbank; docbank.Enabled {
		client, err := docbankmedia.NewClient(docbank.URL, docbank.ResolveAPIKey)
		// An invalid URL leaves Docbank citations unsupported; the media job reports it.
		if err == nil {
			evidence.WithDocbank(kataevidence.NewDocbankReader(store, client, func(ctx context.Context) (string, error) {
				uid, err := store.ArchiveUIDContext(ctx)
				return docbankmedia.DestinationKey(docbank.URL, uid), err
			}))
		}
	}
	return &kataIssueBackend{config: cfg.Integrations.Kata, store: store, evidence: evidence}
}

func (b *kataIssueBackend) Prepare(ctx context.Context, selectors []kataevidence.Selector) ([]kataevidence.Evidence, error) {
	return b.evidence.Prepare(ctx, selectors)
}

func (b *kataIssueBackend) service(ctx context.Context) (*kataissues.Service, error) {
	client, err := connectKata(ctx, b.config)
	if err != nil {
		return nil, err
	}
	return &kataissues.Service{Archive: b.store, Kata: client, Evidence: b.evidence, People: b.store, Project: strings.TrimSpace(b.config.DefaultProject)}, nil
}

func (b *kataIssueBackend) Create(ctx context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error) {
	service, err := b.service(ctx)
	if err != nil {
		return kataissues.Result{}, err
	}
	return service.Create(ctx, key, input)
}

func (b *kataIssueBackend) Link(ctx context.Context, ref string, evidence []kataevidence.Reference) (taskclient.KataTask, error) {
	service, err := b.service(ctx)
	if err != nil {
		return taskclient.KataTask{}, err
	}
	return service.Link(ctx, ref, evidence)
}

func (b *kataIssueBackend) Context(ctx context.Context, ref string, offset int) (kataissues.Context, error) {
	service, err := b.service(ctx)
	if err != nil {
		return kataissues.Context{}, err
	}
	return service.Context(ctx, ref, offset)
}

func (b *kataIssueBackend) Find(ctx context.Context, messageID, attachmentID int64) ([]taskclient.KataTask, bool, error) {
	refs, err := b.store.KataCitationSource(ctx, messageID, attachmentID)
	if err != nil {
		return nil, false, kataevidence.ArchiveError(err)
	}
	service, err := b.service(ctx)
	if err != nil {
		return nil, false, err
	}
	// A file can be cited as extracted text and as each Docbank delivery, under different keys.
	var issues []taskclient.KataTask
	seen, truncated := map[string]bool{}, false
	for _, ref := range refs {
		// The last key names the attachment when there is one, else the message.
		keys := kataevidence.SourceKeys(ref)
		found, more, err := service.Citing(ctx, keys[len(keys)-1], kataIssueFindLimit)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || more
		for _, issue := range found {
			if !seen[issue.UID] {
				seen[issue.UID] = true
				issues = append(issues, issue)
			}
		}
	}
	// Order the merge as Kata orders one lookup, oldest first, before keeping the first page.
	slices.SortFunc(issues, func(a, b taskclient.KataTask) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.IssueID, b.IssueID))
	})
	if len(issues) > kataIssueFindLimit {
		issues, truncated = issues[:kataIssueFindLimit], true
	}
	return issues, truncated, nil
}
