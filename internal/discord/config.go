package discord

import (
	"slices"

	"go.kenn.io/msgvault/internal/config"
)

// ContainerIncluded reports whether a channel, thread, or forum post passes a
// guild's message-container filters. Threads inherit their parent's decision
// unless their own ID appears explicitly; an exclusion wins any same-level
// include/exclude conflict.
func ContainerIncluded(guild config.DiscordGuildConfig, containerID, parentID string) bool {
	if slices.Contains(guild.Exclude, containerID) {
		return false
	}
	if slices.Contains(guild.Include, containerID) {
		return true
	}

	if parentID != "" {
		if slices.Contains(guild.Exclude, parentID) {
			return false
		}
		if slices.Contains(guild.Include, parentID) {
			return true
		}
	}

	return len(guild.Include) == 0
}
