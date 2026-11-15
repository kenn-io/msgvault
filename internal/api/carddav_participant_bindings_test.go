package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVParticipantBindingHTTPPreviewApplyUndo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
	home := t.TempDir()
	srv := NewServer(&config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}, st, nil, testLogger())
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}}})
	require.NoError(err)
	require.Len(books, 1)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:http-card\r\nFN:Alex Example\r\nEMAIL:alex@example.test\r\nEND:VCARD\r\n")
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: books[0].CanonicalURL + "alex.vcf", RemoteUID: "http-card", RemoteETag: "one", RemoteBody: body, SemanticHash: "http-hash", DisplayName: "Alex Example", Emails: []string{"alex@example.test"}}}})
	require.NoError(err)
	_, err = st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
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
	assert.Zero(st.refreshCalls)
	applied := call(`{}`)
	assert.Equal(1, applied.ParticipantsBound)
	assert.Equal(1, st.refreshCalls)
	undoPreview := call(`{"dry_run":true,"undo":true}`)
	assert.Equal(1, undoPreview.ParticipantsUnbound)
	st.refreshErr = errors.New("cache unavailable")
	undone := call(`{"undo":true}`)
	assert.Equal(1, undone.ParticipantsUnbound)
	assert.Equal(2, st.refreshCalls)
	st.refreshErr = nil
	call(`{"undo":true}`)
	assert.Equal(3, st.refreshCalls)
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
}

type partialCardDAVSyncFixture struct {
	cardDAVListFixture

	store       *store.Store
	participant int64
	syncErr     error
}

func (f *partialCardDAVSyncFixture) Sync(ctx context.Context, _ carddav.SyncOptions) (carddav.SyncResult, error) {
	_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
	if err != nil {
		return carddav.SyncResult{}, err
	}
	return carddav.SyncResult{Books: 1}, f.syncErr
}

func TestCardDAVSyncRefreshesIdentityCacheAfterPartialCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
			id, err := st.EnsureParticipant("sync@example.test", "Sync", "example.test")
			require.NoError(err)
			service := &partialCardDAVSyncFixture{store: st.Store, participant: id}
			if fail {
				service.syncErr = errors.New("later book failed")
				st.refreshErr = errors.New("cache unavailable")
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), CardDAV: &CardDAVController{service: service}})
			response := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/sync", []byte(`{}`), "")
			if fail {
				assert.NotEqual(http.StatusOK, response.Code)
			} else {
				assert.Equal(http.StatusOK, response.Code)
			}
			assert.Equal(1, st.refreshCalls)
			person, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.NotNil(person)
		})
	}
}

type cardDAVRoleMutationFixture struct {
	cardDAVListFixture

	store       *store.Store
	participant int64
}

func (f *cardDAVRoleMutationFixture) SetBookRoles(ctx context.Context, _ int64, _ carddav.BookRoles) error {
	_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
	return err
}

func TestCardDAVBookRolesRefreshesIdentityCache(t *testing.T) {
	st := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
	id, err := st.EnsureParticipant("role@example.test", "Role", "example.test")
	require.NoError(t, err)
	service := &cardDAVRoleMutationFixture{store: st.Store, participant: id}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), CardDAV: &CardDAVController{service: service}})
	response := personRequest(t, srv, http.MethodPatch, "/api/v1/carddav/books/7", []byte(`{"write_target":false,"subscribed":false,"lookup_source":false}`), "")
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Equal(t, 1, st.refreshCalls)
}
