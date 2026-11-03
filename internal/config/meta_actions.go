package config

import (
	"fmt"
	"slices"
)

// ValidateMetaActions checks a profile's explicit opt-ins against its write API.
// Profile-specific dependencies, such as human_agent requiring send, are checked
// by the profile validator.
func ValidateMetaActions(kind string, idx int, actions []string, allowed ...string) error {
	seen := make(map[string]bool, len(actions))
	for _, action := range actions {
		if seen[action] || !slices.Contains(allowed, action) {
			return fmt.Errorf("%s[%d].actions has unknown or duplicate action %q", kind, idx, action)
		}
		seen[action] = true
	}
	return nil
}

// ActionsAllow reports whether a profile opted into this provider write.
func ActionsAllow(actions []string, action string) bool { return slices.Contains(actions, action) }
