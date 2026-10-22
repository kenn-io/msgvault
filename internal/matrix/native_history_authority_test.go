package matrix

import (
	"net/http"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available in nested callbacks.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available in nested callbacks.
	"go.kenn.io/msgvault/internal/store"
)

// Removing native authority updates from any error path leaves an earlier
// subscription readable after the provider has denied its current read.
func TestNativeMatrixReadFailureUpdatesCurrentAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, path                string
		status                    int
		accountDenied, roomDenied bool
	}{
		{"members_token_expired", "/rooms/!room:example.org/joined_members", http.StatusUnauthorized, true, false},
		{"history_token_expired", "/rooms/!room:example.org/messages", http.StatusUnauthorized, true, false},
		{"history_room_denied", "/rooms/!room:example.org/messages", http.StatusForbidden, false, true},
		{"history_unknown", "/rooms/!room:example.org/messages", http.StatusBadRequest, false, false},
		{"account_data_token_expired", "/user/@archive:example.org/account_data/m.direct", http.StatusUnauthorized, true, false},
		{"account_data_denied", "/user/@archive:example.org/account_data/m.direct", http.StatusForbidden, true, false},
		{"account_data_unknown", "/user/@archive:example.org/account_data/m.direct", http.StatusBadRequest, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			source, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			before, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			f.mu.Lock()
			f.round = 1
			f.limited = true
			f.failurePath = "/_matrix/client/v3" + tc.path
			f.failureStatus = tc.status
			f.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.Error(err)
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sourceID)
			require.NoError(err)
			assert.Equal(tc.roomDenied, allowed, "only a scoped denial keeps independent account authority")
			currentSource, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			if tc.accountDenied {
				assert.NotEqual(source.Reference, currentSource.Reference)
				return
			}
			assert.Equal(source.Reference, currentSource.Reference, "uncertainty and room denial cannot assert account removal")
			current, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			assert.Equal(!tc.roomDenied, current.Present)
			assert.Equal(before.Identity.Generation, current.Identity.Generation)
			quiet, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!quiet:example.org")
			require.NoError(err)
			require.True(found)
			assert.True(quiet.Present)
		})
	}
}

func TestNativeMatrixHistoryDenialCannotRetireReplacementLifetime(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
	require.NoError(err)
	before, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
	require.NoError(err)
	require.True(found)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.mu.Lock()
	f.round = 1
	f.limited = true
	f.failurePath = "/_matrix/client/v3/rooms/!room:example.org/messages"
	f.failureStatus = http.StatusForbidden
	f.failureEntered = entered
	f.failureRelease = release
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, importErr := imp.Import(t.Context(), opts); done <- importErr }()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		require.FailNow("history request did not reach observed barrier")
	}
	observation := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: source.Reference, SourceType: SourceType, NativeIdentity: opts.UserID, Kind: "matrix_room", RetiredProviderIDs: []string{"!room:example.org"}}
	require.NoError(imp.store.ObserveNativeMessageScopes(t.Context(), observation))
	observation.RetiredProviderIDs = nil
	observation.Scopes = []store.NativeMessageScope{{ProviderID: "!room:example.org", DisplayName: "Restored synthetic room"}}
	require.NoError(imp.store.ObserveNativeMessageScopes(t.Context(), observation))
	replacement, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
	require.NoError(err)
	require.True(found)
	require.NotEqual(before.Identity.Generation, replacement.Identity.Generation)
	unblock()
	select {
	case importErr := <-done:
		require.Error(importErr)
	case <-time.After(30 * time.Second):
		require.FailNow("native importer did not settle after response release")
	}
	after, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
	require.NoError(err)
	require.True(found)
	assert.True(after.Present, "stale denied response cannot retire replacement membership")
	assert.Equal(replacement.Identity.Generation, after.Identity.Generation)
}
