package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

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
	ReadPersonMediaDataContext(
		ctx context.Context, personID, mediaID int64,
	) ([]byte, string, error)
}

// StructuredPersonProfile gives the aggregate store model a distinct OpenAPI
// component name. The query package already exports an unrelated
// PersonProfile, and huma component names are package-agnostic.
type StructuredPersonProfile store.PersonProfile

// ValueEnvelopeInput is the client-writable part of store.ValueEnvelope.
// Database IDs and transaction timestamps are response-only fields, so the
// PATCH schema must not require clients to fabricate them.
type ValueEnvelopeInput store.ValueEnvelopeInput

type PersonProfilePatchRequest struct {
	Names         *PersonNamePatchRequest         `json:"names,omitempty"`
	ContactPoints *PersonContactPointPatchRequest `json:"contact_points,omitempty"`
	Addresses     *PersonAddressPatchRequest      `json:"addresses,omitempty"`
	Dates         *PersonDatePatchRequest         `json:"dates,omitempty"`
	Categories    *PersonCategoryPatchRequest     `json:"categories,omitempty"`
	Media         *PersonMediaPatchRequest        `json:"media,omitempty"`
}

type PersonNamePatchRequest struct {
	Add       []PersonNameInputRequest `json:"add,omitempty"`
	Supersede []int64                  `json:"supersede,omitempty"`
}

type PersonNameInputRequest struct {
	NameKind          store.PersonNameKind `json:"name_kind"`
	Formatted         *string              `json:"formatted,omitempty"`
	FamilyName        *string              `json:"family_name,omitempty"`
	GivenName         *string              `json:"given_name,omitempty"`
	AdditionalNames   *string              `json:"additional_names,omitempty"`
	HonorificPrefixes *string              `json:"honorific_prefixes,omitempty"`
	HonorificSuffixes *string              `json:"honorific_suffixes,omitempty"`
	SecondarySurname  *string              `json:"secondary_surname,omitempty"`
	Generation        *string              `json:"generation,omitempty"`
	Language          *string              `json:"language,omitempty"`
	Script            *string              `json:"script,omitempty"`
	PhoneticSystem    *string              `json:"phonetic_system,omitempty"`
	PhoneticScript    *string              `json:"phonetic_script,omitempty"`
	SortAs            *string              `json:"sort_as,omitempty"`
	IsDerived         bool                 `json:"is_derived,omitempty"`
	OriginalValue     string               `json:"original_value,omitempty"`
	Envelope          ValueEnvelopeInput   `json:"envelope"`
}

type PersonContactPointPatchRequest struct {
	Add       []PersonContactPointInputRequest `json:"add,omitempty"`
	Supersede []int64                          `json:"supersede,omitempty"`
}

type PersonContactPointInputRequest struct {
	AddressKind   store.ContactAddressKind `json:"address_kind"`
	ServiceSlug   *string                  `json:"service_slug,omitempty"`
	ScopeKind     *string                  `json:"scope_kind,omitempty"`
	ScopeValue    *string                  `json:"scope_value,omitempty"`
	OriginalValue string                   `json:"original_value"`
	URI           *string                  `json:"uri,omitempty"`
	Envelope      ValueEnvelopeInput       `json:"envelope"`
}

type PersonAddressPatchRequest struct {
	Add       []PersonAddressInputRequest `json:"add,omitempty"`
	Supersede []int64                     `json:"supersede,omitempty"`
}

type PersonAddressInputRequest struct {
	AddressKind        store.PersonAddressKind `json:"address_kind"`
	PostOfficeBox      *string                 `json:"post_office_box,omitempty"`
	ExtendedAddress    *string                 `json:"extended_address,omitempty"`
	StreetAddress      *string                 `json:"street_address,omitempty"`
	Locality           *string                 `json:"locality,omitempty"`
	Region             *string                 `json:"region,omitempty"`
	PostalCode         *string                 `json:"postal_code,omitempty"`
	CountryName        *string                 `json:"country_name,omitempty"`
	ExtendedComponents *string                 `json:"extended_components,omitempty"`
	FreeText           *string                 `json:"free_text,omitempty"`
	Label              *string                 `json:"label,omitempty"`
	GeoURI             *string                 `json:"geo_uri,omitempty"`
	Timezone           *string                 `json:"timezone,omitempty"`
	CountryCode        *string                 `json:"country_code,omitempty"`
	PlaceURI           *string                 `json:"place_uri,omitempty"`
	OriginalValue      string                  `json:"original_value,omitempty"`
	Envelope           ValueEnvelopeInput      `json:"envelope"`
}

type PersonDatePatchRequest struct {
	Add       []PersonDateInputRequest `json:"add,omitempty"`
	Supersede []int64                  `json:"supersede,omitempty"`
}

type PersonDateInputRequest struct {
	DateKind      store.PersonDateKind `json:"date_kind"`
	Label         *string              `json:"label,omitempty"`
	Date          store.PartialDate    `json:"date,omitzero"`
	DateText      *string              `json:"date_text,omitempty"`
	CalendarScale *string              `json:"calendar_scale,omitempty"`
	OriginalValue string               `json:"original_value,omitempty"`
	Envelope      ValueEnvelopeInput   `json:"envelope"`
}

