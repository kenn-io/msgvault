package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestPatchSavedViewItemsAndDefinitionOneRevision(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	view := createRunTestView(t, st, "Item patch", `{}`)
	source, err := st.GetOrCreateSource("gmail", "pin@example.com")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "pin", "Pin")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "pin-one", MessageType: "email"})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/%d", savedViewsPath, view.ID)
	headers := http.Header{"Authorization": []string{"Bearer " + testSessionAPIKey}, "If-Match": []string{savedViewETag(*view)}}
	body := fmt.Sprintf(`{"name":"Pinned view","canonical_state":{},"include_add":["source:%d:message:pin-one"]}`, source.ID)
	response := performSavedViewRequest(t, srv, http.MethodPatch, path, []byte(body), headers)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"include_added":1`)
	updated, err := st.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	got := performSavedViewRequest(t, srv, http.MethodGet, path, nil, headers)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	assert.Contains(t, got.Body.String(), "pin-one")
	headers.Set("If-Match", savedViewETag(*updated))
	rejected := performSavedViewRequest(t, srv, http.MethodPatch, path, []byte(`{"exclude_add":["source:999999:message:unknown"]}`), headers)
	assert.Equal(t, http.StatusUnprocessableEntity, rejected.Code, rejected.Body.String())
	assert.Contains(t, rejected.Body.String(), "saved_view_items_rejected")
	unchanged, err := st.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), unchanged.Revision)
}

func TestRunSavedViewFailsOnCorruptItemsJSON(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	view := createRunTestView(t, st, "Corrupt items", `{}`)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE saved_views SET items_json='{"version":2}' WHERE id=?`), view.ID)
	require.NoError(t, err)
	response, _ := runSavedView(t, srv, view.ID, `{}`)
	assert.Equal(t, 400, response.Code)
	assert.Contains(t, response.Body.String(), "items_json_invalid")
}
func TestExploreItemsSourceScopeAndMalformedKeys(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	source, err := st.GetOrCreateSource("gmail", "scope@example.com")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "scope", "Scope")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "scoped", MessageType: "email"})
	require.NoError(t, err)
	key := fmt.Sprintf("source:%d:message:scoped", source.ID)
	items, err := srv.resolveExploreItems(t.Context(), []string{key}, nil)
	require.NoError(t, err)
	assert.Equal(t, []int64{id}, items.IncludeMessageIDs)
	items, err = srv.resolveExploreItems(sourcescope.WithSourceIDs(t.Context(), []int64{}), []string{key}, nil)
	require.NoError(t, err)
	assert.Empty(t, items.IncludeMessageIDs)
	_, err = srv.resolveExploreItems(t.Context(), []string{"malformed"}, nil)
	require.Error(t, err)
	var apiErr *apiHTTPError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 400, apiErr.status)
	assert.Contains(t, apiErr.Message, "include_keys[0]")
}

