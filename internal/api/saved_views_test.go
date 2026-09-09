package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestSavedViewsAuthenticatedCRUDAndSharedSessionVisibility(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newSavedViewTestServer(t)
	first := loginSavedViewSession(t, srv)
	second := loginSavedViewSession(t, srv)

	unauthorized := performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath, nil, nil)
	assertions.Equal(http.StatusUnauthorized, unauthorized.Code, unauthorized.Body.String())

	create := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{
			"name":"Invoices",
			"description":"Quarterly review",
			"canonical_state":{"query":"invoice","search_mode":"full_text","filters":[{"field":"source_id","operator":"in","values":["1"]}],"presentation":"table"},
			"schema_version":1
		}`), first.mutationHeaders())
	requirements.Equal(http.StatusCreated, create.Code, create.Body.String())
	created := decodeSavedView(t, create)
	assertions.NotZero(created.ID)
	assertions.Equal(int64(1), created.Revision)
	assertions.Equal(savedViewETag(created), create.Header().Get("ETag"))
	assertions.Equal(savedViewsPath+"/"+strconv.FormatInt(created.ID, 10), create.Header().Get("Location"))

	list := performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath, nil, second.headers())
	requirements.Equal(http.StatusOK, list.Code, list.Body.String())
	var listed SavedViewsResponse
	requirements.NoError(json.Unmarshal(list.Body.Bytes(), &listed))
	requirements.Len(listed.SavedViews, 1)
	assertions.Equal(created.ID, listed.SavedViews[0].ID,
		"a view created by one authenticated browser session is visible to another")

	itemPath := savedViewsPath + "/" + strconv.FormatInt(created.ID, 10)
	get := performSavedViewRequest(t, srv, http.MethodGet, itemPath, nil, second.headers())
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	assertions.Equal(savedViewETag(created), get.Header().Get("ETag"))

	patchHeaders := second.mutationHeaders()
	patchHeaders.Set("If-Match", get.Header().Get("ETag"))
	patch := performSavedViewRequest(t, srv, http.MethodPatch, itemPath,
		[]byte(`{"name":"Invoices 2026"}`), patchHeaders)
	requirements.Equal(http.StatusOK, patch.Code, patch.Body.String())
	updated := decodeSavedView(t, patch)
	assertions.Equal(int64(2), updated.Revision)
	assertions.Equal("Invoices 2026", updated.Name)
	assertions.Equal(savedViewETag(updated), patch.Header().Get("ETag"))

	staleHeaders := first.mutationHeaders()
	staleHeaders.Set("If-Match", savedViewETag(created))
	stale := performSavedViewRequest(t, srv, http.MethodPatch, itemPath,
		[]byte(`{"name":"Stale edit"}`), staleHeaders)
	assertions.Equal(http.StatusConflict, stale.Code, stale.Body.String())

	duplicate := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Invoices 2026","canonical_state":{},"schema_version":1}`), first.mutationHeaders())
	assertions.Equal(http.StatusConflict, duplicate.Code, duplicate.Body.String())

	missing := performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath+"/999999", nil, second.headers())
	assertions.Equal(http.StatusNotFound, missing.Code, missing.Body.String())

	deleteHeaders := first.mutationHeaders()
	deleteHeaders.Set("If-Match", savedViewETag(updated))
	deleted := performSavedViewRequest(t, srv, http.MethodDelete, itemPath, nil, deleteHeaders)
	assertions.Equal(http.StatusNoContent, deleted.Code, deleted.Body.String())
	missing = performSavedViewRequest(t, srv, http.MethodGet, itemPath, nil, second.headers())
	assertions.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
}

