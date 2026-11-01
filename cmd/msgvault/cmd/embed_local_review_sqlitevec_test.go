//go:build sqlite_vec

package cmd

import (
	"context"
	"io"
	"log/slog"
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
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestReviewLocalDocumentVectorHTTPStatus(t *testing.T) {
	fixture, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	cfg.Vector.Embeddings.LocalOnly = true
	cfg.Vector.Embeddings.Endpoint = ""
	// Match serve_vector_init: the API retains the saved config, and receives
	// the discovered endpoint through SetVectorFeatures.
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: fixture.Store, Logger: slog.New(slog.DiscardHandler)})
	resolved := cfg.Vector
	resolved.Embeddings.Endpoint = "http://127.0.0.1:19771/v1"
	server.SetVectorFeatures(nil, nil, nil, resolved)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/documents/vectors/status", nil))
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestReviewDisabledLocalTextDoesNotBlockVisualInit(t *testing.T) {
	dir := t.TempDir()
	oldDir := localEmbeddingsDir
	localEmbeddingsDir = func() (string, error) { return filepath.Join(dir, "never-installed"), nil }
	t.Cleanup(func() { localEmbeddingsDir = oldDir })
	cfg := config.NewDefaultConfig()
	cfg.Vector.Enabled = false
	cfg.Vector.Embeddings.LocalOnly = true
	_, err := resolveLocalVectorConfig(t.Context(), cfg.Vector)
	require.NoError(t, err, "visual-only startup must retain disabled text settings without installing its server")
	_, statErr := os.Stat(filepath.Join(dir, "never-installed"))
	assert.True(t, os.IsNotExist(statErr), "disabled text discovery created an installation directory")
}

func TestLocalEmbeddingStatusDiscoversWithoutInstallation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	fixture, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	cfg.Vector.Embeddings.LocalOnly = true
	cfg.Vector.Embeddings.Endpoint = ""
	cfg.Vector.People.Enabled = true
	cfg.Vector.People.RetentionPosture = setupPostureDeclared
	cfg.Vector.People.TrainingPosture = setupPostureDeclared
	dir := filepath.Join(t.TempDir(), "not-installed")
	original := localEmbeddingsDir
	localEmbeddingsDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { localEmbeddingsDir = original })
	documents := documentsCommandDeps{openStore: func(context.Context) (*store.Store, func(), error) { return fixture.Store, func() {}, nil }}
	status := newDocumentVectorStatusCmd(documents)
	status.SetOut(io.Discard)
	status.SetErr(io.Discard)
	status.SetArgs([]string{"--json"})
	require.ErrorIs(status.ExecuteContext(ctx), embedlocal.ErrNotFound)
	people := localPersonProviderDeps(cfg.People.Sweep, fixture.Store, nil)
	people.vectorConfig = func() vector.Config { return cfg.Vector }
	personCommand := &cobra.Command{}
	personCommand.SetContext(ctx)
	personCommand.SetOut(io.Discard)
	require.ErrorIs(runPersonSemanticProviderStatus(personCommand, people, false, true), embedlocal.ErrNotFound)
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(entries, "status must not install or start the local server")

	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version}))
	t.Cleanup(server.Close)
	endpoint, err := daemon.ParseEndpoint(server.URL, daemon.ParseEndpointOptions{TCPPolicy: daemon.RequireLoopback})
	require.NoError(err)
	_, err = (daemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(daemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(err)
	require.NoError(runPersonSemanticProviderConsent(personCommand, people, true, true))
	require.NoError(runPersonSemanticProviderStatus(personCommand, people, false, true))
	consent := newDocumentVectorConsentCmd(documents)
	consent.SetOut(io.Discard)
	consent.SetErr(io.Discard)
	consent.SetArgs([]string{"--yes"})
	require.NoError(consent.ExecuteContext(ctx))
	require.NoError(status.ExecuteContext(ctx))
}
