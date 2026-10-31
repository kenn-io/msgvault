package telemetry

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineScreenUsesTelemetryAllowlist(t *testing.T) {
	enableTelemetryEnv(t)
	stub := newWireStub(t)
	reporter := newReporterOrDisabled(Options{DataDir: t.TempDir(), ConfigEnabled: true}, testAPIKey, stub.server.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	properties, _ := reporter.SanitizeProperties(EventScreenViewed, map[string]any{"screen": "timeline", "surface": "web", "query": "private"})
	assert.Equal(t, "timeline", properties["screen"])
	assert.Equal(t, "web", properties["surface"])
	assert.NotContains(t, properties, "query")
}
