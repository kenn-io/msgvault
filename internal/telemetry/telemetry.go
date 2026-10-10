// Package telemetry sends anonymous, opt-out daemon and UI usage events.
package telemetry

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.kenn.io/kit/telemetry/posthog"
)

const (
	// EnabledEnv set to 0 turns telemetry off; any value overrides [telemetry] enabled.
	EnabledEnv = "MSGVAULT_TELEMETRY_ENABLED"
	// EventAppOpened is reported by the web UI through the daemon.
	EventAppOpened = "app_opened"
	// EventScreenViewed counts a fixed screen once per installation per UTC day.
	EventScreenViewed = "screen_viewed"
	// EventSessionEnded reports bucketed UI session duration.
	EventSessionEnded = "session_ended"
	// propertySurface names the interface that reported an event.
	propertySurface = "surface"
	application     = "msgvault"
	envPrefix       = "MSGVAULT"
	// PostHog project API keys are public ingest identifiers, not credentials.
	postHogAPIKey = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf" // #nosec G101
)

// Options configures the daemon reporter.
type Options struct {
	DataDir string
	Version string
	Commit  string
	// ConfigEnabled is [telemetry] enabled; a set EnabledEnv wins over it.
	ConfigEnabled bool
}

// NewReporterOrDisabled builds the reporter, logging that telemetry is on and
// how to turn it off. When it can't build one it logs why and returns kit's
// disabled reporter so startup continues.
func NewReporterOrDisabled(opts Options, logger *slog.Logger) *posthog.Reporter {
	return newReporterOrDisabled(opts, "", logger)
}

// newReporterOrDisabled takes the endpoint so the helper-process test can point the enabled path at a stub; production passes "".
func newReporterOrDisabled(opts Options, endpoint string, logger *slog.Logger) *posthog.Reporter {
	reporter, err := buildReporter(opts, endpoint, logger)
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return posthog.DisabledReporter()
	}
	if reporter.Enabled() {
		logger.Info("anonymous telemetry is on; set [telemetry] enabled = false in config.toml or " + EnabledEnv + "=0 to turn it off")
	}
	return reporter
}

// CaptureHandler serves UI events and persists daily screen claims in dataDir.
func CaptureHandler(reporter *posthog.Reporter, dataDir string) http.Handler {
	return &screenCaptureHandler{reporter: reporter, capture: posthog.NewCaptureHandler(reporter), dir: dataDir, now: time.Now}
}

func buildReporter(opts Options, endpoint string, logger *slog.Logger) (*posthog.Reporter, error) {
	allowed := []posthog.Option{
		posthog.WithAllowedEvent(posthog.EventDaemonActive),
		posthog.WithAllowedEvent(EventAppOpened, posthog.AllowProperty(propertySurface, posthog.AllowStringValues("web"))),
		posthog.WithAllowedEvent(EventScreenViewed,
			posthog.AllowProperty("screen", posthog.AllowStringValues(screenNames...)),
			posthog.AllowProperty(propertySurface, posthog.AllowStringValues("web", "tui"))),
		posthog.WithAllowedEvent(EventSessionEnded,
			posthog.AllowProperty(propertySurface, posthog.AllowStringValues("web", "tui")),
			posthog.AllowProperty("duration_bucket", posthog.AllowStringValues("under_1m", "1_to_5m", "5_to_30m", "over_30m"))),
	}
	if strings.TrimSpace(os.Getenv(EnabledEnv)) == "" && !opts.ConfigEnabled {
		// Only the daemon reports, so the process-wide switch is this reporter's switch.
		posthog.DisableProcess()
	}
	options := posthog.Options{
		APIKey: postHogAPIKey, Endpoint: endpoint, Application: application, EnvPrefix: envPrefix,
		Version: opts.Version, Commit: opts.Commit, Source: "daemon", Logger: logger,
	}
	if posthog.EnabledFromEnv(envPrefix) {
		install, err := posthog.LoadOrCreateInstall(opts.DataDir)
		if err != nil {
			return nil, fmt.Errorf("load telemetry install: %w", err)
		}
		options.DistinctID, options.InstalledAt = install.ID, install.InstalledAt
	}
	reporter, err := posthog.NewReporter(options, allowed...)
	if err != nil {
		return nil, fmt.Errorf("build telemetry reporter: %w", err)
	}
	return reporter, nil
}

// DurationBucket groups session runtime without reporting an exact duration.
// Keep thresholds and names in sync with web/src/lib/telemetry/session.ts.
func DurationBucket(duration time.Duration) string {
	switch {
	case duration < time.Minute:
		return "under_1m"
	case duration < 5*time.Minute:
		return "1_to_5m"
	case duration <= 30*time.Minute:
		return "5_to_30m"
	default:
		return "over_30m"
	}
}
