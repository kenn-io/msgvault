package clirun

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvAllowed(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: EnvIMAPPassword, want: true},
		{name: EnvBeeperToken, want: true},
		{name: EnvDiscordToken, want: true},
		{name: EnvRemoteDeleteOptIn, want: true},
		{name: "PATH", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EnvAllowed(tt.name))
		})
	}
}

func TestDiscordTokenEnvironmentName(t *testing.T) {
	assert.Equal(t, "MSGVAULT_DISCORD_TOKEN", EnvDiscordToken)
}
