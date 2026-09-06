package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOrganizationHTTPCreateGetListPatchAndDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company","primary_domain":"Example.com"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.Equal("Example Org", created.Name)
	require.NotNil(created.PrimaryDomain)
	assert.Equal("example.com", *created.PrimaryDomain)
	etag := createdResponse.Header().Get("ETag")
	assert.Equal(fmt.Sprintf(`"organization-%d-r1"`, created.ID), etag)
	assert.Equal(fmt.Sprintf("%s/%d", organizationsPath, created.ID), createdResponse.Header().Get("Location"))

	listResponse := organizationRequest(t, srv, http.MethodGet, organizationsPath, nil, "")
	require.Equal(http.StatusOK, listResponse.Code)
	var listed OrganizationsResponse
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.Organizations, 1)
	assert.Equal(int64(1), listed.Total)
	assert.Equal(store.DefaultOrganizationPageSize, listed.Limit)
	assert.Equal(0, listed.Offset)

	getResponse := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), nil, "")
	require.Equal(http.StatusOK, getResponse.Code)
	assert.Equal(etag, getResponse.Header().Get("ETag"))

	patchResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID),
		[]byte(`{"name":"Example Group","kind":"company","description":"Renamed."}`), etag)
	require.Equal(http.StatusOK, patchResponse.Code)
	var patched store.Organization
	require.NoError(json.Unmarshal(patchResponse.Body.Bytes(), &patched))
	assert.Equal("Example Group", patched.Name)
	assert.Nil(patched.PrimaryDomain, "PATCH replaces the full mutable field set")
	assert.Equal(created.Revision+1, patched.Revision)

	staleResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), []byte(`{"name":"Stale"}`), etag)
	require.Equal(http.StatusConflict, staleResponse.Code)
	assert.Contains(staleResponse.Body.String(), "organization_revision_conflict")

	missingIfMatch := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), nil, "")
	require.Equal(http.StatusPreconditionRequired, missingIfMatch.Code)

	deleteResponse := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), nil, patchResponse.Header().Get("ETag"))
	require.Equal(http.StatusNoContent, deleteResponse.Code)

	goneResponse := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), nil, "")
	require.Equal(http.StatusNotFound, goneResponse.Code)
	assert.Contains(goneResponse.Body.String(), "organization_not_found")
}

func TestOrganizationHTTPRejectsInvalidRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	blankName := organizationRequest(t, srv, http.MethodPost, organizationsPath, []byte(`{"name":"   "}`), "")
	require.Equal(http.StatusBadRequest, blankName.Code)
	assert.Contains(blankName.Body.String(), "invalid_organization")

	unknownKind := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"conglomerate"}`), "")
	require.Equal(http.StatusBadRequest, unknownKind.Code)
	assert.Contains(unknownKind.Body.String(), "invalid_organization")

	unknownField := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","company":"nope"}`), "")
	require.Equal(http.StatusBadRequest, unknownField.Code)

	retiredOnCreate := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Retired At Birth","retired":true}`), "")
	require.Equal(http.StatusBadRequest, retiredOnCreate.Code)
	assert.Contains(retiredOnCreate.Body.String(), `unknown field \"retired\"`)

	badID := organizationRequest(t, srv, http.MethodGet, organizationsPath+"/0", nil, "")
	require.Equal(http.StatusBadRequest, badID.Code)
	assert.Contains(badID.Body.String(), "invalid_organization_id")

	foreignTag := organizationRequest(t, srv, http.MethodDelete, organizationsPath+"/1", nil, `"person-1-r1"`)
	require.Equal(http.StatusBadRequest, foreignTag.Code)
	assert.Contains(foreignTag.Body.String(), "invalid_if_match")
}

func TestOrganizationHTTPRejectsInvalidProfileValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	response := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), []byte(`{
			"contact_points":[{"contact_kind":"username","original_value":"example","service_slug":"missing-service","source":"user"}]
		}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, response.Code)
	assert.Contains(response.Body.String(), "invalid_organization")
}

func TestOrganizationHTTPRejectsUnknownProfileIdentifierKind(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	response := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), []byte(`{
			"identifiers":[{"identifier_kind":"bogus","identifier_value":"example","source":"user"}]
		}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, response.Code)
	assert.Contains(response.Body.String(), "invalid_organization")
}

