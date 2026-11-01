//go:build sqlite_vec

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
)

func TestLocalEmbeddingsKeepsIncrementalMessageGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	embedded := 0
	oldServer := fakeEmbeddingsServer(t, &embedded)
	dir := t.TempDir()
	seedTwoAccountMainDB(t, dir)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Endpoint = oldServer.URL
	cfg.Vector.Embeddings.Model = "nomic-embed-text"
	cfg.Vector.Embeddings.Dimension = 4
	oldRebuild, oldYes, oldBackstop := embedFullRebuild, embedYes, embedBackstop
	oldAccounts, oldCollections := embedAccounts, embedCollections
	t.Cleanup(func() {
		embedFullRebuild, embedYes, embedBackstop = oldRebuild, oldYes, oldBackstop
		embedAccounts, embedCollections = oldAccounts, oldCollections
	})
	embedFullRebuild, embedYes, embedBackstop = true, true, false
	embedAccounts, embedCollections = nil, nil
	command := &cobra.Command{}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.NoError(runEmbeddingsBuildLocal(command))
	assert.Equal(2, embedded)
	oldGeneration := embedGenByID(t, dir)[1]

	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})
	inputs := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			ping.ServeHTTP(w, r)
			return
		}
		assert.Empty(r.Header.Get("Authorization"))
		var request struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		assert.Equal("nomic-embed-text", request.Model)
		assert.Equal([]string{"Subject: new message\n\nnew body"}, request.Input)
		inputs += len(request.Input)
		_, err := io.WriteString(w, `{"data":[{"embedding":[1,0,0,0],"index":0}],"model":"nomic-embed-text"}`)
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	endpoint, err := daemon.ParseEndpoint(server.URL, daemon.ParseEndpointOptions{TCPPolicy: daemon.RequireLoopback})
	require.NoError(err)
	sharedDir, err := localEmbeddingsDir()
	require.NoError(err)
	_, err = (daemon.RuntimeStore{Dir: sharedDir, Prefix: embedlocal.Service}).Write(daemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(err)
	cfg.Vector.Embeddings.LocalOnly = true
	cfg.Vector.Embeddings.Endpoint = ""
	cfg.Vector.Embeddings.APIKeyEnv = "LOCAL_TEST_KEY"
	t.Setenv("LOCAL_TEST_KEY", "synthetic-env-key")
	snapshot, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), snapshot.ETag, providercredentials.VectorEmbeddingsID, server.URL+"/v1", "synthetic-stored-key")
	require.NoError(err)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	_, err = st.DB().Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject) VALUES (3, 1, 1, 'a2', 'email', 'new message'); INSERT INTO message_bodies (message_id, body_text) VALUES (3, 'new body')`)
	require.NoError(err)
	embedFullRebuild = false
	var output bytes.Buffer
	command.SetOut(&output)
	require.NoError(runEmbeddingsBuildLocal(command))
	assert.Equal(1, inputs)
	assert.Equal(oldGeneration, embedGenByID(t, dir)[3])
	assert.Empty(cfg.Vector.Embeddings.Endpoint, "runtime URL stays out of saved config")

	resolved, err := resolvedVectorConfig(t.Context(), st, cfg.Vector)
	require.NoError(err)
	assert.Equal(server.URL+"/v1", resolved.Embeddings.Endpoint)
}
