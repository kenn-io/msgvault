package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
)

// cacheStatsRemote answers cache-stats with its own message count and records
// the API key each request carried.
type cacheStatsRemote struct {
	*httptest.Server

	mu   sync.Mutex
	keys []string
}

func newCacheStatsRemote(t *testing.T, totalMessages int) *cacheStatsRemote {
	t.Helper()
	remote := &cacheStatsRemote{}
	remote.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cli/cache-stats" {
			http.NotFound(w, r)
			return
		}
		remote.mu.Lock()
		remote.keys = append(remote.keys, r.Header.Get("X-Api-Key"))
		remote.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ready","total_messages":%d}`, totalMessages)
	}))
	t.Cleanup(remote.Close)
	return remote
}

func (r *cacheStatsRemote) requestKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

func TestNamedRemoteSelectsServerAndCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Cleanup(func() { clearInvocationFlags(rootCmd) })
	home := t.TempDir()
	defaultRemote := newCacheStatsRemote(t, 11)
	workRemote := newCacheStatsRemote(t, 22)
	require.NoError(fileutil.SecureWriteFile(filepath.Join(home, "work.key"), []byte("work-remote-key\n"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[remote]
url = `+strconv.Quote(defaultRemote.URL)+`
allow_insecure = true

[remotes.work]
url = `+strconv.Quote(workRemote.URL)+`
api_key_file = "work.key"
allow_insecure = true
`), 0o600))

	out, err := runRootForTest(t, home, "cache-stats")
	require.NoError(err)
	assert.Contains(out, "11", "[remote] stays the default")

	out, err = runRootForTest(t, home, "--remote", "work", "cache-stats")
	require.NoError(err)
	assert.Contains(out, "22")
	assert.Equal([]string{""}, defaultRemote.requestKeys())
	assert.Equal([]string{"work-remote-key"}, workRemote.requestKeys(), "the key file resolves next to the config")

	_, err = runRootForTest(t, home, "--remote", "missing", "cache-stats")
	require.ErrorContains(err, `remote "missing" is not configured`)

	_, err = runRootForTest(t, home, "--local", "--remote", "work", "cache-stats")
	require.ErrorContains(err, "--local and --remote are incompatible")
	assert.Len(workRemote.requestKeys(), 1, "rejected invocations send nothing")
}

func TestSavingConfigKeepsRemoteWhileNamedRemoteSelected(t *testing.T) {
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[remote]
url = "https://archive.example.com"

[remotes.work]
url = "https://work.example.com"
api_key_file = "work.key"
`), 0o600))
	cfg, err := config.Load(path, home)
	require.NoError(err)
	require.NoError(cfg.SelectRemote("work"))
	require.Equal("https://work.example.com", cfg.Remote.URL)

	require.NoError(cfg.Save())

	saved, err := config.Load(path, home)
	require.NoError(err)
	require.Equal("https://archive.example.com", saved.Remote.URL)
	require.Empty(saved.Remote.APIKeyFile)
	require.Equal(config.RemoteConfig{URL: "https://work.example.com", APIKeyFile: "work.key"}, saved.Remotes["work"])
}