func TestRunSavedViewItemsPopulationAndSourceScope(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	seedSavedViewItemArchive(t, st)
	view := createRunTestView(t, st, "Run membership", `{"filters":[{"field":"source_id","operator":"in","values":["1"]}]}`)
	_, err := st.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: 3, SourceID: 2}}, ExcludeAdd: []store.SavedViewItemRef{{MessageID: 2, SourceID: 1}}})
	require.NoError(t, err)
	response, page := runSavedView(t, srv, view.ID, `{}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, page.Rows, 2)
	assert.ElementsMatch(t, []string{"source:1:message:m1", "source:2:message:m3"}, []string{page.Rows[0].Key, page.Rows[1].Key})
	require.NotNil(t, page.TotalCount)
	assert.Equal(t, int64(2), *page.TotalCount)
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("%s/%d/run", savedViewsPath, view.ID), strings.NewReader(`{}`))
	request.SetPathValue("id", fmt.Sprint(view.ID))
	request = request.WithContext(sourcescope.WithSourceIDs(t.Context(), []int64{1}))
	scoped := httptest.NewRecorder()
	srv.handleRunSavedView(scoped, request)
	require.Equal(t, http.StatusOK, scoped.Code, scoped.Body.String())
	require.NoError(t, json.Unmarshal(scoped.Body.Bytes(), &page))
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "source:1:message:m1", page.Rows[0].Key)
	assert.Equal(t, 1, page.SavedView.RedactedItemCount)
	assert.Equal(t, 0, page.SavedView.IncludeCount)
}

func TestExploreItemsHTTPMalformedAndUnknownKeys(t *testing.T) {
	srv, _ := newSavedViewRunTestServer(t)
	malformed := postExploreJSON(t, srv, "/api/v1/explore", `{"include_keys":["wrong"]}`)
	assert.Equal(t, http.StatusBadRequest, malformed.Code, malformed.Body.String())
	assert.Contains(t, malformed.Body.String(), "invalid_entry_key")
	assert.Contains(t, malformed.Body.String(), "include_keys[0]")
	unknown := postExploreJSON(t, srv, "/api/v1/explore", `{"include_keys":["source:999999:message:missing"]}`)
	require.Equal(t, http.StatusOK, unknown.Code, unknown.Body.String())
	var page ExploreHTTPResponse
	require.NoError(t, json.Unmarshal(unknown.Body.Bytes(), &page))
	require.NotNil(t, page.TotalCount)
	assert.Equal(t, int64(3), *page.TotalCount)
}

func seedSavedViewItemArchive(t *testing.T, st *store.Store) {
	t.Helper()
	for sourceIndex := 1; sourceIndex <= 2; sourceIndex++ {
		source, err := st.GetOrCreateSource("gmail", fmt.Sprintf("run-%d@example.com", sourceIndex))
		require.NoError(t, err)
		require.Equal(t, int64(sourceIndex), source.ID)
		for _, id := range map[int][]int{1: {1, 2}, 2: {3}}[sourceIndex] {
			conv, err := st.EnsureConversation(source.ID, fmt.Sprint(id), "Run item")
			require.NoError(t, err)
			inserted, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: fmt.Sprintf("m%d", id), MessageType: "email"})
			require.NoError(t, err)
			require.Equal(t, int64(id), inserted)
		}
	}
}

func TestExploreItemsAggregatePopulation(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	seedSavedViewItemArchive(t, st)
	const predicate = `{"filters":[{"dimension":"source","values":["2"]}],"include_keys":["source:1:message:m1"],"exclude_keys":["source:2:message:m3"]}`
	files := postExploreJSON(t, srv, "/api/v1/files/groups", `{"predicate":`+predicate+`,"grouping":["message_type"]}`)
	require.Equal(t, http.StatusOK, files.Code, files.Body.String())
	var groups FileGroupsHTTPResponse
	require.NoError(t, json.Unmarshal(files.Body.Bytes(), &groups))
	require.Len(t, groups.Rows, 1)
	assert.Equal(t, int64(1), groups.Rows[0].Count)
	for _, path := range []string{"/api/v1/participants/search", "/api/v1/domains/search"} {
		response := postExploreJSON(t, srv, path, `{"predicate":`+predicate+`}`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var population struct {
			Rows []struct {
				ActivityCount int64 `json:"activity_count"`
			} `json:"rows"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &population))
		require.Len(t, population.Rows, 1, path)
		assert.Equal(t, int64(1), population.Rows[0].ActivityCount, path)
	}
	for _, path := range []string{"/api/v1/participants/1/summary", "/api/v1/domains/example.com/summary"} {
		response := postExploreJSON(t, srv, path, predicate)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var population struct {
			Summary struct {
				ActivityCount int64 `json:"activity_count"`
			} `json:"summary"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &population))
		assert.Equal(t, int64(1), population.Summary.ActivityCount, path)
	}
}

func TestSavedViewItemsCorruptionListAndRead(t *testing.T) {
	srv, st := newSavedViewRunTestServer(t)
	healthy := createRunTestView(t, st, "Healthy", `{}`)
	corrupt := createRunTestView(t, st, "Corrupt", `{}`)
	for _, raw := range []string{`{"version":2}`, `{"version":1,"include":[{"message_id":1,"source_id":1,"added_by":"` + strings.Repeat("a", 128<<10) + `"}]}`} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE saved_views SET items_json=? WHERE id=?`), raw, corrupt.ID)
		require.NoError(t, err)
		headers := http.Header{"Authorization": []string{"Bearer " + testSessionAPIKey}}
		response := performSavedViewRequest(t, srv, http.MethodGet, savedViewsPath, nil, headers)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var list SavedViewsResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &list))
		require.Len(t, list.SavedViews, 2)
		for _, view := range list.SavedViews {
			if view.ID == healthy.ID {
				assert.Empty(t, view.IncompatibilityReason)
			} else {
				assert.Equal(t, "items_json_invalid", view.IncompatibilityReason)
			}
		}
		get := performSavedViewRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/%d", savedViewsPath, corrupt.ID), nil, headers)
		assert.Equal(t, http.StatusBadRequest, get.Code)
		assert.Contains(t, get.Body.String(), `"error":"invalid_saved_view"`)
		assert.Contains(t, get.Body.String(), "items_json_invalid")
		run, _ := runSavedView(t, srv, corrupt.ID, `{}`)
		assert.Equal(t, http.StatusBadRequest, run.Code)
		assert.Contains(t, run.Body.String(), "items_json_invalid")
	}
}
