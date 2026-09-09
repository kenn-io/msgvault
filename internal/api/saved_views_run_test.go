package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/explorecatalog"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// newSavedViewRunTestServer serves Saved Views from a real SQLite store and
// runs them against the committed DuckDB explore fixture: two live messages in
// source 1 and one in source 2.
func newSavedViewRunTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          &mockStore{},
		SavedViewStore: st,
		Engine:         newExploreDuckDBFixture(t),
		Logger:         testLogger(),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv, st
}

func createRunTestView(t *testing.T, st *store.Store, name, state string) *store.SavedView {
	t.Helper()
	view, err := st.CreateSavedView(context.Background(), store.SavedViewInput{
		Name: name, CanonicalState: json.RawMessage(state), SchemaVersion: store.CurrentSavedViewSchemaVersion,
	})
	require.NoError(t, err)
	return view
}

func runSavedView(t *testing.T, srv *Server, id int64, body string) (*httptest.ResponseRecorder, RunSavedViewResponse) {
	t.Helper()
	response := postExploreJSON(t, srv, savedViewsPath+"/"+strconv.FormatInt(id, 10)+"/run", body)
	var page RunSavedViewResponse
	if response.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page), response.Body.String())
	}
	return response, page
}

func TestRunSavedViewExecutesEntriesWithTheExploreContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := newSavedViewRunTestServer(t)
	view := createRunTestView(t, st, "Primary archive", `{
		"filters":[{"field":"source_id","operator":"in","values":["1"]}],
		"presentation":"timeline",
		"sort":[{"field":"occurred_at","direction":"desc"}],
		"columns":["kind","title"]
	}`)

	first, page := runSavedView(t, srv, view.ID, `{"limit":1}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	assertions.Equal(view.ID, page.SavedView.ID)
	assertions.JSONEq(string(view.CanonicalState), string(page.SavedView.CanonicalState))
	assertions.Equal(string(savedview.ResultEntries), page.ResultKind)
	requirements.NotNil(page.TotalCount)
	assertions.Equal(int64(2), *page.TotalCount, "the legacy source_id alias filters the Explore source dimension")
	requirements.Len(page.Rows, 1)
	assertions.Equal("Newest", page.Rows[0].Title, "timeline runs the same descending entry query as table")
	assertions.NotEmpty(page.CacheRevision)
	requirements.NotEmpty(page.NextCursor)
	assertions.Empty(page.Groups)
	assertions.Empty(page.Files)

	second, next := runSavedView(t, srv, view.ID, `{"limit":1,"cursor":"`+page.NextCursor+`"}`)
	requirements.Equal(http.StatusOK, second.Code, second.Body.String())
	requirements.Len(next.Rows, 1)
	assertions.Equal("Older", next.Rows[0].Title)
	assertions.Empty(next.NextCursor)
}

func TestRunSavedViewExecutesGroupsAtTheFirstChainLevel(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := newSavedViewRunTestServer(t)
	view := createRunTestView(t, st, "By source then participant", `{"grouping":["source","participant"]}`)

	response, page := runSavedView(t, srv, view.ID, `{}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal(string(savedview.ResultGroups), page.ResultKind)
	requirements.Len(page.Groups, 2)
	assertions.Equal("1", page.Groups[0].Key)
	assertions.Equal(int64(2), page.Groups[0].Count)
	requirements.NotNil(page.TotalCount)
	assertions.Equal(int64(2), *page.TotalCount)
	assertions.Empty(page.Rows)
}

