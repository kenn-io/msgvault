package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRapidEmailGmailProfileMismatchDoesNotMutateArchive(t *testing.T) {
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
	keyPath := filepath.Join(dir, "service-account.json")
	require.NoError(t, os.WriteFile(keyPath, keyJSON, 0o600))
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
`), 0o600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	cfg.OAuth.ServiceAccountKey = keyPath
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

func TestGmailPollUsesOneVerifiedProfile(t *testing.T) {
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
	keyPath := filepath.Join(dir, "service-account.json")
	require.NoError(t, os.WriteFile(keyPath, keyJSON, 0600))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = dir
	cfg.Data.DataDir = dir
	cfg.OAuth.ServiceAccountKey = keyPath
	var profiles, histories atomic.Int32
	var changed, healthy, expired atomic.Bool
	raw := base64.RawURLEncoding.EncodeToString([]byte("From: sender@example.test\r\nTo: sample@example.test\r\nSubject: fixture\r\nMessage-ID: <lost@example.test>\r\n\r\nbody\r\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2":
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`))
		case "/gmail/v1/users/me/profile":
			count := profiles.Add(1)
			if expired.Load() && count >= 7 {
				_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"102"}`))
			} else if changed.Load() {
				_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"101"}`))
			} else {
				_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"100"}`))
			}
		case "/gmail/v1/users/me/labels":
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
			assert.Fail(t, "unexpected provider operation", "%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	loopback, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = loopback.Scheme, loopback.Host
		return transport.RoundTrip(clone)
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	st := storetest.New(t)
	src, err := st.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(t, err)
	require.NoError(t, st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = originalRebuild })
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	var failures int
	invocationFromContext(ctx).syncFailures = &telemetry.SyncFailures{Capture: func(string, map[string]any) error { failures++; return nil }}
	require.NoError(t, runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)))
	assert.EqualValues(t, 1, profiles.Load())
	changed.Store(true)
	assert.ErrorIs(t, runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)), scheduler.ErrAcquisitionIncomplete)
	debt, err := st.Store.ListUnresolvedSyncItemErrors(ctx, src.ID)
	require.NoError(t, err)
	require.Len(t, debt, 1)
	assert.Zero(t, failures)
	healthy.Store(true)
	require.NoError(t, runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)))
	debt, err = st.Store.ListUnresolvedSyncItemErrors(ctx, src.ID)
	require.NoError(t, err)
	assert.Empty(t, debt)
	assert.EqualValues(t, 3, profiles.Load())
	assert.EqualValues(t, 1, histories.Load())
	require.NoError(t, st.Store.UpdateSourceSyncCursor(src.ID, ""))
	assert.ErrorContains(t, runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)), "no history ID")
	require.NoError(t, st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	expired.Store(true)
	require.NoError(t, runEmailPoll(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx)))
	current, err := st.Store.GetSourceByID(src.ID)
	require.NoError(t, err)
	assert.Equal(t, "102", current.SyncCursor.String)
	assert.EqualValues(t, 7, profiles.Load())
}
