package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	organizationsPath = "/api/v1/organizations"
	// MaxOrganizationProfileRequestBytes allows one 8 MiB inline media value.
	// The base64 representation plus JSON framing fits under this dedicated cap.
	MaxOrganizationProfileRequestBytes = 12 << 20
)

// OrganizationStore is the feature-local capability for curated organizations.
type OrganizationStore interface {
	CreateOrganizationContext(ctx context.Context, input store.OrganizationInput) (*store.Organization, error)
	GetOrganizationContext(ctx context.Context, id int64) (*store.Organization, error)
	ListOrganizationsContext(ctx context.Context, filter store.OrganizationFilter) ([]store.Organization, error)
	CountOrganizationsContext(ctx context.Context, filter store.OrganizationFilter) (int64, error)
	ReplaceOrganizationContext(ctx context.Context, id, expectedRevision int64, input store.OrganizationInput, retired bool) (*store.Organization, error)
	DeleteOrganizationContext(ctx context.Context, id, expectedRevision int64) error
	MergeOrganizationsContext(ctx context.Context, survivorID, survivorRevision, losingID, losingRevision int64) (*store.Organization, error)
	GetOrganizationProfileContext(ctx context.Context, id int64, includeSuperseded bool) (*store.OrganizationProfile, error)
	ReplaceOrganizationProfileContext(ctx context.Context, id, expectedRevision int64, input store.OrganizationProfileInput) (*store.OrganizationProfile, error)
	ListOrganizationAttributeValuesContext(ctx context.Context, organizationID int64, query store.OrganizationAttributeQuery) ([]store.OrganizationAttributeValue, error)
	SetOrganizationAttributeValueContext(ctx context.Context, input store.OrganizationAttributeValueInput) (*store.OrganizationAttributeWrite, error)
	SupersedeOrganizationAttributeValueContext(ctx context.Context, input store.OrganizationAttributeSupersedeInput) (*store.OrganizationAttributeWrite, error)
	ReadOrganizationMediaDataContext(ctx context.Context, organizationID, mediaID int64) ([]byte, string, error)
}

// OrganizationBody is a full replacement of the mutable organization fields.
// Retired is a pointer so a PATCH that omits it preserves the stored lifecycle
// state instead of silently reactivating a retired organization.
type OrganizationBody struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind,omitempty" enum:"company,nonprofit,school,government,household,other"`
	PrimaryDomain *string `json:"primary_domain,omitempty" nullable:"true"`
	Description   *string `json:"description,omitempty" nullable:"true"`
	Retired       *bool   `json:"retired,omitempty" nullable:"true"`
}

type OrganizationCreateBody struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind,omitempty" enum:"company,nonprofit,school,government,household,other"`
	PrimaryDomain *string `json:"primary_domain,omitempty" nullable:"true"`
	Description   *string `json:"description,omitempty" nullable:"true"`
}

type OrganizationsResponse struct {
	Organizations []store.Organization `json:"organizations"`
	Total         int64                `json:"total"`
	Limit         int                  `json:"limit"`
	Offset        int                  `json:"offset"`
}

type MergeOrganizationBody struct {
	LosingOrganizationID int64 `json:"losing_organization_id"`
	LosingRevision       int64 `json:"losing_revision"`
}

// OrganizationEnvelopeBody is deliberately embedded in the child bodies so
// JSON remains flat while only client-writable envelope fields are accepted.
// It must cover every field organizationEnvelopeMatches compares, or a
// profile PUT could never round-trip a row carrying that metadata and would
// supersede and reinsert it without the metadata on every unrelated update.
type OrganizationEnvelopeBody struct {
	Ordinal           *int       `json:"ordinal,omitempty" minimum:"0"`
	Pref              *int       `json:"pref,omitempty" nullable:"true"`
	TypeLabel         *string    `json:"type_label,omitempty" nullable:"true"`
	TypeTokens        []string   `json:"type_tokens,omitempty"`
	VCardProperty     *string    `json:"vcard_property,omitempty" nullable:"true"`
	VCardGroup        *string    `json:"vcard_group,omitempty" nullable:"true"`
	VCardPropID       *string    `json:"vcard_prop_id,omitempty" nullable:"true"`
	VCardPID          []string   `json:"vcard_pid,omitempty"`
	VCardAltID        *string    `json:"vcard_altid,omitempty" nullable:"true"`
	Source            string     `json:"source" enum:"user,carddav_import,vcard_import,archive_observation,extraction,enrichment,system"`
	SourceRef         *string    `json:"source_ref,omitempty" nullable:"true"`
	SourceResourceUID *string    `json:"source_resource_uid,omitempty" nullable:"true"`
	Confidence        *float64   `json:"confidence,omitempty" nullable:"true"`
	ActiveFrom        *time.Time `json:"active_from,omitempty" nullable:"true"`
}

