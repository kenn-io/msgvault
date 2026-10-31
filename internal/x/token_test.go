package x

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
)

func testRecord() TokenRecord {
	r := NewTokenRecord("1000000001", "example-client", PublicClient, "access-secret", "refresh-secret", "")
	r.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
	r.Expiry = time.Now().Add(-time.Hour)
	return r
}

func TestRefreshPersistsRotationBeforeUse(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := tokenTempDir(t)
			var resourceCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resource" {
					resourceCalls.Add(1)
					return
				}
				require.NoError(t, r.ParseForm())
				assert.Equal(t, "refresh-secret", r.Form.Get("refresh_token"))
				if fail {
					require.NoError(t, os.Remove(filepath.Join(dir, "x_1000000001.json")))
					require.NoError(t, os.Mkdir(filepath.Join(dir, "x_1000000001.json"), 0700))
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read"}`)
			}))
			defer server.Close()
			m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
			require.NoError(t, m.Save(t.Context(), testRecord()))
			creds := m.Credentials("1000000001", nil)
			snapshot, err := creds.Prepare(t.Context())
			if fail {
				require.Error(t, err)
				assert.Zero(t, snapshot.Generation)
				response, useErr := creds.HTTPClient().Get(server.URL + "/resource")
				require.Error(t, useErr)
				assert.Nil(t, response)
			} else {
				require.NoError(t, err)
				saved, err := m.Load(t.Context(), "1000000001")
				require.NoError(t, err)
				assert.Equal(t, "rotated-refresh", saved.RefreshToken())
				assert.Equal(t, []string{"tweet.read", "users.read"}, snapshot.GrantedScopes)
				resp, err := creds.HTTPClient().Get(server.URL + "/resource")
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}
			if fail {
				assert.Zero(t, resourceCalls.Load())
			} else {
				assert.Equal(t, int32(1), resourceCalls.Load())
			}
		})
	}
}

func TestRefreshSerializesAndPreservesOmittedToken(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"next-access","token_type":"bearer","expires_in":7200}`)
	}))
	defer server.Close()
	dir := tokenTempDir(t)
	m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
	original := testRecord()
	require.NoError(t, m.Save(t.Context(), original))
	original, err := m.Load(t.Context(), original.OwnerID)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			other := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
			r, err := other.Refresh(t.Context(), original.OwnerID)
			assert.NoError(t, err)
			assert.Equal(t, "refresh-secret", r.RefreshToken())
			assert.NotEqual(t, original.Generation, r.Generation)
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load())
}

func TestTokenFilesRejectUnsafePathsAndRedact(t *testing.T) {
	r := testRecord()
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%p"} {
		for _, v := range []any{r, &r} {
			rendered := fmt.Sprintf(format, v)
			assert.NotContains(t, rendered, "access-secret")
			assert.NotContains(t, rendered, "refresh-secret")
		}
	}
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "access-secret")
	assert.NotContains(t, string(encoded), "refresh-secret")
	dir := tokenTempDir(t)
	m := NewTokenManager(dir)
	require.NoError(t, m.Save(t.Context(), r))
	path := filepath.Join(dir, "x_1000000001.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	_, err = m.Load(t.Context(), "../escape")
	require.Error(t, err)
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(path, 0644))
		_, err = m.Load(t.Context(), r.OwnerID)
		require.Error(t, err)
		require.Error(t, m.Save(t.Context(), r))
	}
	require.NoError(t, os.Remove(path))
	target := filepath.Join(tokenTempDir(t), "target")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0600))
	tokenSymlink(t, target, path)
	_, err = m.Load(t.Context(), r.OwnerID)
	require.Error(t, err)
	require.Error(t, m.Save(t.Context(), r))
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(content))
}

func TestBearerNeverRefreshesOrReplaysPOST(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "Bearer access-secret", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t))
	r := testRecord()
	r.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.Save(t.Context(), r))
	c := m.Credentials(r.OwnerID, nil)
	_, err := c.Prepare(t.Context())
	require.NoError(t, err)
	resp, err := c.HTTPClient().Post(server.URL, "text/plain", strings.NewReader("example"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 401, resp.StatusCode)
	assert.Equal(t, int32(1), calls.Load())
}

func TestRefreshInvalidGrantIsGenerationBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh-secret"}`)
	}))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
	old := testRecord()
	require.NoError(t, m.Save(t.Context(), old))
	old, loadErr := m.Load(t.Context(), old.OwnerID)
	require.NoError(t, loadErr)
	c := m.Credentials(old.OwnerID, nil)
	_, err := c.Prepare(t.Context())
	var revoked *InvalidGrantError
	require.ErrorAs(t, err, &revoked)
	assert.Equal(t, old.Generation, revoked.Generation)
	assert.NotContains(t, err.Error(), "refresh-secret")
	replacement := testRecord()
	replacement.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.WithLifecycleLock(t.Context(), old.OwnerID, func() error { return m.Save(t.Context(), replacement) }))
	replacement, loadErr = m.Load(t.Context(), old.OwnerID)
	require.NoError(t, loadErr)
	snapshot, err := c.Refresh(t.Context(), old.Generation)
	require.NoError(t, err)
	assert.Equal(t, replacement.Generation, snapshot.Generation)
	assert.NotEqual(t, revoked.Generation, snapshot.Generation)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, m.WithLifecycleLock(ctx, old.OwnerID, func() error { return nil }))
}

