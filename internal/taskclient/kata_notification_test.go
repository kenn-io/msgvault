package taskclient

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

func TestKataNotifyIssue(t *testing.T) {
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(t, err)
	issue, err := client.CreateTask(t.Context(), katatest.Project, "notify-example", KataCreate{Title: "Prepare for an occasion"})
	require.NoError(t, err)
	err = client.NotifyIssue(t.Context(), katatest.Project, issue.UID, "owner/assistant", "Review preparation item")
	require.NoError(t, err)
	updated, err := client.GetTask(t.Context(), katatest.Project, issue.UID)
	require.NoError(t, err)
	key := "notify." + base64.RawURLEncoding.EncodeToString([]byte("owner/assistant"))
	assert.Contains(t, updated.Metadata, key)
	native.Endpoint(server).CloseIssue(t, issue.UID)
	assert.Error(t, client.NotifyIssue(t.Context(), katatest.Project, issue.UID, "owner/assistant", "Review preparation item"))
}