func (b OrganizationEnvelopeBody) envelope() store.ValueEnvelopeInput {
	property := ""
	if b.VCardProperty != nil {
		property = *b.VCardProperty
	}
	return store.ValueEnvelopeInput{
		Ordinal: b.Ordinal, Pref: b.Pref,
		TypeLabel: b.TypeLabel, TypeTokens: b.TypeTokens,
		VCard:  store.VCardIdentity{Property: property, Group: b.VCardGroup, PropID: b.VCardPropID, PID: b.VCardPID, AltID: b.VCardAltID},
		Source: store.Provenance(b.Source), SourceRef: b.SourceRef, SourceResourceUID: b.SourceResourceUID,
		Confidence: b.Confidence, ActiveFrom: b.ActiveFrom,
	}
}

type OrganizationNameBody struct {
	OrganizationEnvelopeBody

	Name     string `json:"name"`
	NameKind string `json:"name_kind" enum:"alias,legal,former,abbreviation,sort"`
}

type OrganizationIdentifierBody struct {
	OrganizationEnvelopeBody

	IdentifierKind string `json:"identifier_kind" enum:"domain,linkedin,duns,tax_id,registry,other"`
	Value          string `json:"identifier_value"`
}

type OrganizationAddressBody struct {
	OrganizationEnvelopeBody

	AddressKind        string  `json:"address_kind"`
	PostOfficeBox      *string `json:"post_office_box,omitempty" nullable:"true"`
	ExtendedAddress    *string `json:"extended_address,omitempty" nullable:"true"`
	StreetAddress      *string `json:"street_address,omitempty" nullable:"true"`
	Locality           *string `json:"locality,omitempty" nullable:"true"`
	Region             *string `json:"region,omitempty" nullable:"true"`
	PostalCode         *string `json:"postal_code,omitempty" nullable:"true"`
	CountryName        *string `json:"country_name,omitempty" nullable:"true"`
	ExtendedComponents *string `json:"extended_components,omitempty" nullable:"true"`
	FreeText           *string `json:"free_text,omitempty" nullable:"true"`
	Label              *string `json:"label,omitempty" nullable:"true"`
	GeoURI             *string `json:"geo_uri,omitempty" nullable:"true"`
	Timezone           *string `json:"timezone,omitempty" nullable:"true"`
	CountryCode        *string `json:"country_code,omitempty" nullable:"true"`
	PlaceURI           *string `json:"place_uri,omitempty" nullable:"true"`
	OriginalValue      string  `json:"original_value,omitempty"`
}

type OrganizationContactPointBody struct {
	OrganizationEnvelopeBody

	ContactKind   string  `json:"contact_kind" enum:"email,phone,username,impp,url,social,calendar,contact_uri,org_directory,language"`
	ServiceSlug   *string `json:"service_slug,omitempty" nullable:"true"`
	ScopeKind     *string `json:"scope_kind,omitempty" nullable:"true"`
	ScopeValue    *string `json:"scope_value,omitempty" nullable:"true"`
	OriginalValue string  `json:"original_value"`
	URI           *string `json:"uri,omitempty" nullable:"true"`
}

// OrganizationMediaBody carries either inline data or a content_hash
// retention identifier: reads expose media metadata without the bytes, so a
// GET-derived PUT re-sends content_hash to keep the stored inline content.
type OrganizationMediaBody struct {
	OrganizationEnvelopeBody

	MediaKind     string  `json:"media_kind" enum:"photo,logo,sound,key"`
	MediaType     *string `json:"media_type,omitempty" nullable:"true"`
	URI           *string `json:"uri,omitempty" nullable:"true"`
	Data          []byte  `json:"data,omitempty"`
	ContentHash   *string `json:"content_hash,omitempty" nullable:"true"`
	OriginalValue string  `json:"original_value,omitempty"`
}

