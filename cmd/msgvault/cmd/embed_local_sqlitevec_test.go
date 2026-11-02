//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	"go.kenn.io/msgvault/internal/vector"
)

func TestLocalEmbeddingsKeepsIncrementalMessageGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	embedded := 0
	oldServer := fakeEmbeddingsServer(t, &embedded, 768)
	dir := t.TempDir()
	seedTwoAccountMainDB(t, dir)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Endpoint = oldServer.URL
	cfg.Vector.Embeddings.Model = "nomic-embed-text"
	cfg.Vector.Embeddings.Dimension = 768
	cfg.Vector.Embeddings.MaxInputChars = 1000
	oldRebuild, oldYes, oldBackstop := embedFullRebuild, embedYes, embedBackstop
	t.Cleanup(func() {
		embedFullRebuild, embedYes, embedBackstop = oldRebuild, oldYes, oldBackstop
	})
	embedFullRebuild, embedYes, embedBackstop = true, true, false
	command := &cobra.Command{}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.NoError(runEmbeddingsBuildLocal(command))
	assert.Equal(2, embedded)
	oldGeneration := embedGenByID(t, dir)[1]

	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			assert.Empty(r.Header.Get("Authorization"))
			oldServer.Config.Handler.ServeHTTP(w, r)
		} else {
			ping.ServeHTTP(w, r)
		}
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
	snapshot, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), snapshot.ETag, providercredentials.VectorEmbeddingsID, "https://api.openai.com/v1", "synthetic-stored-key")
	require.NoError(err)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	_, err = st.DB().Exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject) VALUES (3, 1, 1, 'a2', 'email', 'new message'); INSERT INTO message_bodies (message_id, body_text) VALUES (3, 'new body')`)
	require.NoError(err)
	embedFullRebuild = false
	require.NoError(runEmbeddingsBuildLocal(command))
	assert.Equal(3, embedded)
	assert.Equal(oldGeneration, embedGenByID(t, dir)[3])
	embedFullRebuild = true
	require.NoError(runEmbeddingsBuildLocal(command))
	assert.Equal(6, embedded)
}

func TestLocalEmbeddingStatusDiscoversWithoutInstallation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	fixture, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	cfg.Vector.Embeddings.LocalOnly = true
	cfg.Vector.Embeddings.Endpoint = ""
	cfg.Vector.Embeddings.Model, cfg.Vector.Embeddings.Dimension = "nomic-embed-text", 768
	cfg.Vector.People.Enabled = true
	cfg.Vector.People.RetentionPosture = setupPostureDeclared
	cfg.Vector.People.TrainingPosture = setupPostureDeclared
	original := localEmbeddingsDir
	localEmbeddingsDir = func() (string, error) {
		assert.Fail("policy reads must use saved config")
		return "", embedlocal.ErrNotFound
	}
	t.Cleanup(func() { localEmbeddingsDir = original })
	documents := documentsCommandDeps{openStore: func(context.Context) (*store.Store, func(), error) { return fixture.Store, func() {}, nil }}
	status := newDocumentVectorStatusCmd(documents)
	status.SetOut(io.Discard)
	status.SetErr(io.Discard)
	status.SetArgs([]string{"--json"})
	require.NoError(status.ExecuteContext(ctx))
	people := localPersonProviderDeps(cfg.People.Sweep, fixture.Store, nil)
	people.vectorConfig = func() vector.Config { return cfg.Vector }
	var output bytes.Buffer
	status.SetOut(&output)
	require.NoError(runPersonSemanticProviderStatus(status, people, false, true))
	assert.NotContains(output.String(), `"local_only"`)
	require.NoError(runPersonSemanticProviderConsent(status, people, true, true))
	cfg.Vector.Enabled = false
	_, err := resolveLocalVectorConfig(ctx, cfg.Vector)
	require.NoError(err)
}

func TestLocalEmbeddingRuntimeNeedsSetup(t *testing.T) {
	dir := t.TempDir()
	original := localEmbeddingsDir
	localEmbeddingsDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { localEmbeddingsDir = original })
	cfg := vector.Config{Enabled: true, Embeddings: vector.EmbeddingsConfig{LocalOnly: true, Model: "nomic-embed-text", Dimension: 768}}
	_, err := resolveLocalVectorConfig(t.Context(), cfg)
	require.ErrorContains(t, err, "msgvault setup providers --local-embeddings")
	assert.NotContains(t, err.Error(), "provide Confirm")
}

func TestLocalEmbeddingRuntimeInstallError(t *testing.T) {
	dir := t.TempDir()
	// Discovery needs no install lock. A directory in its place prevents installation.
	lockPath, err := (daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed-install"}).LockPath()
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(lockPath, 0o700))
	original := localEmbeddingsDir
	localEmbeddingsDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { localEmbeddingsDir = original })
	cfg := vector.Config{Enabled: true, Embeddings: vector.EmbeddingsConfig{LocalOnly: true, Model: "nomic-embed-text", Dimension: 768}}
	_, err = resolveLocalVectorConfig(t.Context(), cfg)
	require.ErrorContains(t, err, "install local embeddings")
	assert.NotContains(t, err.Error(), "msgvault setup providers")
}
