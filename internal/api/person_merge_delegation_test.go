package api

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func delegatedMergeNativeRequest(t *testing.T, srv *Server, survivor, absorbed *store.Person, secret, key string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(MergePersonRequest{AbsorbedPersonID: absorbed.ID})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/people/%d/merge", survivor.ID), bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	r.Header.Set("If-Match", personETag(*survivor)+", "+personETag(*absorbed))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestDelegatedPersonMergeNativeIssuerCommitReplayAndRevocation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	promote := func(address, name string) *store.Person {
		participant, err := st.EnsureParticipant(address, name, "example.test")
		requirements.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		requirements.NoError(err)
		return person
	}
	survivor := promote("merge-survivor@example.test", "Merge Survivor")
	absorbed := promote("merge-absorbed@example.test", "Merge Absorbed")
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	issue := func() agentTokenIssueResponse {
		response := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, map[string]any{"label": "Synthetic merge grant", "permissions": []string{"person.read", "person.merge"}, "person_ids": []int64{survivor.ID, absorbed.ID}})
		requirements.Equal(http.StatusCreated, response.Code, "%s", response.Body.String())
		var issued agentTokenIssueResponse
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &issued))
		return issued
	}
	issued := issue()
	discovery := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", issued.Secret, true, nil)
	requirements.Equal(http.StatusOK, discovery.Code, "%s", discovery.Body.String())
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
	ids := []string{}
	for _, route := range capabilities.Routes {
		ids = append(ids, route.OperationID)
	}
	assertions.Contains(ids, "mergePersons")
	for _, id := range []string{"splitPersonMerge", "listPersonMerges", "getPersonMerge", "getPersonMergeSnapshot", "decidePersonMergeCandidate", "patchPerson"} {
		assertions.NotContains(ids, id)
	}
	// A second principal cannot replay the first principal's global native key.
	secondIssued := issue()
	response := delegatedMergeNativeRequest(t, srv, survivor, absorbed, issued.Secret, "synthetic-native-merge")
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	var receipt store.PersonMergeResult
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &receipt))
	assertions.Equal("agent:"+issued.ID, receipt.Merge.Actor)
	assertions.Equal(survivor.ID, receipt.Person.ID)
	_, err := st.GetPerson(absorbed.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	replay := delegatedMergeNativeRequest(t, srv, survivor, absorbed, issued.Secret, "synthetic-native-merge")
	requirements.Equal(http.StatusOK, replay.Code, "%s", replay.Body.String())
	assertions.JSONEq(response.Body.String(), replay.Body.String())
	assertions.Equal(response.Header().Get("ETag"), replay.Header().Get("ETag"))
	crossPrincipal := delegatedMergeNativeRequest(t, srv, survivor, absorbed, secondIssued.Secret, "synthetic-native-merge")
	assertions.Equal(http.StatusConflict, crossPrincipal.Code, "%s", crossPrincipal.Body.String())
	assertions.NotContains(crossPrincipal.Body.String(), "review_candidates")
	requirements.True(srv.agentGrants.Revoke(issued.ID))
	revoked := delegatedMergeNativeRequest(t, srv, survivor, absorbed, issued.Secret, "synthetic-native-merge")
	assertions.Equal(http.StatusUnauthorized, revoked.Code)
	current, err := st.GetPerson(survivor.ID)
	requirements.NoError(err)
	wantPerson, err := json.Marshal(receipt.Person)
	requirements.NoError(err)
	gotPerson, err := json.Marshal(current)
	requirements.NoError(err)
	assertions.JSONEq(string(wantPerson), string(gotPerson))
}

func TestDelegatedPersonMergeRequiresSeparatePermissionAndBothUIDs(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		permissions            []string
		omitAbsorbed, wrongUID bool
	}{
		{name: "read only", permissions: []string{"person.read"}},
		{name: "edit does not authorize merge", permissions: []string{"person.read", "person.edit"}},
		{name: "merge without read", permissions: []string{"person.merge"}},
		{name: "missing absorbed scope", permissions: []string{"person.read", "person.merge"}, omitAbsorbed: true},
		{name: "replacement UID", permissions: []string{"person.read", "person.merge"}, wrongUID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			participant, err := st.EnsureParticipant("denied-survivor@example.test", "Denied Survivor", "example.test")
			requirements.NoError(err)
			survivor, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			participant, err = st.EnsureParticipant("denied-absorbed@example.test", "Denied Absorbed", "example.test")
			requirements.NoError(err)
			absorbed, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
			perms := make([]agentgrant.Permission, len(tc.permissions))
			for i, p := range tc.permissions {
				perms[i] = agentgrant.Permission(p)
			}
			scope := agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: survivor.ID, UID: survivor.VCardUID}}}
			if !tc.omitAbsorbed {
				scope.Persons = append(scope.Persons, agentgrant.PersonRef{ID: absorbed.ID, UID: absorbed.VCardUID})
			}
			if tc.wrongUID {
				scope.Persons[0].UID = "synthetic-replacement-merge-UID"
			}
			_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic denied merge", perms, scope)
			requirements.NoError(err)
			denied := delegatedMergeNativeRequest(t, srv, survivor, absorbed, secret, "synthetic-denied-merge")
			assertions.Equal(http.StatusForbidden, denied.Code, "%s", denied.Body.String())
			current, err := st.GetPerson(survivor.ID)
			requirements.NoError(err)
			assertions.Equal(survivor, current)
			current, err = st.GetPerson(absorbed.ID)
			requirements.NoError(err)
			assertions.Equal(absorbed, current)
		})
	}
}