type OrganizationCategoryBody struct {
	OrganizationEnvelopeBody

	Category string `json:"category"`
}

type OrganizationProfileBody struct {
	Names         []OrganizationNameBody         `json:"names,omitempty"`
	Identifiers   []OrganizationIdentifierBody   `json:"identifiers,omitempty"`
	Addresses     []OrganizationAddressBody      `json:"addresses,omitempty"`
	ContactPoints []OrganizationContactPointBody `json:"contact_points,omitempty"`
	Media         []OrganizationMediaBody        `json:"media,omitempty"`
	Categories    []OrganizationCategoryBody     `json:"categories,omitempty"`
}

type OrganizationAttributesResponse struct {
	Values []store.OrganizationAttributeValue `json:"values"`
}

type SetOrganizationAttributeBody struct {
	DefinitionSlug  string               `json:"definition_slug"`
	Ordinal         *int64               `json:"ordinal,omitempty" nullable:"true"`
	Value           store.AttributeValue `json:"value"`
	ActiveFrom      *time.Time           `json:"active_from,omitempty" nullable:"true"`
	ActiveUntil     *time.Time           `json:"active_until,omitempty" nullable:"true"`
	Source          string               `json:"source" enum:"user,carddav_import,vcard_import,archive_observation,extraction,enrichment,system"`
	SourceRef       *string              `json:"source_ref,omitempty" nullable:"true"`
	Confidence      *float64             `json:"confidence,omitempty" nullable:"true"`
	Actor           *string              `json:"actor,omitempty" nullable:"true"`
	ExpectedValueID *int64               `json:"expected_value_id,omitempty" nullable:"true"`
	DryRun          bool                 `json:"dry_run,omitempty"`
}

