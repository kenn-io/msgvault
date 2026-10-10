package api

import (
	"bytes"
	"context"
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

func delegatedPersonNativeRequest(t *testing.T, srv *Server, method, path, secret, etag string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Msgvault-Agent-Token", secret)
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestDelegatedPersonNativeReadRenameStructuredPatchAndRevocation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	firstParticipant, err := st.EnsureParticipant("first-profile@example.test", "First Example", "example.test")
	requirements.NoError(err)
	secondParticipant, err := st.EnsureParticipant("second-profile@example.test", "Second Example", "example.test")
	requirements.NoError(err)
	first, _, err := st.CreatePersonFromParticipant(firstParticipant)
	requirements.NoError(err)
	second, _, err := st.CreatePersonFromParticipant(secondParticipant)
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, map[string]any{
		"label": "Synthetic exact-person editor", "permissions": []string{"person.read", "person.edit"}, "person_ids": []int64{first.ID},
	})
	requirements.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
	var issued agentTokenIssueResponse
	requirements.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
	discovery := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", issued.Secret, true, nil)
	requirements.Equal(http.StatusOK, discovery.Code, "%s", discovery.Body.String())
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
	var operationIDs []string
	for _, route := range capabilities.Routes {
		operationIDs = append(operationIDs, route.OperationID)
	}
	for _, operation := range []string{"getPersonProfile", "patchPerson", "getPersonStructuredProfile", "patchPersonStructuredProfile"} {
		assertions.Contains(operationIDs, operation)
	}
	firstPath := peoplePath + "/" + strconv.FormatInt(first.ID, 10)
	secondPath := peoplePath + "/" + strconv.FormatInt(second.ID, 10)
	read := delegatedPersonNativeRequest(t, srv, http.MethodGet, firstPath, issued.Secret, "", nil)
	requirements.Equal(http.StatusOK, read.Code, "%s", read.Body.String())
	assertions.Equal(http.StatusForbidden, delegatedPersonNativeRequest(t, srv, http.MethodGet, secondPath, issued.Secret, "", nil).Code)
	assertions.Equal(http.StatusForbidden, delegatedPersonNativeRequest(t, srv, http.MethodGet, peoplePath+"/999999", issued.Secret, "", nil).Code)
	renamed := delegatedPersonNativeRequest(t, srv, http.MethodPatch, firstPath, issued.Secret, read.Header().Get("ETag"), map[string]any{"display_name": "Changed Example"})
	requirements.Equal(http.StatusOK, renamed.Code, "%s", renamed.Body.String())
	var current store.Person
	requirements.NoError(json.Unmarshal(renamed.Body.Bytes(), &current))
	assertions.Equal(new("Changed Example"), current.DisplayName)
	assertions.Equal(first.Revision+1, current.Revision)
	patch := map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Structured Example", "envelope": map[string]any{"source": "user"}}}}}
	structured := delegatedPersonNativeRequest(t, srv, http.MethodPatch, firstPath+"/profile", issued.Secret, renamed.Header().Get("ETag"), patch)
	requirements.Equal(http.StatusOK, structured.Code, "%s", structured.Body.String())
	profile, err := st.GetPersonProfileContext(t.Context(), first.ID)
	requirements.NoError(err)
	assertions.Equal(first.Revision+2, profile.Person.Revision)
	assertions.Equal(new("Changed Example"), profile.Person.DisplayName)
	requirements.NotEmpty(profile.Names)
	assertions.Equal(new("Structured Example"), profile.Names[len(profile.Names)-1].Formatted)
	denied := delegatedPersonNativeRequest(t, srv, http.MethodPatch, secondPath, issued.Secret, personETag(*second), map[string]any{"display_name": "Denied Example"})
	assertions.Equal(http.StatusForbidden, denied.Code)
	unchanged, err := st.GetPerson(second.ID)
	requirements.NoError(err)
	assertions.Equal(second, unchanged)
	requirements.True(srv.agentGrants.Revoke(issued.ID))
	assertions.Equal(http.StatusUnauthorized, delegatedPersonNativeRequest(t, srv, http.MethodGet, firstPath, issued.Secret, "", nil).Code)
}

func TestDelegatedPersonScopeDenialPrecedesOperationGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("scoped-gate@example.test", "Scoped Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	gate := &SerialOperationGate{}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, map[string]any{"label": "Synthetic gate editor", "permissions": []string{"person.read", "person.edit"}, "person_ids": []int64{person.ID}})
	requirements.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
	var issued agentTokenIssueResponse
	requirements.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
	release, ok := gate.BeginWork()
	requirements.True(ok)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPatch, peoplePath+"/999999", bytes.NewBufferString(`{"display_name":"Denied Example"}`))
	r.Header.Set("X-Msgvault-Agent-Token", issued.Secret)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusForbidden, w.Code, "%s", w.Body.String())
}