func TestOrganizationHTTPDeleteWithEmploymentReturnsConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "alice@example.com", "alice")

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	title := "Engineer"
	_, err := st.AddEmploymentContext(context.Background(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: created.ID, Title: &title, Source: store.ProvenanceUser,
	})
	require.NoError(err)

	deleteResponse := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), nil, createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusConflict, deleteResponse.Code)
	assert.Contains(deleteResponse.Body.String(), "organization_has_employments")

	retireResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID),
		[]byte(`{"name":"Example Org","kind":"company","retired":true}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, retireResponse.Code)
	var retired store.Organization
	require.NoError(json.Unmarshal(retireResponse.Body.Bytes(), &retired))
	assert.NotNil(retired.RetiredAt)
}

func TestOrganizationHTTPPatchReplacesFieldsAndLifecycleInOneRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	patchedResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID),
		[]byte(`{"name":"Example Group","kind":"company","retired":true}`),
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, patchedResponse.Code)
	var patched store.Organization
	require.NoError(json.Unmarshal(patchedResponse.Body.Bytes(), &patched))
	assert.Equal(created.Revision+1, patched.Revision)
	assert.Equal("Example Group", patched.Name)
	assert.NotNil(patched.RetiredAt)
}

func TestOrganizationHTTPPatchWithoutRetiredPreservesLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	retireResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID),
		[]byte(`{"name":"Example Org","kind":"company","retired":true}`),
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, retireResponse.Code)

	renameResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID),
		[]byte(`{"name":"Example Group","kind":"company"}`),
		retireResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, renameResponse.Code)
	var renamed store.Organization
	require.NoError(json.Unmarshal(renameResponse.Body.Bytes(), &renamed))
	assert.Equal("Example Group", renamed.Name)
	assert.NotNil(renamed.RetiredAt,
		"a PATCH that omits retired must not reactivate a retired organization")
}

func TestOrganizationHTTPPatchRejectsMergedRowWithoutPartialFieldUpdate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	survivor := mustAPIOrganization(t, st, "Example Org")
	losing := mustAPIOrganization(t, st, "Former Org")

	_, err := st.MergeOrganizationsContext(context.Background(),
		survivor.ID, survivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)
	redirect, err := st.GetOrganizationContext(context.Background(), losing.ID)
	require.NoError(err)

	response := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", organizationsPath, redirect.ID),
		[]byte(`{"name":"Hidden Rewrite","kind":"company","retired":false}`),
		fmt.Sprintf(`"organization-%d-r%d"`, redirect.ID, redirect.Revision))
	require.Equal(http.StatusBadRequest, response.Code)
	assert.Contains(response.Body.String(), "invalid_organization")

	unchanged, err := st.GetOrganizationContext(context.Background(), redirect.ID)
	require.NoError(err)
	assert.Equal("Former Org", unchanged.Name)
	assert.Equal(redirect.Revision, unchanged.Revision)
}

func TestConcurrentOrganizationHTTPPatchesShareOneRevisionCAS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)

	start := make(chan struct{})
	codes := make([]int, 2)
	revisions := make([]int64, 2)
	decodeErrors := make([]error, 2)
	var wait sync.WaitGroup
	for i, name := range []string{"First Group", "Second Group"} {
		wait.Add(1)
		go func(index int, replacement string) {
			defer wait.Done()
			<-start
			response := organizationRequest(t, srv, http.MethodPatch,
				organizationsPath+"/1",
				fmt.Appendf(nil, `{"name":%q,"kind":"company","retired":true}`, replacement),
				createdResponse.Header().Get("ETag"))
			codes[index] = response.Code
			var organization store.Organization
			if response.Code == http.StatusOK {
				decodeErrors[index] = json.Unmarshal(response.Body.Bytes(), &organization)
				revisions[index] = organization.Revision
			}
		}(i, name)
	}
	close(start)
	wait.Wait()

	assert.ElementsMatch([]int{http.StatusOK, http.StatusConflict}, codes)
	for i, code := range codes {
		if code == http.StatusOK {
			require.NoError(decodeErrors[i])
			assert.Equal(int64(2), revisions[i])
		}
	}
}

func TestOrganizationHTTPProfileReplacementAndHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	profilePath := fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID)
	firstResponse := organizationRequest(t, srv, http.MethodPut, profilePath, []byte(`{
		"names":[{"name":"Example Organisation","name_kind":"alias","source":"user"}],
		"identifiers":[{"identifier_kind":"domain","identifier_value":"Example.com","source":"user"}],
		"categories":[{"category":"Vendor","source":"user"}]
	}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, firstResponse.Code)
	var first store.OrganizationProfile
	require.NoError(json.Unmarshal(firstResponse.Body.Bytes(), &first))
	require.Len(first.Names, 1)
	require.Len(first.Identifiers, 1)
	assert.Equal("example.com", first.Identifiers[0].NormalizedValue)

	secondResponse := organizationRequest(t, srv, http.MethodPut, profilePath,
		[]byte(`{"identifiers":[{"identifier_kind":"domain","identifier_value":"Example.com","source":"user"}]}`),
		firstResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, secondResponse.Code)
	var second store.OrganizationProfile
	require.NoError(json.Unmarshal(secondResponse.Body.Bytes(), &second))
	assert.Empty(second.Names)
	require.Len(second.Identifiers, 1)

	historyResponse := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/history", organizationsPath, created.ID), nil, "")
	require.Equal(http.StatusOK, historyResponse.Code)
	var history store.OrganizationProfile
	require.NoError(json.Unmarshal(historyResponse.Body.Bytes(), &history))
	require.Len(history.Names, 1, "the superseded alias is visible in the explicit history view")
	assert.NotNil(history.Names[0].Envelope.ActiveUntil)

	staleResponse := organizationRequest(t, srv, http.MethodPut, profilePath, []byte(`{}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusConflict, staleResponse.Code)
}

func TestOrganizationHTTPMergeMovesEmploymentAndBumpsBothRevisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "alice@example.com", "alice")

	survivorResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, survivorResponse.Code)
	var survivor store.Organization
	require.NoError(json.Unmarshal(survivorResponse.Body.Bytes(), &survivor))

	losingResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org Inc","kind":"company"}`), "")
	require.Equal(http.StatusCreated, losingResponse.Code)
	var losing store.Organization
	require.NoError(json.Unmarshal(losingResponse.Body.Bytes(), &losing))

	title := "Engineer"
	employment, err := st.AddEmploymentContext(context.Background(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: losing.ID, Title: &title, Source: store.ProvenanceUser,
	})
	require.NoError(err)

	mergeResponse := organizationRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/merge", organizationsPath, survivor.ID),
		fmt.Appendf(nil, `{"losing_organization_id":%d,"losing_revision":%d}`, losing.ID, losing.Revision),
		survivorResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, mergeResponse.Code)
	var merged store.Organization
	require.NoError(json.Unmarshal(mergeResponse.Body.Bytes(), &merged))
	assert.Equal(survivor.ID, merged.ID)
	assert.Equal(survivor.Revision+1, merged.Revision)

	moved, err := st.GetEmploymentContext(context.Background(), employment.ID)
	require.NoError(err)
	assert.Equal(survivor.ID, moved.OrganizationID)

	selfMerge := organizationRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/merge", organizationsPath, survivor.ID),
		fmt.Appendf(nil, `{"losing_organization_id":%d,"losing_revision":%d}`, survivor.ID, merged.Revision),
		mergeResponse.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, selfMerge.Code)
	assert.Contains(selfMerge.Body.String(), "invalid_organization")
}