func (s *Server) registerOrganizationRoutes(api huma.API) {
	list := rawAPIV1Operation("listOrganizations", http.MethodGet, "/organizations", "List organizations")
	list.Parameters = append(list.Parameters, queryIntegerParam("limit", "Maximum results"), queryIntegerParam("offset", "Results to skip"), queryBooleanParam("include_retired", "Include retired organizations"), queryStringParam("q", "Normalized-name search", false))
	list.Responses = jsonResponsesFor[OrganizationsResponse](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListOrganizations)

	create := rawAPIV1Operation("createOrganization", http.MethodPost, "/organizations", "Create an organization")
	create.RequestBody = jsonRequestBodyFor[OrganizationCreateBody](api)
	create.Responses = jsonResponsesFor[store.Organization](api, http.StatusCreated)
	addOrganizationETagHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addOrganizationLocationHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, create.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreateOrganization)

	get := rawAPIV1Operation("getOrganization", http.MethodGet, "/organizations/{id}", "Get an organization")
	addOrganizationIDParameter(&get)
	get.Responses = jsonResponsesFor[store.OrganizationProfile](api)
	addOrganizationETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetOrganization)

	patch := rawAPIV1Operation("patchOrganization", http.MethodPatch, "/organizations/{id}", "Replace an organization's mutable fields")
	addOrganizationIDParameter(&patch)
	addOrganizationIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[OrganizationBody](api)
	patch.Responses = jsonResponsesFor[store.Organization](api)
	addOrganizationETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchOrganization)

	remove := rawAPIV1Operation("deleteOrganization", http.MethodDelete, "/organizations/{id}", "Delete an organization without employment records")
	addOrganizationIDParameter(&remove)
	addOrganizationIfMatchParameter(&remove)
	remove.Responses = rawHumaResponses(http.StatusNoContent)
	remove.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, remove.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, remove, s.handleDeleteOrganization)

	merge := rawAPIV1Operation("mergeOrganization", http.MethodPost, "/organizations/{id}/merge", "Merge another organization into this organization")
	addOrganizationIDParameter(&merge)
	addOrganizationIfMatchParameter(&merge)
	merge.RequestBody = jsonRequestBodyFor[MergeOrganizationBody](api)
	merge.Responses = jsonResponsesFor[store.Organization](api)
	addOrganizationETagHeader(merge.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, merge.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, merge, s.handleMergeOrganization)

	history := rawAPIV1Operation("getOrganizationHistory", http.MethodGet, "/organizations/{id}/history", "Get organization profile history")
	addOrganizationIDParameter(&history)
	history.Responses = jsonResponsesFor[store.OrganizationProfile](api)
	addOrganizationETagHeader(history.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, history.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, history, s.handleGetOrganizationHistory)

	profile := rawAPIV1Operation(
		"putOrganizationProfile", http.MethodPut, "/organizations/{id}/profile",
		"Replace organization profile collections",
	)
	profile.Description = fmt.Sprintf(
		"Replaces all structured organization profile collections with at most %d total values.",
		store.MaxOrganizationProfileValues,
	)
	addOrganizationIDParameter(&profile)
	addOrganizationIfMatchParameter(&profile)
	profile.RequestBody = jsonRequestBodyFor[OrganizationProfileBody](api)
	profile.Responses = jsonResponsesFor[store.OrganizationProfile](api)
	addOrganizationETagHeader(profile.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, profile.Responses,
		http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionRequired, http.StatusRequestEntityTooLarge,
		http.StatusServiceUnavailable,
	)
	registerRawHumaRoute(api, profile, s.handlePutOrganizationProfile)

	attributes := rawAPIV1Operation("listOrganizationAttributes", http.MethodGet, "/organizations/{id}/attributes", "List organization typed attributes")
	addOrganizationIDParameter(&attributes)
	attributes.Parameters = append(attributes.Parameters, queryBooleanParam("include_superseded", "Include superseded values"), queryStringParam("definition_slug", "Restrict to one definition", false))
	attributes.Responses = jsonResponsesFor[OrganizationAttributesResponse](api)
	addErrorResponses(api, attributes.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, attributes, s.handleListOrganizationAttributes)

	setAttribute := rawAPIV1Operation("setOrganizationAttribute", http.MethodPost, "/organizations/{id}/attributes", "Set an organization typed attribute")
	addOrganizationIDParameter(&setAttribute)
	setAttribute.RequestBody = jsonRequestBodyFor[SetOrganizationAttributeBody](api)
	setAttribute.Responses = jsonResponsesFor[store.OrganizationAttributeWrite](api, http.StatusOK, http.StatusCreated)
	addErrorResponses(api, setAttribute.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, setAttribute, s.handleSetOrganizationAttribute)

	clearAttribute := rawAPIV1Operation("clearOrganizationAttribute", http.MethodDelete, "/organizations/{id}/attributes/{slug}", "Supersede an organization typed attribute")
	addOrganizationIDParameter(&clearAttribute)
	addAttributeSlugParameter(&clearAttribute)
	clearAttribute.Parameters = append(clearAttribute.Parameters,
		queryIntegerParam("ordinal", "Ordinal for a multi-valued definition"),
		queryIntegerParam("expected_value_id", "Compare-and-swap: the current value ID expected to be superseded"),
		queryBooleanParam("dry_run", "Validate and preview without writing"))
	clearAttribute.Responses = jsonResponsesFor[store.OrganizationAttributeWrite](api)
	addErrorResponses(api, clearAttribute.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, clearAttribute, s.handleClearOrganizationAttribute)

	mediaContent := rawAPIV1Operation(
		"getOrganizationProfileMediaContent", http.MethodGet,
		"/organizations/{id}/profile/media/{media_id}/content",
		"Download stored inline content for one organization profile media value",
	)
	mediaContent.Description = "Returns the exact inline bytes stored for one media value. " +
		"URI-only values have no local content and return 404."
	addOrganizationIDParameter(&mediaContent)
	mediaContent.Parameters = append(mediaContent.Parameters, pathNamedIntegerParam(
		"media_id", "Structured organization profile media value ID",
	))
	mediaContent.Responses = binaryResponsesFor(api, "*/*",
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	)
	registerRawHumaRoute(api, mediaContent, s.handleGetOrganizationProfileMediaContent)
}