func TestSavedViewsAPIRejectsInvalidStateAndRevisionHeaders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newSavedViewTestServer(t)
	session := loginSavedViewSession(t, srv)

	invalid := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Invalid","canonical_state":{"selection":[1]},"schema_version":1}`),
		session.mutationHeaders())
	assertions.Equal(http.StatusBadRequest, invalid.Code, invalid.Body.String())

	nestedInvalid := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Nested invalid","canonical_state":{"filters":[{"field":"sender","operator":"eq","values":["alice@example.com"],"results":[{"id":1}]}]},"schema_version":1}`),
		session.mutationHeaders())
	assertions.Equal(http.StatusBadRequest, nestedInvalid.Code, nestedInvalid.Body.String())

	wrongNestedType := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Wrong nested type","canonical_state":{"sort":[{"field":"sent_at","direction":["desc"]}]},"schema_version":1}`),
		session.mutationHeaders())
	assertions.Equal(http.StatusBadRequest, wrongNestedType.Code, wrongNestedType.Body.String())

	for name, body := range map[string]string{
		"omitted": `{"name":"Missing state","schema_version":1}`,
		"null":    `{"name":"Null state","canonical_state":null,"schema_version":1}`,
	} {
		t.Run("create canonical state "+name, func(t *testing.T) {
			response := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
				[]byte(body), session.mutationHeaders())
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}

	createdResponse := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Current","canonical_state":{},"schema_version":1}`), session.mutationHeaders())
	requirements.Equal(http.StatusCreated, createdResponse.Code, createdResponse.Body.String())
	created := decodeSavedView(t, createdResponse)
	path := savedViewsPath + "/" + strconv.FormatInt(created.ID, 10)

	missingMatch := performSavedViewRequest(t, srv, http.MethodPatch, path,
		[]byte(`{"name":"Changed"}`), session.mutationHeaders())
	assertions.Equal(http.StatusPreconditionRequired, missingMatch.Code, missingMatch.Body.String())

	malformedMatchHeaders := session.mutationHeaders()
	malformedMatchHeaders.Set("If-Match", `"not-a-saved-view-revision"`)
	malformedMatch := performSavedViewRequest(t, srv, http.MethodDelete, path, nil, malformedMatchHeaders)
	assertions.Equal(http.StatusBadRequest, malformedMatch.Code, malformedMatch.Body.String())

	multipleMatchHeaders := session.mutationHeaders()
	multipleMatchHeaders.Add("If-Match", savedViewETag(created))
	multipleMatchHeaders.Add("If-Match", savedViewETag(created))
	multipleMatch := performSavedViewRequest(t, srv, http.MethodDelete, path, nil, multipleMatchHeaders)
	assertions.Equal(http.StatusBadRequest, multipleMatch.Code, multipleMatch.Body.String())

	patchOmissionHeaders := session.mutationHeaders()
	patchOmissionHeaders.Set("If-Match", savedViewETag(created))
	patchOmission := performSavedViewRequest(t, srv, http.MethodPatch, path,
		[]byte(`{"name":"Canonical state preserved"}`), patchOmissionHeaders)
	requirements.Equal(http.StatusOK, patchOmission.Code, patchOmission.Body.String())
	preserved := decodeSavedView(t, patchOmission)
	assertions.JSONEq(`{}`, string(preserved.CanonicalState))

	patchNullHeaders := session.mutationHeaders()
	patchNullHeaders.Set("If-Match", savedViewETag(preserved))
	patchNull := performSavedViewRequest(t, srv, http.MethodPatch, path,
		[]byte(`{"name":"Must not apply","canonical_state":null}`), patchNullHeaders)
	assertions.Equal(http.StatusBadRequest, patchNull.Code, patchNull.Body.String())
	unchanged := performSavedViewRequest(t, srv, http.MethodGet, path, nil, session.headers())
	requirements.Equal(http.StatusOK, unchanged.Code, unchanged.Body.String())
	assertions.Equal("Canonical state preserved", decodeSavedView(t, unchanged).Name)
}

