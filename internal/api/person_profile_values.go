package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const MaxPersonProfilePatchBytes = 12 << 20

type PersonProfileValueStore interface {
	GetPersonProfileContext(ctx context.Context, personID int64) (*store.PersonProfile, error)
	ApplyPersonProfilePatchContext(
		ctx context.Context,
		personID, expectedRevision int64,
		patch store.PersonProfilePatch,
	) (*store.PersonProfile, error)
	GetPersonProfileHistoryContext(
		ctx context.Context, personID int64,
	) (*store.PersonProfileHistory, error)
}

// StructuredPersonProfile gives the aggregate store model a distinct OpenAPI
// component name. The query package already exports an unrelated
// PersonProfile, and huma component names are package-agnostic.
type StructuredPersonProfile store.PersonProfile

func (s *Server) registerPersonProfileValueRoutes(api huma.API) {
	get := rawAPIV1Operation(
		"getPersonStructuredProfile", http.MethodGet, "/persons/{id}/profile",
		"Get a person's current structured profile",
	)
	get.Description = "Returns only current structured values at one person revision. " +
		"Superseded values and archive observations are available from the separate history endpoint."
	addPersonIDParameter(&get)
	get.Responses = jsonResponsesFor[StructuredPersonProfile](api)
	addPersonETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetPersonStructuredProfile)

	patch := rawAPIV1Operation(
		"patchPersonStructuredProfile", http.MethodPatch, "/persons/{id}/profile",
		"Atomically patch a person's structured profile",
	)
	patch.Description = "Applies up to 200 explicit adds and supersedes atomically under If-Match. " +
		"One patch advances the person revision once. Superseding closes world and transaction time without deletion."
	addPersonIDParameter(&patch)
	addPersonIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[store.PersonProfilePatch](api)
	patch.Responses = jsonResponsesFor[StructuredPersonProfile](api)
	addPersonETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusPreconditionRequired, http.StatusRequestEntityTooLarge,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchPersonStructuredProfile)

	history := rawAPIV1Operation(
		"getPersonProfileHistory", http.MethodGet, "/persons/{id}/profile/history",
		"Get a person's structured profile history",
	)
	history.Description = "Returns current and superseded structured values plus source-linked observations " +
		"for every participant bound to the person."
	addPersonIDParameter(&history)
	history.Responses = jsonResponsesFor[store.PersonProfileHistory](api)
	addErrorResponses(api, history.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, history, s.handleGetPersonProfileHistory)
}

func (s *Server) handleGetPersonStructuredProfile(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileValueStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	profile, err := profiles.GetPersonProfileContext(r.Context(), id)
	if err != nil {
		s.writePersonProfileValueError(w, err)
		return
	}
	writePersonStructuredProfile(w, http.StatusOK, profile)
}

func (s *Server) handlePatchPersonStructuredProfile(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileValueStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	revision, ok := personIfMatch(w, r, id)
	if !ok {
		return
	}
	var patch store.PersonProfilePatch
	if !decodeProfilePatchRequest(w, r, &patch) {
		return
	}
	profile, err := profiles.ApplyPersonProfilePatchContext(
		r.Context(), id, revision, patch,
	)
	if err != nil {
		s.writePersonProfileValueError(w, err)
		return
	}
	writePersonStructuredProfile(w, http.StatusOK, profile)
}

func (s *Server) handleGetPersonProfileHistory(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileValueStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	history, err := profiles.GetPersonProfileHistoryContext(r.Context(), id)
	if err != nil {
		s.writePersonProfileValueError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, history)
}

func (s *Server) personProfileValueStore(
	w http.ResponseWriter,
) (PersonProfileValueStore, bool) {
	profiles, ok := s.store.(PersonProfileValueStore)
	if !ok {
		writeError(
			w, http.StatusServiceUnavailable, "profile_values_unavailable",
			"Structured person profile values are unavailable",
		)
	}
	return profiles, ok
}

func (s *Server) writePersonProfileValueError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	case errors.Is(err, store.ErrPersonRevisionConflict):
		writeError(w, http.StatusConflict, "person_revision_conflict", "Person profile changed; reload and retry")
	case errors.Is(err, store.ErrServiceAliasConflict):
		writeError(w, http.StatusConflict, "service_alias_conflict", err.Error())
	case errors.Is(err, store.ErrPersonCategoryDuplicate):
		writeError(w, http.StatusConflict, "person_category_duplicate", err.Error())
	case errors.Is(err, store.ErrPersonProfilePatchTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "profile_patch_too_large", err.Error())
	case isPersonProfileValidationError(err):
		writeError(w, http.StatusBadRequest, "invalid_profile_value", err.Error())
	case errors.Is(err, store.ErrProfileValueNotFound):
		writeError(w, http.StatusNotFound, "profile_value_not_found", err.Error())
	default:
		s.logger.Error("structured person profile operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_profile_failed", "Person profile operation failed")
	}
}

func isPersonProfileValidationError(err error) bool {
	for _, target := range []error{
		store.ErrInvalidProvenance, store.ErrConfidenceScope,
		store.ErrInvalidProfilePref, store.ErrInvalidPartialDate,
		store.ErrInvalidPersonNameKind, store.ErrPersonNameValueMissing,
		store.ErrInvalidContactAddressKind, store.ErrContactPointValueMissing,
		store.ErrInvalidPersonAddressKind, store.ErrPersonAddressValueMissing,
		store.ErrInvalidPersonDateKind, store.ErrPersonDateValueMissing,
		store.ErrPersonCategoryEmpty, store.ErrInvalidPersonMediaKind,
		store.ErrPersonMediaEmpty, store.ErrPersonMediaTooLarge,
		store.ErrServiceNotFound, store.ErrServiceScopeRequired,
		store.ErrServiceScopeForbidden, store.ErrNormalizationRejected,
		store.ErrPersonProfilePatchEmpty,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func writePersonStructuredProfile(
	w http.ResponseWriter, status int, profile *store.PersonProfile,
) {
	w.Header().Set("ETag", personETag(profile.Person))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, profile)
}

// A patch may contain one 8 MiB inline media value. Base64 expansion plus
// surrounding JSON fits under 12 MiB without raising the shared person cap.
func decodeProfilePatchRequest(
	w http.ResponseWriter, r *http.Request, target *store.PersonProfilePatch,
) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPersonProfilePatchBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person profile patch")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person profile patch: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Person profile patch must contain one JSON object")
		return false
	}
	return true
}
