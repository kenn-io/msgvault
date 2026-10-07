package api

import (
	"os"
	"testing"

	"go.kenn.io/msgvault/internal/telemetry"
)

func TestMain(m *testing.M) {
	// A developer's MSGVAULT_TELEMETRY_ENABLED=1 must not make these tests report to PostHog.
	_ = os.Setenv(telemetry.EnabledEnv, "0")
	os.Exit(m.Run())
}