func TestSavedViewsAPIValidatesServerGroupingCatalog(t *testing.T) {
	srv := newSavedViewTestServer(t)
	session := loginSavedViewSession(t, srv)

	for _, dimension := range exploreGroupDimensions {
		t.Run("accept "+string(dimension), func(t *testing.T) {
			body := fmt.Sprintf(
				`{"name":"Group %s","canonical_state":{"grouping":[%q]},"schema_version":1}`,
				dimension, dimension,
			)
			response := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
				[]byte(body), session.mutationHeaders())
			require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		})
	}

	invalid := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Unsupported","canonical_state":{"grouping":["sender"]},"schema_version":1}`),
		session.mutationHeaders())
	assert.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())

	createdResponse := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
		[]byte(`{"name":"Before invalid patch","canonical_state":{"grouping":["source"]},"schema_version":1}`),
		session.mutationHeaders())
	require.Equal(t, http.StatusCreated, createdResponse.Code, createdResponse.Body.String())
	created := decodeSavedView(t, createdResponse)
	headers := session.mutationHeaders()
	headers.Set("If-Match", savedViewETag(created))
	patched := performSavedViewRequest(t, srv, http.MethodPatch,
		savedViewsPath+"/"+strconv.FormatInt(created.ID, 10),
		[]byte(`{"canonical_state":{"grouping":["arbitrary"]}}`), headers)
	assert.Equal(t, http.StatusBadRequest, patched.Code, patched.Body.String())
}

func TestSavedViewsAPIRejectsMixedCaseJSONAliases(t *testing.T) {
	createBodies := map[string]string{
		"request name":            `{"Name":"Alias","canonical_state":{},"schema_version":1}`,
		"request canonical state": `{"name":"Alias","Canonical_State":{},"schema_version":1}`,
		"null request state":      `{"name":"Alias","CANONICAL_STATE":null,"schema_version":1}`,
		"envelope filters":        `{"name":"Alias","canonical_state":{"Filters":null},"schema_version":1}`,
		"nested filter field":     `{"name":"Alias","canonical_state":{"filters":[{"Field":"sender","operator":"eq","values":["alice@example.com"]}]},"schema_version":1}`,
		"nested sort direction":   `{"name":"Alias","canonical_state":{"sort":[{"field":"sent_at","Direction":"desc"}]},"schema_version":1}`,
	}
	for name, body := range createBodies {
		t.Run("create "+name, func(t *testing.T) {
			srv := newSavedViewTestServer(t)
			session := loginSavedViewSession(t, srv)
			response := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
				[]byte(body), session.mutationHeaders())
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}

	patchBodies := map[string]string{
		"request name":           `{"Name":"Must not apply"}`,
		"null request state":     `{"name":"Must not apply","Canonical_State":null}`,
		"envelope filters":       `{"canonical_state":{"Filters":null}}`,
		"nested filter operator": `{"canonical_state":{"filters":[{"field":"sender","Operator":"eq","values":["alice@example.com"]}]}}`,
		"nested sort direction":  `{"canonical_state":{"sort":[{"field":"sent_at","Direction":"desc"}]}}`,
	}
	for name, body := range patchBodies {
		t.Run("patch "+name, func(t *testing.T) {
			requirements := require.New(t)
			srv := newSavedViewTestServer(t)
			session := loginSavedViewSession(t, srv)
			createdResponse := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
				[]byte(`{"name":"Original","canonical_state":{},"schema_version":1}`), session.mutationHeaders())
			requirements.Equal(http.StatusCreated, createdResponse.Code, createdResponse.Body.String())
			created := decodeSavedView(t, createdResponse)
			path := savedViewsPath + "/" + strconv.FormatInt(created.ID, 10)
			headers := session.mutationHeaders()
			headers.Set("If-Match", savedViewETag(created))

			response := performSavedViewRequest(t, srv, http.MethodPatch, path, []byte(body), headers)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			gotResponse := performSavedViewRequest(t, srv, http.MethodGet, path, nil, session.headers())
			requirements.Equal(http.StatusOK, gotResponse.Code, gotResponse.Body.String())
			got := decodeSavedView(t, gotResponse)
			assert.Equal(t, "Original", got.Name)
			assert.Equal(t, int64(1), got.Revision)
		})
	}
}

