package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationRoleAndHubKeys(t *testing.T) {
	for _, tc := range []struct{ name, config, role, loadError, keyError string }{
		{name: "default", role: "off"},
		{name: "spoke", config: "[federation]\nrole = \"spoke\"", role: "spoke"},
		{name: "hub keyless", config: "[federation]\nrole = \"hub\"", role: "hub", keyError: "federation hub role requires an effective API key"},
		{name: "hub owner", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"owner-key\"", role: "hub"},
		{name: "reserved owner", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"mvf1_reserved\"", role: "hub", keyError: "federation hub owner key must not begin with mvf1_"},
		{name: "invalid", config: "[federation]\nrole = \"other\"", loadError: "invalid [federation] role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRemoteClientConfig(t, tc.config, nil)
			cfg, err := Load(path, t.TempDir())
			if tc.loadError != "" {
				require.ErrorContains(t, err, tc.loadError)
				return
			}
			require.NoError(t, err)
			cfg.Data.DataDir = t.TempDir()
			assert.Equal(t, tc.role, cfg.Federation.Role)
			err = cfg.PrepareServerKey()
			if tc.keyError != "" {
				assert.ErrorContains(t, err, tc.keyError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
