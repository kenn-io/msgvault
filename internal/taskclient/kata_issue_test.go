package taskclient

import (
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
