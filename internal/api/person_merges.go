package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	idempotencyKeyHeaderName       = "Idempotency-Key"
	newPersonETagOpenAPIHeaderName = "X-New-Person-ETag"
	apiPersonMergeActor            = "user"
)

var newPersonETagHeaderName = http.CanonicalHeaderKey(newPersonETagOpenAPIHeaderName)

type MergePersonRequest struct {
	AbsorbedPersonID int64 `json:"absorbed_person_id"`
}

type SplitPersonRequest struct {
	MergeID        int64   `json:"merge_id"`
	ParticipantIDs []int64 `json:"participant_ids"`
}

type DecidePersonMergeCandidateRequest struct {
	PersonID int64                              `json:"person_id"`
	Decision store.PersonMergeCandidateDecision `json:"decision" enum:"accept,reject"`
}

type PersonMergesResponse struct {
	Merges []store.PersonMergeSummary `json:"merges"`
	Limit  int                        `json:"limit"`
	Offset int                        `json:"offset"`
}

type PersonMergeRequiredError struct {
	Error    string               `json:"error"`
	Message  string               `json:"message"`
	Profiles []PersonMergeProfile `json:"profiles"`
}

type PersonMergeProfile struct {
	Person store.Person `json:"person"`
	ETag   string       `json:"etag"`
}

func (s *Server) registerPersonMergeRoutes(api huma.API) {
	merge := rawAPIV1Operation("mergePersons", http.MethodPost, "/people/{id}/merge",
		"Merge one durable person profile into another")
	addPersonIDParameter(&merge)
	addPersonMergeIfMatchParameter(&merge)
	addIdempotencyKeyParameter(&merge)
	merge.RequestBody = jsonRequestBodyFor[MergePersonRequest](api)
	merge.Responses = jsonResponsesFor[store.PersonMergeResult](api)
	addPersonETagHeader(merge.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, merge.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusPreconditionRequired, http.StatusInternalServerError,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, merge, s.handleMergePersons)

	split := rawAPIV1Operation("splitPersonMerge", http.MethodPost, "/people/{id}/split",
		"Split absorbed participant lineage into a new person")
	addPersonIDParameter(&split)
	addPersonIfMatchParameter(&split)
	addIdempotencyKeyParameter(&split)
	split.RequestBody = jsonRequestBodyFor[SplitPersonRequest](api)
	split.Responses = jsonResponsesFor[store.PersonSplitResult](api)
	addPersonETagHeader(split.Responses[httpStatusKey(http.StatusOK)])
	addNewPersonETagHeader(split.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, split.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusPreconditionRequired, http.StatusInternalServerError,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, split, s.handleSplitPersonMerge)

	list := rawAPIV1Operation("listPersonMerges", http.MethodGet, "/people/{id}/merges",
		"List merge history for a durable person")
	addPersonIDParameter(&list)
	list.Parameters = append(list.Parameters,
		queryIntegerParam("limit", "Maximum results"),
		queryIntegerParam("offset", "Results to skip"))
	list.Responses = jsonResponsesFor[PersonMergesResponse](api)
	addErrorResponses(api, list.Responses, http.StatusNotFound, http.StatusInternalServerError,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonMerges)

	detail := rawAPIV1Operation("getPersonMerge", http.MethodGet, "/person-merges/{merge_id}",
		"Inspect one durable person merge")
	addMergeIDParameter(&detail)
	detail.Responses = jsonResponsesFor[store.PersonMergeDetail](api)
	addErrorResponses(api, detail.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, detail, s.handleGetPersonMerge)

	snapshot := rawAPIV1Operation("getPersonMergeSnapshot", http.MethodGet,
		"/person-merges/{merge_id}/snapshot", "Read and verify one person merge snapshot")
	addMergeIDParameter(&snapshot)
	snapshot.Responses = jsonResponsesFor[store.PersonMergeSnapshotResponse](api)
	addNoStoreHeader(snapshot.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, snapshot.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, snapshot, s.handleGetPersonMergeSnapshot)

	decision := rawAPIV1Operation("decidePersonMergeCandidate", http.MethodPost,
		"/person-merge-candidates/{candidate_id}/decision",
		"Accept or reject a person merge attribute candidate")
	addCandidateIDParameter(&decision)
	addPersonIfMatchParameter(&decision)
	decision.RequestBody = jsonRequestBodyFor[DecidePersonMergeCandidateRequest](api)
	decision.Responses = jsonResponsesFor[store.PersonMergeReviewCandidate](api)
	addPersonETagHeader(decision.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, decision.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusPreconditionRequired, http.StatusInternalServerError,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, decision, s.handleDecidePersonMergeCandidate)
}