func (s *Server) handleGetOrganizationProfileMediaContent(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	mediaID, err := strconv.ParseInt(r.PathValue("media_id"), 10, 64)
	if err != nil || mediaID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_profile_media_id",
			"Organization profile media ID must be a positive integer")
		return
	}
	data, mediaType, err := organizations.ReadOrganizationMediaDataContext(r.Context(), id, mediaID)
	if err != nil {
		switch {
		case s.writeIfContextError(w, err):
			return
		case errors.Is(err, store.ErrProfileValueNotFound):
			writeError(w, http.StatusNotFound, "profile_media_not_found",
				"Organization profile media value not found")
		case errors.Is(err, store.ErrPersonMediaNoData):
			writeError(w, http.StatusNotFound, "profile_media_content_unavailable",
				"Organization profile media content is not available")
		default:
			s.logger.Error("organization profile media read failed", "error", err,
				"organization_id", id, "media_id", mediaID)
			writeError(w, http.StatusInternalServerError, "organization_profile_media_failed",
				"Organization profile media content could not be read")
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
		s.logger.Error("organization profile media write failed", "error", err,
			"organization_id", id, "media_id", mediaID)
	}
}

func (s *Server) handleListOrganizations(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	filter, ok := s.organizationFilter(w, r)
	if !ok {
		return
	}
	total, err := organizations.CountOrganizationsContext(r.Context(), filter)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	rows, err := organizations.ListOrganizationsContext(r.Context(), filter)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, OrganizationsResponse{Organizations: rows, Total: total, Limit: filter.Limit, Offset: filter.Offset})
}

func (s *Server) organizationFilter(w http.ResponseWriter, r *http.Request) (store.OrganizationFilter, bool) {
	filter := store.OrganizationFilter{Limit: store.DefaultOrganizationPageSize}
	limit, present, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return filter, false
	}
	if present {
		if limit < 0 {
			s.rejectBadParam(w, newParamError("limit", "limit must not be negative"))
			return filter, false
		}
		if limit > 0 {
			filter.Limit = min(limit, store.MaxOrganizationPageSize)
		}
	}
	offset, present, err := queryInt(r, "offset")
	if err != nil {
		s.rejectBadParam(w, err)
		return filter, false
	}
	if present {
		if offset < 0 {
			s.rejectBadParam(w, newParamError("offset", "offset must not be negative"))
			return filter, false
		}
		filter.Offset = offset
	}
	includeRetired, _, err := queryBool(r, "include_retired")
	if err != nil {
		s.rejectBadParam(w, err)
		return filter, false
	}
	filter.IncludeRetired = includeRetired
	filter.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	return filter, true
}

func (s *Server) handleCreateOrganization(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	var body OrganizationCreateBody
	if !decodeEntityRequest(w, r, &body, "organization") {
		return
	}
	organization, err := organizations.CreateOrganizationContext(
		r.Context(), organizationCreateInput(body))
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	writeOrganization(w, http.StatusCreated, organization)
}

func (s *Server) handleGetOrganization(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	profile, err := organizations.GetOrganizationProfileContext(r.Context(), id, false)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	writeOrganizationProfile(w, profile)
}

func (s *Server) handlePatchOrganization(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	revision, ok := organizationIfMatch(w, r, id)
	if !ok {
		return
	}
	var body OrganizationBody
	if !decodeEntityRequest(w, r, &body, "organization") {
		return
	}
	var retired bool
	if body.Retired != nil {
		retired = *body.Retired
	} else {
		current, err := organizations.GetOrganizationContext(r.Context(), id)
		if err != nil {
			s.writeOrganizationError(w, err)
			return
		}
		retired = current.RetiredAt != nil
	}
	organization, err := organizations.ReplaceOrganizationContext(
		r.Context(), id, revision, organizationInput(body), retired)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	writeOrganization(w, http.StatusOK, organization)
}

func (s *Server) handleDeleteOrganization(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	revision, ok := organizationIfMatch(w, r, id)
	if !ok {
		return
	}
	if err := organizations.DeleteOrganizationContext(r.Context(), id, revision); err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMergeOrganization(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	revision, ok := organizationIfMatch(w, r, id)
	if !ok {
		return
	}
	var body MergeOrganizationBody
	if !decodeEntityRequest(w, r, &body, "organization") {
		return
	}
	if body.LosingOrganizationID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_organization", "losing_organization_id must be a positive integer")
		return
	}
	organization, err := organizations.MergeOrganizationsContext(r.Context(), id, revision, body.LosingOrganizationID, body.LosingRevision)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	writeOrganization(w, http.StatusOK, organization)
}

