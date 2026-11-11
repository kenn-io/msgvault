package cmd

import "testing"

func TestZoomPhoneValidation(t *testing.T) { assertZoomNeedsToken(t, "import-zoom-phone") }