func TestOrganizationHTTPAttributesListAndSet(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	definition := mustAPIOrganizationAttributeDefinition(t, st, "industry_focus")

	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	attributesPath := fmt.Sprintf("%s/%d/attributes", organizationsPath, created.ID)
	setResponse := organizationRequest(t, srv, http.MethodPost, attributesPath,
		[]byte(`{"definition_slug":"industry_focus","value":{"type":"text","text":"archival software"},"source":"user"}`), "")
	require.Equal(http.StatusCreated, setResponse.Code)
	assert.Equal("industry_focus", definition.Slug)

	listResponse := organizationRequest(t, srv, http.MethodGet, attributesPath, nil, "")
	require.Equal(http.StatusOK, listResponse.Code)
	var listed OrganizationAttributesResponse
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.Values, 1)

	historyResponse := organizationRequest(t, srv, http.MethodGet,
		attributesPath+"?include_superseded=true", nil, "")
	require.Equal(http.StatusOK, historyResponse.Code)
	var history OrganizationAttributesResponse
	require.NoError(json.Unmarshal(historyResponse.Body.Bytes(), &history))
	assert.Len(history.Values, 1)
}

func TestOrganizationHTTPAttributeClearSupportsCASOrdinalAndDryRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	mustAPIOrganizationAttributeDefinition(t, st, "industry_focus")
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	path := fmt.Sprintf("%s/%d/attributes/industry_focus", organizationsPath, created.ID)
	setResponse := organizationRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/attributes", organizationsPath, created.ID),
		[]byte(`{"definition_slug":"industry_focus","value":{"type":"text","text":"archival software"},"source":"user"}`), "")
	require.Equal(http.StatusCreated, setResponse.Code, setResponse.Body.String())
	var set store.OrganizationAttributeWrite
	require.NoError(json.Unmarshal(setResponse.Body.Bytes(), &set))
	require.NotNil(set.Value)

	dryRun := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s?ordinal=0&expected_value_id=%d&dry_run=true", path, set.Value.ID), nil, "")
	require.Equal(http.StatusOK, dryRun.Code, dryRun.Body.String())
	var preview store.OrganizationAttributeWrite
	require.NoError(json.Unmarshal(dryRun.Body.Bytes(), &preview))
	assert.True(preview.DryRun)
	require.NotNil(preview.Superseded)
	assert.Equal(set.Value.ID, preview.Superseded.ID)

	cleared := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s?ordinal=0&expected_value_id=%d", path, set.Value.ID), nil, "")
	require.Equal(http.StatusOK, cleared.Code, cleared.Body.String())
	var write store.OrganizationAttributeWrite
	require.NoError(json.Unmarshal(cleared.Body.Bytes(), &write))
	require.NotNil(write.Superseded)
	assert.Equal(set.Value.ID, write.Superseded.ID)

	stale := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s?expected_value_id=%d", path, set.Value.ID), nil, "")
	assert.Equal(http.StatusNotFound, stale.Code, stale.Body.String())

	badOrdinal := organizationRequest(t, srv, http.MethodDelete,
		path+"?ordinal=-1", nil, "")
	assert.Equal(http.StatusBadRequest, badOrdinal.Code, badOrdinal.Body.String())
}