func (s *Server) handleGetOrganizationHistory(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	profile, err := organizations.GetOrganizationProfileContext(r.Context(), id, true)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	writeOrganizationProfile(w, profile)
}

func (s *Server) handlePutOrganizationProfile(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	revision, ok := organizationIfMatch(w, r, id)
	if !ok {
		return
	}
	var body OrganizationProfileBody
	if !decodeOrganizationProfileRequest(w, r, &body) {
		return
	}
	input, err := organizationProfileInput(body)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	profile, err := organizations.ReplaceOrganizationProfileContext(r.Context(), id, revision, input)
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	writeOrganizationProfile(w, profile)
}

func (s *Server) handleListOrganizationAttributes(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	includeSuperseded, _, err := queryBool(r, "include_superseded")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if _, err := organizations.GetOrganizationContext(r.Context(), id); err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	values, err := organizations.ListOrganizationAttributeValuesContext(r.Context(), id, store.OrganizationAttributeQuery{DefinitionSlug: strings.TrimSpace(r.URL.Query().Get("definition_slug")), IncludeHistory: includeSuperseded})
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, OrganizationAttributesResponse{Values: values})
}

func (s *Server) handleSetOrganizationAttribute(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, ok := organizationID(w, r)
	if !ok {
		return
	}
	var body SetOrganizationAttributeBody
	if !decodeEntityRequest(w, r, &body, "organization") {
		return
	}
	result, err := organizations.SetOrganizationAttributeValueContext(r.Context(), store.OrganizationAttributeValueInput{OrganizationID: id, DefinitionSlug: body.DefinitionSlug, Ordinal: body.Ordinal, Value: body.Value, ActiveFrom: body.ActiveFrom, ActiveUntil: body.ActiveUntil, Source: store.Provenance(body.Source), SourceRef: body.SourceRef, Confidence: body.Confidence, Actor: body.Actor, ExpectedValueID: body.ExpectedValueID, DryRun: body.DryRun})
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	status := http.StatusCreated
	if body.DryRun {
		status = http.StatusOK
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, result)
}

func (s *Server) handleClearOrganizationAttribute(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	id, slug, ok := organizationAttributeTarget(w, r)
	if !ok {
		return
	}
	dryRun, _, err := queryBool(r, "dry_run")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	expectedValueID, hasExpectedValueID, err := queryInt64(r, "expected_value_id")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if hasExpectedValueID && expectedValueID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_expected_value_id",
			"expected_value_id must be a positive integer")
		return
	}
	var expectedValueIDPtr *int64
	if hasExpectedValueID {
		expectedValueIDPtr = &expectedValueID
	}
	var ordinal *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("ordinal")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_ordinal",
				"ordinal must be a non-negative integer")
			return
		}
		ordinal = &parsed
	}
	write, err := organizations.SupersedeOrganizationAttributeValueContext(r.Context(), store.OrganizationAttributeSupersedeInput{
		OrganizationID: id, DefinitionSlug: slug, Ordinal: ordinal,
		ExpectedValueID: expectedValueIDPtr, DryRun: dryRun,
	})
	if err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, write)
}

func organizationInput(body OrganizationBody) store.OrganizationInput {
	return store.OrganizationInput{Name: body.Name, Kind: store.OrganizationKind(body.Kind), PrimaryDomain: body.PrimaryDomain, Description: body.Description}
}

func organizationAttributeTarget(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	id, ok := organizationID(w, r)
	if !ok {
		return 0, "", false
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if err := store.ValidateAttributeSlug(slug); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attribute_slug", err.Error())
		return 0, "", false
	}
	return id, slug, true
}

func decodeOrganizationProfileRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxOrganizationProfileRequestBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "organization_profile_too_large",
				"Organization profile request is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid organization profile request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Invalid organization profile request: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Organization profile request must contain one JSON object")
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid organization profile request")
		return false
	}
	return true
}

func organizationCreateInput(body OrganizationCreateBody) store.OrganizationInput {
	return store.OrganizationInput{
		Name: body.Name, Kind: store.OrganizationKind(body.Kind),
		PrimaryDomain: body.PrimaryDomain, Description: body.Description,
	}
}

