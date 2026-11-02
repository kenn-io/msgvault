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
	"strconv"
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
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			dir := tokenTempDir(t)
			var resourceCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resource" {
					resourceCalls.Add(1)
					return
				}
				if !assert.NoError(t, r.ParseForm()) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, "refresh-secret", r.Form.Get("refresh_token"))
				if fail {
					if !assert.NoError(t, os.Remove(filepath.Join(dir, "x_1000000001.json"))) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					if !assert.NoError(t, os.Mkdir(filepath.Join(dir, "x_1000000001.json"), 0700)) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read"}`)
			}))
			defer server.Close()
			m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
			_testRequire.NoError(m.Save(t.Context(), testRecord()))
			creds := m.Credentials("1000000001", nil)
			snapshot, err := creds.Prepare(t.Context())
			if fail {
				_testRequire.Error(err)
				_testAssert.Zero(snapshot.Generation)
				response, useErr := creds.HTTPClient().Get(server.URL + "/resource")
				_testRequire.Error(useErr)
				if response != nil {
					_testRequire.NoError(response.Body.Close())
				}
				_testAssert.Nil(response)
			} else {
				_testRequire.NoError(err)
				saved, err := m.Load(t.Context(), "1000000001")
				_testRequire.NoError(err)
				_testAssert.Equal("rotated-refresh", saved.RefreshToken())
				_testAssert.Equal([]string{"tweet.read", "users.read"}, snapshot.GrantedScopes)
				resp, err := creds.HTTPClient().Get(server.URL + "/resource")
				_testRequire.NoError(err)
				_testRequire.NoError(resp.Body.Close())
			}
			if fail {
				_testAssert.Zero(resourceCalls.Load())
			} else {
				_testAssert.Equal(int32(1), resourceCalls.Load())
			}
		})
	}
}

func TestRefreshSerializesAndPreservesOmittedToken(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"next-access","token_type":"bearer","expires_in":7200}`)
	}))
	defer server.Close()
	dir := tokenTempDir(t)
	m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
	original := testRecord()
	_testRequire.NoError(m.Save(t.Context(), original))
	original, err := m.Load(t.Context(), original.OwnerID)
	_testRequire.NoError(err)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			other := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
			r, err := other.Refresh(t.Context(), original.OwnerID)
			if err != nil {
				errs <- err
				return
			}
			_testAssert.Equal("refresh-secret", r.RefreshToken())
			_testAssert.NotEqual(original.Generation, r.Generation)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		_testRequire.NoError(err)
	}
	_testAssert.Equal(int32(1), calls.Load())
}

func TestTokenFilesRejectUnsafePathsAndRedact(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	r := testRecord()
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%p"} {
		for _, v := range []any{r, &r} {
			rendered := fmt.Sprintf(format, v)
			_testAssert.NotContains(rendered, "access-secret")
			_testAssert.NotContains(rendered, "refresh-secret")
		}
	}
	encoded, err := json.Marshal(r)
	_testRequire.NoError(err)
	_testAssert.NotContains(string(encoded), "access-secret")
	_testAssert.NotContains(string(encoded), "refresh-secret")
	dir := tokenTempDir(t)
	m := NewTokenManager(dir)
	_testRequire.NoError(m.Save(t.Context(), r))
	path := filepath.Join(dir, "x_1000000001.json")
	info, err := os.Stat(path)
	_testRequire.NoError(err)
	if runtime.GOOS != "windows" {
		_testAssert.Equal(os.FileMode(0600), info.Mode().Perm())
	}
	_, err = m.Load(t.Context(), "../escape")
	_testRequire.Error(err)
	if runtime.GOOS != "windows" {
		_testRequire.NoError(os.Chmod(path, 0644))
		_, err = m.Load(t.Context(), r.OwnerID)
		_testRequire.Error(err)
		_testRequire.Error(m.Save(t.Context(), r))
	}
	_testRequire.NoError(os.Remove(path))
	target := filepath.Join(tokenTempDir(t), "target")
	_testRequire.NoError(os.WriteFile(target, []byte("untouched"), 0600))
	tokenSymlink(t, target, path)
	_, err = m.Load(t.Context(), r.OwnerID)
	_testRequire.Error(err)
	_testRequire.Error(m.Save(t.Context(), r))
	content, err := os.ReadFile(target)
	_testRequire.NoError(err)
	_testAssert.Equal("untouched", string(content))
}

func TestBearerNeverRefreshesOrReplaysPOST(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

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
	_testRequire.NoError(m.Save(t.Context(), r))
	c := m.Credentials(r.OwnerID, nil)
	_, err := c.Prepare(t.Context())
	_testRequire.NoError(err)
	resp, err := c.HTTPClient().Post(server.URL, "text/plain", strings.NewReader("example"))
	_testRequire.NoError(err)
	_testRequire.NoError(resp.Body.Close())
	_testAssert.Equal(401, resp.StatusCode)
	_testAssert.Equal(int32(1), calls.Load())
}

func TestRefreshInvalidGrantIsGenerationBound(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh-secret"}`)
	}))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
	old := testRecord()
	_testRequire.NoError(m.Save(t.Context(), old))
	old, loadErr := m.Load(t.Context(), old.OwnerID)
	_testRequire.NoError(loadErr)
	c := m.Credentials(old.OwnerID, nil)
	_, err := c.Prepare(t.Context())
	var revoked *InvalidGrantError
	_testRequire.ErrorAs(err, &revoked)
	_testAssert.Equal(old.Generation, revoked.Generation)
	_testAssert.NotContains(err.Error(), "refresh-secret")
	replacement := testRecord()
	replacement.Expiry = time.Now().Add(time.Hour)
	_testRequire.NoError(m.WithLifecycleLock(t.Context(), old.OwnerID, func() error { return m.Save(t.Context(), replacement) }))
	replacement, loadErr = m.Load(t.Context(), old.OwnerID)
	_testRequire.NoError(loadErr)
	snapshot, err := c.Refresh(t.Context(), old.Generation)
	_testRequire.NoError(err)
	_testAssert.Equal(replacement.Generation, snapshot.Generation)
	_testAssert.NotEqual(revoked.Generation, snapshot.Generation)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_testRequire.Error(m.WithLifecycleLock(ctx, old.OwnerID, func() error { return nil }))
}