func TestOrganizationHTTPProfileAcceptsInlineMediaBeyondGenericRequestLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	profileBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{{
		Source:    string(store.ProvenanceUser),
		MediaKind: "logo", Data: bytes.Repeat([]byte("x"), 800*1024),
	}}})
	require.NoError(err)
	assert.Greater(len(profileBody), 1<<20, "regression body must exceed the generic decoder limit")
	response := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), profileBody,
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var profile store.OrganizationProfile
	require.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
	require.Len(profile.Media, 1)
	require.NotNil(profile.Media[0].ByteSize)
	assert.Equal(int64(800*1024), *profile.Media[0].ByteSize)
}

func TestOrganizationHTTPProfileRejectsTooManyValues(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	requirements.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	requirements.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	categories := make([]OrganizationCategoryBody, store.MaxOrganizationProfileValues+1)
	for i := range categories {
		categories[i] = OrganizationCategoryBody{
			Source:   string(store.ProvenanceUser),
			Category: fmt.Sprintf("category-%d", i),
		}
	}
	body, err := json.Marshal(OrganizationProfileBody{Categories: categories})
	requirements.NoError(err)
	response := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), body,
		createdResponse.Header().Get("ETag"))
	requirements.Equal(http.StatusRequestEntityTooLarge, response.Code, response.Body.String())

	apiError := decodeErrorEnvelope(t, response)
	assertions.Equal("organization_profile_too_large", apiError.Error)
}

