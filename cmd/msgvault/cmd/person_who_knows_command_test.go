package cmd

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWhoKnowsCommandValidatesBeforeConnecting(t *testing.T) {
	command := newPersonWhoKnowsCommand()
	command.SetArgs([]string{"cloud", "--half-life", "-1"})
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	err := command.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--half-life")
}

func TestWhoKnowsCommandJSONPreservesDaemonBody(t *testing.T) {
	const body = "{ \"topic\": \"cloud\", \"people\": [], \"reason\": null, \"future_field\": {\"precision\": 0.123456789012345} }\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/people/who-knows", r.URL.Path)
		assert.Equal(t, "cloud", r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	command := newPersonWhoKnowsCommand()
	command.SetContext(ctx)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{"cloud", "--json"})
	require.NoError(t, command.Execute())
	assert.Equal(t, body, output.String()) //nolint:testifylint // The CLI must preserve whitespace and unknown fields byte for byte.
}