func TestTokenRootAndLockSecurity(t *testing.T) {
	for _, scenario := range []string{"root-symlink", "root-permissions", "lock-symlink", "lock-permissions", "owner-mismatch", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.Contains(scenario, "permissions") {
				t.Skip("Unix permission modes")
			}
			dir := tokenTempDir(t)
			m := NewTokenManager(dir)
			r := testRecord()
			require.NoError(t, m.Save(t.Context(), r))
			path := filepath.Join(dir, "x_1000000001.json")
			switch scenario {
			case "root-symlink":
				link := filepath.Join(tokenTempDir(t), "tokens")
				tokenSymlink(t, dir, link)
				m = NewTokenManager(link)
			case "root-permissions":
				require.NoError(t, os.Chmod(dir, 0755))
			case "lock-symlink":
				lock := filepath.Join(dir, ".x-store.lock")
				require.NoError(t, os.Remove(lock))
				tokenSymlink(t, path, lock)
			case "lock-permissions":
				require.NoError(t, os.Chmod(filepath.Join(dir, ".x-store.lock"), 0644))
			case "owner-mismatch":
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "1000000001", "1000000002")), 0600))
			case "oversized":
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat(" ", 2<<20)), 0600))
			}
			_, err := m.Load(t.Context(), r.OwnerID)
			require.Error(t, err)
		})
	}
}