func TestOrganizationHTTPProfilePutRoundTripsEnvelopeMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	activeFrom := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	name := OrganizationNameBody{
		TypeLabel:  new("work"),
		TypeTokens: []string{"work", "primary"},
		Source:     string(store.ProvenanceExtraction),
		SourceRef:  new("message:synthetic-1"),
		Confidence: new(0.75),
		ActiveFrom: &activeFrom,
		Name:       "Example Organisation", NameKind: "alias",
	}
	firstBody, err := json.Marshal(OrganizationProfileBody{Names: []OrganizationNameBody{name}})
	require.NoError(err)
	firstPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), firstBody,
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, firstPut.Code, firstPut.Body.String())
	var firstProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(firstPut.Body.Bytes(), &firstProfile))
	require.Len(firstProfile.Names, 1)
	envelope := firstProfile.Names[0].Envelope
	require.NotNil(envelope.TypeLabel)
	assert.Equal("work", *envelope.TypeLabel)
	assert.Equal([]string{"work", "primary"}, envelope.TypeTokens)
	require.NotNil(envelope.Confidence)
	assert.InDelta(0.75, *envelope.Confidence, 1e-9)
	require.NotNil(envelope.ActiveFrom)
	assert.True(envelope.ActiveFrom.Equal(activeFrom))

	secondBody, err := json.Marshal(OrganizationProfileBody{
		Names: []OrganizationNameBody{name},
		Categories: []OrganizationCategoryBody{{
			Source:   string(store.ProvenanceUser),
			Category: "vendor",
		}},
	})
	require.NoError(err)
	secondPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), secondBody,
		firstPut.Header().Get("ETag"))
	require.Equal(http.StatusOK, secondPut.Code, secondPut.Body.String())
	var secondProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(secondPut.Body.Bytes(), &secondProfile))
	require.Len(secondProfile.Names, 1)
	kept := secondProfile.Names[0].Envelope
	assert.Equal(envelope.ID, kept.ID,
		"an unrelated profile update must not supersede a name whose metadata round-tripped")
	require.NotNil(kept.TypeLabel)
	assert.Equal("work", *kept.TypeLabel)
	assert.Equal([]string{"work", "primary"}, kept.TypeTokens)
	require.NotNil(kept.Confidence)
	assert.InDelta(0.75, *kept.Confidence, 1e-9)
}

func TestOrganizationHTTPProfilePutRetainsSourceResourceUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	sourceRef := "address-book"
	resourceUID := "card-42"
	seeded, err := st.ReplaceOrganizationProfileContext(context.Background(), created.ID, created.Revision,
		store.OrganizationProfileInput{Names: []store.OrganizationNameInput{{
			Name: "Imported alias", NameKind: store.OrganizationNameKindAlias,
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceVCardImport, SourceRef: &sourceRef, SourceResourceUID: &resourceUID,
			},
		}}})
	require.NoError(err)
	require.Len(seeded.Names, 1)

	getResponse := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", organizationsPath, created.ID), nil, "")
	require.Equal(http.StatusOK, getResponse.Code, getResponse.Body.String())
	var fetched store.OrganizationProfile
	require.NoError(json.Unmarshal(getResponse.Body.Bytes(), &fetched))
	require.Len(fetched.Names, 1)
	fetchedName := fetched.Names[0]
	require.NotNil(fetchedName.Envelope.SourceResourceUID)

	putBody, err := json.Marshal(map[string]any{"names": []map[string]any{{
		"name": fetchedName.Name, "name_kind": fetchedName.NameKind,
		"ordinal": fetchedName.Envelope.Ordinal, "source": fetchedName.Envelope.Source,
		"source_ref":          fetchedName.Envelope.SourceRef,
		"source_resource_uid": fetchedName.Envelope.SourceResourceUID,
	}}})
	require.NoError(err)
	putResponse := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), putBody,
		getResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, putResponse.Code, putResponse.Body.String())
	var replaced store.OrganizationProfile
	require.NoError(json.Unmarshal(putResponse.Body.Bytes(), &replaced))
	require.Len(replaced.Names, 1)

	assert.Equal(fetchedName.Envelope.ID, replaced.Names[0].Envelope.ID)
	assert.Equal(fetchedName.Envelope.SourceRef, replaced.Names[0].Envelope.SourceRef)
	assert.Equal(fetchedName.Envelope.SourceResourceUID, replaced.Names[0].Envelope.SourceResourceUID)
}

func TestOrganizationHTTPProfileMediaContentRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	logo := []byte("synthetic-logo-bytes")
	profileBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{
		{
			Source:    string(store.ProvenanceUser),
			MediaKind: "logo", MediaType: new("image/png"), Data: logo,
		},
		{
			Ordinal: new(1), Source: string(store.ProvenanceUser),
			MediaKind: "photo", URI: new("https://example.com/photo.png"),
		},
	}})
	require.NoError(err)
	putResponse := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), profileBody,
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, putResponse.Code, putResponse.Body.String())
	var profile store.OrganizationProfile
	require.NoError(json.Unmarshal(putResponse.Body.Bytes(), &profile))
	require.Len(profile.Media, 2)

	var inlineID, uriOnlyID int64
	for _, media := range profile.Media {
		if media.HasData {
			inlineID = media.Envelope.ID
		} else {
			uriOnlyID = media.Envelope.ID
		}
	}
	require.NotZero(inlineID)
	require.NotZero(uriOnlyID)

	content := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/profile/media/%d/content", organizationsPath, created.ID, inlineID),
		nil, "")
	require.Equal(http.StatusOK, content.Code, content.Body.String())
	assert.Equal("image/png", content.Header().Get("Content-Type"))
	assert.Equal(logo, content.Body.Bytes())

	uriOnly := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/profile/media/%d/content", organizationsPath, created.ID, uriOnlyID),
		nil, "")
	require.Equal(http.StatusNotFound, uriOnly.Code)
	assert.Contains(uriOnly.Body.String(), "profile_media_content_unavailable")

	missing := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/profile/media/999999/content", organizationsPath, created.ID),
		nil, "")
	require.Equal(http.StatusNotFound, missing.Code)
	assert.Contains(missing.Body.String(), "profile_media_not_found")
}

func TestOrganizationHTTPProfilePutRetainsInlineMediaViaContentHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	logo := []byte("synthetic-logo-bytes")
	uri := "https://example.com/logo.png"
	firstBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{{
		Source:    string(store.ProvenanceUser),
		MediaKind: "logo", MediaType: new("image/png"), URI: &uri, Data: logo,
	}}})
	require.NoError(err)
	firstPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), firstBody,
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, firstPut.Code, firstPut.Body.String())
	var firstProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(firstPut.Body.Bytes(), &firstProfile))
	require.Len(firstProfile.Media, 1)
	require.True(firstProfile.Media[0].HasData)
	require.NotNil(firstProfile.Media[0].ContentHash)
	mediaID := firstProfile.Media[0].Envelope.ID

	// A GET-derived payload has content_hash but no bytes; an unrelated
	// update re-sending it must keep the stored inline content.
	secondBody, err := json.Marshal(OrganizationProfileBody{
		Media: []OrganizationMediaBody{{
			Source:    string(store.ProvenanceUser),
			MediaKind: "logo", MediaType: new("image/png"), URI: &uri,
			ContentHash: firstProfile.Media[0].ContentHash,
		}},
		Categories: []OrganizationCategoryBody{{
			Source:   string(store.ProvenanceUser),
			Category: "vendor",
		}},
	})
	require.NoError(err)
	secondPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), secondBody,
		firstPut.Header().Get("ETag"))
	require.Equal(http.StatusOK, secondPut.Code, secondPut.Body.String())
	var secondProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(secondPut.Body.Bytes(), &secondProfile))
	require.Len(secondProfile.Media, 1)
	assert.Equal(mediaID, secondProfile.Media[0].Envelope.ID,
		"re-sending content_hash must retain the media row, not rewrite it")
	assert.True(secondProfile.Media[0].HasData,
		"an unrelated update must not strip stored inline media content")

	content := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/profile/media/%d/content", organizationsPath, created.ID, mediaID),
		nil, "")
	require.Equal(http.StatusOK, content.Code, content.Body.String())
	assert.Equal(logo, content.Body.Bytes())

	// A retention hash that matches no active row is a client error, not a
	// silent hash-without-bytes insert.
	staleBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{{
		Source:    string(store.ProvenanceUser),
		MediaKind: "logo", MediaType: new("image/png"), URI: &uri,
		ContentHash: new("0000000000000000000000000000000000000000000000000000000000000000"),
	}}})
	require.NoError(err)
	stalePut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), staleBody,
		secondPut.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, stalePut.Code, stalePut.Body.String())
	assert.Contains(stalePut.Body.String(), "content_hash")
}

