package cmd

import (
	"testing"
)

func TestZoomDiscoveryValidation(t *testing.T) { assertZoomNeedsToken(t, "import-zoom-meetings") }
