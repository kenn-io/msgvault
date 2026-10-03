// Package telemetry sends anonymous, opt-out daemon and web UI usage events.
package telemetry

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"go.kenn.io/kit/telemetry/posthog"
)

const (
	// EnabledEnv set to 0 turns telemetry off; any value overrides [telemetry] enabled.
	EnabledEnv = "MSGVAULT_TELEMETRY_ENABLED"
	// EventAppOpened is reported by the web UI through the daemon.
	EventAppOpened = "app_opened"
	application    = "msgvault"
	envPrefix      = "MSGVAULT"
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
	reporter, err := buildReporter(opts, endpoint)
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return posthog.DisabledReporter()
	}
	if reporter.Enabled() {
		logger.Info("anonymous telemetry is on; set [telemetry] enabled = false in config.toml or " + EnabledEnv + "=0 to turn it off")
	}
	return reporter
}

// CaptureHandler serves the web UI's event posts through reporter. A nil reporter admits no event.
func CaptureHandler(reporter *posthog.Reporter) http.Handler {
	return posthog.NewCaptureHandler(reporter)
}

func buildReporter(opts Options, endpoint string) (*posthog.Reporter, error) {
	allowed := []posthog.Option{
		posthog.WithAllowedEvent(posthog.EventDaemonActive),
		posthog.WithAllowedEvent(EventAppOpened),
	}
	if strings.TrimSpace(os.Getenv(EnabledEnv)) == "" && !opts.ConfigEnabled {
		// Only the daemon reports, so the process-wide switch is this reporter's switch.
		posthog.DisableProcess()
	}
	options := posthog.Options{
		APIKey: postHogAPIKey, Endpoint: endpoint, Application: application, EnvPrefix: envPrefix,
		Version: opts.Version, Commit: opts.Commit, Source: "daemon",
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
