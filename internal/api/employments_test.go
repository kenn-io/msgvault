package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestEmploymentHTTPLifecycleAndProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "alice@example.com", "alice")
	dayJob := mustAPIOrganization(t, st, "Example Org")
	sideJob := mustAPIOrganization(t, st, "Another Org")

	createdResponse := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{
			"person_id":%d,"organization_id":%d,
			"title":"Staff Engineer","role":"Engineering","department":"Archive Platform",
			"start_date":"2019-04","source":"user"
		}`, person.ID, dayJob.ID), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Employment
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.True(created.IsCurrent)
	assert.True(created.IsPrimary)
	require.NotNil(created.StartDate)
	assert.Equal("2019-04", created.StartDate.String())
	assert.Equal(fmt.Sprintf(`"employment-%d-r1"`, created.ID),
		createdResponse.Header().Get("ETag"))

	personPath := fmt.Sprintf("/api/v1/persons/%d/employments", person.ID)
	listResponse := organizationRequest(t, srv, http.MethodGet, personPath, nil, "")
	require.Equal(http.StatusOK, listResponse.Code)
	var listed EmploymentsResponse
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.Employments, 1)
	require.NotNil(listed.Projection)
	assert.Equal("Example Org", listed.Projection.OrganizationName)
	assert.Equal("Staff Engineer", listed.Projection.Title)
	assert.Equal([]string{"Example Org", "Archive Platform"}, listed.Projection.VCard.Org)
	assert.Equal("Staff Engineer", listed.Projection.VCard.Title)
	assert.Equal("Engineering", listed.Projection.VCard.Role)

	sideResponse := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Advisor","source":"user"}`,
			person.ID, sideJob.ID), "")
	require.Equal(http.StatusCreated, sideResponse.Code)
	var side store.Employment
	require.NoError(json.Unmarshal(sideResponse.Body.Bytes(), &side))
	assert.False(side.IsPrimary)

	conflictResponse := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Consultant","is_primary":true,"source":"user"}`,
			person.ID, sideJob.ID), "")
	require.Equal(http.StatusConflict, conflictResponse.Code)
	assert.Contains(conflictResponse.Body.String(), "employment_primary_conflict")

	rotateResponse := organizationRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/primary", employmentsPath, side.ID), nil,
		sideResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, rotateResponse.Code)
	var rotated store.Employment
	require.NoError(json.Unmarshal(rotateResponse.Body.Bytes(), &rotated))
	assert.True(rotated.IsPrimary)

	afterRotation := organizationRequest(t, srv, http.MethodGet, personPath, nil, "")
	require.Equal(http.StatusOK, afterRotation.Code)
	var rotatedList EmploymentsResponse
	require.NoError(json.Unmarshal(afterRotation.Body.Bytes(), &rotatedList))
	require.NotNil(rotatedList.Projection)
	assert.Equal("Another Org", rotatedList.Projection.OrganizationName)

	endResponse := organizationRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/end", employmentsPath, rotated.ID),
		[]byte(`{"end_date":"2026-06"}`), rotateResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, endResponse.Code)
	var ended store.Employment
	require.NoError(json.Unmarshal(endResponse.Body.Bytes(), &ended))
	assert.False(ended.IsCurrent)
	assert.False(ended.IsPrimary)

	afterEnd := organizationRequest(t, srv, http.MethodGet, personPath, nil, "")
	require.Equal(http.StatusOK, afterEnd.Code)
	var endedList EmploymentsResponse
	require.NoError(json.Unmarshal(afterEnd.Body.Bytes(), &endedList))
	require.Len(endedList.Employments, 2, "ending an employment keeps it in history")
	assert.Nil(endedList.Projection,
		"the day job is still current but was demoted by the earlier rotation, "+
			"so there is no primary current employment and therefore no projection")

	promoteResponse := organizationRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/primary", employmentsPath, created.ID), nil,
		fmt.Sprintf(`"employment-%d-r2"`, created.ID))
	require.Equal(http.StatusOK, promoteResponse.Code)

	afterPromotion := organizationRequest(t, srv, http.MethodGet, personPath, nil, "")
	require.Equal(http.StatusOK, afterPromotion.Code)
	var promotedList EmploymentsResponse
	require.NoError(json.Unmarshal(afterPromotion.Body.Bytes(), &promotedList))
	require.NotNil(promotedList.Projection)
	assert.Equal("Example Org", promotedList.Projection.OrganizationName,
		"explicit promotion restores the projection; demotion is never auto-undone")
}

func TestPersonEmploymentsProjectionIsAbsentWithoutAPrimaryCurrentRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "bob@example.com", "bob")

	response := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/persons/%d/employments", person.ID), nil, "")
	require.Equal(http.StatusOK, response.Code)
	var listed EmploymentsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &listed))
	assert.Empty(listed.Employments)
	assert.Nil(listed.Projection, "no primary current employment means no projection")
}

func TestEmploymentHTTPValidationAndConflictMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "alice@example.com", "alice")
	organization := mustAPIOrganization(t, st, "Example Org")

	missingPerson := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"source":"user"}`,
			person.ID+9999, organization.ID), "")
	require.Equal(http.StatusBadRequest, missingPerson.Code)
	assert.Contains(missingPerson.Body.String(), "invalid_person_id")

	missingOrganization := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"source":"user"}`,
			person.ID, organization.ID+9999), "")
	require.Equal(http.StatusBadRequest, missingOrganization.Code,
		"a body reference to a missing record is a bad request, not a missing resource")
	assert.Contains(missingOrganization.Body.String(), "invalid_organization_id")

	missingSource := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusBadRequest, missingSource.Code)
	assert.Contains(missingSource.Body.String(), "invalid_source")

	emptySource := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"source":""}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusBadRequest, emptySource.Code)
	assert.Contains(emptySource.Body.String(), "invalid_source")

	missingPathOrganization := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/employments", organizationsPath, organization.ID+9999), nil, "")
	require.Equal(http.StatusNotFound, missingPathOrganization.Code,
		"a path-addressed organization that does not exist is a missing resource")
	assert.Contains(missingPathOrganization.Body.String(), "organization_not_found")

	missingPathPerson := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/persons/%d/employments", person.ID+9999), nil, "")
	require.Equal(http.StatusNotFound, missingPathPerson.Code,
		"a path-addressed person that does not exist is a missing resource")
	assert.Contains(missingPathPerson.Body.String(), "person_profile_not_found")

	badDate := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"start_date":"2019-13","source":"user"}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusBadRequest, badDate.Code)
	assert.Contains(badDate.Body.String(), "invalid_partial_date")

	reversedDates := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"start_date":"2020-06","end_date":"2020-05","source":"user"}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusBadRequest, reversedDates.Code)
	assert.Contains(reversedDates.Body.String(), "invalid_employment")

	firstResponse := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Engineer","source":"user"}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusCreated, firstResponse.Code)

	duplicate := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"engineer","source":"user"}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusConflict, duplicate.Code)
	assert.Contains(duplicate.Body.String(), "employment_duplicate_active")

	var first store.Employment
	require.NoError(json.Unmarshal(firstResponse.Body.Bytes(), &first))

	missingIfMatch := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", employmentsPath, first.ID), nil, "")
	require.Equal(http.StatusPreconditionRequired, missingIfMatch.Code)

	staleDelete := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", employmentsPath, first.ID), nil,
		fmt.Sprintf(`"employment-%d-r99"`, first.ID))
	require.Equal(http.StatusConflict, staleDelete.Code)
	assert.Contains(staleDelete.Body.String(), "employment_revision_conflict")

	deleted := organizationRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", employmentsPath, first.ID), nil,
		firstResponse.Header().Get("ETag"))
	require.Equal(http.StatusNoContent, deleted.Code)
}

func TestOrganizationEmploymentsListing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	alice := mustAPIPerson(t, st, "alice@example.com", "alice")
	bob := mustAPIPerson(t, st, "bob@example.com", "bob")
	organization := mustAPIOrganization(t, st, "Example Org")

	for _, person := range []*store.Person{alice, bob} {
		response := organizationRequest(t, srv, http.MethodPost, employmentsPath,
			fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Engineer","source":"user"}`,
				person.ID, organization.ID), "")
		require.Equal(http.StatusCreated, response.Code)
	}

	response := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/employments", organizationsPath, organization.ID), nil, "")
	require.Equal(http.StatusOK, response.Code)
	var listed EmploymentsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &listed))
	assert.Len(listed.Employments, 2)
	assert.Nil(listed.Projection,
		"an organization listing has no single-person company projection")

	currentOnly := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d/employments?current_only=true", organizationsPath, organization.ID),
		nil, "")
	require.Equal(http.StatusOK, currentOnly.Code)
	var current EmploymentsResponse
	require.NoError(json.Unmarshal(currentOnly.Body.Bytes(), &current))
	assert.Len(current.Employments, 2)
}

func TestEmploymentHTTPGetPatchAndStrictBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "casey@example.com", "casey")
	organization := mustAPIOrganization(t, st, "Example Org")

	createdResponse := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Engineer","department":"Platform","source":"user"}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Employment
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))

	getResponse := organizationRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", employmentsPath, created.ID), nil, "")
	require.Equal(http.StatusOK, getResponse.Code)
	assert.Equal(createdResponse.Header().Get("ETag"), getResponse.Header().Get("ETag"))

	patchedResponse := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", employmentsPath, created.ID),
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Senior Engineer","source":"user"}`,
			person.ID, organization.ID), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, patchedResponse.Code)
	var patched store.Employment
	require.NoError(json.Unmarshal(patchedResponse.Body.Bytes(), &patched))
	assert.Equal("Senior Engineer", *patched.Title)
	assert.Nil(patched.Department, "PATCH replaces every mutable employment field")

	patchMissingSource := organizationRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", employmentsPath, patched.ID),
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Principal Engineer"}`,
			person.ID, organization.ID), patchedResponse.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, patchMissingSource.Code)
	assert.Contains(patchMissingSource.Body.String(), "invalid_source")

	unknownField := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"source":"user","company":"ignored"}`,
			person.ID, organization.ID), "")
	require.Equal(http.StatusBadRequest, unknownField.Code)
	assert.Contains(unknownField.Body.String(), "bad_request")
}

func mustAPIOrganization(t *testing.T, st *store.Store, name string) *store.Organization {
	t.Helper()
	organization, err := st.CreateOrganizationContext(context.Background(),
		store.OrganizationInput{Name: name, Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	return organization
}
