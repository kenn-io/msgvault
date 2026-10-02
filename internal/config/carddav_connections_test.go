package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVNamedConnectionsLoadAndSave(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[carddav]
base_url = "https://contacts.example/default/"
username = "personal@example.com"
enabled = true
[carddav_connections.work]
base_url = "https://contacts.example/work/"
username = "work@example.com"
schedule = "0 */2 * * *"
enabled = true
trusted_origin = "https://contacts.example"
trusted_addresses = ["10.1.2.3"]
[carddav_connections.google]
provider = "google"
oauth_app = "contacts"
username = "google@example.com"
schedule = "15 */6 * * *"
enabled = false
`), 0o600))
	cfg, err := Load(path, "")
	require.NoError(err)
	require.Len(cfg.CardDAVConnections, 2)
	assertions.Equal("personal@example.com", cfg.CardDAV.Username)
	assertions.Equal("work@example.com", cfg.CardDAVConnections["work"].Username)
	assertions.Equal([]string{"10.1.2.3"}, cfg.CardDAVConnections["work"].TrustedAddresses)
	assertions.Equal("contacts", cfg.CardDAVConnections["google"].OAuthApp)
	assertions.False(cfg.CardDAVConnections["google"].Enabled)
	require.NoError(cfg.Save())
	reloaded, err := Load(path, "")
	require.NoError(err)
	assertions.Equal(cfg.CardDAVConnections, reloaded.CardDAVConnections)
}

func TestCardDAVNamedConnectionsRejectInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"reserved", `[carddav_connections.default]`},
		{"traversal", `[carddav_connections."../work"]`},
		{"unicode", `[carddav_connections."wörk"]`},
		{"empty", `[carddav_connections.""]`},
		{"provider", "[carddav_connections.work]\nprovider = 'unknown'"},
		{"password", "[carddav_connections.work]\npassword = 'synthetic-secret'"},
		{"trusted origin", "[carddav_connections.work]\ntrusted_addresses = ['10.1.2.3']"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			_, err := Load(path, "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "carddav_connections")
		})
	}
}

func TestCardDAVConnectionNameBoundaries(t *testing.T) {
	for _, name := range []string{"default", "work", "a", "a_b-2", strings.Repeat("a", 64)} {
		require.NoError(t, ValidateCardDAVConnectionName(name), name)
	}
	for _, name := range []string{"", "Work", "1work", ".", "..", "../work", "work/path", `work\path`, "wörk", "work.name", strings.Repeat("a", 65)} {
		assert.Error(t, ValidateCardDAVConnectionName(name), name)
	}
}

func TestCardDAVConnectionsRejectDuplicateAccounts(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{"default and named", "[carddav]\nbase_url = 'https://contacts.example/dav/'\nusername = 'person'", "base_url = 'https://contacts.example/dav/'\nusername = 'person'"},
		{"named and disabled", "[carddav_connections.personal]\nbase_url = 'https://contacts.example/dav/'\nusername = 'person'", "base_url = 'https://contacts.example/dav/'\nusername = 'person'\nenabled = false"},
		{"Google email and different apps", "[carddav_connections.personal]\nprovider = 'google'\nusername = 'Person@example.com'\noauth_app = 'personal'", "provider = 'google'\nusername = 'person@example.com'\noauth_app = 'work'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.first+"\n[carddav_connections.work]\n"+tc.second), 0o600))
			_, err := Load(path, "")
			require.ErrorContains(t, err, "already belongs to connection")
		})
	}
}

// The oracle follows the documented ASCII grammar byte by byte, independently
// of the production validator. Arbitrary strings exercise acceptance and rejection.
func FuzzCardDAVConnectionName(f *testing.F) {
	for _, name := range []string{"default", "work-2", "", "../work", "wörk", strings.Repeat("a", 64), strings.Repeat("a", 65)} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		want := len(name) >= 1 && len(name) <= 64
		if want {
			want = name[0] >= 'a' && name[0] <= 'z'
			for i := 1; i < len(name); i++ {
				c := name[i]
				want = want && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-')
			}
		}
		assert.Equal(t, want, ValidateCardDAVConnectionName(name) == nil)
	})
}
