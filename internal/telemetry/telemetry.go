// Package telemetry sends anonymous, opt-out daemon and UI usage events.
package telemetry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/syncerr"
	"go.kenn.io/msgvault/internal/twilio"
	"golang.org/x/oauth2"
	"maunium.net/go/mautrix"
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
	// EventSearchRun counts a person searching the archive.
	EventSearchRun = "search_run"
	// EventMessageOpened counts a message becoming visible.
	EventMessageOpened = "message_opened"
	// EventSyncFinished counts a normally completed manual sync.
	EventSyncFinished = "sync_finished"
	EventSearchEmpty  = "search_empty"
	EventErrorShown   = "error_shown"
	EventSyncFailed   = "sync_failed"
	// propertySurface names the interface that reported an event.
	propertySurface = "surface"
	application     = "msgvault"
	envPrefix       = "MSGVAULT"
)

// Core actions require every listed property, using the same values as the allowlist.
var actionProperties = map[string]map[string][]string{
	EventSearchEmpty:   {propertySurface: {"web", "tui", "cli"}, "search_type": {"text", "semantic", "hybrid"}},
	EventErrorShown:    {propertySurface: {"web", "tui"}, "kind": {"search", "message", "sync", "other"}},
	EventSyncFailed:    {"sync_source": {"gmail", "imap", "msmail", "teams", "discord", "beeper", "matrix", "slack", "carddav", "chatwoot", "synctech_sms", "gcal", "omi", "granola", "plaud", "circleback", "notion_meetings", "twilio", "twenty", "muesli"}, "reason": {"auth", "network", "rate_limited", "other"}},
	EventSearchRun:     {propertySurface: {"web", "tui", "cli"}, "search_type": {"text", "semantic", "hybrid"}},
	EventMessageOpened: {propertySurface: {"web", "tui", "cli"}},
	EventSyncFinished:  {propertySurface: {"web", "cli"}},
}

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
	return newReporterOrDisabled(opts, "", "", logger)
}

// newReporterOrDisabled takes the API key and endpoint so the helper-process test can point the
// enabled path at a stub. Production passes "", so kit uses the key release builds set at link time.
func newReporterOrDisabled(opts Options, apiKey, endpoint string, logger *slog.Logger) *posthog.Reporter {
	reporter, err := buildReporter(opts, apiKey, endpoint, logger)
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return posthog.DisabledReporter()
	}
	if reporter.Enabled() {
		logger.Info("anonymous telemetry is on; set [telemetry] enabled = false in config.toml or " + EnabledEnv + "=0 to turn it off")
	}
	return reporter
}

// CaptureHandler serves usage events and persists daily screen and action claims in dataDir.
func CaptureHandler(reporter *posthog.Reporter, dataDir string) http.Handler {
	return &screenCaptureHandler{reporter: reporter, capture: posthog.NewCaptureHandler(reporter, posthog.RejectEvents(EventSyncFailed)), captureEvent: reporter.Capture, dir: dataDir, now: time.Now}
}

// Enabled applies the environment overrides to the configured telemetry choice.
func Enabled(configEnabled bool) bool {
	return posthog.EnabledFromEnv(envPrefix) && (strings.TrimSpace(os.Getenv(EnabledEnv)) != "" || configEnabled)
}