func organizationProfileInput(body OrganizationProfileBody) (store.OrganizationProfileInput, error) {
	input := store.OrganizationProfileInput{Names: make([]store.OrganizationNameInput, 0, len(body.Names)), Identifiers: make([]store.OrganizationIdentifierInput, 0, len(body.Identifiers)), Addresses: make([]store.OrganizationAddressInput, 0, len(body.Addresses)), ContactPoints: make([]store.OrganizationContactPointInput, 0, len(body.ContactPoints)), Media: make([]store.OrganizationMediaInput, 0, len(body.Media)), Categories: make([]store.OrganizationCategoryInput, 0, len(body.Categories))}
	for _, row := range body.Names {
		input.Names = append(input.Names, store.OrganizationNameInput{Name: row.Name, NameKind: row.NameKind, Envelope: row.envelope()})
	}
	for i, row := range body.Identifiers {
		if !organizationIdentifierKindValid(row.IdentifierKind) {
			return store.OrganizationProfileInput{}, fmt.Errorf(
				"%w: identifiers[%d].identifier_kind %q is unknown",
				store.ErrOrganizationInvalid, i, row.IdentifierKind)
		}
		input.Identifiers = append(input.Identifiers, store.OrganizationIdentifierInput{IdentifierKind: row.IdentifierKind, Value: row.Value, Envelope: row.envelope()})
	}
	for _, row := range body.Addresses {
		input.Addresses = append(input.Addresses, store.OrganizationAddressInput{AddressKind: store.PersonAddressKind(row.AddressKind), PostOfficeBox: row.PostOfficeBox, ExtendedAddress: row.ExtendedAddress, StreetAddress: row.StreetAddress, Locality: row.Locality, Region: row.Region, PostalCode: row.PostalCode, CountryName: row.CountryName, ExtendedComponents: row.ExtendedComponents, FreeText: row.FreeText, Label: row.Label, GeoURI: row.GeoURI, Timezone: row.Timezone, CountryCode: row.CountryCode, PlaceURI: row.PlaceURI, OriginalValue: row.OriginalValue, Envelope: row.envelope()})
	}
	for _, row := range body.ContactPoints {
		input.ContactPoints = append(input.ContactPoints, store.OrganizationContactPointInput{AddressKind: store.ContactAddressKind(row.ContactKind), ServiceSlug: row.ServiceSlug, ScopeKind: row.ScopeKind, ScopeValue: row.ScopeValue, OriginalValue: row.OriginalValue, URI: row.URI, Envelope: row.envelope()})
	}
	for _, row := range body.Media {
		input.Media = append(input.Media, store.OrganizationMediaInput{MediaKind: store.PersonMediaKind(row.MediaKind), MediaType: row.MediaType, URI: row.URI, Data: row.Data, ContentHash: row.ContentHash, OriginalValue: row.OriginalValue, Envelope: row.envelope()})
	}
	for _, row := range body.Categories {
		input.Categories = append(input.Categories, store.OrganizationCategoryInput{Category: row.Category, Envelope: row.envelope()})
	}
	return input, nil
}

func organizationIdentifierKindValid(kind string) bool {
	switch kind {
	case store.OrganizationIdentifierKindDomain, store.OrganizationIdentifierKindLinkedIn,
		store.OrganizationIdentifierKindDUNS, store.OrganizationIdentifierKindTaxID,
		store.OrganizationIdentifierKindRegistry, store.OrganizationIdentifierKindOther:
		return true
	default:
		return false
	}
}

func (s *Server) organizationStore(w http.ResponseWriter) (OrganizationStore, bool) {
	organizations, ok := s.store.(OrganizationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "organizations_unavailable", "Organizations are unavailable")
	}
	return organizations, ok
}

