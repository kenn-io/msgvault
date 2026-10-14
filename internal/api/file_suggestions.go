package api

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

type filenameSuggestionStore interface {
	BeginReadSnapshotContext(ctx context.Context) (context.Context, func(), error)
	FilenamePersonContext(ctx context.Context, id int64) (*store.Person, error)
	FilenameCandidatesContext(ctx context.Context, query filenamediscovery.Query, scope store.FilenameSuggestionScope) (store.FilenameCandidateSet, error)
	FilenameSuggestionRowsContext(ctx context.Context, ids []int64, scope store.FilenameSuggestionScope) ([]store.FilenameSuggestionRow, error)
}

type FileSuggestionHTTPRequest struct {
	FilenameQuery string                `json:"filename_query"`
	SourceIDs     []int64               `json:"source_ids,omitempty" maxItems:"64"`
	AccountScopes []search.AccountScope `json:"account_scopes,omitempty" maxItems:"16"`
	After         *time.Time            `json:"after,omitempty"`
	Before        *time.Time            `json:"before,omitempty"`
	MIMEFamilies  []string              `json:"mime_families,omitempty" maxItems:"8" enum:"image,pdf,audio,video,text,document,archive,other"`
	Limit         *int                  `json:"limit,omitempty" minimum:"1" maximum:"100" nullable:"false"`
}

type PersonFileSuggestionHTTPRequest struct {
	FileSuggestionHTTPRequest

	Directions []personscope.Direction `json:"directions,omitempty" enum:"from_person,to_person,group"`
}

type FileSuggestionRow struct {
	ID               int64            `json:"id"`
	MessageID        int64            `json:"message_id"`
	ConversationID   int64            `json:"conversation_id"`
	SourceID         int64            `json:"source_id"`
	OccurredAt       *time.Time       `json:"occurred_at"`
	Filename         string           `json:"filename"`
	MimeType         string           `json:"mime_type"`
	MIMEFamily       string           `json:"mime_family" enum:"image,pdf,audio,video,text,document,archive,other"`
	FilenameMatch    string           `json:"filename_match" enum:"exact,prefix,typo"`
	Size             int64            `json:"size_bytes"`
	ContentState     FileContentState `json:"content_state" enum:"metadata_only,url_only,missing_blob,local_content"`
	ContentAvailable bool             `json:"content_available"`
}

type PersonFileSuggestionRow struct {
	FileSuggestionRow

	PersonProvenance personscope.Provenance `json:"person_provenance"`
}

type FileSuggestionHTTPResponse struct {
	Suggestions           []FileSuggestionRow `json:"suggestions"`
	CandidateLimitReached bool                `json:"candidate_limit_reached"`
	IndexRevision         string              `json:"index_revision"`
}

type PersonFileSuggestionHTTPResponse struct {
	Suggestions           []PersonFileSuggestionRow `json:"suggestions"`
	CandidateLimitReached bool                      `json:"candidate_limit_reached"`
	IndexRevision         string                    `json:"index_revision"`
}

func (s *Server) registerFileSuggestionRoutes(api huma.API) {
	op := rawAPIV1Operation("suggestFiles", http.MethodPost, "/files/suggest", "Suggest bounded attachment filenames")
	op.Tags = []string{"Exploration"}
	op.RequestBody = jsonRequestBodyFor[FileSuggestionHTTPRequest](api)
	op.Responses = jsonResponsesFor[FileSuggestionHTTPResponse](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleSuggestFiles)
	op = rawAPIV1Operation("suggestPersonFiles", http.MethodPost, "/people/{id}/files/suggest", "Suggest bounded attachment filenames for one person")
	op.Tags = []string{"Exploration"}
	op.Parameters = append(op.Parameters, &huma.Param{Name: "id", In: pathKey, Required: true, Description: "Durable person ID", Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64}})
	op.RequestBody = jsonRequestBodyFor[PersonFileSuggestionHTTPRequest](api)
	op.Responses = jsonResponsesFor[PersonFileSuggestionHTTPResponse](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleSuggestPersonFiles)
}

func decodeFilenameSuggestionJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	// Limit is optional, but a supplied value must be an integer, not null.
	// Keep the public request's *int shape while preserving omission semantics.
	limitPresence := json.UnmarshalFromFunc(func(decoder *jsontext.Decoder, _ **int) error {
		if decoder.PeekKind() == 'n' {
			return errors.New("limit must be an integer")
		}
		return errors.ErrUnsupported
	})
	decoder := jsontext.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10), json.RejectUnknownMembers(true), json.WithUnmarshalers(limitPresence))
	if decoder.PeekKind() != '{' {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return false
	}
	if err := json.UnmarshalDecode(decoder, dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body: "+err.Error())
		return false
	}
	if err := json.UnmarshalDecode(decoder, &struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return false
	}
	return true
}

func (s *Server) handleSuggestFiles(w http.ResponseWriter, r *http.Request) {
	var request FileSuggestionHTTPRequest
	if !decodeFilenameSuggestionJSON(w, r, &request) {
		return
	}
	s.handleFilenameSuggestions(w, r, request, nil, nil)
}
func (s *Server) handleSuggestPersonFiles(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_person_id", "person ID must be a positive integer")
		return
	}
	var request PersonFileSuggestionHTTPRequest
	if !decodeFilenameSuggestionJSON(w, r, &request) {
		return
	}
	if _, _, err := resolver.NormalizeDirections(request.Directions); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_person_file_directions", err.Error())
		return
	}
	s.handleFilenameSuggestions(w, r, request.FileSuggestionHTTPRequest, &id, request.Directions)
}

type filenamePersonResolver struct{ store filenameSuggestionStore }

func (a filenamePersonResolver) GetPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return a.store.FilenamePersonContext(ctx, id)
}

type verifiedFilenameSuggestion struct {
	authority store.FilenameSuggestionRow
	name      filenamediscovery.Name
	rank      filenamediscovery.Rank
}

