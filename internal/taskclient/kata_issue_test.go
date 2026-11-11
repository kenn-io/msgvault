package taskclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestSearchOpenIssues(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	native := katatest.New(t)
	handler := native.Service.Handler()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/search") {
			assert.Equal("open", r.URL.Query().Get("status"))
			assert.Equal("lexical", r.URL.Query().Get("mode"))
			assert.Equal("26", r.URL.Query().Get("limit"))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(err)
	hash, err := client.CreateTask(t.Context(), katatest.Project, "hash-title", KataCreate{Title: "C#guide"})
	require.NoError(err)
	hashMatches, _, err := client.SearchOpenIssues(t.Context(), katatest.Project, "C#guide", 25)
	require.NoError(err)
	require.Len(hashMatches, 1)
	assert.Equal(hash.UID, hashMatches[0].UID)
	budget, err := client.CreateTask(t.Context(), katatest.Project, "budget", KataCreate{Title: "Review the budget"})
	require.NoError(err)
	closed, err := client.CreateTask(t.Context(), katatest.Project, "closed-budget", KataCreate{Title: "Review the budget"})
	require.NoError(err)
	native.Endpoint(server).CloseIssue(t, closed.UID)
	budgetMatches, _, err := client.SearchOpenIssues(t.Context(), katatest.Project, "budget", 25)
	require.NoError(err)
	require.Len(budgetMatches, 1)
	assert.Equal(budget.UID, budgetMatches[0].UID)
	closedMatches, _, err := client.SearchOpenIssues(t.Context(), katatest.Project, closed.QualifiedRef, 25)
	require.NoError(err)
	require.Len(closedMatches, 1)
	assert.Equal(closed.UID, closedMatches[0].UID)
	assert.Equal("closed", closedMatches[0].Status)
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
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(err)
	created, err := client.CreateTask(t.Context(), katatest.Project, "older-api-issue", KataCreate{Title: "Review the budget"})
	require.NoError(err)
	initial, initialTruncated, err := client.OpenIssues(t.Context(), katatest.Project, 25)
	require.NoError(err)
	require.Len(initial, 1)
	assert.Equal(created.UID, initial[0].UID)
	assert.False(initialTruncated)
	var newest KataTask
	for i := range 30 {
		newest, err = client.CreateTask(t.Context(), katatest.Project, fmt.Sprintf("other-%d", i), KataCreate{Title: fmt.Sprintf("Other issue %d", i)})
		require.NoError(err)
	}

	issues, truncated, err := client.OpenIssues(t.Context(), katatest.Project, 25)
	require.NoError(err)
	require.Len(issues, 25)
	assert.Equal(newest.UID, issues[0].UID)
	assert.True(truncated)
}
