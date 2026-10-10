package api

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDelegatedPersonAttributesRequireNativeScopeAndRevision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("custom-person@example.test", "Custom Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	otherParticipant, err := st.EnsureParticipant("other-custom@example.test", "Other Custom Example", "example.test")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, map[string]any{"label": "Synthetic custom-field editor", "permissions": []string{"person.read", "person.edit"}, "person_ids": []int64{person.ID}})
	requirements.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
	var issued agentTokenIssueResponse
	requirements.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "synthetic-page-id-api", ObjectType: store.AttributeObjectPerson, Slug: "synthetic_page_id", Label: "Synthetic page ID", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, APIMutable: true, IsDeletable: true})
	requirements.NoError(err)
	discovery := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", issued.Secret, true, nil)
	requirements.Equal(http.StatusOK, discovery.Code)
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
	var operations []string
	for _, route := range capabilities.Routes {
		operations = append(operations, route.OperationID)
	}
	for _, operation := range []string{"listPersonAttributes", "setPersonAttribute", "clearPersonAttribute"} {
		assertions.Contains(operations, operation)
	}
	assertions.NotContains(operations, "createAttributeDefinition")
	path := personAttributesPath(person.ID)
	read := delegatedPersonNativeRequest(t, srv, http.MethodGet, path, issued.Secret, "", nil)
	requirements.Equal(http.StatusOK, read.Code, "%s", read.Body.String())
	assertions.Equal(personETag(*person), read.Header().Get("ETag"))
	assertions.Equal(http.StatusForbidden, delegatedPersonNativeRequest(t, srv, http.MethodGet, personAttributesPath(other.ID), issued.Secret, "", nil).Code)
	valuePath := path + "/" + definition.Slug
	forged := map[string]any{"value": map[string]any{"type": "text", "text": "chat"}, "source": "enrichment", "expected_value_id": int64(0)}
	assertions.Equal(http.StatusBadRequest, delegatedPersonNativeRequest(t, srv, http.MethodPut, valuePath, issued.Secret, read.Header().Get("ETag"), forged).Code)
	body := map[string]any{"value": map[string]any{"type": "text", "text": "email"}, "source": "user", "expected_value_id": int64(0)}
	assertions.Equal(http.StatusPreconditionRequired, delegatedPersonNativeRequest(t, srv, http.MethodPut, valuePath, issued.Secret, "", body).Code)
	preview := delegatedPersonNativeRequest(t, srv, http.MethodPut, valuePath+"?dry_run=true", issued.Secret, read.Header().Get("ETag"), body)
	requirements.Equal(http.StatusOK, preview.Code, "%s", preview.Body.String())
	before, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person, before)
	write := delegatedPersonNativeRequest(t, srv, http.MethodPut, valuePath, issued.Secret, read.Header().Get("ETag"), body)
	requirements.Equal(http.StatusOK, write.Code, "%s", write.Body.String())
	var saved store.PersonAttributeWrite
	requirements.NoError(json.Unmarshal(write.Body.Bytes(), &saved))
	requirements.NotNil(saved.Value)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person.Revision, current.Revision)
	assertions.Equal(http.StatusConflict, delegatedPersonNativeRequest(t, srv, http.MethodPut, valuePath, issued.Secret, read.Header().Get("ETag"), body).Code)
	assertions.Equal(http.StatusForbidden, delegatedPersonNativeRequest(t, srv, http.MethodPut, personAttributesPath(other.ID)+"/"+definition.Slug, issued.Secret, personETag(*other), body).Code)
	cleared := delegatedPersonNativeRequest(t, srv, http.MethodDelete, valuePath+"?expected_value_id="+strconv.FormatInt(saved.Value.ID, 10), issued.Secret, personETag(*current), nil)
	requirements.Equal(http.StatusOK, cleared.Code, "%s", cleared.Body.String())
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
	requirements.NoError(err)
	assertions.Empty(values)
	requirements.True(srv.agentGrants.Revoke(issued.ID))
	assertions.Equal(http.StatusUnauthorized, delegatedPersonNativeRequest(t, srv, http.MethodGet, path, issued.Secret, "", nil).Code)
}

func TestPersonAttributesHonorOwnerSuppliedRevision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("owner-attribute@example.test", "Owner Attribute Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	saved, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("email")}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	requirements.NotNil(saved.Value)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, new("Changed Owner Attribute Example"))
	requirements.NoError(err)
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	body, err := json.Marshal(map[string]any{"value": map[string]any{"type": "text", "text": "chat"}, "expected_value_id": saved.Value.ID})
	requirements.NoError(err)
	rejected := attributeRequest(t, srv, http.MethodPut, personAttributesPath(person.ID)+"/"+store.AttributeSlugPrimaryChannel, body, personETag(*person))
	assertions.Equal(http.StatusConflict, rejected.Code, "%s", rejected.Body.String())
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	requirements.NoError(err)
	requirements.Len(values, 1)
	assertions.Equal(saved.Value.ID, values[0].ID)
}