func TestTokenRootAndLockSecurity(t *testing.T) {
	for _, scenario := range []string{"root-symlink", "root-permissions", "lock-symlink", "lock-permissions", "owner-mismatch", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			_testRequire := require.New(t)

			if runtime.GOOS == "windows" && strings.Contains(scenario, "permissions") {
				t.Skip("Unix permission modes")
			}
			dir := tokenTempDir(t)
			m := NewTokenManager(dir)
			r := testRecord()
			_testRequire.NoError(m.Save(t.Context(), r))
			path := filepath.Join(dir, "x_1000000001.json")
			switch scenario {
			case "root-symlink":
				link := filepath.Join(tokenTempDir(t), "tokens")
				tokenSymlink(t, dir, link)
				m = NewTokenManager(link)
			case "root-permissions":
				_testRequire.NoError(os.Chmod(dir, 0755))
			case "lock-symlink":
				lock := filepath.Join(dir, ".x-store.lock")
				_testRequire.NoError(os.Remove(lock))
				tokenSymlink(t, path, lock)
			case "lock-permissions":
				_testRequire.NoError(os.Chmod(filepath.Join(dir, ".x-store.lock"), 0644))
			case "owner-mismatch":
				data, err := os.ReadFile(path)
				_testRequire.NoError(err)
				_testRequire.NoError(os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "1000000001", "1000000002")), 0600))
			case "oversized":
				_testRequire.NoError(os.WriteFile(path, []byte(strings.Repeat(" ", 2<<20)), 0600))
			}
			_, err := m.Load(t.Context(), r.OwnerID)
			_testRequire.Error(err)
		})
	}
}

