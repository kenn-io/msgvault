package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestSenderAuthConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   SenderAuthConfig
		valid bool
	}{
		{"empty", SenderAuthConfig{}, true},
		{"valid", SenderAuthConfig{Sources: []SenderAuthSource{{SourceID: 1, TrustedAuthservIDs: []string{"MX.test", "*.provider.test"}}}}, true},
		{"invalid id", SenderAuthConfig{Sources: []SenderAuthSource{{SourceID: 0}}}, false},
		{"duplicate id", SenderAuthConfig{Sources: []SenderAuthSource{{SourceID: 1}, {SourceID: 1}}}, false},
		{"invalid pin", SenderAuthConfig{Sources: []SenderAuthSource{{SourceID: 1, TrustedAuthservIDs: []string{"*.test; dmarc=pass"}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			if tc.valid {
				checks.NoError(tc.cfg.Validate())
			} else {
				checks.Error(tc.cfg.Validate())
			}
		})
	}
}

func TestSenderAuthConfigLoad(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	must.NoError(os.WriteFile(path, []byte("[[sender_auth.sources]]\nsource_id = 7\ntrusted_authserv_ids = [\"mx.test\"]\n"), 0600))
	cfg, err := Load(path, t.TempDir())
	must.NoError(err)
	checks.Equal([]string{"mx.test"}, cfg.SenderAuth.Sources[0].TrustedAuthservIDs)
	must.NoError(os.WriteFile(path, []byte("[[sender_auth.sources]]\nsource_id = 0\n"), 0600))
	_, err = Load(path, t.TempDir())
	checks.ErrorContains(err, "sender_auth")
}
