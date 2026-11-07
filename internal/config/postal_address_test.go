package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostalAddressConfigDisabledByDefaultAndExplicitProvider(t *testing.T) {
	assert.False(t, NewDefaultConfig().People.AddressValidation.Enabled)
	for _, tc := range []struct {
		name, settings string
		wantErr        bool
	}{
		{"local", "", false},
		{"configured but disabled", "provider = \"google\"", false},
		{"enabled", "enabled = true\nprovider = \"google\"", false},
		{"missing provider", "enabled = true", true},
		{"unsupported provider", "enabled = true\nprovider = \"other\"", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			require := require.New(t)
			require.NoError(os.WriteFile(path, []byte("[people.address_validation]\n"+tc.settings), 0o600))
			cfg, err := Load(path, home)
			if tc.wantErr {
				require.Error(err)
				return
			}
			require.NoError(err)
			assert.Equal(t, tc.name == "enabled", cfg.People.AddressValidation.Enabled)
		})
	}
}