type savedViewSession struct {
	cookie *http.Cookie
	csrf   string
}

func (s savedViewSession) headers() http.Header {
	return http.Header{"Cookie": []string{s.cookie.String()}}
}

func (s savedViewSession) mutationHeaders() http.Header {
	headers := s.headers()
	headers.Set("Origin", "http://example.com")
	headers.Set(csrfHeaderName, s.csrf)
	return headers
}

func newSavedViewTestServer(t *testing.T) *Server {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}},
		Store:          st,
		SavedViewStore: st,
		Logger:         testLogger(),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv
}

func loginSavedViewSession(t *testing.T, srv *Server) savedViewSession {
	t.Helper()
	login := performSavedViewRequest(t, srv, http.MethodPost, sessionLoginPath,
		[]byte(`{"api_key":"`+testSessionAPIKey+`"}`), nil)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	status := decodeSessionStatus(t, login)
	return savedViewSession{cookie: requireSessionCookie(t, login), csrf: status.CSRFToken}
}

func performSavedViewRequest(
	t *testing.T, srv *Server, method, path string, body []byte, headers http.Header,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://example.com"+path, bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.10:4242"
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

func decodeSavedView(t *testing.T, response *httptest.ResponseRecorder) store.SavedView {
	t.Helper()
	var view store.SavedView
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
	return view
}

// TestSavedViewsAPIRejectDefinitionsExploreCannotRun pins that a definition
// inside the store vocabulary but outside Explore's value rules is refused
// when it is saved, not when it is finally run.
func TestSavedViewsAPIRejectDefinitionsExploreCannotRun(t *testing.T) {
	cases := map[string]string{
		"non-numeric source ID":    `{"filters":[{"field":"source","operator":"in","values":["primary"]}]}`,
		"invalid timestamp":        `{"filters":[{"field":"after","operator":"eq","values":["yesterday"]}]}`,
		"duplicate timestamp":      `{"filters":[{"field":"after","operator":"eq","values":["2026-01-01T00:00:00Z"]},{"field":"after","operator":"eq","values":["2026-02-01T00:00:00Z"]}]}`,
		"identity without source":  `{"filters":[{"field":"identity","operator":"eq","values":["1","me@example.com","sender"]}]}`,
		"identity missing parts":   `{"filters":[{"field":"source","operator":"eq","values":["1"]},{"field":"identity","operator":"eq","values":["1"]}]}`,
		"unknown deletion value":   `{"filters":[{"field":"deletion","operator":"eq","values":["purged"]}]}`,
		"more than one sort entry": `{"sort":[{"field":"occurred_at","direction":"desc"},{"field":"occurred_at","direction":"desc"}]}`,
		"query syntax":             `{"query":"invoice after:yesterday","search_mode":"full_text"}`,
		"semantic deleted scope":   `{"query":"invoice","search_mode":"semantic","filters":[{"field":"deletion","operator":"eq","values":["deleted"]}]}`,
		"hybrid without free text": `{"query":"from:alice@example.com","search_mode":"hybrid"}`,
		"semantic empty query":     `{"search_mode":"semantic"}`,
		"semantic blank query":     `{"query":" \t\n ","search_mode":"semantic"}`,
		"hybrid empty query":       `{"search_mode":"hybrid"}`,
		"hybrid blank query":       `{"query":" \t\n ","search_mode":"hybrid"}`,
	}
	for name, state := range cases {
		t.Run("create "+name, func(t *testing.T) {
			srv := newSavedViewTestServer(t)
			session := loginSavedViewSession(t, srv)
			response := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
				[]byte(`{"name":"Invalid","canonical_state":`+state+`,"schema_version":1}`), session.mutationHeaders())
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "invalid_saved_view")
		})
	}

	t.Run("patch keeps the stored definition", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		srv := newSavedViewTestServer(t)
		session := loginSavedViewSession(t, srv)
		created := performSavedViewRequest(t, srv, http.MethodPost, savedViewsPath,
			[]byte(`{"name":"Valid","canonical_state":{"filters":[{"field":"source","operator":"eq","values":["1"]},{"field":"identity","operator":"eq","values":["1","me@example.com","sender"]}]},"schema_version":1}`),
			session.mutationHeaders())
		requirements.Equal(http.StatusCreated, created.Code, created.Body.String())
		view := decodeSavedView(t, created)
		itemPath := savedViewsPath + "/" + strconv.FormatInt(view.ID, 10)

		headers := session.mutationHeaders()
		headers.Set("If-Match", savedViewETag(view))
		patched := performSavedViewRequest(t, srv, http.MethodPatch, itemPath,
			[]byte(`{"canonical_state":`+cases["non-numeric source ID"]+`}`), headers)
		assertions.Equal(http.StatusBadRequest, patched.Code, patched.Body.String())
		assertions.Contains(patched.Body.String(), "invalid_saved_view")

		unchanged := performSavedViewRequest(t, srv, http.MethodGet, itemPath, nil, session.headers())
		requirements.Equal(http.StatusOK, unchanged.Code, unchanged.Body.String())
		assertions.Equal(int64(1), decodeSavedView(t, unchanged).Revision)
	})

	t.Run("metadata patch rejects an unsearchable stored definition", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		srv := newSavedViewTestServer(t)
		session := loginSavedViewSession(t, srv)
		view, err := srv.savedViewStore.CreateSavedView(t.Context(), store.SavedViewInput{
			Name: "Old view", SchemaVersion: store.CurrentSavedViewSchemaVersion,
			CanonicalState: json.RawMessage(cases["hybrid without free text"]),
		})
		requirements.NoError(err)
		itemPath := savedViewsPath + "/" + strconv.FormatInt(view.ID, 10)
		headers := session.mutationHeaders()
		headers.Set("If-Match", savedViewETag(*view))
		patched := performSavedViewRequest(t, srv, http.MethodPatch, itemPath,
			[]byte(`{"name":"Renamed"}`), headers)
		assertions.Equal(http.StatusBadRequest, patched.Code, patched.Body.String())
		var failure ErrorResponse
		requirements.NoError(json.Unmarshal(patched.Body.Bytes(), &failure))
		assertions.Equal("invalid_saved_view", failure.Error)

		unchanged, err := srv.savedViewStore.GetSavedView(t.Context(), view.ID)
		requirements.NoError(err)
		assertions.Equal(view.Name, unchanged.Name)
		assertions.Equal(view.Revision, unchanged.Revision)
	})
}

