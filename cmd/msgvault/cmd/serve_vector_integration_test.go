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
	"go.kenn.io/msgvault/internal/store"
)

// TestRunServeServesHealthWhileVectorInitBlocked verifies the daemon starts
// serving the HTTP API — with /health reporting vector status "initializing"
// — before the expensive vector backend setup completes. The vector init
// seam is overridden to block until daemon shutdown, so a passing test
// proves the API listener comes up independently of vector maintenance.
func TestRunServeServesHealthWhileVectorInitBlocked(t *testing.T) {
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Vector.ApplyDefaults()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 768
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

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
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(cmd, nil)
	}()

	// Health must answer 200 while the vector init seam is still blocked.
	waitForServeHealth(t, cfg.Server.APIPort, errCh)

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Server.APIPort)
	var health struct {
		Status string `json:"status"`
		Vector *struct {
			Status string `json:"status"`
		} `json:"vector"`
	}
	require.Eventually(t, func() bool {
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
	require.NotNil(t, health.Vector)
	assert.Equal(t, "initializing", health.Vector.Status)

	// Shut down via context cancellation (which also unblocks the seam)
	// and confirm a clean exit.
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "runServe did not stop after context cancellation")
	}
}