func TestCredentialsRejectedGenerationRefreshesExplicitly(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"replacement","token_type":"bearer","expires_in":7200,"scope":"tweet.read"}`)
	}))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	old, err := c.Prepare(t.Context())
	require.NoError(t, err)
	assert.Zero(t, calls.Load())
	next, err := c.Refresh(t.Context(), old.Generation)
	require.NoError(t, err)
	assert.NotEqual(t, old.Generation, next.Generation)
	assert.Equal(t, []string{"tweet.read"}, next.GrantedScopes)
	again, err := c.Refresh(t.Context(), old.Generation)
	require.NoError(t, err)
	assert.Equal(t, next.Generation, again.Generation)
	assert.Equal(t, int32(1), calls.Load())
}

func TestCredentialGenerationMonotoneAndStaleSaveRejected(t *testing.T) {
	dir := tokenTempDir(t)
	m := NewTokenManager(dir)
	record := testRecord()
	require.NoError(t, m.Save(t.Context(), record))
	first, err := m.Load(t.Context(), record.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), first.Generation)
	require.NoError(t, m.Save(t.Context(), testRecord()))
	second, err := m.Load(t.Context(), record.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), second.Generation)
	require.Error(t, m.Save(t.Context(), first))
	current, err := m.Load(t.Context(), record.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), current.Generation)
	assert.Less(t, current.Generation, int64(math.MaxInt64))
	path := filepath.Join(dir, "x_1000000001.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), `"generation":2`, `"generation":9223372036854775807`, 1))
	require.NoError(t, os.WriteFile(path, data, 0600))
	require.Error(t, m.Save(t.Context(), testRecord()))
}

func TestRefreshAuthenticationAndCancellation(t *testing.T) {
	for _, kind := range []ClientType{PublicClient, ConfidentialClient} {
		t.Run(string(kind), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.NoError(t, r.ParseForm())
				assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
				if kind == PublicClient {
					assert.Equal(t, "example-client", r.Form.Get("client_id"))
					assert.Empty(t, r.Header.Get("Authorization"))
				} else {
					id, secret, ok := r.BasicAuth()
					assert.True(t, ok)
					assert.Equal(t, "example-client", id)
					assert.Equal(t, "client-secret", secret)
					assert.Empty(t, r.Form.Get("client_id"))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":"invalid_client","error_description":"client-secret access-secret"}`)
			}))
			defer server.Close()
			r := NewTokenRecord("1000000001", "example-client", kind, "access-secret", "refresh-secret", "")
			if kind == ConfidentialClient {
				r = NewTokenRecord("1000000001", "example-client", kind, "access-secret", "refresh-secret", "client-secret")
			}
			r.GrantedScopes = []string{"tweet.read"}
			r.Expiry = time.Now().Add(-time.Hour)
			m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
			require.NoError(t, m.Save(t.Context(), r))
			_, err := m.Refresh(t.Context(), r.OwnerID)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "client-secret")
			assert.NotContains(t, err.Error(), "access-secret")
			assert.Equal(t, int32(1), calls.Load())
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = m.Refresh(ctx, r.OwnerID)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestBearerRefusesRedirectsAndUseBeforePrepare(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	m := NewTokenManager(tokenTempDir(t))
	r := testRecord()
	r.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.Save(t.Context(), r))
	c := m.Credentials(r.OwnerID, nil)
	resp, err := c.HTTPClient().Post(origin.URL, "text/plain", strings.NewReader("example"))
	require.Error(t, err)
	assert.Nil(t, resp)
	_, err = c.Prepare(t.Context())
	require.NoError(t, err)
	resp, err = c.HTTPClient().Post(origin.URL, "text/plain", strings.NewReader("example"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 307, resp.StatusCode)
	assert.Zero(t, forwarded.Load())
}

