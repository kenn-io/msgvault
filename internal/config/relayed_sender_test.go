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
		{"reserved email type", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"email\"\n", false},
		{"reserved email type case", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"Email\"\n", false},
		{"reserved phone type", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"phone\"\n", false},
		{"reserved phone type case", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"PHONE\"\n", false},
		{"duplicate address", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"example\"\n[[relayed_senders]]\naddress = \"relay@example.test\"\nheader = \"X-Other\"\nidentifier_type = \"example\"\n", false},
		{"duplicate address case", "address = \"relay@example.test\"\nheader = \"X-Author\"\nidentifier_type = \"example\"\n[[relayed_senders]]\naddress = \"Relay@Example.test\"\nheader = \"X-Other\"\nidentifier_type = \"example\"\n", false},
		{"missing type", "address = \"relay@example.test\"\nheader = \"X-Author\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte("[[relayed_senders]]\n"+tc.content), 0600))
			cfg, err := Load(path, "")
			if !tc.valid {
				assertions.ErrorContains(err, "relayed_senders")
				return
			}
			requirements.NoError(err)
			requirements.Len(cfg.RelayedSenders, 1)
			assertions.Equal("relay@example.test", cfg.RelayedSenders[0].Address)
		})
	}
}
