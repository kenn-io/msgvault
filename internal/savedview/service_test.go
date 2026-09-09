package savedview

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestExecutableStateDecodesCanonicalDefinition(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	state, err := ExecutableState(store.SavedView{
		ID: 7, SchemaVersion: store.CurrentSavedViewSchemaVersion,
		CanonicalState: json.RawMessage(`{
			"query":"invoice","search_mode":"hybrid",
			"filters":[{"field":"source_id","operator":"in","values":["3"]}],
			"grouping":["domain"],"presentation":"files",
			"sort":[{"field":"occurred_at","direction":"desc"}],
			"columns":["kind","title"]
		}`),
	})
	requirements.NoError(err)
	assertions.Equal("invoice", state.Query)
	assertions.Equal("hybrid", state.SearchMode)
	requirements.Len(state.Filters, 1)
	assertions.Equal("source_id", state.Filters[0].Field, "aliases are preserved in the decoded state")
	assertions.Equal([]string{"domain"}, state.Grouping)
	assertions.Equal("files", state.Presentation)
}

func TestExecutableStateRejectsWhatTheStoreWouldNotPersist(t *testing.T) {
	cases := []struct {
		name    string
		view    store.SavedView
		wantErr error
		message string
	}{
		{
			name:    "unsupported schema version",
			view:    store.SavedView{ID: 1, SchemaVersion: 2, CanonicalState: json.RawMessage(`{}`)},
			wantErr: store.ErrSavedViewUnsupportedSchemaVersion,
			message: "schema version 2",
		},
		{
			name:    "malformed state",
			view:    store.SavedView{ID: 2, SchemaVersion: 1, CanonicalState: json.RawMessage(`{"query":`)},
			wantErr: store.ErrSavedViewInvalidState,
			message: "saved view 2: invalid saved view canonical state",
		},
		{
			// A typed decoder would drop the unknown field and run an empty,
			// unfiltered definition instead of refusing the stale row.
			name:    "transient state a typed decoder would drop",
			view:    store.SavedView{ID: 4, SchemaVersion: 1, CanonicalState: json.RawMessage(`{"selection":[1]}`)},
			wantErr: store.ErrSavedViewInvalidState,
			message: "selection",
		},
		{
			name:    "explicit null a typed decoder would erase",
			view:    store.SavedView{ID: 5, SchemaVersion: 1, CanonicalState: json.RawMessage(`{"filters":null}`)},
			wantErr: store.ErrSavedViewInvalidState,
			message: "filters must not be null",
		},
		{
			// A row written before the store enforced the vocabulary must not
			// reach Explore, so the store's own validator runs on read too.
			name: "definition outside the store vocabulary",
			view: store.SavedView{ID: 3, SchemaVersion: 1, CanonicalState: json.RawMessage(
				`{"sort":[{"field":"occurred_at","direction":"asc"}]}`)},
			wantErr: store.ErrSavedViewInvalidState,
			message: "saved view 3: invalid saved view canonical state: sort[0] must be occurred_at desc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExecutableState(tc.view)
			require.ErrorIs(t, err, tc.wantErr)
			assert.ErrorContains(t, err, tc.message)
		})
	}
}

func TestRunPageReturnedCountsTheTypedArray(t *testing.T) {
	assertions := assert.New(t)
	assertions.Equal(0, (&RunPage{}).Returned())
	assertions.Equal(2, (&RunPage{ResultKind: ResultEntries, Rows: make([]query.EntryRow, 2)}).Returned())
	assertions.Equal(1, (&RunPage{ResultKind: ResultGroups, Groups: make([]query.ExploreGroupRow, 1)}).Returned())
	assertions.Equal(3, (&RunPage{ResultKind: ResultFiles, Files: make([]query.ExploreFileFact, 3)}).Returned())
}
