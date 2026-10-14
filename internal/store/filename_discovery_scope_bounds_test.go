package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

// Closing the real pool makes accidental SQL at these input boundaries fail.
// The public method must reject oversized supplied arrays before deduplication.
func TestFilenameSuggestionInputBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := testutil.NewTestStore(t)
	require.NoError(s.DB().Close())
	empty, err := s.FilenameSuggestionRowsContext(t.Context(), nil, store.FilenameSuggestionScope{})
	require.NoError(err)
	assert.Empty(empty)
	empty, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{Person: &personscope.Scope{Directions: []personscope.Direction{personscope.FromPerson}}})
	require.NoError(err)
	assert.Empty(empty, "empty mandatory person population never checks out SQL")
	ids := make([]int64, 501)
	for i := range ids {
		ids[i] = 1
	}
	_, err = s.FilenameSuggestionRowsContext(t.Context(), ids, store.FilenameSuggestionScope{})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	for _, invalid := range []int64{0, -1} {
		_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{invalid}, store.FilenameSuggestionScope{})
		require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	}
	sources := make([]int64, 65)
	for i := range sources {
		sources[i] = 1
	}
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{SourceIDs: sources})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{SourceIDs: []int64{-1}})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{MIMEFamilies: []string{"unsupported"}})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	mimes := make([]string, 9)
	for i := range mimes {
		mimes[i] = "image"
	}
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{MIMEFamilies: mimes})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	scopes := make([]search.AccountScope, 17)
	for i := range scopes {
		scopes[i] = search.AccountScope{Unattributed: true}
	}
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{AccountScopes: scopes})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
	members := make([]int64, 65)
	for i := range members {
		members[i] = 1
	}
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{Person: &personscope.Scope{ParticipantIDs: members, Directions: []personscope.Direction{personscope.FromPerson}}})
	require.ErrorIs(err, store.ErrFilenameScopeTooLarge)
	_, err = s.FilenameSuggestionRowsContext(t.Context(), []int64{1}, store.FilenameSuggestionScope{Person: &personscope.Scope{ParticipantIDs: []int64{1}, Directions: []personscope.Direction{"unknown"}}})
	require.ErrorIs(err, store.ErrFilenameRequestInvalid)
}
