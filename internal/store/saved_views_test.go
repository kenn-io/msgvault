package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

var savedViewState = json.RawMessage(`{
	"query":"invoice",
	"search_mode":"full_text",
	"filters":[{"field":"source_id","operator":"in","values":["1"]}],
	"grouping":["source"],
	"presentation":"table",
	"sort":[{"field":"count","direction":"desc"}],
	"columns":["sender"],
	"inspector_pinned":true
}`)

func savedViewInput(name string) store.SavedViewInput {
	description := "Quarterly review"
	return store.SavedViewInput{
		Name:           name,
		Description:    &description,
		CanonicalState: savedViewState,
		SchemaVersion:  store.CurrentSavedViewSchemaVersion,
	}
}

func TestSavedViewsSQLiteSchema(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)

	rows, err := st.DB().Query(`PRAGMA table_info('saved_views')`)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		requirements.NoError(rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey))
		columns[name] = true
	}
	requirements.NoError(rows.Err())
	assert.Equal(t, map[string]bool{
		"id": true, "name": true, "description": true, "canonical_state": true,
		"schema_version": true, "revision": true, "created_at": true, "updated_at": true,
	}, columns)
}

func TestSavedViewsCRUDAndStableListOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	zebra, err := st.CreateSavedView(ctx, savedViewInput("Zebra"))
	requirements.NoError(err)
	requirements.NotZero(zebra.ID)
	assertions.Equal(int64(1), zebra.Revision)
	assertions.False(zebra.CreatedAt.IsZero())
	assertions.Equal(zebra.CreatedAt, zebra.UpdatedAt)
	assertions.JSONEq(string(savedViewState), string(zebra.CanonicalState))

	alpha, err := st.CreateSavedView(ctx, savedViewInput("alpha"))
	requirements.NoError(err)

	views, err := st.ListSavedViews(ctx)
	requirements.NoError(err)
	requirements.Len(views, 2)
	assertions.Equal([]int64{alpha.ID, zebra.ID}, []int64{views[0].ID, views[1].ID})

	got, err := st.GetSavedView(ctx, zebra.ID)
	requirements.NoError(err)
	assertions.Equal(zebra, got)

	updatedInput := savedViewInput("Beta")
	updated, err := st.UpdateSavedView(ctx, zebra.ID, zebra.Revision, updatedInput)
	requirements.NoError(err)
	assertions.Equal(int64(2), updated.Revision)
	assertions.Equal("Beta", updated.Name)
	assertions.Equal(zebra.CreatedAt, updated.CreatedAt)
	assertions.False(updated.UpdatedAt.Before(updated.CreatedAt))

	requirements.NoError(st.DeleteSavedView(ctx, updated.ID, updated.Revision))
	_, err = st.GetSavedView(ctx, updated.ID)
	assertions.ErrorIs(err, store.ErrSavedViewNotFound)
}

func TestSavedViewsRejectDuplicateNamesAndStaleRevisions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	created, err := st.CreateSavedView(ctx, savedViewInput("Invoices"))
	requirements.NoError(err)

	_, err = st.CreateSavedView(ctx, savedViewInput("Invoices"))
	requirements.ErrorIs(err, store.ErrSavedViewNameConflict)

	updated, err := st.UpdateSavedView(ctx, created.ID, created.Revision, savedViewInput("Invoices 2026"))
	requirements.NoError(err)
	_, err = st.UpdateSavedView(ctx, created.ID, created.Revision, savedViewInput("Stale edit"))
	requirements.ErrorIs(err, store.ErrSavedViewRevisionConflict)
	requirements.ErrorIs(st.DeleteSavedView(ctx, created.ID, created.Revision), store.ErrSavedViewRevisionConflict)
	requirements.NoError(st.DeleteSavedView(ctx, updated.ID, updated.Revision))
	assertions.ErrorIs(st.DeleteSavedView(ctx, updated.ID, updated.Revision), store.ErrSavedViewNotFound)
}

