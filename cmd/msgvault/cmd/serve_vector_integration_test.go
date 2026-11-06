//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// TestRunServeServesHealthWhileVectorInitBlocked verifies the daemon starts
// serving the HTTP API — with /health reporting vector status "initializing"
// — before the expensive vector backend setup completes. The vector init
// seam is overridden to block until daemon shutdown, so a passing test
// proves the API listener comes up independently of vector maintenance.
func TestRunServeServesHealthWhileVectorInitBlocked(t *testing.T) {
	cfg := testConfigValue()

	assert := assert.New(t)
	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Analytics.AutoBuildCache = false
	c.Vector.ApplyDefaults()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 768
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

	// An existing archive starts with sampled statistics. Startup maintenance
	// must repair them even while vector initialization remains blocked.
	s := testutil.NewSQLiteTestStoreAt(t, c.DatabaseDSN())
	fixture := &dbtest.TestDB{DB: s.DB(), T: t}
	source := fixture.AddSource(dbtest.SourceOpts{Identifier: "account@example.com"})
	conversation := fixture.AddConversation(dbtest.ConversationOpts{SourceID: source})
	for range 3003 {
		fixture.AddMessage(dbtest.MessageOpts{SourceID: source, ConversationID: conversation})
	}
	_, err := s.DB().Exec("PRAGMA analysis_limit=1000; ANALYZE messages")
	require.NoError(err)
	var stat string
	require.NoError(s.DB().QueryRow("SELECT stat FROM sqlite_stat1 WHERE idx='idx_messages_source'").Scan(&stat))
	require.NotEqual("3003 3003", stat)
	sampled := stat

	// The seam blocks until the daemon shuts down (ctx cancelled), so
	// health is polled while vector init is guaranteed still pending. It
	// only returns once ctx is done, so ctx.Err() is always non-nil and
	// the seam never yields (nil, nil).
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(cmd, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(err, "runServe")
		case <-time.After(10 * time.Second):
			require.FailNow("runServe did not stop after context cancellation")
		}
	})

	// Health must answer 200 while the vector init seam is still blocked.
	waitForServeHealth(t, cfg.Server.APIPort, errCh)

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Server.APIPort)
	var health struct {
		Status string `json:"status"`
		Vector *struct {
			Status string `json:"status"`
		} `json:"vector"`
	}
	require.Eventually(func() bool {
		resp, err := http.Get(healthURL) //nolint:gosec // local test server
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		return json.NewDecoder(resp.Body).Decode(&health) == nil
	}, 10*time.Second, 25*time.Millisecond, "health must answer while vector init is blocked")
	require.NotNil(health.Vector)
	assert.Equal("initializing", health.Vector.Status)

	require.Eventually(func() bool {
		return s.DB().QueryRow("SELECT stat FROM sqlite_stat1 WHERE idx='idx_messages_source'").Scan(&stat) == nil && stat != sampled
	}, serveLifecycleTestTimeout, 25*time.Millisecond, "daemon startup must replace sampled message statistics")
}

func TestRunServeStartsVectorWhileAnalyticsInitializationBlocked(t *testing.T) {
	cfg := testConfigValue()

	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	c.Vector.ApplyDefaults()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 768
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

	analyticsStarted := make(chan struct{})
	releaseAnalytics := make(chan struct{})
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		close(analyticsStarted)
		<-releaseAnalytics
		return nil
	})
	vectorStarted := make(chan struct{})
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		close(vectorStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()

	select {
	case <-analyticsStarted:
	case err := <-errCh:
		require.NoError(err, "runServe exited before analytics initialization")
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("analytics initialization did not start")
	}
	waitForServeHealth(t, c.Server.APIPort, errCh)
	select {
	case <-vectorStarted:
	case err := <-errCh:
		require.NoError(err, "runServe exited before vector initialization")
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("vector initialization did not start while analytics initialization was blocked")
	}

	close(releaseAnalytics)
	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}
