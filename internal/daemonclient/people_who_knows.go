package daemonclient

import (
	"context"
	"fmt"
)

// WhoKnowsMinAPISchemaVersion is the first schema with POST /people/who-knows.
const WhoKnowsMinAPISchemaVersion = "3.24.0"

// RequireWhoKnows fails with an upgrade message when the daemon predates the
// who-knows route.
func (c *Client) RequireWhoKnows(ctx context.Context) error {
	version, err := c.APISchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("check daemon who-knows capability: %w", err)
	}
	if !apiSchemaVersionAtLeast(version, WhoKnowsMinAPISchemaVersion) {
		return fmt.Errorf("person who-knows requires daemon API schema %s or newer (daemon reports %q); upgrade the daemon",
			WhoKnowsMinAPISchemaVersion, version)
	}
	return nil
}
