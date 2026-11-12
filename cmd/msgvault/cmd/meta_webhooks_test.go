package cmd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
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

// TestMetaWebhookOnlyStoredDeliveriesPostponeIdle checks that rejected
// requests leave the idle deadline alone and a stored delivery moves it.
func TestMetaWebhookOnlyStoredDeliveriesPostponeIdle(t *testing.T) {
	const timeout = 100 * time.Millisecond
	body := `{"object":"page","entry":[]}`
	t.Run("rejected requests", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			handler, fired := newIdleMetaWebhook(t, timeout)
			synctest.Sleep(60 * time.Millisecond)
			wrong := strings.Repeat("0", 64)
			require.Equal(http.StatusUnauthorized, postMetaWebhook(t, handler, body, "sha256="+wrong))
			require.Equal(http.StatusUnauthorized, postMetaWebhook(t, handler, body, ""))
			synctest.Sleep(50 * time.Millisecond)
			assert.True(idleFired(fired), "rejected requests postponed idle shutdown")
		})
	})
	t.Run("stored delivery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			handler, fired := newIdleMetaWebhook(t, timeout)
			synctest.Sleep(60 * time.Millisecond)
			require.Equal(http.StatusOK, postMetaWebhook(t, handler, body, metaWebhookSignature(body)))
			synctest.Sleep(50 * time.Millisecond)
			assert.False(idleFired(fired), "idle fired despite a stored delivery")
			synctest.Sleep(60 * time.Millisecond)
			assert.True(idleFired(fired), "idle did not fire after the postponed deadline")
		})
	})
}

const (
	testMetaWebhookApp    = "1234567890"
	testMetaWebhookSecret = "synthetic-app-secret"
)

// newIdleMetaWebhook builds the receiver's handler around a running idle
// tracker. The returned channel receives once the tracker goes idle.
func newIdleMetaWebhook(t *testing.T, timeout time.Duration) (http.Handler, <-chan struct{}) {
	t.Helper()
	fired := make(chan struct{}, 1)
	idle := api.NewIdleTracker(timeout, func() { fired <- struct{}{} })
	pending := filepath.Join(t.TempDir(), "pending")
	apps := map[string]metawebhook.App{
		testMetaWebhookApp: {Secret: []byte(testMetaWebhookSecret), VerifyToken: "synthetic-verify-token"},
	}
	handler, err := metaWebhookHandler(apps, pending, idle, metawebhook.NewWorker(pending, nil))
	require.NoError(t, err)
	go idle.Run(t.Context())
	return handler, fired
}

func metaWebhookSignature(body string) string {
	mac := hmac.New(sha256.New, []byte(testMetaWebhookSecret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postMetaWebhook posts body to the test app and returns the status.
func postMetaWebhook(t *testing.T, handler http.Handler, body, signature string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/meta/webhook/"+testMetaWebhookApp, strings.NewReader(body))
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

func idleFired(fired <-chan struct{}) bool {
	select {
	case <-fired:
		return true
	default:
		return false
	}
}