func (s *Server) handleMergePersons(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	survivorID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	var body MergePersonRequest
	if !decodeEntityRequest(w, r, &body, "person merge") {
		return
	}
	if body.AbsorbedPersonID <= 0 || body.AbsorbedPersonID == survivorID {
		writeError(w, http.StatusBadRequest, "person_merge_invalid",
			"absorbed_person_id must name a different positive person ID")
		return
	}
	revisions, ok := personMergeIfMatch(w, r, survivorID, body.AbsorbedPersonID)
	if !ok {
		return
	}
	idempotencyKey, ok := personOperationIdempotencyKey(w, r)
	if !ok {
		return
	}
	result, err := profiles.MergePersonsContext(r.Context(), store.PersonMergeRequest{
		SurvivorID: survivorID, AbsorbedID: body.AbsorbedPersonID,
		ExpectedSurvivorRevision: revisions[survivorID],
		ExpectedAbsorbedRevision: revisions[body.AbsorbedPersonID],
		IdempotencyKey:           idempotencyKey, Actor: apiPersonMergeActor,
	})
	if err != nil {
		s.writePersonMergeError(w, err)
		return
	}
	result.CacheState = s.refreshIdentityCacheState(r.Context())
	w.Header().Set(etagHeaderName, personETag(result.Person))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSplitPersonMerge(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	sourceID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	var body SplitPersonRequest
	if !decodeEntityRequest(w, r, &body, "person split") {
		return
	}
	revision, ok := personIfMatch(w, r, sourceID)
	if !ok {
		return
	}
	idempotencyKey, ok := personOperationIdempotencyKey(w, r)
	if !ok {
		return
	}
	result, err := profiles.SplitPersonMergeContext(r.Context(), store.PersonSplitRequest{
		SourcePersonID: sourceID, MergeID: body.MergeID,
		ParticipantIDs: body.ParticipantIDs, ExpectedSourceRevision: revision,
		IdempotencyKey: idempotencyKey, Actor: apiPersonMergeActor,
	})
	if err != nil {
		s.writePersonMergeError(w, err)
		return
	}
	result.CacheState = s.refreshIdentityCacheState(r.Context())
	w.Header().Set(etagHeaderName, personETag(result.SourcePerson))
	w.Header().Set(newPersonETagHeaderName, personETag(result.NewPerson))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListPersonMerges(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	limit, _, err := queryInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer")
		return
	}
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	offset, _, err := queryInt(r, "offset")
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
		return
	}
	merges, err := profiles.ListPersonMergesPageContext(r.Context(), personID, limit, offset)
	if err != nil {
		s.writePersonMergeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PersonMergesResponse{Merges: merges, Limit: limit, Offset: offset})
}

