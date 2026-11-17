package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func testGmailServiceAccountConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyJSON, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "service@example.test",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   "https://token.example.test/oauth2",
	})
	require.NoError(t, err)
	path := filepath.Join(dir, "service-account.json")
	require.NoError(t, os.WriteFile(path, keyJSON, 0600))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir, cfg.OAuth.ServiceAccountKey = dir, dir, path
	return cfg
}

func TestRapidEmailGmailProfileMismatchDoesNotMutateArchive(t *testing.T) {
	cfg := testGmailServiceAccountConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2":
			if !assert.NoError(t, r.ParseForm()) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parts := strings.Split(r.Form.Get("assertion"), ".")
			if !assert.Len(t, parts, 3) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var claims map[string]any
			if !assert.NoError(t, json.Unmarshal(claimsJSON, &claims)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, strings.Join(oauth.Scopes, " "), claims["scope"])
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`))
		case "/gmail/v1/users/me/profile":
			_, _ = w.Write([]byte(`{"emailAddress":"other@example.test","historyId":"100"}`))
		default:
			assert.Fail(t, "unexpected provider operation after a mismatched profile", "%s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	loopback, err := url.Parse(server.URL)
	require.NoError(t, err)
	assert := assert.New(t)
	require := require.New(t)
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = loopback.Scheme, loopback.Host
		return transport.RoundTrip(clone)
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	st := storetest.New(t)
	src, err := st.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(err)
	require.NoError(st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = originalRebuild })
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	invocationFromContext(ctx).pollAccounts = []config.AccountSchedule{{Email: src.Identifier, Enabled: true, PollInterval: 15 * time.Second}}
	err = runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx))
	require.ErrorContains(err, "does not match", "authenticated mismatch must fail closed with stable remediation")
	for _, table := range []string{"account_identities", "sync_runs", "messages"} {
		var count int
		require.NoError(st.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		assert.Zero(count, "profile verification precedes every %s mutation", table)
	}
	current, err := st.Store.GetSourceByID(src.ID)
	require.NoError(err)
	assert.Equal("100", current.SyncCursor.String)
}

func TestGmailPollAcquisitionAndQuota(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	cfg := testGmailServiceAccountConfig(t)
	var profiles, histories, expiredProfiles atomic.Int32
	var changed, healthy, expired, quota, labelsQuota atomic.Bool
	quotaSeen := make(chan struct{}, 1)
	raw := base64.RawURLEncoding.EncodeToString([]byte("From: sender@example.test\r\nTo: sample@example.test\r\nSubject: fixture\r\nMessage-ID: <lost@example.test>\r\n\r\nbody\r\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2":
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`))
		case "/gmail/v1/users/me/profile":
			if quota.CompareAndSwap(true, false) {
				w.Header().Set("Retry-After", "600")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`))
				quotaSeen <- struct{}{}
				return
			}
			profiles.Add(1)
			if expired.Load() && expiredProfiles.Add(1) >= 2 {
				_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"102"}`))
			} else if changed.Load() {
				_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"101"}`))
			} else {
				_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"100"}`))
			}
		case "/gmail/v1/users/me/labels":
			if labelsQuota.CompareAndSwap(true, false) {
				w.Header().Set("Retry-After", "600")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"}]}`))
		case "/gmail/v1/users/me/history":
			histories.Add(1)
			if expired.Load() {
				if r.URL.Query().Get("startHistoryId") == "100" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(`{"historyId":"102","history":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"historyId":"101","history":[{"id":"101","messagesAdded":[{"message":{"id":"lost","threadId":"thread-lost","labelIds":["INBOX"]}}]}]}`))
		case "/gmail/v1/users/me/messages":
			_, _ = w.Write([]byte(`{"messages":[{"id":"lost","threadId":"thread-lost"}],"resultSizeEstimate":1}`))
		case "/gmail/v1/users/me/messages/lost":
			if !healthy.Load() {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"fixture failure"}}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"lost","threadId":"thread-lost","labelIds":["INBOX"],"historyId":"101","internalDate":"1704110400000","raw":%q}`, raw)
		default:
			checks.Fail("unexpected provider operation", "%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	loopback, err := url.Parse(server.URL)
	requirements.NoError(err)
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = loopback.Scheme, loopback.Host
		return transport.RoundTrip(clone)
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	st := storetest.New(t)
	src, err := st.Store.GetOrCreateSource("gmail", "sample@example.test")
	requirements.NoError(err)
	requirements.NoError(st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = originalRebuild })
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	var failures int
	invocationFromContext(ctx).syncFailures = &telemetry.SyncFailures{Capture: func(string, map[string]any) error { failures++; return nil }}
	alias := "archive@example.test"
	_, err = st.Store.UpdateSourceSettingsContext(ctx, src.ID, store.SourceSettingsUpdate{Alias: &alias})
	requirements.NoError(err)
	cfg.Accounts = []config.AccountSchedule{{Email: alias, Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}
	invocationFromContext(ctx).pollAccounts = append([]config.AccountSchedule(nil), cfg.Accounts...)
	requirements.NoError(runEmailPoll(ctx, alias, st.Store, nil, invocationFromContext(ctx)))
	checks.EqualValues(1, profiles.Load())
	invocationFromContext(ctx).pollAccounts = []config.AccountSchedule{{Email: src.Identifier, Enabled: true, PollInterval: 15 * time.Second}}
	changed.Store(true)
	requirements.ErrorIs(runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)), scheduler.ErrAcquisitionIncomplete)
	debt, err := st.Store.ListUnresolvedSyncItemErrors(ctx, src.ID)
	requirements.NoError(err)
	requirements.Len(debt, 1)
	checks.Zero(failures)
	broken, err := st.Store.GetOrCreateSource(sourceTypeIMAP, "imaps://broken@example.test")
	requirements.NoError(err)
	_, err = st.Store.DB().ExecContext(t.Context(), st.Store.Rebind(`UPDATE sources SET display_name = ? WHERE id = ?`), src.Identifier, broken.ID)
	requirements.NoError(err)
	mixedErr := runScheduledSync(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx))
	requirements.ErrorContains(mixedErr, "has no config")
	requirements.NotErrorIs(mixedErr, scheduler.ErrAcquisitionIncomplete)
	checks.Equal(1, failures)
	requirements.NoError(st.Store.RemoveSource(broken.ID))
	failures = 0
	requirements.ErrorIs(runScheduledSync(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)), scheduler.ErrAcquisitionIncomplete)
	checks.Zero(failures)
	healthy.Store(true)
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return errors.New("queue unavailable") }
	requirements.NoError(runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)))
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return nil }
	debt, err = st.Store.ListUnresolvedSyncItemErrors(ctx, src.ID)
	requirements.NoError(err)
	checks.Empty(debt)
	checks.EqualValues(1, histories.Load())
	requirements.NoError(st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	expired.Store(true)
	requirements.NoError(runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)))
	current, err := st.Store.GetSourceByID(src.ID)
	requirements.NoError(err)
	checks.Equal("102", current.SyncCursor.String)
	quota.Store(true)
	otherRan := make(chan struct{}, 1)
	gate := api.NewSerialOperationGate()
	pollScheduler := scheduler.New(func(context.Context, string) error {
		otherRan <- struct{}{}
		return nil
	}).WithAccountPoll(func(runCtx context.Context, identifier string) error {
		return runEmailPoll(runCtx, identifier, st.Store, nil, invocationFromContext(ctx))
	}).WithWorkTracker(labelWorkTracker(gate, "test"))
	t.Cleanup(func() { <-pollScheduler.Stop().Done() })
	n, scheduleErrors := pollScheduler.AddAccountsFromConfig(&config.Config{Accounts: []config.AccountSchedule{{Email: src.Identifier, Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}})
	requirements.Empty(scheduleErrors)
	requirements.Equal(1, n)
	requirements.NoError(pollScheduler.AddAccount("other@example.test", "0 * * * *"))
	pollScheduler.Start()
	select {
	case <-quotaSeen:
	case <-time.After(3 * time.Second):
		requirements.FailNow("poll did not reach Gmail")
	}
	requirements.NoError(pollScheduler.TriggerSync("other@example.test", false))
	select {
	case <-otherRan:
	case <-time.After(3 * time.Second):
		requirements.FailNow("another account did not acquire the gate during quota pause")
	}
	requirements.NoError(st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	expired.Store(false)
	labelsQuota.Store(true)
	freshCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	invocationFromContext(freshCtx).pollAccounts = []config.AccountSchedule{{Email: src.Identifier, Enabled: true, PollInterval: 15 * time.Second}}
	requirements.ErrorIs(runEmailPoll(freshCtx, src.Identifier, st.Store, nil, invocationFromContext(freshCtx)), scheduler.ErrAcquisitionIncomplete)
	run, err := st.Store.GetLatestSync(src.ID)
	requirements.NoError(err)
	checks.Equal(store.SyncStatusPaused, run.Status)
	checks.Empty(run.ErrorMessage.String)
}
