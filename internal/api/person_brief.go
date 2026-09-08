package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// personBriefActor is the actor recorded on enrollment changes made over HTTP.
const personBriefActor = "api"

const (
	defaultPersonBriefVersionLimit = 20
	maxPersonBriefVersionLimit     = 200
)

// PersonBriefStore is the feature-local person brief capability: immutable
// dated versions, their evidence pointers, the owner's rejection, and the
// enrollment opt-in that sits on top of tracking.
type PersonBriefStore interface {
	GetPersonBriefContext(
		ctx context.Context, personID int64, version int,
	) (*store.PersonBrief, error)
	ListPersonBriefVersionsContext(
		ctx context.Context, personID int64, limit int,
	) ([]store.PersonBrief, error)
	ListPersonBriefEvidenceContext(
		ctx context.Context, briefID int64,
	) ([]store.PersonBriefEvidencePointer, error)
	RejectPersonBriefContext(
		ctx context.Context, personID int64, reason string, at time.Time,
	) (*store.PersonBrief, error)
	GetPersonBriefEnrollmentContext(
		ctx context.Context, personID int64,
	) (*store.PersonBriefEnrollment, error)
	SetPersonBriefEnrollmentContext(
		ctx context.Context, personID int64, enabled bool, actor string, track bool,
	) (*store.PersonBriefEnrollment, error)
}

var _ PersonBriefStore = (*store.Store)(nil)

// PersonBriefRun is the outcome of one manual brief generation. BriefVersion is
// the version the attempt stored, or zero; BriefFailureClass says why there is
// none when the attempt itself succeeded.
type PersonBriefRun struct {
	RunID             string `json:"run_id"`
	AttemptID         string `json:"attempt_id"`
	BriefVersion      int    `json:"brief_version"`
	BriefFailureClass string `json:"brief_failure_class"`
}

// PersonBriefGenerator runs one manual, forced brief attempt for a single
// person through the daemon's people sweep worker. The daemon installs it; a
// process without the worker leaves it nil and the route reports unavailable.
type PersonBriefGenerator func(ctx context.Context, personID int64) (PersonBriefRun, error)

// PersonBriefSentence is one rendered sentence and the structured item it came
// from. EvidenceOrdinals names the entries of the version's own evidence list
// that this sentence cites, so a client expands a sentence to its citations
// instead of guessing at the join. It is empty when the citation order could
// not be recovered, and a client falls back to the whole-brief list.
type PersonBriefSentence struct {
	Kind             string `json:"kind"`
	Index            int    `json:"index"`
	Text             string `json:"text"`
	EvidenceOrdinals []int  `json:"evidence_ordinals"`
}

// PersonBrief is one immutable brief version. It carries the paragraph, the
// sentence-to-item map, the validated structure, and the cited archive items —
// never an excerpt of the archive text the brief was derived from.
type PersonBrief struct {
	Version          int                                `json:"version"`
	Status           string                             `json:"status"`
	GeneratedAt      time.Time                          `json:"generated_at"`
	RenderedText     string                             `json:"rendered_text"`
	RendererPolicy   string                             `json:"renderer_policy"`
	Sentences        []PersonBriefSentence              `json:"sentences"`
	Structured       json.RawMessage                    `json:"structured"`
	Evidence         []store.PersonBriefEvidencePointer `json:"evidence"`
	Boundary         json.RawMessage                    `json:"boundary"`
	DroppedItemCount int                                `json:"dropped_item_count"`
	ProgramID        string                             `json:"program_id"`
	ProgramVersion   string                             `json:"program_version"`
	Provider         string                             `json:"provider"`
	Model            string                             `json:"model"`
	RejectedAt       *time.Time                         `json:"rejected_at"`
	RejectedReason   string                             `json:"rejected_reason"`
	SupersededAt     *time.Time                         `json:"superseded_at"`
}

// PersonBriefVersionsResponse is the newest-first version history.
type PersonBriefVersionsResponse struct {
	Versions []PersonBrief `json:"versions"`
}