func (s *Server) handleGetPersonMerge(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	mergeID, ok := personMergePositivePathID(w, r, "merge_id", "invalid_merge_id", "Merge ID")
	if !ok {
		return
	}
	detail, err := profiles.GetPersonMergeContext(r.Context(), mergeID)
	if err != nil {
		s.writePersonMergeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleGetPersonMergeSnapshot(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	mergeID, ok := personMergePositivePathID(w, r, "merge_id", "invalid_merge_id", "Merge ID")
	if !ok {
		return
	}
	snapshot, err := profiles.GetPersonMergeSnapshotContext(r.Context(), mergeID)
	if err != nil {
		s.writePersonMergeError(w, err)
		return
	}
	digest := sha256.Sum256(snapshot.JSON)
	if hex.EncodeToString(digest[:]) != snapshot.SHA256 {
		s.writePersonMergeError(w, store.ErrPersonMergeSnapshotCorrupt)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", applicationJSONMediaType)
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleDecidePersonMergeCandidate(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	candidateID, ok := personMergePositivePathID(
		w, r, "candidate_id", "invalid_candidate_id", "Candidate ID",
	)
	if !ok {
		return
	}
	var body DecidePersonMergeCandidateRequest
	if !decodeEntityRequest(w, r, &body, "person merge candidate") {
		return
	}
	if body.PersonID <= 0 {
		writeError(w, http.StatusBadRequest, "person_merge_invalid", "person_id must be positive")
		return
	}
	revision, ok := personIfMatch(w, r, body.PersonID)
	if !ok {
		return
	}
	decision, err := profiles.DecidePersonMergeCandidateContext(
		r.Context(), store.PersonMergeCandidateDecisionRequest{
			CandidateID: candidateID, PersonID: body.PersonID,
			ExpectedPersonRevision: revision, Decision: body.Decision,
			Actor: apiPersonMergeActor,
		},
	)
	if err != nil {
		s.writePersonMergeError(w, err)
		return
	}
	w.Header().Set(etagHeaderName, personRevisionETag(body.PersonID, decision.PersonRevision))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, decision.PersonMergeReviewCandidate)
}

func (s *Server) writePersonMergeError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	case errors.Is(err, store.ErrPersonMergeNotFound), errors.Is(err, store.ErrPersonSplitNotFound):
		writeError(w, http.StatusNotFound, "person_merge_not_found", "Person merge not found")
	case errors.Is(err, store.ErrPersonRevisionConflict), errors.Is(err, store.ErrPersonSplitRevision):
		writeError(w, http.StatusConflict, "person_merge_revision_conflict",
			"Person profile changed; reload and retry")
	case errors.Is(err, store.ErrPersonMergeIdempotency):
		writeError(w, http.StatusConflict, "person_merge_idempotency_conflict",
			"Idempotency key was already used for a different merge")
	case errors.Is(err, store.ErrPersonSplitIdempotency):
		writeError(w, http.StatusConflict, "person_split_idempotency_conflict",
			"Idempotency key was already used for a different split")
	case errors.Is(err, store.ErrPersonMergeAlreadySplit):
		writeError(w, http.StatusConflict, "person_merge_already_split",
			"The selected merge lineage has already been split")
	case errors.Is(err, store.ErrPersonSplitReviewed):
		writeError(w, http.StatusConflict, "person_split_reviewed_candidates",
			"Exact reversal is unavailable after a merge review candidate was accepted")
	case errors.Is(err, store.ErrPersonSplitOwnership):
		writeError(w, http.StatusConflict, "person_split_merge_not_owned",
			"The selected merge is no longer owned by this person")
	case errors.Is(err, store.ErrPersonMergeCandidateNotFound):
		writeError(w, http.StatusNotFound, "person_merge_candidate_not_found",
			"Person merge candidate not found")
	case errors.Is(err, store.ErrPersonMergeCandidateState):
		writeError(w, http.StatusConflict, "person_merge_candidate_state_changed",
			"The candidate or current person value changed; reload and retry")
	case errors.Is(err, store.ErrPersonSplitParticipants):
		writeError(w, http.StatusBadRequest, "person_split_invalid_participants", err.Error())
	case errors.Is(err, store.ErrPersonMergeInvalid):
		writeError(w, http.StatusBadRequest, "person_merge_invalid", err.Error())
	case errors.Is(err, store.ErrPersonMergeSnapshotCorrupt):
		s.logger.Error("person merge snapshot integrity failure", "error", err)
		writeError(w, http.StatusInternalServerError, "person_merge_snapshot_corrupt",
			"Person merge snapshot failed integrity verification")
	default:
		s.logger.Error("person merge operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_merge_failed",
			"Person merge operation failed")
	}
}

func (s *Server) writePersonMergeRequired(
	w http.ResponseWriter, r *http.Request, err error,
) bool {
	var conflict *store.PersonBindingConflictError
	if !errors.As(err, &conflict) {
		return false
	}
	personIDs := append([]int64(nil), conflict.PersonIDs...)
	slices.Sort(personIDs)
	personIDs = slices.Compact(personIDs)
	if len(personIDs) != 2 {
		return false
	}
	profiles, ok := s.store.(PersonProfileStore)
	if !ok {
		return false
	}

	response := PersonMergeRequiredError{
		Error:    "person_merge_required",
		Message:  "The identity clusters belong to different person profiles; merge one profile before retrying",
		Profiles: make([]PersonMergeProfile, 0, len(personIDs)),
	}
	for _, personID := range personIDs {
		person, loadErr := profiles.GetPersonContext(r.Context(), personID)
		if loadErr != nil {
			s.logger.Error("load person merge conflict profile", "person_id", personID, "error", loadErr)
			writeError(w, http.StatusInternalServerError, "person_merge_failed",
				"Person merge profiles could not be loaded")
			return true
		}
		response.Profiles = append(response.Profiles, PersonMergeProfile{
			Person: *person,
			ETag:   personETag(*person),
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusConflict, response)
	return true
}

func personMergeConflictResponseFor(api huma.API) *huma.Response {
	return &huma.Response{
		Description: http.StatusText(http.StatusConflict),
		Content: map[string]*huma.MediaType{
			applicationJSONMediaType: {
				Schema: &huma.Schema{AnyOf: []*huma.Schema{
					schemaFor[PersonMergeRequiredError](api),
					schemaFor[ErrorResponse](api),
				}},
			},
		},
	}
}

func personMergeIfMatch(
	w http.ResponseWriter, r *http.Request, survivorID, absorbedID int64,
) (map[int64]int64, bool) {
	values := r.Header.Values(ifMatchHeaderName)
	if len(values) == 0 {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return nil, false
	}
	tags := []string{}
	for _, value := range values {
		for tag := range strings.SplitSeq(value, ",") {
			tags = append(tags, strings.TrimSpace(tag))
		}
	}
	if len(tags) != 2 {
		writeError(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match must contain exactly two strong person revision tags")
		return nil, false
	}
	revisions := make(map[int64]int64, 2)
	for _, tag := range tags {
		id, revision, ok := parsePersonETag(tag)
		if !ok || (id != survivorID && id != absorbedID) {
			writeError(w, http.StatusBadRequest, "invalid_if_match",
				"If-Match must contain one strong revision tag for each person")
			return nil, false
		}
		if _, duplicate := revisions[id]; duplicate {
			writeError(w, http.StatusBadRequest, "invalid_if_match",
				"If-Match contains a duplicate person revision tag")
			return nil, false
		}
		revisions[id] = revision
	}
	if len(revisions) != 2 {
		writeError(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match must contain one strong revision tag for each person")
		return nil, false
	}
	return revisions, true
}

func parsePersonETag(value string) (int64, int64, bool) {
	if len(value) < len(`"person-1-r1"`) || value[0] != '"' || value[len(value)-1] != '"' {
		return 0, 0, false
	}
	inner := value[1 : len(value)-1]
	if !strings.HasPrefix(inner, "person-") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(inner, "person-"), "-r")
	if len(parts) != 2 {
		return 0, 0, false
	}
	id, idErr := strconv.ParseInt(parts[0], 10, 64)
	revision, revisionErr := strconv.ParseInt(parts[1], 10, 64)
	return id, revision, idErr == nil && revisionErr == nil && id > 0 && revision > 0
}

func personOperationIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values(idempotencyKeyHeaderName)
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "idempotency_key_required",
			"Idempotency-Key is required")
		return "", false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key",
			"Idempotency-Key must contain exactly one value")
		return "", false
	}
	value := strings.TrimSpace(values[0])
	if len(value) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key",
			"Idempotency-Key must be at most 128 bytes")
		return "", false
	}
	return value, true
}