func TestCredentialsRejectedGenerationRefreshesExplicitly(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"replacement","token_type":"bearer","expires_in":7200,"scope":"tweet.read"}`)
	}))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	_testRequire.NoError(m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	old, err := c.Prepare(t.Context())
	_testRequire.NoError(err)
	_testAssert.Zero(calls.Load())
	next, err := c.Refresh(t.Context(), old.Generation)
	_testRequire.NoError(err)
	_testAssert.NotEqual(old.Generation, next.Generation)
	_testAssert.Equal([]string{"tweet.read"}, next.GrantedScopes)
	again, err := c.Refresh(t.Context(), old.Generation)
	_testRequire.NoError(err)
	_testAssert.Equal(next.Generation, again.Generation)
	_testAssert.Equal(int32(1), calls.Load())
}

func TestCredentialGenerationMonotoneAndStaleSaveRejected(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	dir := tokenTempDir(t)
	m := NewTokenManager(dir)
	record := testRecord()
	_testRequire.NoError(m.Save(t.Context(), record))
	first, err := m.Load(t.Context(), record.OwnerID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(1), first.Generation)
	_testRequire.NoError(m.Save(t.Context(), testRecord()))
	second, err := m.Load(t.Context(), record.OwnerID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(2), second.Generation)
	_testRequire.Error(m.Save(t.Context(), first))
	current, err := m.Load(t.Context(), record.OwnerID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(2), current.Generation)
	_testAssert.Less(current.Generation, int64(math.MaxInt64))
	path := filepath.Join(dir, "x_1000000001.json")
	data, err := os.ReadFile(path)
	_testRequire.NoError(err)
	data = []byte(strings.Replace(string(data), `"generation":2`, `"generation":9223372036854775807`, 1))
	_testRequire.NoError(os.WriteFile(path, data, 0600))
	_testRequire.Error(m.Save(t.Context(), testRecord()))
}

func TestRefreshAuthenticationAndCancellation(t *testing.T) {
	for _, kind := range []ClientType{PublicClient, ConfidentialClient} {
		t.Run(string(kind), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !assert.NoError(t, r.ParseForm()) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
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
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"error":"invalid_client","error_description":"client-secret access-secret"}`)
			}))
			defer server.Close()
			r := NewTokenRecord("1000000001", "example-client", kind, "access-secret", "refresh-secret", "")
			if kind == ConfidentialClient {
				r = NewTokenRecord("1000000001", "example-client", kind, "access-secret", "refresh-secret", "client-secret")
			}
			r.GrantedScopes = []string{"tweet.read"}
			r.Expiry = time.Now().Add(-time.Hour)
			m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: server.URL})
			_testRequire.NoError(m.Save(t.Context(), r))
			_, err := m.Refresh(t.Context(), r.OwnerID)
			_testRequire.Error(err)
			_testAssert.NotContains(err.Error(), "client-secret")
			_testAssert.NotContains(err.Error(), "access-secret")
			_testAssert.Equal(int32(1), calls.Load())
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = m.Refresh(ctx, r.OwnerID)
			_testRequire.ErrorIs(err, context.Canceled)
			_testAssert.Equal(int32(1), calls.Load())
		})
	}
}

func TestBearerRefusesRedirectsAndUseBeforePrepare(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	m := NewTokenManager(tokenTempDir(t))
	r := testRecord()
	r.Expiry = time.Now().Add(time.Hour)
	_testRequire.NoError(m.Save(t.Context(), r))
	c := m.Credentials(r.OwnerID, nil)
	resp, err := c.HTTPClient().Post(origin.URL, "text/plain", strings.NewReader("example"))
	_testRequire.Error(err)
	if resp != nil {
		_testRequire.NoError(resp.Body.Close())
	}
	_testAssert.Nil(resp)
	_, err = c.Prepare(t.Context())
	_testRequire.NoError(err)
	resp, err = c.HTTPClient().Post(origin.URL, "text/plain", strings.NewReader("example"))
	_testRequire.NoError(err)
	_testRequire.NoError(resp.Body.Close())
	_testAssert.Equal(307, resp.StatusCode)
	_testAssert.Zero(forwarded.Load())
}