func TestDelegatedPersonAttributeRechecksRevocationAfterGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("queued-attribute@example.test", "Queued Attribute Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, map[string]any{"label": "Synthetic queued attribute editor", "permissions": []string{"person.read", "person.edit"}, "person_ids": []int64{person.ID}})
	requirements.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
	var issued agentTokenIssueResponse
	requirements.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
	release, ok := gate.BeginWork()
	requirements.True(ok)
	defer release()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, personAttributesPath(person.ID)+"/"+store.AttributeSlugPrimaryChannel, bytes.NewBufferString(`{"value":{"type":"text","text":"email"},"expected_value_id":0}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Msgvault-Agent-Token", issued.Secret)
	request.Header.Set("If-Match", personETag(*person))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		done <- response
	}()
	var response *httptest.ResponseRecorder
	completed := false
	assertions.Eventually(func() bool {
		select {
		case response = <-done:
			completed = true
			return true
		default:
			return gate.HasRequestWaiters()
		}
	}, 15*time.Second, 10*time.Millisecond)
	assertions.False(completed, "authorized attribute edit must wait for native daemon work")
	requirements.True(srv.agentGrants.Revoke(issued.ID))
	release()
	if !completed {
		select {
		case response = <-done:
		case <-time.After(15 * time.Second):
			requirements.FailNow("queued attribute edit did not finish after release")
		}
	}
	requirements.NotNil(response)
	assertions.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel, IncludeHistory: true})
	requirements.NoError(err)
	assertions.Empty(values)
	after, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person, after)
}

func TestDelegatedPersonAttributeRequiresExactUIDAndEditAuthority(t *testing.T) {
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("attribute-scope@example.test", "Attribute Scope Example", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		scope       agentgrant.ResourceScopes
		readStatus  int
	}{
		{"read only", []agentgrant.Permission{agentgrant.PermissionPersonRead}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}}, http.StatusOK},
		{"replacement UID", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: "synthetic-replacement-attribute-uid"}}}, http.StatusForbidden},
		{"source only", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "attribute-source@example.test"}}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
			_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic attribute scope", tc.permissions, tc.scope)
			requirements.NoError(err)
			read := delegatedPersonNativeRequest(t, srv, http.MethodGet, personAttributesPath(person.ID), secret, "", nil)
			assertions.Equal(tc.readStatus, read.Code, "%s", read.Body.String())
			rejected := delegatedPersonNativeRequest(t, srv, http.MethodPut, personAttributesPath(person.ID)+"/"+store.AttributeSlugPrimaryChannel, secret, personETag(*person), map[string]any{"value": map[string]any{"type": "text", "text": "email"}, "expected_value_id": int64(0)})
			assertions.Equal(http.StatusForbidden, rejected.Code, "%s", rejected.Body.String())
			values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel, IncludeHistory: true})
			requirements.NoError(err)
			assertions.Empty(values)
		})
	}
}

type unguardedPersonAttributeStore struct {
	MessageStore
	PersonAttributeStore
	PersonProfileStore
}

func TestDelegatedPersonAttributesRequireNativeGuardCapability(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("legacy-attribute@example.test", "Legacy Attribute Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, unguardedPersonAttributeStore{MessageStore: st, PersonAttributeStore: st, PersonProfileStore: st}, nil, testLogger())
	_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic legacy attribute scope", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}})
	requirements.NoError(err)
	discovery := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", secret, true, nil)
	requirements.Equal(http.StatusOK, discovery.Code)
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
	for _, route := range capabilities.Routes {
		assertions.NotContains([]string{"listPersonAttributes", "setPersonAttribute", "clearPersonAttribute"}, route.OperationID)
	}
	read := delegatedPersonNativeRequest(t, srv, http.MethodGet, personAttributesPath(person.ID), secret, "", nil)
	assertions.Equal(http.StatusNotImplemented, read.Code, "%s", read.Body.String())
	rejected := delegatedPersonNativeRequest(t, srv, http.MethodPut, personAttributesPath(person.ID)+"/"+store.AttributeSlugPrimaryChannel, secret, personETag(*person), map[string]any{"value": map[string]any{"type": "text", "text": "email"}, "expected_value_id": int64(0)})
	assertions.Equal(http.StatusNotImplemented, rejected.Code, "%s", rejected.Body.String())
}

func TestOwnerPersonAttributeConditionalWritesUseExistingGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("gated-owner-attribute@example.test", "Gated Owner Attribute Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), OperationGate: gate})
	body, err := json.Marshal(map[string]any{"value": map[string]any{"type": "text", "text": "email"}, "expected_value_id": int64(0)})
	requirements.NoError(err)
	path := personAttributesPath(person.ID) + "/" + store.AttributeSlugPrimaryChannel
	saved := attributeRequest(t, srv, http.MethodPut, path, body, personETag(*person))
	requirements.Equal(http.StatusOK, saved.Code, "%s", saved.Body.String())
	var write store.PersonAttributeWrite
	requirements.NoError(json.Unmarshal(saved.Body.Bytes(), &write))
	requirements.NotNil(write.Value)
	cleared := attributeRequest(t, srv, http.MethodDelete, path+"?expected_value_id="+strconv.FormatInt(write.Value.ID, 10), nil, personETag(*person))
	requirements.Equal(http.StatusOK, cleared.Code, "%s", cleared.Body.String())
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	requirements.NoError(err)
	assertions.Empty(values)
}

func TestDelegatedPersonAttributeRejectsImplicitRecordReference(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("reference-source@example.test", "Reference Source Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	otherParticipant, err := st.EnsureParticipant("reference-target@example.test", "Reference Target Example", "example.test")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "synthetic-reference-api", ObjectType: store.AttributeObjectPerson, Slug: "synthetic_reference", Label: "Synthetic reference", ValueType: store.AttributeValueRecordReference, FieldType: store.AttributeFieldPerson, RecordTarget: new("person"), APIMutable: true, IsDeletable: true})
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic reference editor", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}})
	requirements.NoError(err)
	body := map[string]any{"value": map[string]any{"record_type": "person", "record_id": other.ID}, "expected_value_id": int64(0)}
	rejected := delegatedPersonNativeRequest(t, srv, http.MethodPut, personAttributesPath(person.ID)+"/"+definition.Slug, secret, personETag(*person), body)
	assertions.Equal(http.StatusBadRequest, rejected.Code, "%s", rejected.Body.String())
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
	requirements.NoError(err)
	assertions.Empty(values)
}