// RejectPersonBriefRequest records the owner's verdict on the current version.
// The reason is optional: the store's column is NOT NULL and defaults to the
// empty string, so a rejection with nothing to say is representable.
type RejectPersonBriefRequest struct {
	Reason string `json:"reason" required:"false"`
}

// PutPersonBriefEnrollmentRequest replaces a person's brief enrollment.
// Enrolled is required. Track is optional and adds the tracking row enrollment
// requires, in the same transaction; without it an untracked person is refused.
type PutPersonBriefEnrollmentRequest struct {
	Enrolled bool `json:"enrolled"`
	Track    bool `json:"track" required:"false"`
}

func (s *Server) registerPersonBriefRoutes(api huma.API) {
	current := rawAPIV1Operation("getPersonBrief", http.MethodGet,
		"/people/{id}/brief", "Get a person's current brief version")
	addPersonIDParameter(&current)
	current.Responses = jsonResponsesFor[PersonBrief](api)
	addErrorResponses(api, current.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, current, s.handleGetPersonBrief)

	versions := rawAPIV1Operation("listPersonBriefVersions", http.MethodGet,
		"/people/{id}/brief/versions", "List a person's brief version history")
	addPersonIDParameter(&versions)
	versions.Parameters = append(versions.Parameters,
		queryIntegerParam(limitParam, "Maximum versions to return (default 20, max 200)"))
	versions.Responses = jsonResponsesFor[PersonBriefVersionsResponse](api)
	addErrorResponses(api, versions.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, versions, s.handleListPersonBriefVersions)

	reject := rawAPIV1Operation("rejectPersonBrief", http.MethodPost,
		"/people/{id}/brief/reject", "Reject a person's current brief version")
	addPersonIDParameter(&reject)
	reject.RequestBody = jsonRequestBodyFor[RejectPersonBriefRequest](api)
	reject.Responses = jsonResponsesFor[PersonBrief](api)
	addErrorResponses(api, reject.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, reject, s.handleRejectPersonBrief)

	generate := rawAPIV1Operation("generatePersonBrief", http.MethodPost,
		"/people/{id}/brief/generate", "Generate a person's brief now")
	addPersonIDParameter(&generate)
	generate.Responses = jsonResponsesFor[PersonBriefRun](api)
	addErrorResponses(api, generate.Responses, http.StatusBadRequest,
		http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, generate, s.handleGeneratePersonBrief)

	getEnrollment := rawAPIV1Operation("getPersonBriefEnrollment", http.MethodGet,
		"/people/{id}/brief-enrollment", "Get a person's brief enrollment")
	addPersonIDParameter(&getEnrollment)
	getEnrollment.Responses = jsonResponsesFor[store.PersonBriefEnrollment](api)
	addErrorResponses(api, getEnrollment.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, getEnrollment, s.handleGetPersonBriefEnrollment)

	setEnrollment := rawAPIV1Operation("setPersonBriefEnrollment", http.MethodPut,
		"/people/{id}/brief-enrollment", "Replace a person's brief enrollment")
	addPersonIDParameter(&setEnrollment)
	setEnrollment.RequestBody = jsonRequestBodyFor[PutPersonBriefEnrollmentRequest](api)
	setEnrollment.Responses = jsonResponsesFor[store.PersonBriefEnrollment](api)
	addErrorResponses(api, setEnrollment.Responses, http.StatusBadRequest,
		http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, setEnrollment, s.handleSetPersonBriefEnrollment)
}