func TestDelegatedPersonRequiresExactUIDAndEditPermission(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("exact-profile@example.test", "Exact Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	path := peoplePath + "/" + strconv.FormatInt(person.ID, 10)
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		scope       agentgrant.ResourceScopes
		readStatus  int
	}{
		{"read only", []agentgrant.Permission{agentgrant.PermissionPersonRead}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}}, http.StatusOK},
		{"source only", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "synthetic-owner@example.test"}}}, http.StatusForbidden},
		{"replacement uid", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: "synthetic-replacement-uid"}}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic exact scope", tc.permissions, tc.scope)
			requirements.NoError(err)
			read := delegatedPersonNativeRequest(t, srv, http.MethodGet, path, secret, "", nil)
			assertions.Equal(tc.readStatus, read.Code, "%s", read.Body.String())
			write := delegatedPersonNativeRequest(t, srv, http.MethodPatch, path, secret, personETag(*person), map[string]any{"display_name": "Denied Example"})
			assertions.Equal(http.StatusForbidden, write.Code, "%s", write.Body.String())
			after, err := st.GetPerson(person.ID)
			requirements.NoError(err)
			assertions.Equal(person, after)
		})
	}
}

func TestDelegatedPublishedPersonEditRequiresExactNativeAddressBook(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("published-profile@example.test", "Published Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic/"
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic-owner",
		PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: person.ID, Desired: true, AddressBookID: books[0].ID,
		Href: bookURL + person.VCardUID + ".vcf", LocalHash: local.Fingerprint,
		OutgoingBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Published Example\r\nEND:VCARD\r\n"), OutgoingSemanticHash: "synthetic-semantic",
	})
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	path := peoplePath + "/" + strconv.FormatInt(person.ID, 10)
	for _, withBook := range []bool{false, true} {
		request := map[string]any{"label": "Synthetic publication editor", "permissions": []string{"person.read", "person.edit", "carddav.write"}, "person_ids": []int64{person.ID}}
		if withBook {
			request["address_book_ids"] = []int64{books[0].ID}
		}
		issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, request)
		requirements.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
		var issued agentTokenIssueResponse
		requirements.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
		write := delegatedPersonNativeRequest(t, srv, http.MethodPatch, path, issued.Secret, personETag(*person), map[string]any{"display_name": "Published Changed Example"})
		if withBook {
			requirements.Equal(http.StatusOK, write.Code, "%s", write.Body.String())
		} else {
			assertions.Equal(http.StatusForbidden, write.Code, "%s", write.Body.String())
			after, err := st.GetPerson(person.ID)
			requirements.NoError(err)
			assertions.Equal(person, after)
		}
	}
}

func TestDelegatedPersonRechecksRevocationAfterWaitingForGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("queued-profile@example.test", "Queued Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	issuedResponse := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, map[string]any{"label": "Synthetic queued editor", "permissions": []string{"person.read", "person.edit"}, "person_ids": []int64{person.ID}})
	requirements.Equal(http.StatusCreated, issuedResponse.Code, "%s", issuedResponse.Body.String())
	var issued agentTokenIssueResponse
	requirements.NoError(json.Unmarshal(issuedResponse.Body.Bytes(), &issued))
	release, ok := gate.BeginWork()
	requirements.True(ok)
	defer release()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, peoplePath+"/"+strconv.FormatInt(person.ID, 10), bytes.NewBufferString(`{"display_name":"Denied Queued Example"}`))
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
	assertions.False(completed, "authorized edit must wait for daemon-owned work before writing")
	requirements.True(srv.agentGrants.Revoke(issued.ID))
	release()
	if !completed {
		select {
		case response = <-done:
		case <-time.After(15 * time.Second):
			requirements.FailNow("queued edit did not finish after gate release")
		}
	}
	requirements.NotNil(response)
	assertions.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
	after, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person, after)
}

// This facade exposes the existing native owner profile methods without the
// newer transaction authorization capability, as an older daemon adapter does.
type unguardedPersonStore struct {
	MessageStore
	PersonProfileStore
	PersonProfileValueStore
}

func TestDelegatedPersonEditingRequiresNativeAuthorizationCapability(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("legacy-profile@example.test", "Legacy Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, unguardedPersonStore{MessageStore: st, PersonProfileStore: st, PersonProfileValueStore: st}, nil, testLogger())
	_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic legacy editor", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonEdit}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}})
	requirements.NoError(err)
	discovery := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", secret, true, nil)
	requirements.Equal(http.StatusOK, discovery.Code, "%s", discovery.Body.String())
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
	for _, route := range capabilities.Routes {
		assertions.NotContains([]string{"getPersonProfile", "patchPerson", "getPersonStructuredProfile", "patchPersonStructuredProfile"}, route.OperationID)
	}
	write := delegatedPersonNativeRequest(t, srv, http.MethodPatch, peoplePath+"/"+strconv.FormatInt(person.ID, 10), secret, personETag(*person), map[string]any{"display_name": "Denied Legacy Example"})
	assertions.Equal(http.StatusNotImplemented, write.Code, "%s", write.Body.String())
	after, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person, after)
}
