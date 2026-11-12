package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationRoleAndHubKeys(t *testing.T) {
	for _, tc := range []struct{ name, config, role, loadError, keyError string }{
		{name: "default", role: "off"},
		{name: "case normalization", config: "[federation]\nrole = \"SPOKE\"", role: "spoke"},
		{name: "hub keyless", config: "[federation]\nrole = \"hub\"", role: "hub", keyError: "federation hub role requires an effective API key"},
		{name: "hub owner", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"owner-key\"", role: "hub"},
		{name: "reserved owner", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"mvf1_reserved\"", role: "hub", keyError: "federation hub owner key must not begin with mvf1_"},
		{name: "hub remote readers", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"owner-key\"\n[[server.remote_clients]]\nclient_id = \"reader\"\napi_key_file = \"reader\"", role: "hub", keyError: "federation hub role refuses [[server.remote_clients]]"},
		{name: "invalid", config: "[federation]\nrole = \"other\"", loadError: "invalid [federation] role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := writeRemoteClientConfig(t, tc.config, nil)
			cfg, err := Load(path, t.TempDir())
			if tc.loadError != "" {
				requirements.ErrorContains(err, tc.loadError)
				return
			}
			requirements.NoError(err)
			cfg.Data.DataDir = t.TempDir()
			assertions.Equal(tc.role, cfg.Federation.Role)
			err = cfg.ValidateServerKey()
			if tc.keyError != "" {
				assertions.ErrorContains(err, tc.keyError)
			} else {
				assertions.NoError(err)
			}
		})
	}
}