func TestOrganizationHTTPProfilePutEditsInlineMediaMetadataViaContentHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	logo := []byte("metadata-edit-logo-bytes")
	firstBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{{
		Source:    string(store.ProvenanceUser),
		MediaKind: "logo", MediaType: new("image/png"), Data: logo,
	}}})
	require.NoError(err)
	firstPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), firstBody,
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, firstPut.Code, firstPut.Body.String())
	var firstProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(firstPut.Body.Bytes(), &firstProfile))
	require.Len(firstProfile.Media, 1)
	require.NotNil(firstProfile.Media[0].ContentHash)

	// Editing the media type with only the retention hash must carry the
	// stored bytes into the replacement row, not fail for lack of data.
	uri := "https://example.com/logo.webp"
	editBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{{
		Source:    string(store.ProvenanceUser),
		MediaKind: "logo", MediaType: new("image/webp"), URI: &uri,
		ContentHash: firstProfile.Media[0].ContentHash,
	}}})
	require.NoError(err)
	editPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), editBody,
		firstPut.Header().Get("ETag"))
	require.Equal(http.StatusOK, editPut.Code,
		"a metadata edit with a retention hash must succeed: %s", editPut.Body.String())
	var editedProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(editPut.Body.Bytes(), &editedProfile))
	require.Len(editedProfile.Media, 1)
	edited := editedProfile.Media[0]
	require.NotNil(edited.MediaType)
	assert.Equal("image/webp", *edited.MediaType)
	require.NotNil(edited.URI)
	assert.True(edited.HasData, "the replacement row must carry the stored bytes")
	assert.Equal(firstProfile.Media[0].ContentHash, edited.ContentHash)

	content := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/profile/media/%d/content",
			organizationsPath, created.ID, edited.Envelope.ID), nil, "")
	require.Equal(http.StatusOK, content.Code, content.Body.String())
	assert.Equal(logo, content.Body.Bytes())
}

func TestOrganizationHTTPProfilePutRetainsInlineOnlyMediaWithoutURI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	logo := []byte("inline-only-logo-bytes")
	firstBody, err := json.Marshal(OrganizationProfileBody{Media: []OrganizationMediaBody{{
		Source:    string(store.ProvenanceUser),
		MediaKind: "logo", MediaType: new("image/png"), Data: logo,
	}}})
	require.NoError(err)
	firstPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), firstBody,
		createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, firstPut.Code, firstPut.Body.String())
	var firstProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(firstPut.Body.Bytes(), &firstProfile))
	require.Len(firstProfile.Media, 1)
	require.Nil(firstProfile.Media[0].URI)
	require.NotNil(firstProfile.Media[0].ContentHash)
	mediaID := firstProfile.Media[0].Envelope.ID

	secondBody, err := json.Marshal(OrganizationProfileBody{
		Media: []OrganizationMediaBody{{
			Source:    string(store.ProvenanceUser),
			MediaKind: "logo", MediaType: new("image/png"),
			ContentHash: firstProfile.Media[0].ContentHash,
		}},
		Categories: []OrganizationCategoryBody{{
			Source:   string(store.ProvenanceUser),
			Category: "vendor",
		}},
	})
	require.NoError(err)
	secondPut := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), secondBody,
		firstPut.Header().Get("ETag"))
	require.Equal(http.StatusOK, secondPut.Code,
		"a GET-derived payload with inline-only media must not be rejected: %s",
		secondPut.Body.String())
	var secondProfile store.OrganizationProfile
	require.NoError(json.Unmarshal(secondPut.Body.Bytes(), &secondProfile))
	require.Len(secondProfile.Media, 1)
	assert.Equal(mediaID, secondProfile.Media[0].Envelope.ID)
	assert.True(secondProfile.Media[0].HasData)

	content := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/profile/media/%d/content", organizationsPath, created.ID, mediaID),
		nil, "")
	require.Equal(http.StatusOK, content.Code, content.Body.String())
	assert.Equal(logo, content.Body.Bytes())
}

