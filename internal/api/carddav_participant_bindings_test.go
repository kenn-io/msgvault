package api

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVParticipantBindingHTTPPreviewApplyUndo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	home := t.TempDir()
	srv := NewServer(&config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}, st, nil, testLogger())
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}}})
	require.NoError(err)
	require.Len(books, 1)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:http-card\r\nFN:Alex Example\r\nEMAIL:alex@example.test\r\nEND:VCARD\r\n")
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: books[0].CanonicalURL + "alex.vcf", RemoteUID: "http-card", RemoteETag: "one", RemoteBody: body, SemanticHash: "http-hash", DisplayName: "Alex Example", Emails: []string{"alex@example.test"}}}})
	require.NoError(err)
	id, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	call := func(body string) store.CardDAVParticipantBindingResult {
		t.Helper()
		response := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/participant-bindings", []byte(body), "")
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var result store.CardDAVParticipantBindingResult
		require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
		return result
	}
	preview := call(`{"dry_run":true}`)
	assert.Equal(1, preview.ParticipantsBound)
	person, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
	require.NoError(err)
	assert.Nil(person)
	applied := call(`{}`)
	assert.Equal(1, applied.ParticipantsBound)
	undoPreview := call(`{"dry_run":true,"undo":true}`)
	assert.Equal(1, undoPreview.ParticipantsUnbound)
	undone := call(`{"undo":true}`)
	assert.Equal(1, undone.ParticipantsUnbound)
	malformed := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/participant-bindings", []byte(`{"dry_run":"yes"}`), "")
	assert.Equal(http.StatusBadRequest, malformed.Code)
}

func TestCardDAVParticipantBindingHTTPRequiresOwner(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	home := t.TempDir()
	srv := NewServer(&config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, st, nil, testLogger())
	response := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/participant-bindings", []byte(`{"dry_run":true}`), "")
	assert.Equal(http.StatusUnauthorized, response.Code)
	assert.False(delegatedOperationAllowed("reconcileCardDAVParticipantBindings"))
}