func TestRunSavedViewExecutesFilesPresentation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := newSavedViewRunTestServer(t)
	view := createRunTestView(t, st, "Attachments", `{"presentation":"files"}`)

	response, page := runSavedView(t, srv, view.ID, `{"limit":1}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal(string(savedview.ResultFiles), page.ResultKind)
	requirements.Len(page.Files, 1)
	assertions.Equal("newest.pdf", page.Files[0].Filename)
	requirements.NotNil(page.TotalCount)
	assertions.Equal(int64(2), *page.TotalCount)
	assertions.NotEmpty(page.NextCursor)
}

// staleSavedViewStore serves a definition an older binary could have written
// before the store enforced the executable vocabulary.
type staleSavedViewStore struct {
	SavedViewStore

	view store.SavedView
}

func (s *staleSavedViewStore) GetSavedView(context.Context, int64) (*store.SavedView, error) {
	view := s.view
	return &view, nil
}

func TestRunSavedViewRelaysSavedViewAndExploreErrors(t *testing.T) {
	assertions := assert.New(t)
	srv, st := newSavedViewRunTestServer(t)
	view := createRunTestView(t, st, "Everything", `{}`)

	missing, _ := runSavedView(t, srv, view.ID+1, `{}`)
	assertions.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
	assertions.Contains(missing.Body.String(), "saved_view_not_found")

	tooMany, _ := runSavedView(t, srv, view.ID, `{"limit":101}`)
	assertions.Equal(http.StatusBadRequest, tooMany.Code, tooMany.Body.String())
	assertions.Contains(tooMany.Body.String(), "invalid_limit")

	badCursor, _ := runSavedView(t, srv, view.ID, `{"cursor":"not-a-cursor"}`)
	assertions.Equal(http.StatusBadRequest, badCursor.Code, badCursor.Body.String())
	assertions.Contains(badCursor.Body.String(), "invalid_cursor", "Explore errors pass through unchanged")

	srv.savedViewStore = &staleSavedViewStore{view: store.SavedView{
		ID: 1, Name: "Stale", SchemaVersion: 2, CanonicalState: json.RawMessage(`{}`), Revision: 1,
	}}
	stale, _ := runSavedView(t, srv, 1, `{}`)
	assertions.Equal(http.StatusBadRequest, stale.Code, stale.Body.String())
	assertions.Contains(stale.Body.String(), "invalid_saved_view")

	srv.savedViewStore = &staleSavedViewStore{view: store.SavedView{
		ID: 1, Name: "Stale", SchemaVersion: 1, Revision: 1,
		CanonicalState: json.RawMessage(`{"sort":[{"field":"occurred_at","direction":"asc"}]}`),
	}}
	ascending, _ := runSavedView(t, srv, 1, `{}`)
	assertions.Equal(http.StatusBadRequest, ascending.Code, ascending.Body.String())
	assertions.Contains(ascending.Body.String(), "sort[0] must be occurred_at desc")
}

func TestRunSavedViewRejectsInvalidStoredDefinitions(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	cases := map[string]string{
		"non-numeric source ID": `{"filters":[{"field":"source","operator":"in","values":["primary"]}]}`,
		"invalid timestamp":     `{"filters":[{"field":"after","operator":"eq","values":["yesterday"]}]}`,
		"semantic empty query":  `{"search_mode":"semantic"}`,
		"semantic blank query":  `{"query":" \t\n ","search_mode":"semantic"}`,
		"hybrid empty query":    `{"search_mode":"hybrid"}`,
		"hybrid blank query":    `{"query":" \t\n ","search_mode":"hybrid"}`,
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			// The store accepts these v1 definitions, as older API versions did.
			view := createRunTestView(t, st, name, state)
			response, _ := runSavedView(t, srv, view.ID, `{}`)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			var failure ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
			assert.Equal(t, "invalid_saved_view", failure.Error)
		})
	}
}

// TestSavedViewLifecycleThroughDaemonClient drives the daemon client the MCP
// server embeds against a real API server, store, and explore engine, so the
// revision contract and the run translation are proven on the production
// path rather than on canned responses.
func TestSavedViewLifecycleThroughDaemonClient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, _ := newSavedViewRunTestServer(t)
	daemon := httptest.NewServer(srv.Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client(), Context: t.Context(),
	})
	requirements.NoError(err)
	ctx := t.Context()

	description := "Quarterly review"
	created, err := client.CreateSavedView(ctx, store.SavedViewInput{
		Name: "Invoices", Description: &description, SchemaVersion: store.CurrentSavedViewSchemaVersion,
		CanonicalState: json.RawMessage(`{"filters":[{"field":"source","operator":"in","values":["1"]}],"presentation":"table"}`),
	})
	requirements.NoError(err)
	assertions.Equal(int64(1), created.Revision)

	_, err = client.CreateSavedView(ctx, store.SavedViewInput{
		Name: "Invoices", SchemaVersion: store.CurrentSavedViewSchemaVersion,
		CanonicalState: json.RawMessage(`{"filters":[{"field":"subject","operator":"eq","values":["x"]}]}`),
	})
	requirements.ErrorIs(err, store.ErrSavedViewInvalidState, "the store's vocabulary is the only one")

	listed, err := client.ListSavedViews(ctx)
	requirements.NoError(err)
	requirements.Len(listed, 1)
	assertions.Equal(created.ID, listed[0].ID)

	name, cleared := "Receipts", ""
	updated, err := client.UpdateSavedView(ctx, created.ID, created.Revision, savedview.Patch{
		Name: &name, Description: &cleared,
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), updated.Revision)
	assertions.Equal("Receipts", updated.Name)
	assertions.Nil(updated.Description, "an empty description clears it")

	_, err = client.UpdateSavedView(ctx, created.ID, created.Revision, savedview.Patch{Name: &name})
	requirements.ErrorIs(err, store.ErrSavedViewRevisionConflict)

	page, err := client.RunSavedView(ctx, created.ID, 10, "")
	requirements.NoError(err)
	assertions.Equal("Receipts", page.View.Name)
	assertions.Equal(savedview.ResultEntries, page.ResultKind)
	assertions.Equal(2, page.Returned())
	requirements.NotNil(page.TotalCount)
	assertions.Equal(int64(2), *page.TotalCount)
	assertions.Equal("Newest", page.Rows[0].Title)

	requirements.NoError(client.DeleteSavedView(ctx, created.ID, updated.Revision))
	_, err = client.GetSavedView(ctx, created.ID)
	requirements.ErrorIs(err, store.ErrSavedViewNotFound)
	_, err = client.RunSavedView(ctx, created.ID, 10, "")
	requirements.ErrorIs(err, store.ErrSavedViewNotFound)
}

// TestExploreRequestEnumsMatchTheCatalog pins the static struct tags the
// OpenAPI document is generated from to the catalog the store validates
// Saved Views against, so the two cannot drift apart silently.
func TestExploreRequestEnumsMatchTheCatalog(t *testing.T) {
	assertions := assert.New(t)
	enumTag := func(value any, field string) []string {
		structField, ok := reflect.TypeOf(value).FieldByName(field)
		require.True(t, ok, "field %s", field)
		return strings.Split(structField.Tag.Get("enum"), ",")
	}
	assertions.ElementsMatch(explorecatalog.FilterDimensions(), enumTag(ExploreFilter{}, "Dimension"))
	assertions.ElementsMatch(explorecatalog.SearchModes(), enumTag(ExploreHTTPRequest{}, "SearchMode"))
	assertions.ElementsMatch(explorecatalog.Presentations(), enumTag(ExploreHTTPRequest{}, "Presentation"))
	assertions.Equal([]string{explorecatalog.EntrySortField}, enumTag(ExploreSort{}, "Field"))
	assertions.Equal([]string{explorecatalog.EntrySortDirection}, enumTag(ExploreSort{}, "Direction"))
}

// TestRunSavedViewFilesDeclaresSemanticDeletionScope pins that a semantic
// files view reports the active-only narrowing instead of hiding it.
func TestRunSavedViewFilesDeclaresSemanticDeletionScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	st := testutil.NewSQLiteTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          &mockStore{messages: []APIMessage{{ID: 1}}, total: 1, stats: &StoreStats{}},
		SavedViewStore: st, Engine: engine, HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	view := createRunTestView(t, st, "Semantic files", `{"query":"alpha","search_mode":"semantic","presentation":"files"}`)

	response, page := runSavedView(t, srv, view.ID, `{}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal(string(savedview.ResultFiles), page.ResultKind)
	assertions.Equal("active", page.SearchDeletionScope)
	assertions.NotEmpty(page.CandidateSnapshotID)
}
