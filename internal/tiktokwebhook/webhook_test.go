package tiktokwebhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signature(now time.Time, body []byte) string {
	stamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte("fixture-secret"))
	_, _ = mac.Write(append([]byte(stamp+"."), body...))
	return "t=" + stamp + ",s=" + hex.EncodeToString(mac.Sum(nil))
}
func TestSignatureUsesExactRawBytesAndFiveSecondWindow(t *testing.T) {
	assertions := assert.New(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	body := []byte("{\"example\": 1}\n")
	header := signature(now, body)
	assertions.True(VerifySignature([]byte("fixture-secret"), header, body, now))
	assertions.True(VerifySignature([]byte("fixture-secret"), header, body, now.Add(5*time.Second)))
	assertions.False(VerifySignature([]byte("fixture-secret"), header, body, now.Add(5*time.Second+time.Nanosecond)))
	assertions.False(VerifySignature([]byte("fixture-secret"), header, body, now.Add(-time.Second)))
	assertions.False(VerifySignature([]byte("fixture-secret"), header, bytes.TrimSpace(body), now))
	assertions.False(VerifySignature([]byte("other-secret"), header, body, now))
	assertions.False(VerifySignature([]byte("fixture-secret"), header+",t=1", body, now))
}

func TestReceiverAcknowledgesDurableRawBytesAndDeduplicates(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root := t.TempDir()
	spool, err := OpenSpool(root, 1<<20)
	requirements.NoError(err)
	now := time.Now().Truncate(time.Second)
	body := []byte("{\"signed\":\"synthetic data\"}\n")
	h := &Handler{Apps: map[string][]byte{"app-example": []byte("fixture-secret")}, Spool: spool, Now: func() time.Time { return now }}
	for range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/tiktok/app-example", bytes.NewReader(body))
		req.Header.Set("Tiktok-Signature", signature(now, body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		requirements.Equal(http.StatusOK, rec.Code)
		files, err := filepath.Glob(filepath.Join(root, "*.json"))
		requirements.NoError(err)
		requirements.Len(files, 1)
		info, err := os.Stat(files[0])
		requirements.NoError(err)
		if runtime.GOOS != "windows" {
			assertions.Equal(os.FileMode(0600), info.Mode().Perm())
		}
	}
	// A fresh process-shaped spool object recovers exactly the accepted bytes.
	restarted, err := OpenSpool(root, 1<<20)
	requirements.NoError(err)
	applied, err := restarted.Drain(t.Context(), func(_ context.Context, app string, raw []byte) (Outcome, error) {
		assertions.Equal("app-example", app)
		assertions.Equal(body, raw)
		return Outcome{Class: "applied"}, nil
	})
	requirements.NoError(err)
	assertions.Equal(1, applied)
	status, err := restarted.Status(t.Context(), "app-example")
	requirements.NoError(err)
	assertions.Equal(1, status.Done)
	assertions.Zero(status.Pending)
}

func TestReceiverRejectsBeforeAcknowledging(t *testing.T) {
	requirements := require.New(t)
	now := time.Now().Truncate(time.Second)
	body := []byte("synthetic")
	spool, err := OpenSpool(t.TempDir(), 1100)
	requirements.NoError(err)
	for _, tc := range []struct {
		name, header string
		maximum      int64
		status       int
	}{
		{"signature", "bad", 1024, http.StatusUnauthorized},
		{"old", signature(now.Add(-6*time.Second), body), 1024, http.StatusUnauthorized},
		{"size", signature(now, body), 1, http.StatusRequestEntityTooLarge},
		{"full", signature(now, body), 1024, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{Apps: map[string][]byte{"app-example": []byte("fixture-secret")}, Spool: spool, MaxBodyBytes: tc.maximum, Now: func() time.Time { return now }}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/tiktok/app-example", bytes.NewReader(body))
			req.Header.Set("Tiktok-Signature", tc.header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tc.status, rec.Code)
		})
	}
	status, err := spool.Status(t.Context(), "")
	requirements.NoError(err)
	assert.Zero(t, status.Pending)
}

func TestSpoolRetriesHoldsAndReplaysWithoutExposingBodies(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	requirements.NoError(err)
	now := time.Now()
	spool.now = func() time.Time { return now }
	_, _, err = spool.Accept(t.Context(), "app-example", []byte("synthetic private payload"))
	requirements.NoError(err)
	_, err = spool.Drain(t.Context(), func(context.Context, string, []byte) (Outcome, error) {
		return Outcome{}, errors.New("synthetic secret failure")
	})
	requirements.NoError(err)
	now = now.Add(time.Hour)
	_, err = spool.Drain(t.Context(), func(context.Context, string, []byte) (Outcome, error) { return Outcome{Class: "unrouted"}, nil })
	requirements.NoError(err)
	status, err := spool.Status(t.Context(), "")
	requirements.NoError(err)
	assertions.Equal(1, status.Held)
	assertions.NotContains(fmt.Sprint(status), "synthetic")
	count, err := spool.Replay(t.Context(), "app-example")
	requirements.NoError(err)
	assertions.Equal(1, count)
	_, err = spool.Drain(t.Context(), func(context.Context, string, []byte) (Outcome, error) { return Outcome{Class: "unsupported_eu"}, nil })
	requirements.NoError(err)
	status, err = spool.Status(t.Context(), "")
	requirements.NoError(err)
	assertions.Equal(1, status.UnsupportedEU)
}

func TestSpoolCancellationPreservesUnprocessedDeliveries(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root := t.TempDir()
	spool, err := OpenSpool(root, 1<<20)
	requirements.NoError(err)
	for _, body := range []string{"first synthetic delivery", "second synthetic delivery"} {
		_, _, err = spool.Accept(t.Context(), "app-example", []byte(body))
		requirements.NoError(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	applied, err := spool.Drain(ctx, func(context.Context, string, []byte) (Outcome, error) {
		cancel()
		return Outcome{Class: "applied"}, nil
	})
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(1, applied)
	restarted, err := OpenSpool(root, 1<<20)
	requirements.NoError(err)
	status, err := restarted.Status(t.Context(), "app-example")
	requirements.NoError(err)
	assertions.Equal(1, status.Done)
	assertions.Equal(1, status.Pending)
	applied, err = restarted.Drain(t.Context(), func(context.Context, string, []byte) (Outcome, error) {
		return Outcome{Class: "applied"}, nil
	})
	requirements.NoError(err)
	assertions.Equal(1, applied)
}
