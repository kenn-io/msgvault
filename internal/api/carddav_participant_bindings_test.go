package api

import (
	"context"
	"errors"
	"net/http"
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
	assert.Equal([]store.CardDAVParticipantBindingOptions{{DryRun: true}, {}, {DryRun: true, Undo: true}, {Undo: true}}, st.options)
	malformed := personRequest(t, srv, http.MethodPost, "/api/v1/carddav/participant-bindings", []byte(`{"dry_run":"yes"}`), "")
	assert.Equal(http.StatusBadRequest, malformed.Code)
}

type cardDAVMutationFixture struct {
	cardDAVListFixture
	store       *store.Store
	participant int64
	syncErr     error
}

func (f *cardDAVMutationFixture) mutate(ctx context.Context) error {
	_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
	return err
}

func (f *cardDAVMutationFixture) Sync(ctx context.Context, _ carddav.SyncOptions) (carddav.SyncResult, error) {
	if err := f.mutate(ctx); err != nil {
		return carddav.SyncResult{}, err
	}
	return carddav.SyncResult{Books: 1}, f.syncErr
}

func (f *cardDAVMutationFixture) SetBookRoles(ctx context.Context, _ int64, _ carddav.BookRoles) error {
	return f.mutate(ctx)
}

func (f *cardDAVMutationFixture) PublishPerson(ctx context.Context, _ int64) error {
	if err := f.mutate(ctx); err != nil {
		return err
	}
	return errors.New("publication conflict")
}

func (f *cardDAVMutationFixture) ResolveConflict(ctx context.Context, _ int64, _ carddav.ResolutionChoice) error {
	return f.PublishPerson(ctx, 0)
}

func TestCardDAVMutationRefreshesIdentityCache(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		fail                     bool
	}{
		{"sync", http.MethodPost, "/api/v1/carddav/sync", `{}`, false},
		{"sync failure", http.MethodPost, "/api/v1/carddav/sync", `{}`, true},
		{"roles", http.MethodPatch, "/api/v1/carddav/books/7", `{"write_target":false,"subscribed":false,"lookup_source":false}`, false},
		{"publish", http.MethodPost, "/api/v1/carddav/publications/7", `{}`, true},
		{"resolve", http.MethodPost, "/api/v1/carddav/conflicts/7/resolve", `{"choice":"keep_local"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
			id, err := st.EnsureParticipant("mutation@example.test", "Mutation", "example.test")
			require.NoError(t, err)
			service := &cardDAVMutationFixture{store: st.Store, participant: id}
			if tc.name == "sync failure" {
				service.syncErr = errors.New("later book failed")
				st.refreshErr = errors.New("cache unavailable")
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), CardDAV: &CardDAVController{service: service}})
			response := personRequest(t, srv, tc.method, tc.path, []byte(tc.body), "")
			if tc.name == "publish" || tc.name == "resolve" {
				assert.Equal(t, http.StatusInternalServerError, response.Code)
			} else if tc.fail {
				assert.NotEqual(t, http.StatusOK, response.Code)
			} else if tc.name != "roles" {
				assert.Equal(t, http.StatusOK, response.Code)
			}
			assert.Equal(t, 1, st.refreshCalls)
		})
	}
}
