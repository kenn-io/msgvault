package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayedSenderConfig(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"valid", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"example\"\n", true},
		{"invalid address", "address = \"not-a-mailbox\"\nheader = \"X-Author\"\nidentifier_type = \"example\"\n", false},
		{"invalid header", "address = \"relay@example.test\"\nheader = \"bad header\"\nidentifier_type = \"example\"\n", false},
		{"missing type", "address = \"relay@example.test\"\nheader = \"X-Author\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[[relayed_senders]]\n"+tc.content), 0600))
			cfg, err := Load(path, "")
			if !tc.valid {
				assert.ErrorContains(t, err, "relayed_senders")
				return
			}
			require.NoError(t, err)
			require.Len(t, cfg.RelayedSenders, 1)
			assert.Equal(t, "relay@example.test", cfg.RelayedSenders[0].Address)
		})
	}
}