func TestSavedViewsReadReportsIncompatibleDefinitions(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		for _, query := range []string{"", " \t\n ", "from:alice@example.com", "invoice"} {
			t.Run(mode+"/"+query, func(t *testing.T) {
				srv := newSavedViewTestServer(t)
				session := loginSavedViewSession(t, srv)
				assertions := assert.New(t)
				requirements := require.New(t)
				state, err := json.Marshal(store.SavedViewStateEnvelope{Query: query, SearchMode: mode})
				requirements.NoError(err)
				view, err := srv.savedViewStore.CreateSavedView(t.Context(), store.SavedViewInput{
					Name: mode + query, CanonicalState: state, SchemaVersion: store.CurrentSavedViewSchemaVersion,
				})
				requirements.NoError(err)
				itemPath := savedViewsPath + "/" + strconv.FormatInt(view.ID, 10)
				response := performSavedViewRequest(t, srv, http.MethodGet, itemPath, nil, session.headers())
				requirements.Equal(http.StatusOK, response.Code, response.Body.String())
				var item SavedView
				requirements.NoError(json.Unmarshal(response.Body.Bytes(), &item))
				if query == "invoice" {
					assertions.Empty(item.IncompatibilityReason)
				} else {
					assertions.Contains(item.IncompatibilityReason, "free text")
				}
				listed := performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath, nil, session.headers())
				requirements.Equal(http.StatusOK, listed.Code, listed.Body.String())
				var library SavedViewsResponse
				requirements.NoError(json.Unmarshal(listed.Body.Bytes(), &library))
				requirements.Len(library.SavedViews, 1)
				assertions.Equal(item.IncompatibilityReason, library.SavedViews[0].IncompatibilityReason)
			})
		}
	}
}

