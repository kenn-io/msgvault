package discord

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
)

func TestContainerIncluded(t *testing.T) {
	tests := []struct {
		name        string
		guild       config.DiscordGuildConfig
		containerID string
		parentID    string
		want        bool
	}{
		{name: "no filters includes top-level channel", containerID: "channel", want: true},
		{name: "no filters includes thread", containerID: "thread", parentID: "channel", want: true},
		{name: "include list omits unrelated channel", guild: config.DiscordGuildConfig{Include: []string{"included"}}, containerID: "other", want: false},
		{name: "top-level channel matches itself", guild: config.DiscordGuildConfig{Include: []string{"channel"}}, containerID: "channel", want: true},
		{name: "thread inherits included parent", guild: config.DiscordGuildConfig{Include: []string{"channel"}}, containerID: "thread", parentID: "channel", want: true},
		{name: "thread inherits excluded parent", guild: config.DiscordGuildConfig{Exclude: []string{"channel"}}, containerID: "thread", parentID: "channel", want: false},
		{name: "explicit thread include overrides excluded parent", guild: config.DiscordGuildConfig{Include: []string{"thread"}, Exclude: []string{"channel"}}, containerID: "thread", parentID: "channel", want: true},
		{name: "explicit thread exclude overrides included parent", guild: config.DiscordGuildConfig{Include: []string{"channel"}, Exclude: []string{"thread"}}, containerID: "thread", parentID: "channel", want: false},
		{name: "exclude wins explicit conflict", guild: config.DiscordGuildConfig{Include: []string{"thread"}, Exclude: []string{"thread"}}, containerID: "thread", parentID: "channel", want: false},
		{name: "exclude wins parent conflict", guild: config.DiscordGuildConfig{Include: []string{"channel"}, Exclude: []string{"channel"}}, containerID: "thread", parentID: "channel", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ContainerIncluded(tt.guild, tt.containerID, tt.parentID))
		})
	}
}
