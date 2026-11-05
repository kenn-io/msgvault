package taskclient

import (
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
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(err)
	open, err := client.CreateTask(t.Context(), katatest.Project, "open-budget", KataCreate{Title: "Send the budget"})
	require.NoError(err)
	for i := range 30 {
		_, err := client.CreateTask(t.Context(), katatest.Project, fmt.Sprintf("other-%d", i), KataCreate{Title: fmt.Sprintf("Other issue %d", i), Body: strings.Repeat("Unrelated detail. ", 20000)})
		require.NoError(err)
	}
	closed, err := client.CreateTask(t.Context(), katatest.Project, "closed-budget", KataCreate{Title: "Review the budget"})
	require.NoError(err)
	native.Endpoint(server).CloseIssue(t, closed.UID)
	issues, truncated, err := client.OpenIssues(t.Context(), katatest.Project, 25)
	require.NoError(err)
	assert.Len(issues, 25)
	assert.True(truncated)
	assert.Equal("Other issue 29", issues[0].Title)
	for _, issue := range issues {
		assert.Equal("open", issue.Status)
		assert.NotEqual(open.UID, issue.UID)
		assert.NotEqual(closed.UID, issue.UID)
	}
}

func TestOpenIssuesWithOlderKataAPI(t *testing.T) {
	t.Parallel()
	native := katatest.New(t)
	handler := native.Service.Handler()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writeTestJSON(t, w, map[string]any{"api_schema_version": "0.21.0"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet && r.URL.Query().Get("sort") == "created" {
			http.Error(w, "unsupported sort", http.StatusUnprocessableEntity)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(t, err)
	created, err := client.CreateTask(t.Context(), katatest.Project, "older-api-issue", KataCreate{Title: "Review the budget"})
	require.NoError(t, err)
	issues, truncated, err := client.OpenIssues(t.Context(), katatest.Project, 25)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, created.UID, issues[0].UID)
	assert.False(t, truncated)
}