func (s *Server) handleFilenameSuggestions(w http.ResponseWriter, r *http.Request, request FileSuggestionHTTPRequest, personID *int64, directions []personscope.Direction) {
	q, err := filenamediscovery.ParseQuery(request.FilenameQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filename_query", err.Error())
		return
	}
	limit := 20
	if request.Limit != nil {
		limit = *request.Limit
	}
	if limit < 1 || limit > 100 {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
		return
	}
	scope := store.FilenameSuggestionScope{SourceIDs: request.SourceIDs, AccountScopes: request.AccountScopes, After: request.After, Before: request.Before, MIMEFamilies: request.MIMEFamilies}
	if err := store.ValidateFilenameSuggestionScope(scope); err != nil {
		s.writeFilenameSuggestionError(w, err)
		return
	}
	catalog, ok := s.store.(filenameSuggestionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "filename_index_unavailable", "Filename suggestions are not available")
		return
	}
	ctx, release, err := catalog.BeginReadSnapshotContext(r.Context())
	if err != nil {
		s.writeFilenameSuggestionError(w, err)
		return
	}
	defer release()
	if personID != nil {
		reference := resolver.Reference{Kind: resolver.ReferencePerson, ID: *personID}
		resolved, err := resolver.Resolve(ctx, filenamePersonResolver{catalog}, reference, directions)
		if err != nil {
			if errors.Is(err, store.ErrFilenameScopeTooLarge) {
				s.writeFilenameSuggestionError(w, err)
			} else {
				s.writePersonScopeError(w, reference, err, "filename suggestion")
			}
			return
		}
		scope.Person = &resolved.Scope
	}
	candidates, err := catalog.FilenameCandidatesContext(ctx, q, scope)
	if err != nil {
		s.writeFilenameSuggestionError(w, err)
		return
	}
	rows, err := catalog.FilenameSuggestionRowsContext(ctx, candidates.IDs, scope)
	if err != nil {
		s.writeFilenameSuggestionError(w, err)
		return
	}
	verified := make([]verifiedFilenameSuggestion, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			s.writeFilenameSuggestionError(w, err)
			return
		}
		name, ok := filenamediscovery.ProjectName(row.File.Filename)
		if !ok {
			continue
		}
		rank, ok := filenamediscovery.Match(q, name)
		if ok {
			verified = append(verified, verifiedFilenameSuggestion{authority: row, name: name, rank: rank})
		}
	}
	// Within a match class, newest first matches candidate admission, so a
	// limited lookup returns the top of the complete order for its streams.
	slices.SortFunc(verified, func(a, b verifiedFilenameSuggestion) int {
		if order := cmp.Compare(a.rank, b.rank); order != 0 {
			return order
		}
		return cmp.Compare(b.authority.File.ID, a.authority.File.ID)
	})
	verified = verified[:min(limit, len(verified))]
	// Candidate authority, person provenance, and current filenames are fully
	// materialized. Release the shared read snapshot before checking physical
	// blob availability, which may wait on a separate resolver or the filesystem.
	release()
	ctx = store.WithoutReadSnapshotContext(r.Context())
	result := FileSuggestionHTTPResponse{Suggestions: []FileSuggestionRow{}, CandidateLimitReached: candidates.CandidateLimitReached, IndexRevision: candidates.IndexRevision}
	personResult := PersonFileSuggestionHTTPResponse{Suggestions: []PersonFileSuggestionRow{}, CandidateLimitReached: candidates.CandidateLimitReached, IndexRevision: candidates.IndexRevision}
	for _, match := range verified {
		if err := ctx.Err(); err != nil {
			s.writeFilenameSuggestionError(w, err)
			return
		}
		row := match.authority
		file := row.File
		label := "exact"
		switch match.rank {
		case filenamediscovery.FullExact, filenamediscovery.TokensExact:
			// Both exact ranks use the same public label.
		case filenamediscovery.Prefix:
			label = "prefix"
		case filenamediscovery.Typo:
			label = "typo"
		}
		state, available := s.fileContentState(ctx, file)
		suggestion := FileSuggestionRow{ID: file.ID, MessageID: file.MessageID, ConversationID: file.ConversationID, SourceID: file.SourceID, OccurredAt: row.OccurredAt, Filename: file.Filename, MimeType: file.MimeType, MIMEFamily: row.MIMEFamily, FilenameMatch: label, Size: file.Size, ContentState: state, ContentAvailable: available}
		if personID == nil {
			result.Suggestions = append(result.Suggestions, suggestion)
		} else {
			if row.PersonProvenance == nil {
				writeError(w, http.StatusInternalServerError, "filename_suggestion_failed", "Person provenance is unavailable")
				return
			}
			personResult.Suggestions = append(personResult.Suggestions, PersonFileSuggestionRow{FileSuggestionRow: suggestion, PersonProvenance: *row.PersonProvenance})
		}
	}
	if err := ctx.Err(); err != nil {
		s.writeFilenameSuggestionError(w, err)
		return
	}
	if personID == nil {
		writeJSON(w, http.StatusOK, result)
	} else {
		writeJSON(w, http.StatusOK, personResult)
	}
}

func (s *Server) writeFilenameSuggestionError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrFilenameIndexUnavailable):
		writeError(w, http.StatusServiceUnavailable, "filename_index_unavailable", "Filename suggestions are warming up; retry after maintenance completes")
	case errors.Is(err, store.ErrFilenameScopeTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "filename_scope_too_large", "The mandatory person scope exceeds filename suggestion limits")
	case errors.Is(err, store.ErrFilenameRequestInvalid):
		writeError(w, http.StatusBadRequest, "invalid_filename_request", err.Error())
	default:
		s.logger.Error("filename suggestions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "filename_suggestion_failed", "Could not suggest attachment filenames")
	}
}
