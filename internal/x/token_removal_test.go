package x

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXRemovalRevokeIsSingleRequestAndRedacted(t *testing.T) {
	for _, kind := range []ClientType{PublicClient, ConfidentialClient} {
		t.Run(string(kind), func(t *testing.T) {
			_testAssert := assert.New(t)

			var calls atomic.Int32
			record := NewTokenRecord("1001", "example-client", kind, "example-access", "example-refresh", "example-secret")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				if !assert.NoError(t, r.ParseForm()) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, "example-refresh", r.Form.Get("token"))
				if kind == PublicClient {
					assert.Equal(t, "example-client", r.Form.Get("client_id"))
					assert.Empty(t, r.Header.Get("Authorization"))
				} else {
					client, secret, ok := r.BasicAuth()
					assert.True(t, ok)
					assert.Equal(t, "example-client", client)
					assert.Equal(t, "example-secret", secret)
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, "example-refresh example-secret")
			}))
			t.Cleanup(server.Close)
			err := RevokeCredential(t.Context(), record, server.URL, server.Client())
			require.Error(t, err)
			_testAssert.NotContains(err.Error(), "example-refresh")
			_testAssert.NotContains(err.Error(), "example-secret")
			_testAssert.Equal(int32(1), calls.Load())
		})
	}
}

func TestXRemovalDeletesMalformedKnownCredentialWithoutFollowingLinks(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	dir := t.TempDir()
	_testRequire.NoError(os.Chmod(dir, 0o700))
	manager := NewTokenManager(dir)
	path := filepath.Join(dir, "x_1001.json")
	_testRequire.NoError(os.WriteFile(path, []byte("malformed credential"), 0o600))
	_testRequire.NoError(manager.RemoveCredentials(t.Context(), "1001"))
	_, err := os.Lstat(path)
	_testRequire.ErrorIs(err, os.ErrNotExist)
	_testRequire.NoError(manager.RemoveCredentials(t.Context(), "1001"))
	_testRequire.Error(manager.RemoveCredentials(t.Context(), "../outside"))
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows privileges")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	_testRequire.NoError(os.WriteFile(outside, []byte("untouched synthetic secret"), 0o600))
	_testRequire.NoError(os.Symlink(outside, path))
	_testRequire.NoError(manager.RemoveCredentials(t.Context(), "1001"))
	data, err := os.ReadFile(outside)
	_testRequire.NoError(err)
	_testAssert.Equal("untouched synthetic secret", string(data))
}
