package config

import (
	"fmt"
	"strings"
)

// FederationConfig selects this archive's federation role.
type FederationConfig struct {
	Role string `toml:"role"`
}

func (f *FederationConfig) Validate() error {
	f.Role = strings.ToLower(f.Role)
	if f.Role == "" {
		f.Role = "off"
	}
	switch f.Role {
	case "off", "spoke", "hub":
		return nil
	default:
		return fmt.Errorf("invalid [federation] role %q", f.Role)
	}
}
