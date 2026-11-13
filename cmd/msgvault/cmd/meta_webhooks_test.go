package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMetaWebhookErrorRetriesOnlyRecoverableFailures(t *testing.T) {
	type errorCase struct {
		name          string
		err           error
		unrecoverable bool
	}
	cases := []errorCase{
		{"source busy", fmt.Errorf("apply: %w", store.ErrSyncAlreadyActive), false},
		{"source not registered", store.ErrSourceNotFound, true},
		{"invalid change", fmt.Errorf("%w: bad", meta.ErrInvalidChange), true},
	}
	// With a code, the status does not decide: Meta can refuse with HTTP 200.
	for _, code := range []struct {
		name          string
		code, subcode int
		unrecoverable bool
	}{
		{"token expired", 190, 0, false},
		{"throttled", 4, 0, false},
		{"business use case throttled", 80001, 0, false},
		{"temporary failure", 2, 0, false},
		{"unknown code", 12345, 0, false},
		{"object missing", 100, 33, true},
		{"permission denied", 10, 0, true},
	} {
		for _, status := range []int{200, 400, 403} {
			cases = append(cases, errorCase{
				fmt.Sprintf("%s with HTTP %d", code.name, status),
				&meta.APIError{Status: status, Code: code.code, Subcode: code.subcode}, code.unrecoverable,
			})
		}
	}
	for status, unrecoverable := range map[int]bool{401: false, 429: false, 503: false, 404: true} {
		cases = append(cases, errorCase{fmt.Sprintf("HTTP %d without a code", status), &meta.APIError{Status: status}, unrecoverable})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := metaWebhookError(tc.err)
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.unrecoverable, errors.Is(err, metawebhook.ErrUnrecoverable))
		})
	}
}

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
