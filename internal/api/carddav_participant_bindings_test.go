package api

import (
	"context"
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

type cardDAVBindingOptionsStore struct {
	*stubIdentityCacheStore

	options []store.CardDAVParticipantBindingOptions
}

func (s *cardDAVBindingOptionsStore) ReconcileCardDAVParticipantBindingsContext(_ context.Context, options store.CardDAVParticipantBindingOptions) (*store.CardDAVParticipantBindingResult, error) {
	s.options = append(s.options, options)
	return &store.CardDAVParticipantBindingResult{}, nil
}

func TestCardDAVParticipantBindingHTTPPreviewApplyUndo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &cardDAVBindingOptionsStore{stubIdentityCacheStore: &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}}
	home := t.TempDir()
	srv := NewServer(&config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}, st, nil, testLogger())
	call := func(body string) {
		t.Helper()
		response := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/participant-bindings", []byte(body), "")
		require.Equal(http.StatusOK, response.Code, response.Body.String())
	}
	call(`{"dry_run":true}`)
	assert.Zero(st.refreshCalls)
	call(`{}`)
	assert.Equal(1, st.refreshCalls)
	call(`{"dry_run":true,"undo":true}`)
	assert.Equal(1, st.refreshCalls)
	st.refreshErr = errors.New("cache unavailable")
	call(`{"undo":true}`)
	assert.Equal(2, st.refreshCalls)
	st.refreshErr = nil
	call(`{"undo":true}`)
	assert.Equal(3, st.refreshCalls)
	assert.Equal([]store.CardDAVParticipantBindingOptions{{DryRun: true}, {}, {DryRun: true, Undo: true}, {Undo: true}, {Undo: true}}, st.options)
	malformed := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/participant-bindings", []byte(`{"dry_run":"yes"}`), "")
	assert.Equal(http.StatusBadRequest, malformed.Code)
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
	personRequest(t, srv, http.MethodPatch, "/api/v1/carddav/books/7", []byte(`{"write_target":false,"subscribed":false,"lookup_source":false}`), "")
	assert.Equal(t, 1, st.refreshCalls)
}

type cardDAVPublicationMutationFixture struct {
	cardDAVListFixture

	store       *store.Store
	participant int64
}

func (f *cardDAVPublicationMutationFixture) PublishPerson(ctx context.Context, _ int64) error {
	_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
	if err != nil {
		return err
	}
	return errors.New("publication conflict")
}

func (f *cardDAVPublicationMutationFixture) ResolveConflict(ctx context.Context, _ int64, _ carddav.ResolutionChoice) error {
	return f.PublishPerson(ctx, 0)
}

func TestCardDAVPublicationRefreshesIdentityCacheAfterConflict(t *testing.T) {
	for _, test := range []struct{ path, body string }{
		{"/api/v1/carddav/publications/7", `{}`},
		{"/api/v1/carddav/conflicts/7/resolve", `{"choice":"keep_local"}`},
	} {
		t.Run(test.path, func(t *testing.T) {
			st := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
			id, err := st.EnsureParticipant("publish@example.test", "Publish", "example.test")
			require.NoError(t, err)
			service := &cardDAVPublicationMutationFixture{store: st.Store, participant: id}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), CardDAV: &CardDAVController{service: service}})
			response := personRequest(t, srv, http.MethodPost, test.path, []byte(test.body), "")
			assert.Equal(t, http.StatusInternalServerError, response.Code)
			assert.Equal(t, 1, st.refreshCalls)
		})
	}
}
