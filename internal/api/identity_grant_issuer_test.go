package api

import (
	"bytes"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIdentityGrantIssuerResolvesPersonUIDFromNativeStore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "synthetic-owner@example.test")
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("synthetic-person@example.test", "Synthetic Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	data, err := json.Marshal(map[string]any{
		"label":       "Synthetic exact person grant",
		"permissions": []string{"identity.read", "identity.link"},
		"source_ids":  []int64{source.ID},
		"person_ids":  []int64{person.ID},
	})
	requirements.NoError(err)
	request := httptest.NewRequest(http.MethodPost, agentTokensPath, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	requirements.Equal(http.StatusCreated, response.Code, "%s", response.Body.String())
	var issued struct {
		Secret string `json:"secret"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &issued))
	grant, ok := srv.agentGrants.Lookup(issued.Secret)
	requirements.True(ok)
	assertions.True(grant.AllowsPerson(agentgrant.PermissionIdentityRead, agentgrant.PersonRef{ID: person.ID, UID: person.VCardUID}))
	assertions.True(grant.Allows(agentgrant.PermissionIdentityLink, agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}))
	assertions.False(grant.AllowsPerson(agentgrant.PermissionIdentityUnlink, agentgrant.PersonRef{ID: person.ID, UID: person.VCardUID}))
}

func TestIdentityGrantIssuerResolvesContactOnlyAddressBook(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic-owner", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/synthetic/", DisplayName: "Synthetic Book"}}})
	requirements.NoError(err)
	requirements.Len(books, 1)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	data, err := json.Marshal(map[string]any{"label": "Synthetic book grant", "permissions": []string{"identity.read"}, "address_book_ids": []int64{books[0].ID}})
	requirements.NoError(err)
	request := httptest.NewRequest(http.MethodPost, agentTokensPath, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	requirements.Equal(http.StatusCreated, response.Code, "%s", response.Body.String())
	var issued agentTokenIssueResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &issued))
	requirements.Len(issued.AddressBooks, 1)
	assertions.Equal(account.ID, issued.AddressBooks[0].AccountID)
	assertions.Equal(books[0].ID, issued.AddressBooks[0].BookID)
	assertions.Equal(books[0].CanonicalURL, issued.AddressBooks[0].CanonicalURL)
	assertions.Len(issued.AddressBooks[0].OwnershipFingerprint, 64)
	assertions.Empty(issued.Sources)
	assertions.Empty(issued.Persons)
	grant, ok := srv.agentGrants.Lookup(issued.Secret)
	requirements.True(ok)
	assertions.True(grant.AllowsAddressBook(agentgrant.PermissionIdentityRead, agentgrant.AddressBookRef{AccountID: account.ID, BookID: books[0].ID, CanonicalURL: books[0].CanonicalURL, OwnershipFingerprint: issued.AddressBooks[0].OwnershipFingerprint}))
}

func TestIdentityGrantIssuerRejectsInvalidSelectionsAndCallerEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "synthetic-owner@example.test")
	requirements.NoError(err)
	for _, extra := range []map[string]any{
		{"person_ids": []int64{0}},
		{"person_ids": []int64{9_007_199_254_740_992}},
		{"person_ids": []int64{8_000_000}},
		{"person_ids": make([]int64, 101)},
		{"address_book_ids": []int64{8_000_000}},
		{"persons": []map[string]any{{"id": 1, "uid": "caller-selected-uid"}}},
		{"address_books": []map[string]any{{"book_id": 1, "ownership_fingerprint": "caller-selected-owner"}}},
	} {
		srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
		body := map[string]any{"label": "Synthetic invalid scope", "permissions": []string{"identity.read"}, "source_ids": []int64{source.ID}}
		maps.Copy(body, extra)
		data, err := json.Marshal(body)
		requirements.NoError(err)
		request := httptest.NewRequest(http.MethodPost, agentTokensPath, bytes.NewReader(data))
		request.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		assertions.Equal(http.StatusBadRequest, response.Code, "%s", response.Body.String())
		assertions.Empty(srv.agentGrants.List())
	}
}