type PersonCategoryPatchRequest struct {
	Add       []PersonCategoryInputRequest `json:"add,omitempty"`
	Supersede []int64                      `json:"supersede,omitempty"`
}

type PersonCategoryInputRequest struct {
	OriginalValue string             `json:"original_value"`
	Envelope      ValueEnvelopeInput `json:"envelope"`
}

type PersonMediaPatchRequest struct {
	Add       []PersonMediaInputRequest `json:"add,omitempty"`
	Supersede []int64                   `json:"supersede,omitempty"`
}

type PersonMediaInputRequest struct {
	MediaKind     store.PersonMediaKind `json:"media_kind"`
	MediaType     *string               `json:"media_type,omitempty"`
	URI           *string               `json:"uri,omitempty"`
	Data          []byte                `json:"data,omitempty"`
	OriginalValue string                `json:"original_value,omitempty"`
	Envelope      ValueEnvelopeInput    `json:"envelope"`
}

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
	patch.RequestBody = jsonRequestBodyFor[PersonProfilePatchRequest](api)
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

	mediaContent := rawAPIV1Operation(
		"getPersonProfileMediaContent", http.MethodGet,
		"/persons/{id}/profile/media/{media_id}/content",
		"Download stored inline content for one person profile media value",
	)
	mediaContent.Description = "Returns the exact inline bytes stored for one media value. " +
		"URI-only values have no local content and return 404."
	addPersonIDParameter(&mediaContent)
	mediaContent.Parameters = append(mediaContent.Parameters, pathNamedIntegerParam(
		"media_id", "Structured person profile media value ID",
	))
	mediaContent.Responses = binaryResponsesFor(api, "*/*",
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	)
	registerRawHumaRoute(api, mediaContent, s.handleGetPersonProfileMediaContent)
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
	patch, ok := decodeProfilePatchRequest(w, r)
	if !ok {
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

func (s *Server) handleGetPersonProfileMediaContent(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileValueStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	mediaID, err := strconv.ParseInt(r.PathValue("media_id"), 10, 64)
	if err != nil || mediaID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_profile_media_id",
			"Person profile media ID must be a positive integer")
		return
	}
	data, mediaType, err := profiles.ReadPersonMediaDataContext(
		r.Context(), personID, mediaID,
	)
	if err != nil {
		switch {
		case s.writeIfContextError(w, err):
			return
		case errors.Is(err, store.ErrProfileValueNotFound):
			writeError(w, http.StatusNotFound, "profile_media_not_found",
				"Person profile media value not found")
		case errors.Is(err, store.ErrPersonMediaNoData):
			writeError(w, http.StatusNotFound, "profile_media_content_unavailable",
				"Person profile media content is not available")
		default:
			s.logger.Error("person profile media read failed", "error", err,
				"person_id", personID, "media_id", mediaID)
			writeError(w, http.StatusInternalServerError, "person_profile_media_failed",
				"Person profile media content could not be read")
		}
		return
	}
	mediaType = strings.TrimSpace(mediaType)
	if _, _, parseErr := mime.ParseMediaType(mediaType); mediaType == "" || parseErr != nil {
		mediaType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(data); err != nil {
		s.logger.Error("person profile media write failed", "error", err,
			"person_id", personID, "media_id", mediaID)
	}
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
		store.ErrInvalidProfilePref, store.ErrInvalidProfileOrdinal, store.ErrInvalidPartialDate,
		store.ErrInvalidPersonNameKind, store.ErrPersonNameValueMissing,
		store.ErrInvalidContactAddressKind, store.ErrContactPointValueMissing,
		store.ErrInvalidPersonAddressKind, store.ErrPersonAddressValueMissing,
		store.ErrInvalidPersonDateKind, store.ErrPersonDateValueMissing,
		store.ErrPersonCategoryEmpty, store.ErrInvalidPersonMediaKind,
		store.ErrPersonMediaEmpty, store.ErrPersonMediaTooLarge,
		store.ErrServiceNotFound, store.ErrServiceScopeRequired,
		store.ErrServiceScopeForbidden, store.ErrServiceScopeIncomplete,
		store.ErrNormalizationRejected,
		store.ErrPersonProfilePatchEmpty, store.ErrProfileValueCloseBeforeActive,
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
	w.Header().Set(etagHeaderName, personETag(profile.Person))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, profile)
}

// A patch may contain one 8 MiB inline media value. Base64 expansion plus
// surrounding JSON fits under 12 MiB without raising the shared person cap.
func decodeProfilePatchRequest(
	w http.ResponseWriter, r *http.Request,
) (store.PersonProfilePatch, bool) {
	var patch store.PersonProfilePatch
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPersonProfilePatchBytes))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "profile_patch_too_large",
				"Person profile patch is too large")
			return patch, false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person profile patch")
		return patch, false
	}
	var request PersonProfilePatchRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person profile patch: "+err.Error())
		return patch, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Person profile patch must contain one JSON object")
		return patch, false
	}
	// The request DTO is the OpenAPI allowlist. Transcoding that validated
	// subset keeps runtime acceptance identical to the generated contract
	// while the store retains its response-oriented envelope model.
	encoded, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person profile patch")
		return patch, false
	}
	if err := json.Unmarshal(encoded, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person profile patch")
		return patch, false
	}
	return patch, true
}