func TestOrganizationHTTPAttributeSetForwardsTemporalFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	mustAPIOrganizationAttributeDefinition(t, st, "temporal_note")
	mustAPIOrganizationAttributeDefinition(t, st, "historical_note")
	attributesPath := fmt.Sprintf("%s/%d/attributes", organizationsPath, created.ID)

	backdated := organizationRequest(t, srv, http.MethodPost, attributesPath,
		[]byte(`{"definition_slug":"temporal_note","source":"user",
			"value":{"type":"text","text":"backdated"},
			"active_from":"2024-03-01T00:00:00Z"}`), "")
	require.Equal(http.StatusCreated, backdated.Code, backdated.Body.String())
	var backdatedWrite store.OrganizationAttributeWrite
	require.NoError(json.Unmarshal(backdated.Body.Bytes(), &backdatedWrite))
	require.NotNil(backdatedWrite.Value)
	assert.True(backdatedWrite.Value.ActiveFrom.Equal(
		time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
		"active_from must reach the store, not default to the write time")

	historical := organizationRequest(t, srv, http.MethodPost, attributesPath,
		[]byte(`{"definition_slug":"historical_note","source":"user",
			"value":{"type":"text","text":"former"},
			"active_from":"2020-01-01T00:00:00Z","active_until":"2021-01-01T00:00:00Z"}`), "")
	require.Equal(http.StatusCreated, historical.Code, historical.Body.String())

	current, err := st.ListOrganizationAttributeValuesContext(context.Background(),
		created.ID, store.OrganizationAttributeQuery{DefinitionSlug: "historical_note"})
	require.NoError(err)
	assert.Empty(current, "a fully historical write must not become the current value")
	history, err := st.ListOrganizationAttributeValuesContext(context.Background(),
		created.ID, store.OrganizationAttributeQuery{
			DefinitionSlug: "historical_note", IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(history, 1)
	require.NotNil(history[0].ActiveUntil)
	assert.True(history[0].ActiveUntil.Equal(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)))
}

func newOrganizationTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newOrganizationTestServerWithStore(t)
	return srv
}

func newOrganizationTestServerWithStore(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv, st
}

func mustAPIOrganizationAttributeDefinition(t *testing.T, st *store.Store, slug string) *store.AttributeDefinition {
	t.Helper()
	definition, err := st.CreateAttributeDefinitionContext(context.Background(), store.AttributeDefinitionInput{
		UniversalID: "test-organization-" + slug,
		ObjectType:  store.AttributeObjectOrganization,
		Slug:        slug, Label: slug,
		ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText,
		Cardinality: store.AttributeCardinalitySingle,
		Ownership:   store.AttributeOwnershipUser,
		APIMutable:  true, UICreatable: true, UIEditable: true, IsDeletable: true,
	})
	require.NoError(t, err)
	return definition
}

func mustAPIPerson(t *testing.T, st *store.Store, email, name string) *store.Person {
	t.Helper()
	participantID, err := st.EnsureParticipant(email, name, "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	return person
}

func organizationRequest(t *testing.T, srv *Server, method, path string, body []byte, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	return response
}
