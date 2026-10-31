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
			var calls atomic.Int32
			record := NewTokenRecord("1001", "example-client", kind, "example-access", "example-refresh", "example-secret")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				require.NoError(t, r.ParseForm())
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
			assert.NotContains(t, err.Error(), "example-refresh")
			assert.NotContains(t, err.Error(), "example-secret")
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestXRemovalDeletesMalformedKnownCredentialWithoutFollowingLinks(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	manager := NewTokenManager(dir)
	path := filepath.Join(dir, "x_1001.json")
	require.NoError(t, os.WriteFile(path, []byte("malformed credential"), 0o600))
	require.NoError(t, manager.RemoveCredentials(t.Context(), "1001"))
	_, err := os.Lstat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, manager.RemoveCredentials(t.Context(), "1001"))
	assert.Error(t, manager.RemoveCredentials(t.Context(), "../outside"))
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows privileges")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	require.NoError(t, os.WriteFile(outside, []byte("untouched synthetic secret"), 0o600))
	require.NoError(t, os.Symlink(outside, path))
	require.NoError(t, manager.RemoveCredentials(t.Context(), "1001"))
	data, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "untouched synthetic secret", string(data))

}