func TestFailedRotationInvalidatesPreparedBearer(t *testing.T) {
	dir := tokenTempDir(t)
	var resources atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resource" {
			resources.Add(1)
			return
		}
		path := filepath.Join(dir, "x_1000000001.json")
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Mkdir(path, 0700))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"bearer","expires_in":7200}`)
	}))
	defer server.Close()
	m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	snapshot, err := c.Prepare(t.Context())
	require.NoError(t, err)
	_, err = c.Refresh(t.Context(), snapshot.Generation)
	require.Error(t, err)
	resp, err := c.HTTPClient().Get(server.URL + "/resource")
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Zero(t, resources.Load())
}

func TestSnapshotScopesCannotMutatePreparedGrant(t *testing.T) {
	m := NewTokenManager(tokenTempDir(t))
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	snapshot, err := c.Prepare(t.Context())
	require.NoError(t, err)
	snapshot.GrantedScopes[0] = "tweet.write"
	actual, err := c.Prepare(t.Context())
	require.NoError(t, err)
	assert.True(t, actual.HasScope("tweet.read"))
	assert.False(t, actual.HasScope("tweet.write"))
	assert.Equal(t, int64(1), actual.Generation)
}

func TestRefreshCancellationDuringTokenHTTP(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
	record := testRecord()
	require.NoError(t, m.Save(t.Context(), record))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Refresh(ctx, record.OwnerID); done <- err }()
	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	saved, err := m.Load(t.Context(), record.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), saved.Generation)
	assert.Equal(t, "refresh-secret", saved.RefreshToken())
}

func tokenSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func TestInvalidRefreshGenerationClearsBearer(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t))
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	_, err := c.Prepare(t.Context())
	require.NoError(t, err)
	_, err = c.Refresh(t.Context(), 0)
	require.Error(t, err)
	resp, err := c.HTTPClient().Get(server.URL)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Zero(t, calls.Load())
}

func tokenTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fileutil.SecureChmod(dir, 0700))
	return dir
}

func TestInstallVerifiedCredentialsAboveGenerationFloor(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fileGeneration, want int64
	}{
		{"missing credential", 0, 101}, {"fresh copied authorization", 1, 101}, {"file ahead of source", 200, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tokenCalls.Add(1) }))
			defer server.Close()
			dir := tokenTempDir(t)
			m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
			replacement := testRecord()
			replacement.Expiry = time.Now().Add(time.Hour)
			if tc.fileGeneration > 0 {
				require.NoError(t, m.Save(t.Context(), testRecord()))
				if tc.fileGeneration == 200 {
					path := filepath.Join(dir, "x_1000000001.json")
					data, err := os.ReadFile(path)
					require.NoError(t, err)
					data = []byte(strings.Replace(string(data), `"generation":1`, `"generation":200`, 1))
					require.NoError(t, os.WriteFile(path, data, 0600))
				}
			}
			installed, err := m.InstallVerified(t.Context(), replacement, tc.fileGeneration, 100)
			require.NoError(t, err)
			assert.Equal(t, tc.want, installed.Generation)
			loaded, err := m.Load(t.Context(), replacement.OwnerID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, loaded.Generation)
			assert.Equal(t, "access-secret", loaded.AccessToken())
			// A delayed rejection of the source's old generation cannot refresh or
			// invalidate the explicitly installed replacement.
			snapshot, err := m.Credentials(replacement.OwnerID, nil).Refresh(t.Context(), 100)
			require.NoError(t, err)
			assert.Equal(t, tc.want, snapshot.Generation)
			assert.Zero(t, tokenCalls.Load())
		})
	}
}

func TestInstallVerifiedCredentialsRequiresExactFileGeneration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		expected int64
		rotated  bool
	}{
		{"file appeared after absence check", true, 0, false}, {"file absent after prior read", false, 1, false}, {"stale copied authorization", true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewTokenManager(tokenTempDir(t))
			record := testRecord()
			record.Expiry = time.Now().Add(time.Hour)
			if tc.existing {
				require.NoError(t, m.Save(t.Context(), record))
				if tc.rotated {
					require.NoError(t, m.Save(t.Context(), record))
				}
			}
			result, err := m.InstallVerified(t.Context(), record, tc.expected, 100)
			require.Error(t, err)
			assert.Empty(t, result.AccessToken())
			current, err := m.Load(t.Context(), record.OwnerID)
			if tc.existing {
				require.NoError(t, err)
				want := int64(1)
				if tc.rotated {
					want = 2
				}
				assert.Equal(t, want, current.Generation)
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestInstallVerifiedCredentialsRejectsInvalidGenerationFloor(t *testing.T) {
	for _, tc := range []struct {
		name            string
		expected, floor int64
	}{
		{"negative floor", 0, -1}, {"negative expected file", -1, 100}, {"exhausted source generation", 0, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewTokenManager(tokenTempDir(t))
			record := testRecord()
			result, err := m.InstallVerified(t.Context(), record, tc.expected, tc.floor)
			require.Error(t, err)
			assert.Empty(t, result.AccessToken())
			_, err = m.Load(t.Context(), record.OwnerID)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestTokenRecordBearerClientUsesStagedTokenOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "Bearer access-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"title":"Synthetic rejection"}`)
	}))
	defer server.Close()
	record := testRecord()
	assert.Zero(t, record.Generation)
	_, err := NewClient(server.URL, record.BearerClient(server.Client())).Me(t.Context())
	var rejected *APIError
	require.ErrorAs(t, err, &rejected)
	assert.Equal(t, 401, rejected.StatusCode)
	assert.Equal(t, int32(1), calls.Load())
}
func TestTokenManagerDeleteRequiresCurrentGeneration(t *testing.T) {
	manager := NewTokenManager(tokenTempDir(t))
	record := testRecord()
	require.NoError(t, manager.Save(t.Context(), record))
	require.NoError(t, manager.Save(t.Context(), record))
	require.Error(t, manager.Delete(t.Context(), record.OwnerID, 1))
	current, err := manager.Load(t.Context(), record.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), current.Generation)
	require.NoError(t, manager.Delete(t.Context(), record.OwnerID, 2))
	_, err = manager.Load(t.Context(), record.OwnerID)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, manager.Delete(t.Context(), record.OwnerID, 2))
	require.Error(t, manager.Delete(t.Context(), "../escape", 2))
}