func personMergePositivePathID(
	w http.ResponseWriter, r *http.Request, name, code, label string,
) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, code, label+" must be a positive integer")
		return 0, false
	}
	return id, true
}

func addPersonMergeIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
		Description: "Exactly two comma-separated strong person revision tags, one for each profile",
		Schema:      &huma.Schema{Type: huma.TypeString},
	})
}

func addIdempotencyKeyParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: idempotencyKeyHeaderName, In: headerParamLocation, Required: true,
		Description: "Opaque 1..128-byte retry key",
		Schema:      &huma.Schema{Type: huma.TypeString, MinLength: new(1), MaxLength: new(128)},
	})
}

func addMergeIDParameter(operation *huma.Operation) {
	addPositiveInt64PathParameter(operation, "merge_id", "Durable person merge ID")
}

func addCandidateIDParameter(operation *huma.Operation) {
	addPositiveInt64PathParameter(operation, "candidate_id", "Person merge review candidate ID")
}

func addPositiveInt64PathParameter(operation *huma.Operation, name, description string) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: name, In: pathKey, Required: true, Description: description,
		Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64, Minimum: new(float64(1))},
	})
}

func addNewPersonETagHeader(response *huma.Response) {
	if response.Headers == nil {
		response.Headers = map[string]*huma.Param{}
	}
	response.Headers[newPersonETagOpenAPIHeaderName] = &huma.Param{
		Description: "Strong revision tag for the new person created by a split",
		Schema:      &huma.Schema{Type: huma.TypeString},
	}
}

func addNoStoreHeader(response *huma.Response) {
	if response.Headers == nil {
		response.Headers = map[string]*huma.Param{}
	}
	response.Headers["Cache-Control"] = &huma.Param{
		Description: "Always no-store because the response contains merge provenance",
		Schema:      &huma.Schema{Type: huma.TypeString},
	}
}