func TestSavedViewsReadReportsUnsupportedSchema(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newSavedViewTestServer(t)
	session := loginSavedViewSession(t, srv)
	st, ok := srv.savedViewStore.(*store.Store)
	requirements.True(ok)
	view, err := st.CreateSavedView(t.Context(), store.SavedViewInput{
		Name: "Future view", CanonicalState: json.RawMessage(`{}`),
		SchemaVersion: store.CurrentSavedViewSchemaVersion,
	})
	requirements.NoError(err)
	// Simulate an archive containing a definition from another schema version.
	_, err = st.DB().ExecContext(t.Context(), "UPDATE saved_views SET schema_version = 2 WHERE id = ?", view.ID)
	requirements.NoError(err)
	itemPath := savedViewsPath + "/" + strconv.FormatInt(view.ID, 10)
	response := performSavedViewRequest(t, srv, http.MethodGet, itemPath, nil, session.headers())
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var item SavedView
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &item))
	assertions.Contains(item.IncompatibilityReason, "schema version 2")
	listed := performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath, nil, session.headers())
	requirements.Equal(http.StatusOK, listed.Code, listed.Body.String())
	var library SavedViewsResponse
	requirements.NoError(json.Unmarshal(listed.Body.Bytes(), &library))
	requirements.Len(library.SavedViews, 1)
	assertions.Equal(item.IncompatibilityReason, library.SavedViews[0].IncompatibilityReason)
}

func TestSavedViewsReadPreservesIncompatibleDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		state   string
	}{
		{"older sort", 1, `{"sort":[{"field":"size","direction":"asc"}]}`},
		{"future shape", 2, `{"query":{"text":"invoice"},"layout":"future"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv := newSavedViewTestServer(t)
			session := loginSavedViewSession(t, srv)
			st, ok := srv.savedViewStore.(*store.Store)
			requirements.True(ok)
			view, err := st.CreateSavedView(t.Context(), store.SavedViewInput{
				Name: tc.name, CanonicalState: json.RawMessage(`{}`), SchemaVersion: 1,
			})
			requirements.NoError(err)
			_, err = st.DB().ExecContext(t.Context(),
				"UPDATE saved_views SET canonical_state = ?, schema_version = ? WHERE id = ?", tc.state, tc.version, view.ID)
			requirements.NoError(err)
			path := savedViewsPath + "/" + strconv.FormatInt(view.ID, 10)
			response := performSavedViewRequest(t, srv, http.MethodGet, path, nil, session.headers())
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var item generated.SavedView
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &item))
			assertions.NoError(item.Validate())
			requirements.NotNil(item.IncompatibilityReason)
			assertions.NotEmpty(*item.IncompatibilityReason)
			state, err := json.Marshal(item.CanonicalState)
			requirements.NoError(err)
			assertions.JSONEq(tc.state, string(state))

			response = performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath, nil, session.headers())
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var library generated.SavedViewsResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &library))
			assertions.NoError(library.Validate())
			requirements.Len(library.SavedViews, 1)
			assertions.Equal(item, library.SavedViews[0])
		})
	}
}
