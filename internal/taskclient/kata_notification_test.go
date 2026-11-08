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
	checks := assert.New(t)
	must := require.New(t)
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	must.NoError(err)
	issue, err := client.CreateTask(t.Context(), katatest.Project, "notify-example", KataCreate{Title: "Prepare for an occasion"})
	must.NoError(err)
	err = client.NotifyIssue(t.Context(), katatest.Project, issue.UID, "owner/assistant", "Review preparation item")
	must.NoError(err)
	updated, err := client.GetTask(t.Context(), katatest.Project, issue.UID)
	must.NoError(err)
	key := "notify." + base64.RawURLEncoding.EncodeToString([]byte("owner/assistant"))
	checks.Contains(updated.Metadata, key)
	native.Endpoint(server).CloseIssue(t, issue.UID)
	must.Error(client.NotifyIssue(t.Context(), katatest.Project, issue.UID, "owner/assistant", "Review preparation item"))
}
