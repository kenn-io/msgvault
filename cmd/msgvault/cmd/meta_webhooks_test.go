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

func TestMetaWebhookErrorRetriesOnlyRecoverableFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"source busy", fmt.Errorf("apply: %w", store.ErrSyncAlreadyActive), false},
		{"Graph unavailable", &meta.APIError{Status: 503}, false},
		{"Graph throttled with 400", &meta.APIError{Status: 400, Code: 4}, false},
		{"Graph throttled with 429", &meta.APIError{Status: 429}, false},
		{"Graph throttled with 403", &meta.APIError{Status: 403, Code: 613}, false},
		{"Page token expired", &meta.APIError{Status: 400, Code: 190}, false},
		{"Page token invalidated", &meta.APIError{Status: 400, Code: 190, Subcode: 463}, false},
		{"API session", &meta.APIError{Status: 400, Code: 102}, false},
		{"unauthorized without a code", &meta.APIError{Status: 401}, false},
		{"API unknown", &meta.APIError{Status: 400, Code: 1}, false},
		{"API service", &meta.APIError{Status: 400, Code: 2}, false},
		{"application limit reached", &meta.APIError{Status: 400, Code: 341}, false},
		{"temporarily blocked", &meta.APIError{Status: 400, Code: 368}, false},
		{"business use case throttled 80001", &meta.APIError{Status: 400, Code: 80001}, false},
		{"business use case throttled 80002", &meta.APIError{Status: 400, Code: 80002}, false},
		{"business use case throttled 80006", &meta.APIError{Status: 400, Code: 80006}, false},
		{"business use case throttled 80014", &meta.APIError{Status: 400, Code: 80014}, false},
		{"token expired with HTTP 200", &meta.APIError{Status: 200, Code: 190}, false},
		{"unknown code with 500", &meta.APIError{Status: 500, Code: 100}, false},
		{"post deleted", fmt.Errorf("read post: %w", &meta.APIError{Status: 400, Code: 100, Subcode: 33}), true},
		{"invalid parameter with HTTP 200", &meta.APIError{Status: 200, Code: 100}, true},
		{"permission denied", &meta.APIError{Status: 403, Code: 10}, true},
		{"permission family", &meta.APIError{Status: 403, Code: 200}, true},
		{"capability missing", &meta.APIError{Status: 400, Code: 3}, true},
		{"bad request without a code", &meta.APIError{Status: 400}, true},
		{"source not registered", store.ErrSourceNotFound, true},
		{"invalid change", fmt.Errorf("%w: bad", meta.ErrInvalidChange), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := metaWebhookError(tc.err)
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.permanent, errors.Is(err, metawebhook.ErrUnrecoverable))
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
