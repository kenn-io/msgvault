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

type metaWebhookErrorCase struct {
	name string
	err  error
	want error // ErrUnrecoverable, ErrBusy, or nil for a retryable failure
}

func TestMetaWebhookErrorRetriesOnlyRecoverableFailures(t *testing.T) {
	cases := []metaWebhookErrorCase{
		{"source busy", fmt.Errorf("apply: %w", store.ErrSyncAlreadyActive), metawebhook.ErrBusy},
		{"source not registered", store.ErrSourceNotFound, metawebhook.ErrUnrecoverable},
		{"invalid change", fmt.Errorf("%w: bad", meta.ErrInvalidChange), metawebhook.ErrUnrecoverable},
		{"post deleted, wrapped", fmt.Errorf("read post: %w", &meta.APIError{Status: 400, Code: 100, Subcode: 33}), metawebhook.ErrUnrecoverable},
	}
	// With a code, the status does not decide: Meta can refuse with HTTP 200.
	for _, code := range []struct {
		name          string
		code, subcode int
		want          error
	}{
		{"object missing", 100, 33, metawebhook.ErrUnrecoverable},
		{"invalid parameter", 100, 0, metawebhook.ErrUnrecoverable},
		{"permission denied", 10, 0, metawebhook.ErrUnrecoverable},
		{"permission 200", 200, 0, metawebhook.ErrUnrecoverable},
		{"permission 299", 299, 0, metawebhook.ErrUnrecoverable},
		{"capability missing", 3, 0, metawebhook.ErrUnrecoverable},
		{"throttled 4", 4, 0, nil},
		{"throttled 17", 17, 0, nil},
		{"throttled 32", 32, 0, nil},
		{"throttled 613", 613, 0, nil},
		{"business use case throttled 80000", 80000, 0, nil},
		{"business use case throttled 80001", 80001, 0, nil},
		{"business use case throttled 80002", 80002, 0, nil},
		{"business use case throttled 80006", 80006, 0, nil},
		{"business use case throttled 80014", 80014, 0, nil},
		{"Page token expired", 190, 0, nil},
		{"Page token invalidated", 190, 463, nil},
		{"API session", 102, 0, nil},
		{"API unknown", 1, 0, nil},
		{"API service", 2, 0, nil},
		{"application limit reached", 341, 0, nil},
		{"temporarily blocked", 368, 0, nil},
		{"unknown code", 12345, 0, nil},
	} {
		for _, status := range []int{200, 400, 403} {
			cases = append(cases, metaWebhookErrorCase{
				fmt.Sprintf("%s with HTTP %d", code.name, status),
				&meta.APIError{Status: status, Code: code.code, Subcode: code.subcode}, code.want,
			})
		}
	}
	for _, status := range []struct {
		status int
		want   error
	}{
		{400, metawebhook.ErrUnrecoverable},
		{403, metawebhook.ErrUnrecoverable},
		{404, metawebhook.ErrUnrecoverable},
		{401, nil},
		{429, nil},
		{503, nil},
		{200, nil},
	} {
		cases = append(cases, metaWebhookErrorCase{
			fmt.Sprintf("HTTP %d without a code", status.status), &meta.APIError{Status: status.status}, status.want,
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := metaWebhookError(tc.err)
			require.ErrorIs(t, err, tc.err)
			for _, class := range []error{metawebhook.ErrUnrecoverable, metawebhook.ErrBusy} {
				assert.Equal(t, errors.Is(tc.want, class), errors.Is(err, class), "is %v", class)
			}
		})
	}
}

const (
	testMetaWebhookSecretEnv = "MSGVAULT_TEST_META_APP_SECRET"
	testMetaWebhookVerifyEnv = "MSGVAULT_TEST_META_VERIFY_TOKEN"
)

// TestMetaWebhookReceiverHoldsOffIdleShutdown checks that a listening
// receiver keeps an automatic daemon up, and that idle shutdown works as
// before once it stops or when it does not start. A real listener cannot run
// under synctest, so the waits use real time.
func TestMetaWebhookReceiverHoldsOffIdleShutdown(t *testing.T) {
	const timeout = 100 * time.Millisecond
	t.Run("listening", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		t.Setenv(testMetaWebhookSecretEnv, "synthetic-app-secret")
		t.Setenv(testMetaWebhookVerifyEnv, "synthetic-verify-token")
		receiver, fired := startIdleMetaWebhooks(t, "127.0.0.1:0", timeout) //nolint:kennlint // deadlinetest: the idle timeout expiring is the asserted result
		require.NotNil(receiver.server, "receiver did not start")
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
	})
	for _, tc := range []struct{ name, listen string }{
		{"not configured", ""},
		{"app secret unset", "127.0.0.1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(testMetaWebhookSecretEnv, "")
			t.Setenv(testMetaWebhookVerifyEnv, "synthetic-verify-token")
			receiver, fired := startIdleMetaWebhooks(t, tc.listen, timeout) //nolint:kennlint // deadlinetest: the idle timeout expiring is the asserted result
			assert.Nil(t, receiver.server, "receiver started")
			select {
			case <-fired:
			case <-time.After(30 * time.Second):
				assert.Fail(t, "idle shutdown did not fire without a listening receiver")
			}
		})
	}
}

// startIdleMetaWebhooks starts the receiver for one synthetic app, then runs
// an idle tracker the way the daemon does. The returned channel closes once
// the tracker goes idle.
func startIdleMetaWebhooks(t *testing.T, listen string, timeout time.Duration) (*metaWebhookRuntime, <-chan struct{}) {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.MetaWebhooks.ListenAddr = listen
	if listen != "" {
		cfg.MetaWebhooks.Apps = []config.MetaWebhookApp{{AppID: "1234567890", AppSecretEnv: testMetaWebhookSecretEnv, VerifyTokenEnv: testMetaWebhookVerifyEnv}}
	}
	fired := make(chan struct{})
	idle := api.NewIdleTracker(timeout, func() { close(fired) })
	receiver, err := startMetaWebhooks(t.Context(), cfg, testutil.NewTestStore(t), idle, idle)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		assert.NoError(t, receiver.Shutdown(ctx))
	})
	idle.Touch()
	go idle.Run(t.Context())
	return receiver, fired
}
