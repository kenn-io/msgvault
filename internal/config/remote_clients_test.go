package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
)

func writeRemoteClientConfig(t *testing.T, content string, keys map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "keys"), 0o700))
	for name, key := range keys {
		require.NoError(t, fileutil.SecureWriteFile(filepath.Join(dir, "keys", name), []byte(key+"\n"), 0o600))
	}
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, fileutil.SecureWriteFile(path, []byte(content), 0o600))
	return path
}

func TestRemoteClientKeysLoadWhenServerKeyIsPrepared(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeRemoteClientConfig(t, `
[server]
api_key_file = "keys/owner"

[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/laptop"
`, map[string]string{"owner": "owner-key", "laptop": "reader-key"})

	cfg, err := Load(path, "")
	require.NoError(err)
	require.Len(cfg.Server.RemoteClients, 1)
	require.NoError(cfg.PrepareServerKey())
	assert.Equal("reader-key", cfg.Server.RemoteClients[0].APIKey)

	require.NoError(cfg.Save())
	saved, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(string(saved), "reader-key")
	assert.Contains(string(saved), `api_key_file = "keys/laptop"`)
}

func TestRemoteClientKeysRejectInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantErr string
		keys                   map[string]string
		atLoad                 bool
	}{
		{
			name: "requires effective server key",
			content: `[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/laptop"`,
			keys:    map[string]string{"laptop": "reader-key"},
			wantErr: "requires an effective API key",
		},
		{
			name: "empty client_id",
			content: `[server]
api_key = "owner-key"
[[server.remote_clients]]
api_key_file = "keys/laptop"`,
			keys:    map[string]string{"laptop": "reader-key"},
			wantErr: "client_id",
			atLoad:  true,
		},
		{
			name: "duplicate client_id",
			content: `[server]
api_key = "owner-key"
[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/a"
[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/b"`,
			keys:    map[string]string{"a": "reader-a", "b": "reader-b"},
			wantErr: "client_id",
			atLoad:  true,
		},
		{
			name: "missing key file",
			content: `[server]
api_key = "owner-key"
[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/missing"`,
			wantErr: "credential file",
		},
		{
			name: "key equals owner key from file",
			content: `[server]
api_key_file = "keys/owner"
[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/laptop"`,
			keys:    map[string]string{"owner": "owner-key", "laptop": "owner-key"},
			wantErr: "duplicates the server API key",
		},
		{
			name: "bearer alias of owner key",
			content: `[server]
api_key = "owner-key"
[[server.remote_clients]]
client_id = "laptop"
api_key_file = "keys/laptop"`,
			keys:    map[string]string{"laptop": "Bearer owner-key"},
			wantErr: "must not contain whitespace",
		},
		{
			name: "duplicate keys",
			content: `[server]
api_key = "owner-key"
[[server.remote_clients]]
client_id = "a"
api_key_file = "keys/a"
[[server.remote_clients]]
client_id = "b"
api_key_file = "keys/b"`,
			keys:    map[string]string{"a": "reader-key", "b": "reader-key"},
			wantErr: "duplicates remote client a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeRemoteClientConfig(t, tc.content, tc.keys), "")
			if tc.atLoad {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err, "config load must not read reader key files")
			assert.ErrorContains(t, cfg.PrepareServerKey(), tc.wantErr)
		})
	}
}

func TestRemoteClientKeyMustDifferFromMintedServerKey(t *testing.T) {
	require := require.New(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(configPath, []byte("[server]\nbind_addr = \"0.0.0.0\"\n"), 0o600))
	minted, err := Load("", home)
	require.NoError(err)
	require.NoError(minted.PrepareServerKey())
	key := minted.Server.AuthenticationKey()
	require.NotEmpty(key)

	require.NoError(fileutil.SecureWriteFile(filepath.Join(home, "reader-key"), []byte(key), 0o600))
	require.NoError(fileutil.SecureWriteFile(configPath, []byte(`[server]
bind_addr = "0.0.0.0"
[[server.remote_clients]]
client_id = "laptop"
api_key_file = "reader-key"
`), 0o600))
	cfg, err := Load("", home)
	require.NoError(err)
	require.ErrorContains(cfg.PrepareServerKey(), "duplicates the server API key")
}
