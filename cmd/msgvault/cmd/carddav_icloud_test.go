package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
)

// serial: supplies the existing CLI password reader with synthetic stdin.
func TestAddICloudUsesPublishedBootstrapAndAppPassword(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/carddav/account", r.URL.Path)
		assert.NoError(json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"base_url":"https://p42-contacts.icloud.com","username":"synthetic@example.net","enabled":true,"books":1}`))
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	reader, writer, err := os.Pipe()
	require.NoError(err)
	_, err = writer.WriteString("synthetic-app-password\n")
	require.NoError(err)
	require.NoError(writer.Close())
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = previous; _ = reader.Close() })
	cmd := newAddCardDAVCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--icloud", "synthetic@example.net"})
	require.NoError(cmd.Execute())
	assert.Equal(carddav.ICloudDiscoveryURL, body["base_url"])
	assert.Equal("synthetic@example.net", body["username"])
	assert.Equal("synthetic-app-password", body["password"])
	assert.NotContains(body, "provider")
}

func TestAddICloudRejectsOtherProviderFlagsBeforePassword(t *testing.T) {
	for _, flag := range []string{"--google", "--microsoft"} {
		cmd := newAddCardDAVCmd()
		cmd.SetArgs([]string{"--icloud", flag, "synthetic@example.net"})
		require.ErrorContains(t, cmd.Execute(), "--icloud cannot be combined with --google or --microsoft")
	}
}