func TestFailedRotationInvalidatesPreparedBearer(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	dir := tokenTempDir(t)
	var resources atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resource" {
			resources.Add(1)
			return
		}
		path := filepath.Join(dir, "x_1000000001.json")
		if !assert.NoError(t, os.Remove(path)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(t, os.Mkdir(path, 0700)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"bearer","expires_in":7200}`)
	}))
	defer server.Close()
	m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	_testRequire.NoError(m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	snapshot, err := c.Prepare(t.Context())
	_testRequire.NoError(err)
	_, err = c.Refresh(t.Context(), snapshot.Generation)
	_testRequire.Error(err)
	resp, err := c.HTTPClient().Get(server.URL + "/resource")
	_testRequire.Error(err)
	if resp != nil {
		_testRequire.NoError(resp.Body.Close())
	}
	_testAssert.Nil(resp)
	_testAssert.Zero(resources.Load())
}

func TestSnapshotScopesCannotMutatePreparedGrant(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	m := NewTokenManager(tokenTempDir(t))
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	_testRequire.NoError(m.Save(t.Context(), record))
	c, ok := m.Credentials(record.OwnerID, nil).(*accountCredentials)
	_testRequire.True(ok)
	snapshot, err := c.Prepare(t.Context())
	_testRequire.NoError(err)
	snapshot.GrantedScopes[0] = "tweet.write"
	_testAssert.Equal(record.GrantedScopes, c.record.GrantedScopes)
}

func TestRefreshCancellationDuringTokenHTTP(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, r.ParseForm()) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
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
	_testRequire.NoError(m.Save(t.Context(), record))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Refresh(ctx, record.OwnerID); done <- err }()
	<-entered
	cancel()
	_testRequire.ErrorIs(<-done, context.Canceled)
	saved, err := m.Load(t.Context(), record.OwnerID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(1), saved.Generation)
	_testAssert.Equal("refresh-secret", saved.RefreshToken())
}

func tokenSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func TestInvalidRefreshGenerationClearsBearer(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	m := NewTokenManager(tokenTempDir(t))
	record := testRecord()
	record.Expiry = time.Now().Add(time.Hour)
	_testRequire.NoError(m.Save(t.Context(), record))
	c := m.Credentials(record.OwnerID, nil)
	_, err := c.Prepare(t.Context())
	_testRequire.NoError(err)
	_, err = c.Refresh(t.Context(), 0)
	_testRequire.Error(err)
	resp, err := c.HTTPClient().Get(server.URL)
	_testRequire.Error(err)
	if resp != nil {
		_testRequire.NoError(resp.Body.Close())
	}
	_testAssert.Nil(resp)
	_testAssert.Zero(calls.Load())
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
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			var tokenCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tokenCalls.Add(1) }))
			defer server.Close()
			dir := tokenTempDir(t)
			m := NewTokenManager(dir, TokenManagerOptions{TokenURL: server.URL})
			replacement := testRecord()
			replacement.Expiry = time.Now().Add(time.Hour)
			if tc.fileGeneration > 0 {
				_testRequire.NoError(m.Save(t.Context(), testRecord()))
				if tc.fileGeneration == 200 {
					path := filepath.Join(dir, "x_1000000001.json")
					data, err := os.ReadFile(path)
					_testRequire.NoError(err)
					data = []byte(strings.Replace(string(data), `"generation":1`, `"generation":200`, 1))
					_testRequire.NoError(os.WriteFile(path, data, 0600))
				}
			}
			installed, err := m.InstallVerified(t.Context(), replacement, tc.fileGeneration, 100)
			_testRequire.NoError(err)
			_testAssert.Equal(tc.want, installed.Generation)
			loaded, err := m.Load(t.Context(), replacement.OwnerID)
			_testRequire.NoError(err)
			_testAssert.Equal(tc.want, loaded.Generation)
			_testAssert.Equal("access-secret", loaded.AccessToken())
			// A delayed rejection of the source's old generation cannot refresh or
			// invalidate the explicitly installed replacement.
			snapshot, err := m.Credentials(replacement.OwnerID, nil).Refresh(t.Context(), 100)
			_testRequire.NoError(err)
			_testAssert.Equal(tc.want, snapshot.Generation)
			_testAssert.Zero(tokenCalls.Load())
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
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			m := NewTokenManager(tokenTempDir(t))
			record := testRecord()
			record.Expiry = time.Now().Add(time.Hour)
			if tc.existing {
				_testRequire.NoError(m.Save(t.Context(), record))
				if tc.rotated {
					_testRequire.NoError(m.Save(t.Context(), record))
				}
			}
			result, err := m.InstallVerified(t.Context(), record, tc.expected, 100)
			_testRequire.Error(err)
			_testAssert.Empty(result.AccessToken())
			current, err := m.Load(t.Context(), record.OwnerID)
			if tc.existing {
				_testRequire.NoError(err)
				want := int64(1)
				if tc.rotated {
					want = 2
				}
				_testAssert.Equal(want, current.Generation)
			} else {
				_testRequire.ErrorIs(err, os.ErrNotExist)
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
	_testAssert := assert.New(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "Bearer access-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"title":"Synthetic rejection"}`)
	}))
	defer server.Close()
	record := testRecord()
	_testAssert.Zero(record.Generation)
	_, err := NewClient(server.URL, record.BearerClient(server.Client())).Me(t.Context())
	var rejected *APIError
	require.ErrorAs(t, err, &rejected)
	_testAssert.Equal(401, rejected.StatusCode)
	_testAssert.Equal(int32(1), calls.Load())
}
func TestTokenManagerDeleteRequiresCurrentGeneration(t *testing.T) {
	_testRequire := require.New(t)

	manager := NewTokenManager(tokenTempDir(t))
	record := testRecord()
	_testRequire.NoError(manager.Save(t.Context(), record))
	_testRequire.NoError(manager.Save(t.Context(), record))
	_testRequire.Error(manager.Delete(t.Context(), record.OwnerID, 1))
	current, err := manager.Load(t.Context(), record.OwnerID)
	_testRequire.NoError(err)
	assert.Equal(t, int64(2), current.Generation)
	_testRequire.NoError(manager.Delete(t.Context(), record.OwnerID, 2))
	_, err = manager.Load(t.Context(), record.OwnerID)
	_testRequire.ErrorIs(err, os.ErrNotExist)
	_testRequire.NoError(manager.Delete(t.Context(), record.OwnerID, 2))
	_testRequire.Error(manager.Delete(t.Context(), "../escape", 2))
}