func (s *Server) handleGetPersonBrief(w http.ResponseWriter, r *http.Request) {
	briefs, personID, ok := s.personBriefRequestStore(w, r)
	if !ok {
		return
	}
	brief, err := briefs.GetPersonBriefContext(r.Context(), personID, 0)
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	response, ok := s.personBriefDTO(w, r, briefs, *brief)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleListPersonBriefVersions(w http.ResponseWriter, r *http.Request) {
	briefs, personID, ok := s.personBriefRequestStore(w, r)
	if !ok {
		return
	}
	limit := defaultPersonBriefVersionLimit
	requested, present, err := queryInt(r, limitParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	if present {
		if requested < 1 || requested > maxPersonBriefVersionLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit",
				"limit must be between 1 and "+strconv.Itoa(maxPersonBriefVersionLimit))
			return
		}
		limit = requested
	}
	rows, err := briefs.ListPersonBriefVersionsContext(r.Context(), personID, limit)
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	response := PersonBriefVersionsResponse{Versions: make([]PersonBrief, 0, len(rows))}
	for _, row := range rows {
		version, ok := s.personBriefDTO(w, r, briefs, row)
		if !ok {
			return
		}
		response.Versions = append(response.Versions, version)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleRejectPersonBrief(w http.ResponseWriter, r *http.Request) {
	briefs, personID, ok := s.personBriefRequestStore(w, r)
	if !ok {
		return
	}
	var request RejectPersonBriefRequest
	if !decodePersonRequest(w, r, &request) {
		return
	}
	brief, err := briefs.RejectPersonBriefContext(r.Context(), personID,
		request.Reason, s.clockNow().UTC())
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	response, ok := s.personBriefDTO(w, r, briefs, *brief)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGeneratePersonBrief(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	generate := s.personBriefGeneratorFunc()
	if generate == nil {
		writeError(w, http.StatusServiceUnavailable, "brief_generation_unavailable",
			"Brief generation requires the daemon's people sweep worker")
		return
	}
	run, err := generate(r.Context(), personID)
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleGetPersonBriefEnrollment(w http.ResponseWriter, r *http.Request) {
	briefs, personID, ok := s.personBriefRequestStore(w, r)
	if !ok {
		return
	}
	state, err := briefs.GetPersonBriefEnrollmentContext(r.Context(), personID)
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleSetPersonBriefEnrollment(w http.ResponseWriter, r *http.Request) {
	briefs, personID, ok := s.personBriefRequestStore(w, r)
	if !ok {
		return
	}
	var request PutPersonBriefEnrollmentRequest
	fields, ok := decodePersonRequestFields(w, r, &request)
	if !ok {
		return
	}
	rawEnrolled, present := fields["enrolled"]
	if !present {
		writeError(w, http.StatusBadRequest, "bad_request", "enrolled is required")
		return
	}
	if bytes.Equal(bytes.TrimSpace(rawEnrolled), []byte("null")) {
		writeError(w, http.StatusBadRequest, "bad_request", "enrolled must be a boolean")
		return
	}
	state, err := briefs.SetPersonBriefEnrollmentContext(r.Context(), personID,
		request.Enrolled, personBriefActor, request.Track)
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) personBriefRequestStore(
	w http.ResponseWriter, r *http.Request,
) (PersonBriefStore, int64, bool) {
	briefs, ok := s.store.(PersonBriefStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "briefs_unavailable",
			"Person briefs are unavailable")
		return nil, 0, false
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return nil, 0, false
	}
	return briefs, personID, true
}

// personBriefDTO projects one stored version, loading its evidence pointers and
// rebuilding the sentence map. It writes the error response and reports false
// when the evidence read fails.
func (s *Server) personBriefDTO(
	w http.ResponseWriter, r *http.Request, briefs PersonBriefStore, brief store.PersonBrief,
) (PersonBrief, bool) {
	pointers, err := briefs.ListPersonBriefEvidenceContext(r.Context(), brief.ID)
	if err != nil {
		s.writePersonBriefError(w, err)
		return PersonBrief{}, false
	}
	if pointers == nil {
		pointers = []store.PersonBriefEvidencePointer{}
	}
	// The paragraph and the structure are provider prose derived from other
	// people's messages. The sentence map is rebuilt from the stored values
	// first, because it must match the stored paragraph byte for byte, and
	// only then is every prose string stripped of control characters and
	// terminal escapes, the same way the TUI strips them before rendering.
	sentences := s.personBriefSentences(brief, len(pointers))
	for index := range sentences {
		sentences[index].Text = textutil.SanitizeTerminal(sentences[index].Text)
	}
	return PersonBrief{
		Version:          brief.Version,
		Status:           brief.Status,
		GeneratedAt:      brief.GeneratedAt,
		RenderedText:     textutil.SanitizeTerminal(brief.RenderedText),
		RendererPolicy:   brief.RendererPolicy,
		Sentences:        sentences,
		Structured:       s.sanitizedPersonBriefStructure(brief),
		Evidence:         pointers,
		Boundary:         personBriefJSON(brief.Boundary),
		DroppedItemCount: brief.DroppedItemCount,
		ProgramID:        brief.ProgramID,
		ProgramVersion:   brief.ProgramVersion,
		Provider:         brief.Provider,
		Model:            brief.Model,
		RejectedAt:       brief.RejectedAt,
		RejectedReason:   brief.RejectedReason,
		SupersededAt:     brief.SupersededAt,
	}, true
}

// sanitizedPersonBriefStructure returns the stored structure with every
// prose field stripped of control characters. Evidence IDs, kinds, indexes,
// and confidence values are left as stored. A structure that does not decode
// is passed through untouched, as it was before, and the sentence map already
// reports the mismatch.
func (s *Server) sanitizedPersonBriefStructure(brief store.PersonBrief) json.RawMessage {
	if len(brief.Structured) == 0 {
		return personBriefJSON(brief.Structured)
	}
	var output peoplesweep.BriefOutput
	if err := json.Unmarshal(brief.Structured, &output); err != nil {
		return personBriefJSON(brief.Structured)
	}
	if output.LastMeaningfulInteraction != nil {
		output.LastMeaningfulInteraction.Summary =
			textutil.SanitizeTerminal(output.LastMeaningfulInteraction.Summary)
	}
	for index := range output.Highlights {
		output.Highlights[index].Text = textutil.SanitizeTerminal(output.Highlights[index].Text)
	}
	for index := range output.FollowUps {
		output.FollowUps[index].Question = textutil.SanitizeTerminal(output.FollowUps[index].Question)
		output.FollowUps[index].Why = textutil.SanitizeTerminal(output.FollowUps[index].Why)
	}
	for index := range output.Appreciations {
		output.Appreciations[index].Text = textutil.SanitizeTerminal(output.Appreciations[index].Text)
	}
	for index := range output.Uncertainties {
		output.Uncertainties[index].Text = textutil.SanitizeTerminal(output.Uncertainties[index].Text)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		s.logger.Error("person brief structure could not be re-encoded",
			"person_id", brief.PersonID, "version", brief.Version)
		return personBriefJSON(brief.Structured)
	}
	return encoded
}

func personBriefJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// personBriefSentences rebuilds the sentence-to-item map the renderer produced.
// Only the header sentence depends on generation-time state the store does not
// keep (the deterministic last-contact date and channel), so the map is
// recovered by re-rendering the stored structure and taking the header back out
// of the stored paragraph: the paragraph is the sentences joined by a space, so
// whatever precedes the re-rendered tail is exactly the header the owner read.
//
// A version stored under a different renderer policy cannot be mapped by this
// renderer, and a paragraph that does not end in the re-rendered tail is
// inconsistent with its own structure; both report no sentences rather than
// text the owner never saw. The paragraph and the structure are still returned,
// and renderer_policy tells the client which case it is looking at.
//
// Each sentence also carries the ordinals of the evidence pointers its own
// structured item cites, recovered by peoplesweep.BriefCitationOrdinals from
// the parser's citation order. That walk fails closed: when it cannot account
// for exactly pointerCount pointers, every sentence gets no ordinals and a
// client falls back to the version's whole evidence list.
func (s *Server) personBriefSentences(
	brief store.PersonBrief, pointerCount int,
) []PersonBriefSentence {
	empty := []PersonBriefSentence{}
	if brief.RendererPolicy != peoplesweep.BriefRendererPolicyV1 {
		return empty
	}
	var output peoplesweep.BriefOutput
	if err := json.Unmarshal(brief.Structured, &output); err != nil {
		s.logger.Error("person brief structure could not be decoded",
			"person_id", brief.PersonID, "version", brief.Version)
		return empty
	}
	rendered, err := peoplesweep.RenderBrief(
		peoplesweep.ParsedBrief{Output: output}, peoplesweep.BriefWindow{})
	if err != nil || len(rendered.Sentences) == 0 {
		return empty
	}
	hasHeader := rendered.Sentences[0].Kind == peoplesweep.BriefSentenceLastInteraction
	start := 0
	if hasHeader {
		start = 1
	}
	texts := make([]string, 0, len(rendered.Sentences)-start)
	for _, sentence := range rendered.Sentences[start:] {
		texts = append(texts, sentence.Text)
	}
	tail := strings.Join(texts, " ")
	head := brief.RenderedText
	if !hasHeader {
		if brief.RenderedText != tail {
			s.logger.Error("person brief paragraph does not match its structure",
				"person_id", brief.PersonID, "version", brief.Version)
			return empty
		}
	} else if tail != "" {
		if !strings.HasSuffix(brief.RenderedText, " "+tail) {
			s.logger.Error("person brief paragraph does not match its structure",
				"person_id", brief.PersonID, "version", brief.Version,
				"sentences", len(rendered.Sentences))
			return empty
		}
		head = strings.TrimSuffix(brief.RenderedText, " "+tail)
	}
	citations, _ := peoplesweep.BriefCitationOrdinals(output, pointerCount)
	sentences := make([]PersonBriefSentence, 0, len(rendered.Sentences))
	for index, sentence := range rendered.Sentences {
		text := sentence.Text
		if hasHeader && index == 0 {
			text = head
		}
		ordinals := citations[peoplesweep.BriefSentenceKey{
			Kind: sentence.Kind, Index: sentence.Index,
		}]
		if ordinals == nil {
			ordinals = []int{}
		}
		sentences = append(sentences, PersonBriefSentence{
			Kind: string(sentence.Kind), Index: sentence.Index, Text: text,
			EvidenceOrdinals: ordinals,
		})
	}
	return sentences
}

func (s *Server) writePersonBriefError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	var notTracked *store.PersonBriefNotTrackedError
	switch {
	case errors.Is(err, store.ErrPersonBriefNotFound):
		writeError(w, http.StatusNotFound, "person_brief_not_found", "Person brief not found")
	case errors.As(err, &notTracked):
		writeError(w, http.StatusConflict, "person_brief_not_tracked", notTracked.Error())
	case errors.Is(err, store.ErrPersonBriefNotTracked):
		writeError(w, http.StatusConflict, "person_brief_not_tracked",
			"Track the person with `msgvault person track` before enrolling it in briefs")
	case errors.Is(err, peoplesweep.ErrPersonBriefNotEnrolled):
		writeError(w, http.StatusConflict, "person_brief_not_enrolled",
			"Enroll the person in briefs before generating one")
	case errors.Is(err, peoplesweep.ErrPersonBriefLaneDisabled):
		writeError(w, http.StatusConflict, "person_brief_lane_disabled",
			"Brief generation is turned off by `[people.sweep.brief] enabled = false`")
	case errors.Is(err, peoplesweep.ErrPersonBriefPolicyRefused):
		writeError(w, http.StatusConflict, "person_brief_policy_refused",
			"The consented people inference profile sets `allow_sensitive = false`, "+
				"and a brief packet always carries verbatim archive text")
	case errors.Is(err, peoplesweep.ErrPersonBriefNoSupportedLane):
		writeError(w, http.StatusConflict, "person_brief_no_supported_lane",
			"The consented people inference profile's `allowed_sources` do not include "+
				"`conversation_text`, the only lane a brief reads in this version")
	case errors.Is(err, peoplesweep.ErrPersonBriefBusy):
		writeError(w, http.StatusConflict, "person_brief_busy",
			"Another worker is sweeping this person right now; nothing was generated. "+
				"Retry once it finishes")
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	default:
		s.logger.Error("person brief operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_brief_failed",
			"Person brief operation failed")
	}
}