func TestDelegatedPersonMergeRevocationWhileWaitingForGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("queued-merge-survivor@example.test", "Queued Survivor", "example.test")
	requirements.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	participant, err = st.EnsureParticipant("queued-merge-absorbed@example.test", "Queued Absorbed", "example.test")
	requirements.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	_, secret, grant, err := srv.agentGrants.IssueScoped("Synthetic queued merge", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonMerge}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: survivor.ID, UID: survivor.VCardUID}, {ID: absorbed.ID, UID: absorbed.VCardUID}}})
	requirements.NoError(err)
	release, ok := gate.BeginWork()
	requirements.True(ok)
	defer release()
	data, err := json.Marshal(MergePersonRequest{AbsorbedPersonID: absorbed.ID})
	requirements.NoError(err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/people/%d/merge", survivor.ID), bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, secret)
	request.Header.Set("If-Match", personETag(*survivor)+", "+personETag(*absorbed))
	request.Header.Set("Idempotency-Key", "synthetic-queued-merge")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		done <- response
	}()
	var response *httptest.ResponseRecorder
	completed := false
	requirements.Eventually(func() bool {
		select {
		case response = <-done:
			completed = true
			return true
		default:
			return gate.HasRequestWaiters()
		}
	}, 15*time.Second, 10*time.Millisecond)
	assertions.False(completed, "authorized merge must wait for daemon-owned work")
	requirements.True(srv.agentGrants.Revoke(grant.ID))
	release()
	if !completed {
		select {
		case response = <-done:
		case <-time.After(15 * time.Second):
			requirements.FailNow("queued merge did not finish after gate release")
		}
	}
	requirements.NotNil(response)
	assertions.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
	current, err := st.GetPerson(survivor.ID)
	requirements.NoError(err)
	assertions.Equal(survivor, current)
	current, err = st.GetPerson(absorbed.ID)
	requirements.NoError(err)
	assertions.Equal(absorbed, current)
}

func TestDelegatedPersonMergeRequiresAffectedThirdPersonAndBook(t *testing.T) {
	for _, tc := range []struct {
		name        string
		third, book bool
		want        int
	}{
		{"missing third owner", false, true, http.StatusForbidden},
		{"missing third book", true, false, http.StatusForbidden},
		{"complete native scope", true, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			promote := func(address, name string) *store.Person {
				participant, err := st.EnsureParticipant(address, name, "example.test")
				requirements.NoError(err)
				person, _, err := st.CreatePersonFromParticipant(participant)
				requirements.NoError(err)
				return person
			}
			survivor := promote("third-scope-survivor@example.test", "Third Scope Survivor")
			absorbed := promote("third-scope-absorbed@example.test", "Third Scope Absorbed")
			third := promote("third-scope-reference@example.test", "Third Scope Reference")
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic-merge-owner", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/merge/", DisplayName: "Synthetic Merge Book"}}})
			requirements.NoError(err)
			requirements.Len(books, 1)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (address_book_id,href,remote_etag,remote_body,remote_semantic_hash,local_hash,mapping_status,governance,person_id,person_revision_at_bind) VALUES (?, ?, ?, ?, ?, ?, 'mapped', 'local', ?, ?)`), books[0].ID, books[0].CanonicalURL+third.VCardUID+".vcf", `"synthetic-etag"`, []byte("synthetic card"), "synthetic-remote-hash", "synthetic-local-hash", third.ID, third.Revision)
			requirements.NoError(err)
			_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: absorbed.ID, TargetPersonID: third.ID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "synthetic-owner"})
			requirements.NoError(err)
			survivor, err = st.GetPerson(survivor.ID)
			requirements.NoError(err)
			absorbed, err = st.GetPerson(absorbed.ID)
			requirements.NoError(err)
			third, err = st.GetPerson(third.ID)
			requirements.NoError(err)
			srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
			personIDs := []int64{survivor.ID, absorbed.ID}
			if tc.third {
				personIDs = append(personIDs, third.ID)
			}
			body := map[string]any{"label": "Synthetic affected merge", "permissions": []string{"person.read", "person.merge", "carddav.write"}, "person_ids": personIDs}
			if tc.book {
				body["address_book_ids"] = []int64{books[0].ID}
			}
			response := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, body)
			requirements.Equal(http.StatusCreated, response.Code, "%s", response.Body.String())
			var issued agentTokenIssueResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &issued))
			merged := delegatedMergeNativeRequest(t, srv, survivor, absorbed, issued.Secret, "synthetic-third-scope-merge")
			assertions.Equal(tc.want, merged.Code, "%s", merged.Body.String())
			if tc.want != http.StatusOK {
				current, err := st.GetPerson(survivor.ID)
				requirements.NoError(err)
				assertions.Equal(survivor, current)
				current, err = st.GetPerson(absorbed.ID)
				requirements.NoError(err)
				assertions.Equal(absorbed, current)
				current, err = st.GetPerson(third.ID)
				requirements.NoError(err)
				assertions.Equal(third, current)
			}
		})
	}
}