func TestSavedViewsValidateCanonicalStateBeforePersistence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	tests := []struct {
		name    string
		version int
		state   string
		wantErr error
	}{
		{name: "malformed JSON", version: 1, state: `{`, wantErr: store.ErrSavedViewInvalidState},
		{name: "unsupported schema", version: 99, state: `{}`, wantErr: store.ErrSavedViewUnsupportedSchemaVersion},
		{name: "result rows", version: 1, state: `{"results":[{"id":1}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "selection", version: 1, state: `{"selection":[1]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "nested result rows", version: 1, state: `{"filters":{"results":[{"id":1}]}}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "nested selection", version: 1, state: `{"presentation":{"selection":[1]}}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "result rows in typed filter", version: 1, state: `{"filters":[{"field":"sender","operator":"eq","values":["alice@example.com"],"results":[{"id":1}]}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "selection in typed sort", version: 1, state: `{"sort":[{"field":"sent_at","direction":"desc","selection":[1]}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "wrong nested filter type", version: 1, state: `{"filters":[{"field":17,"operator":"eq","values":["x"]}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "wrong nested sort type", version: 1, state: `{"sort":[{"field":"sent_at","direction":["desc"]}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "null filters", version: 1, state: `{"filters":null}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "null filter values", version: 1, state: `{"filters":[{"field":"source_id","operator":"eq","values":null}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "null filter value element", version: 1, state: `{"filters":[{"field":"source_id","operator":"eq","values":[null]}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "null presentation", version: 1, state: `{"presentation":null}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "null canonical state", version: 1, state: `null`, wantErr: store.ErrSavedViewInvalidState},
		{name: "mixed-case top-level field", version: 1, state: `{"Filters":null}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "uppercase top-level field", version: 1, state: `{"QUERY":"invoice"}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "mixed-case nested filter field", version: 1, state: `{"filters":[{"Field":"sender","operator":"eq","values":["alice@example.com"]}]}`, wantErr: store.ErrSavedViewInvalidState},
		{name: "mixed-case nested sort field", version: 1, state: `{"sort":[{"field":"sent_at","Direction":"desc"}]}`, wantErr: store.ErrSavedViewInvalidState},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := savedViewInput(tt.name)
			input.SchemaVersion = tt.version
			input.CanonicalState = json.RawMessage(tt.state)
			_, err := st.CreateSavedView(ctx, input)
			assertions.ErrorIs(err, tt.wantErr)
		})
	}

	views, err := st.ListSavedViews(ctx)
	requirements.NoError(err)
	assertions.Empty(views)
}

func TestSavedViewsPersistOnlyServerGroupingDimensions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	supported := []string{"source", "participant", "domain", "message_type", "kind", "year", "month"}

	for _, dimension := range supported {
		t.Run("accept "+dimension, func(t *testing.T) {
			subAssertions := assert.New(t)
			subRequirements := require.New(t)
			input := savedViewInput("Group by " + dimension)
			input.CanonicalState = json.RawMessage(`{"grouping":["` + dimension + `"]}`)
			created, err := st.CreateSavedView(ctx, input)
			subRequirements.NoError(err)
			subAssertions.JSONEq(`{"grouping":["`+dimension+`"]}`, string(created.CanonicalState))
		})
	}

	invalid := savedViewInput("Unsupported grouping")
	invalid.CanonicalState = json.RawMessage(`{"grouping":["sender"]}`)
	_, err := st.CreateSavedView(ctx, invalid)
	requirements.ErrorIs(err, store.ErrSavedViewInvalidState)

	created, err := st.CreateSavedView(ctx, store.SavedViewInput{
		Name: "Valid before update", CanonicalState: json.RawMessage(`{"grouping":["source"]}`),
		SchemaVersion: store.CurrentSavedViewSchemaVersion,
	})
	requirements.NoError(err)
	invalid.Name = "Invalid update"
	_, err = st.UpdateSavedView(ctx, created.ID, created.Revision, invalid)
	requirements.ErrorIs(err, store.ErrSavedViewInvalidState)
	unchanged, err := st.GetSavedView(ctx, created.ID)
	requirements.NoError(err)
	assertions.Equal(int64(1), unchanged.Revision)
	assertions.JSONEq(`{"grouping":["source"]}`, string(unchanged.CanonicalState))
}

func TestSavedViewsPreserveLargeNumericFilterIDs(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	input := savedViewInput("Large source ID")
	input.CanonicalState = json.RawMessage(`{"filters":[{"field":"source_id","operator":"eq","values":["9223372036854775807"]}]}`)

	created, err := st.CreateSavedView(ctx, input)
	require.NoError(t, err)
	assert.Contains(t, string(created.CanonicalState), "9223372036854775807",
		"canonicalization must not round 64-bit identifiers")
}
