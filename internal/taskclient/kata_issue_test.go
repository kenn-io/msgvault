package taskclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

// A static Kata token carries no actor, so Kata rejects writes that omit one.
func TestKataStaticTokenWritesAsMsgvault(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(err)
	create := KataCreate{Title: "Send the revised budget", Body: "Synthetic body", Metadata: map[string]any{ActionMetadataKey: "action-a"}}
	created, reused, err := client.CreateTaskReused(t.Context(), "example", "native-key-a", create)
	require.NoError(err)
	assert.False(reused)
	again, reused, err := client.CreateTaskReused(t.Context(), "example", "native-key-a", create)
	require.NoError(err)
	assert.True(reused)
	assert.Equal(created.UID, again.UID)

	patched, err := client.MutateMetadata(t.Context(), "example", created.UID, created.Revision, map[string]any{"msgvault.list": "agenda"})
	require.NoError(err)
	assert.Equal("agenda", patched.Metadata["msgvault.list"])
}

func TestSearchOpenIssuesHashInTitle(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(err)
	created, err := client.CreateTask(t.Context(), katatest.Project, "hash-title", KataCreate{Title: "C#guide"})
	require.NoError(err)
	query := "C#guide"
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/search?q=%s&status=open&mode=lexical&limit=26", server.URL, native.ProjectID, url.QueryEscape(query))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	require.NoError(err)
	request.Header.Set("Authorization", "Bearer "+katatest.Token)
	response, err := server.Client().Do(request)
	require.NoError(err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(http.StatusOK, response.StatusCode)
	var result struct {
		Results []struct {
			Issue struct {
				UID string `json:"uid"`
			} `json:"issue"`
		} `json:"results"`
	}
	require.NoError(json.NewDecoder(response.Body).Decode(&result))
	require.Len(result.Results, 1, "native Kata lexical search")
	assert.Equal(created.UID, result.Results[0].Issue.UID)
	issues, _, err := client.SearchOpenIssues(t.Context(), katatest.Project, query, 25)
	require.NoError(err)
	require.Len(issues, 1, "msgvault search")
	assert.Equal(created.UID, issues[0].UID)
	partial, _, err := client.SearchOpenIssues(t.Context(), katatest.Project, "guid", 25)
	require.NoError(err)
	assert.Empty(partial)
}

func TestSearchOpenIssuesRefAndText(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(err)
	exact, err := client.CreateTask(t.Context(), katatest.Project, "exact-ref", KataCreate{Title: "Original title"})
	require.NoError(err)
	match, err := client.CreateTask(t.Context(), katatest.Project, "text-match", KataCreate{Title: exact.QualifiedRef})
	require.NoError(err)
	issues, truncated, err := client.SearchOpenIssues(t.Context(), katatest.Project, exact.QualifiedRef, 25)
	require.NoError(err)
	assert.False(truncated)
	require.Len(issues, 2)
	assert.Equal(exact.UID, issues[0].UID)
	assert.Equal(match.UID, issues[1].UID)
	for i, title := range []string{"other#" + exact.Ref, "example#missing"} {
		textMatch, err := client.CreateTask(t.Context(), katatest.Project, fmt.Sprintf("ref-like-title-%d", i), KataCreate{Title: title})
		require.NoError(err)
		results, _, err := client.SearchOpenIssues(t.Context(), katatest.Project, title, 25)
		require.NoError(err)
		require.Len(results, 1)
		assert.Equal(textMatch.UID, results[0].UID)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", server.URL, native.ProjectID, exact.UID), strings.NewReader(fmt.Sprintf(`{"actor":"msgvault","title":%q}`, exact.QualifiedRef)))
	require.NoError(err)
	request.Header.Set("Authorization", "Bearer "+katatest.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"rev-`+exact.Revision+`"`)
	response, err := server.Client().Do(request)
	require.NoError(err)
	require.Equal(http.StatusOK, response.StatusCode)
	require.NoError(response.Body.Close())
	for i := range 25 {
		_, err = client.CreateTask(t.Context(), katatest.Project, fmt.Sprintf("text-match-%d", i), KataCreate{Title: exact.QualifiedRef})
		require.NoError(err)
	}
	issues, truncated, err = client.SearchOpenIssues(t.Context(), katatest.Project, exact.QualifiedRef, 25)
	require.NoError(err)
	assert.True(truncated)
	require.Len(issues, 25)
	assert.Equal(exact.UID, issues[0].UID)
	for _, issue := range issues[1:] {
		assert.NotEqual(exact.UID, issue.UID)
	}
	native.Endpoint(server).CloseIssue(t, exact.UID)
	issues, truncated, err = client.SearchOpenIssues(t.Context(), katatest.Project, exact.QualifiedRef, 25)
	require.NoError(err)
	assert.True(truncated)
	require.Len(issues, 25)
	for _, issue := range issues {
		assert.NotEqual(exact.UID, issue.UID)
	}
}

// FindActionTask finds an issue by its marker, closed or not, and reports an
// unused marker as unused. A host grant scoped to one project refuses the
// cross-project list, which still reads as a miss.
func TestFindActionTask(t *testing.T) {
	t.Parallel()
	for name, native := range map[string]katatest.Kata{"static token": katatest.New(t), "project-scoped grant": katatest.NewScoped(t)} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			handler := native.Service.Handler()
			if name != "static token" {
				handler = native.Handler()
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/issues" {
					if strings.HasPrefix(r.URL.Query().Get("meta"), ActionMetadataKey+"=") {
						assert.Equal("oldest", r.URL.Query().Get("sort"))
					}
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
			require.NoError(err)
			metadata := map[string]any{ActionMetadataKey: "action-a", "msgvault.evidence": map[string]any{"version": 1, "entries": []any{}}}
			created, err := client.CreateTask(t.Context(), "example", "marker-key", KataCreate{Title: "Send the revised budget", Metadata: metadata})
			require.NoError(err)
			native.Endpoint(server).CloseIssue(t, created.UID)

			found, ok, err := client.FindActionTask(t.Context(), "example", "action-a")
			require.NoError(err)
			require.True(ok)
			assert.Equal(created.UID, found.UID)
			assert.Equal("closed", found.Status)
			_, ok, err = client.FindActionTask(t.Context(), "example", "action-b")
			require.NoError(err)
			assert.False(ok)

			citing, err := client.FindMetadataTasks(t.Context(), "example", "msgvault.evidence", 2)
			require.NoError(err)
			require.Len(citing, 1)
			assert.Equal(created.UID, citing[0].UID)
			assert.Equal("closed", citing[0].Status)
			delete(metadata, ActionMetadataKey)
			second, err := client.CreateTask(t.Context(), "example", "second-key", KataCreate{Title: "Review the budget", Metadata: metadata})
			require.NoError(err)
			citing, err = client.FindMetadataTasks(t.Context(), "example", "msgvault.evidence", 0)
			require.NoError(err)
			require.Len(citing, 2)
			assert.Equal(second.UID, citing[0].UID)
			assert.Equal(created.UID, citing[1].UID)
			citing, err = client.FindMetadataTasks(t.Context(), "example", "msgvault.missing", 2)
			require.NoError(err)
			assert.Empty(citing)
		})
	}
}

func TestOpenIssues(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	native := katatest.New(t)
	handler := native.Service.Handler()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet {
			assert.Equal("open", r.URL.Query().Get("status"))
			assert.False(r.URL.Query().Has("sort"))
			recorder := httptest.NewRecorder()
			r.Header.Del("Accept-Encoding")
			handler.ServeHTTP(recorder, r)
			var body map[string]any
			if !assert.NoError(json.Unmarshal(recorder.Body.Bytes(), &body)) {
				http.Error(w, "invalid list response", http.StatusInternalServerError)
				return
			}
			delete(body, "truncated")
			assert.NoError(json.NewEncoder(w).Encode(body))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/search") {
			assert.Equal("open", r.URL.Query().Get("status"))
			assert.Equal("lexical", r.URL.Query().Get("mode"))
			assert.Equal("26", r.URL.Query().Get("limit"))
			recorder := httptest.NewRecorder()
			r.Header.Del("Accept-Encoding")
			handler.ServeHTTP(recorder, r)
			var body map[string]any
			if !assert.NoError(json.Unmarshal(recorder.Body.Bytes(), &body)) {
				http.Error(w, "invalid search response", http.StatusInternalServerError)
				return
			}
			delete(body, "truncated") // Kata v0.18.0 omits this field.
			assert.NoError(json.NewEncoder(w).Encode(body))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project, MaxResponseBytes: 4 << 20})
	require.NoError(err)
	created, err := client.CreateTask(t.Context(), katatest.Project, "older-api-issue", KataCreate{Title: "Review the budget"})
	require.NoError(err)
	initial, initialTruncated, err := client.OpenIssues(t.Context(), katatest.Project, 25)
	require.NoError(err)
	require.Len(initial, 1)
	assert.Equal(created.UID, initial[0].UID)
	assert.False(initialTruncated)
	for i := range 30 {
		_, err := client.CreateTask(t.Context(), katatest.Project, fmt.Sprintf("other-%d", i), KataCreate{Title: fmt.Sprintf("Other issue %d", i)})
		require.NoError(err)
	}
	closed, err := client.CreateTask(t.Context(), katatest.Project, "closed-budget", KataCreate{Title: "Review the budget"})
	require.NoError(err)
	native.Endpoint(server).CloseIssue(t, closed.UID)
	byRef, refTruncated, err := client.SearchOpenIssues(t.Context(), katatest.Project, created.QualifiedRef, 25)
	require.NoError(err)
	assert.False(refTruncated)
	require.Len(byRef, 1)
	assert.Equal(created.UID, byRef[0].UID)
	for _, ref := range []string{closed.QualifiedRef, "other#" + created.Ref} {
		matches, truncated, err := client.SearchOpenIssues(t.Context(), katatest.Project, ref, 25)
		require.NoError(err)
		assert.False(truncated)
		assert.Empty(matches)
	}
	searched, searchTruncated, err := client.SearchOpenIssues(t.Context(), katatest.Project, "budget", 25)
	require.NoError(err)
	assert.False(searchTruncated)
	require.Len(searched, 1)
	assert.Equal(created.QualifiedRef, searched[0].QualifiedRef)
	many, manyTruncated, err := client.SearchOpenIssues(t.Context(), katatest.Project, "issue", 25)
	require.NoError(err)
	assert.Len(many, 25)
	assert.True(manyTruncated)
	issues, truncated, err := client.OpenIssues(t.Context(), katatest.Project, 25)
	require.NoError(err)
	assert.Len(issues, 25)
	assert.True(truncated)
}