func buildReporter(opts Options, apiKey, endpoint string, logger *slog.Logger) (*posthog.Reporter, error) {
	claims := posthog.NewDailyClaims(filepath.Join(opts.DataDir, "telemetry-screen-claims.json"))
	allowed := []posthog.Option{
		posthog.WithAllowedEvent(posthog.EventDaemonActive),
		posthog.WithAllowedEvent(EventAppOpened, posthog.AllowProperty(propertySurface, posthog.AllowStringValues("web"))),
		posthog.WithAllowedEvent(EventScreenViewed,
			posthog.AllowProperty("screen", posthog.AllowStringValues(screenNames...)),
			posthog.RequireProperty(propertySurface, posthog.AllowStringValues("web", "tui"))),
		posthog.WithDailyEvent(EventScreenViewed, "screen", claims),
		posthog.WithDailyEvent(EventMessageOpened, propertySurface, claims),
		posthog.WithAllowedEvent(EventSessionEnded,
			posthog.AllowProperty(propertySurface, posthog.AllowStringValues("web", "tui")),
			posthog.AllowProperty("duration_bucket", posthog.AllowStringValues("under_1m", "1_to_5m", "5_to_30m", "over_30m"))),
	}
	for event, required := range actionProperties {
		properties := make([]posthog.AllowedProperty, 0, len(required))
		for key, values := range required {
			filter := posthog.AllowStringValues(values...)
			properties = append(properties, posthog.AllowProperty(key, func(value any) (any, bool) {
				filtered, ok := filter(value)
				return cmp.Or(filtered, any("other")), ok || key == "kind" || key == "reason" || key == "sync_source"
			}))
		}
		allowed = append(allowed, posthog.WithAllowedEvent(event, properties...))
	}
	if !Enabled(opts.ConfigEnabled) {
		// Only the daemon reports, so the process-wide switch is this reporter's switch.
		posthog.DisableProcess()
	}
	options := posthog.Options{
		APIKey: apiKey, Endpoint: endpoint, Application: application, EnvPrefix: envPrefix,
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

type SyncFailures struct {
	Capture func(string, map[string]any) error
	failing sync.Map
}

func (s *SyncFailures) Record(ctx context.Context, sourceType, identifier string, err error) {
	if s == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil || errors.Is(err, jobctx.ErrYieldedToWaiter) || errors.Is(err, jobctx.ErrRunBudgetExceeded) {
		return
	}
	key := [2]string{sourceType, identifier}
	if err == nil {
		s.failing.Delete(key)
	} else if _, loaded := s.failing.LoadOrStore(key, true); !loaded && s.Capture != nil {
		_ = s.Capture(EventSyncFailed, map[string]any{"sync_source": sourceType, "reason": reason(err)})
	}
}

func reason(err error) string {
	status := 0
	if e, ok := errors.AsType[*gmail.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*msgraph.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*twilio.APIError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*discord.APIError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*carddav.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*omi.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[mautrix.HTTPError](err); ok && e.Response != nil {
		status = e.Response.StatusCode
	}
	imapErr, _ := errors.AsType[*imap.Error](err)
	notionErr, _ := errors.AsType[*notionmeetings.APIError](err)
	_, gmailThrottled := errors.AsType[*gmail.ThrottledError](err)
	_, graphThrottled := errors.AsType[*msgraph.ThrottledError](err)
	retrieveError, _ := errors.AsType[*oauth2.RetrieveError](err)
	switch {
	case errors.Is(err, calcontrol.ErrDenied) || errors.Is(err, granola.ErrUnauthorized) || errors.Is(err, omi.ErrUnauthorized) || (retrieveError != nil && retrieveError.ErrorCode == "invalid_grant") || errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, gcal.ErrUnauthorized) || errors.Is(err, slack.ErrAuth) || errors.Is(err, notionmeetings.ErrUnauthorized) || errors.Is(err, oauth.ErrTokenUnrefreshable) || errors.Is(err, oauth.ErrInvalidTokenJSON) || errors.Is(err, beeper.ErrUnauthorized) || errors.Is(err, chatwoot.ErrUnauthorized) || imapErr != nil && imapErr.Code == imap.ResponseCodeAuthenticationFailed || status == 401:
		return "auth"
	case gmailThrottled || graphThrottled || errors.Is(err, gcal.ErrRateLimited) || notionErr != nil && notionErr.Status == 429 || status == 429:
		return "rate_limited"
	case status == 0 && retrieveError == nil && syncerr.IsTransientNetwork(err):
		return "network"
	}
	return "other"
}