func (s *Server) writeOrganizationError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrOrganizationNotFound):
		writeError(w, http.StatusNotFound, "organization_not_found", "Organization not found")
	case errors.Is(err, store.ErrOrganizationRevisionConflict):
		writeError(w, http.StatusConflict, "organization_revision_conflict", "Organization changed; reload and retry")
	case errors.Is(err, store.ErrOrganizationHasEmployments):
		writeError(w, http.StatusConflict, "organization_has_employments", "Organization still has employment records; retire it instead of deleting it")
	case errors.Is(err, store.ErrOrganizationMergeConflict):
		writeError(w, http.StatusConflict, "organization_merge_conflict", "Organization merge conflicts with existing employment records")
	case errors.Is(err, store.ErrOrganizationProfileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge,
			"organization_profile_too_large", err.Error())
	case errors.Is(err, store.ErrOrganizationInvalid):
		writeError(w, http.StatusBadRequest, "invalid_organization", err.Error())
	case isOrganizationProfileValidationError(err):
		writeError(w, http.StatusBadRequest, "invalid_organization", err.Error())
	case errors.Is(err, store.ErrInvalidProvenance):
		writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
	case errors.Is(err, store.ErrAttributeObjectTypeMismatch):
		writeError(w, http.StatusBadRequest, "invalid_attribute_definition", err.Error())
	case errors.Is(err, store.ErrAttributeDefinitionNotFound):
		writeError(w, http.StatusNotFound, "attribute_definition_not_found", "Attribute definition not found")
	case errors.Is(err, store.ErrAttributeDefinitionInactive):
		writeError(w, http.StatusConflict, "attribute_definition_inactive", err.Error())
	case errors.Is(err, store.ErrAttributeDefinitionNotWritable):
		writeError(w, http.StatusConflict, "attribute_definition_not_writable", err.Error())
	case errors.Is(err, store.ErrAttributeValueInvalid):
		writeError(w, http.StatusBadRequest, "invalid_attribute_value", err.Error())
	case errors.Is(err, store.ErrAttributeValueNotFound):
		writeError(w, http.StatusNotFound, "attribute_value_not_found", "Attribute value not found")
	case errors.Is(err, store.ErrAttributeValueConflict):
		writeError(w, http.StatusConflict, "attribute_value_conflict", "Attribute value changed; reload and retry")
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusBadRequest, "invalid_person_id", "Person ID is invalid")
	default:
		s.logger.Error("organization operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "organization_failed", "Organization operation failed")
	}
}

func isOrganizationProfileValidationError(err error) bool {
	for _, target := range []error{
		store.ErrConfidenceScope, store.ErrInvalidProfilePref,
		store.ErrInvalidProfileOrdinal, store.ErrServiceScopeIncomplete,
		store.ErrInvalidContactAddressKind, store.ErrContactPointValueMissing,
		store.ErrInvalidPersonAddressKind, store.ErrPersonAddressValueMissing,
		store.ErrInvalidPersonMediaKind, store.ErrPersonMediaEmpty,
		store.ErrPersonMediaTooLarge, store.ErrServiceNotFound,
		store.ErrServiceScopeRequired, store.ErrServiceScopeForbidden,
		store.ErrNormalizationRejected,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func writeOrganization(w http.ResponseWriter, status int, organization *store.Organization) {
	w.Header().Set(etagHeaderName, organizationETag(*organization))
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusCreated {
		w.Header().Set("Location", organizationsPath+"/"+strconv.FormatInt(organization.ID, 10))
	}
	writeJSON(w, status, organization)
}

func writeOrganizationProfile(w http.ResponseWriter, profile *store.OrganizationProfile) {
	w.Header().Set(etagHeaderName, organizationETag(profile.Organization))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, profile)
}

func addOrganizationIDParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{Name: "id", In: pathKey, Required: true, Description: "Organization ID", Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64}})
}
func addOrganizationIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true, Description: "Strong ETag returned by the latest organization read", Schema: &huma.Schema{Type: huma.TypeString}})
}
func addOrganizationETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{etagHeaderName: {Description: "Strong organization revision tag for optimistic concurrency", Schema: &huma.Schema{Type: huma.TypeString}}}
}

func addOrganizationLocationHeader(response *huma.Response) {
	response.Headers["Location"] = &huma.Param{
		Description: "Canonical URL of the created organization",
		Schema:      &huma.Schema{Type: huma.TypeString},
	}
}

func organizationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_organization_id", "Organization ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func organizationETag(organization store.Organization) string {
	return fmt.Sprintf(`"organization-%d-r%d"`, organization.ID, organization.Revision)
}

func organizationIfMatch(w http.ResponseWriter, r *http.Request, id int64) (int64, bool) {
	values := r.Header.Values(ifMatchHeaderName)
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return 0, false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must contain exactly one revision tag")
		return 0, false
	}
	prefix := fmt.Sprintf(`"organization-%d-r`, id)
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not an organization revision tag")
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, prefix), `"`), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not an organization revision tag")
		return 0, false
	}
	return revision, true
}
