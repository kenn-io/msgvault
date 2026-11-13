package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestMetaWebhookReceiverHoldsOffIdleShutdown checks that a listening
// receiver keeps an automatic daemon up until it stops. A real listener
// cannot run under synctest, so the waits use real time.
func TestMetaWebhookReceiverHoldsOffIdleShutdown(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	const timeout = 100 * time.Millisecond
	t.Setenv("MSGVAULT_TEST_META_APP_SECRET", "synthetic-app-secret")
	t.Setenv("MSGVAULT_TEST_META_VERIFY_TOKEN", "synthetic-verify-token")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.MetaWebhooks.ListenAddr = "127.0.0.1:0"
	cfg.MetaWebhooks.Apps = []config.MetaWebhookApp{{AppID: "1234567890", AppSecretEnv: "MSGVAULT_TEST_META_APP_SECRET", VerifyTokenEnv: "MSGVAULT_TEST_META_VERIFY_TOKEN"}}
	fired := make(chan struct{})
	idle := api.NewIdleTracker(timeout, func() { close(fired) }) //nolint:kennlint // deadlinetest: the idle timeout expiring is the asserted result
	receiver, err := startMetaWebhooks(t.Context(), cfg, testutil.NewTestStore(t), idle, idle)
	require.NoError(err)
	require.NotNil(receiver.server, "receiver did not start")
	idle.Touch()
	go idle.Run(t.Context())

	select {
	case <-fired:
		assert.Fail("idle shutdown fired while the receiver was listening")
	case <-time.After(5 * timeout): //nolint:kennlint // deadlinetest: absence check: the listening receiver holds off idle shutdown
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(receiver.Shutdown(ctx))
	select {
	case <-fired:
	case <-time.After(30 * time.Second):
		assert.Fail("idle shutdown did not fire after the receiver stopped")
	}
}
