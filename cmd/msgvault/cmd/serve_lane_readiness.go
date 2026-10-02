package cmd

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
)

type laneReadinessFacts struct {
	enabled    bool
	configured bool
	credential string
	blockers   []string
}

// Facts are derived alongside the shared report, never from its diagnostic
// Reason or operator Next commands. Those fields stay at the CLI boundary.
func enrichLaneReadinessFacts(cfg *config.Config, lane *laneStatus) {
	textConfigured := cfg.Vector.Embeddings.Endpoint != "" && cfg.Vector.Embeddings.Model != "" && cfg.Vector.Embeddings.Dimension > 0
	switch lane.Lane {
	case laneTextSearch:
		lane.readiness.enabled, lane.readiness.configured = cfg.Vector.Enabled, textConfigured
	case lanePersonSearch:
		lane.readiness.enabled, lane.readiness.configured = cfg.Vector.People.Enabled, textConfigured
		if lane.readiness.enabled && !cfg.Vector.Enabled {
			lane.readiness.blockers = append(lane.readiness.blockers, "dependency_unready")
		}
	case laneVisualSearch:
		visual := cfg.Vector.Multimodal
		lane.readiness.enabled = visual.Enabled
		lane.readiness.configured = visual.Provider != "" && visual.Model != "" && visual.Dimension > 0 && visual.CapabilitiesFile != ""
	case laneDocuments:
		documents := cfg.Attachments.Documents
		lane.readiness.enabled = documents.Enabled
		lane.readiness.configured = documents.Provider != "" && documents.Model != ""
	case laneDocumentVectors:
		lane.readiness.enabled = cfg.Attachments.Documents.Index.Embeddings.Enabled
		lane.readiness.configured = textConfigured && cfg.Attachments.Documents.Enabled
		if lane.readiness.enabled && !cfg.Vector.Enabled {
			lane.readiness.blockers = append(lane.readiness.blockers, "dependency_unready")
		}
	case lanePeopleInference:
		lane.readiness.enabled = cfg.People.Sweep.Enabled
		_, _, err := cfg.People.Sweep.ActiveProviderConfig()
		lane.readiness.configured = err == nil
	case laneActivity:
		lane.readiness.enabled = cfg.Activity.Schedule != ""
		lane.readiness.configured = lane.readiness.enabled
	case laneMediaPolicy:
		lane.readiness.enabled, lane.readiness.configured = true, true
	}
	if lane.readiness.credential == "" {
		lane.readiness.credential = "not_required"
	}
}

// Capture the daemon's own environment once. No MCP argument can replace it.
func daemonSetupEnvironment(cfg *config.Config) setupEnvironment {
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[key] = value
		}
	}
	credentials, _ := providercredentials.Read(cfg.TokensDir())
	return setupEnvironment{lookupEnv: func(key string) (string, bool) { value, ok := environment[key]; return value, ok }, fileExists: defaultFileExists, credentials: credentials}
}

func newDaemonLaneReadinessReader(cfg *config.Config, st *store.Store) api.LaneReadinessReader {
	// API settings writes replace persisted configuration and only update live
	// Accounts; none of the lane configuration below is mutated in place.
	startup := *cfg
	env := daemonSetupEnvironment(cfg)
	return func(ctx context.Context, runtime api.LaneRuntimeSnapshot) (api.LaneReadinessResponse, error) {
		if err := ctx.Err(); err != nil {
			return api.LaneReadinessResponse{}, err
		}
		current, err := config.Load(startup.ConfigFilePath(), startup.HomeDir)
		if err != nil {
			return api.LaneReadinessResponse{}, err
		}
		currentEnv := env
		currentEnv.credentials, _ = providercredentials.Read(current.TokensDir())
		runtime.PendingRestart = runtime.PendingRestart || !sameReadinessConfiguration(&startup, current) || env.credentials.ETag != currentEnv.credentials.ETag
		storeAvailable := st != nil && st.DB().PingContext(ctx) == nil
		if storeAvailable {
			currentEnv.consent = setupConsentFromStore(ctx, current, st)
		}
		if err := ctx.Err(); err != nil {
			return api.LaneReadinessResponse{}, err
		}
		report := buildLaneReport(current, currentEnv)
		return projectLaneReadiness(report, runtime, storeAvailable), nil
	}
}

func sameReadinessConfiguration(startup, current *config.Config) bool {
	return startup.DatabaseDSN() == current.DatabaseDSN() && startup.TokensDir() == current.TokensDir() && reflect.DeepEqual(startup.Vector, current.Vector) && reflect.DeepEqual(startup.Attachments.Documents, current.Attachments.Documents) && reflect.DeepEqual(startup.People.Sweep, current.People.Sweep) && reflect.DeepEqual(startup.Activity, current.Activity) && reflect.DeepEqual(startup.Beeper, current.Beeper) && reflect.DeepEqual(startup.Slack, current.Slack) && reflect.DeepEqual(startup.Discord, current.Discord) && reflect.DeepEqual(startup.Teams, current.Teams)
}

func projectLaneReadiness(report laneReport, runtime api.LaneRuntimeSnapshot, storeAvailable bool) api.LaneReadinessResponse {
	response := api.LaneReadinessResponse{StoreAvailable: storeAvailable, PendingRestart: runtime.PendingRestart, Lanes: make([]api.LaneReadiness, 0, len(report.Lanes))}
	for _, row := range report.Lanes {
		facts := row.readiness
		lane := api.LaneReadiness{Lane: row.Lane, Enabled: facts.enabled, Configured: facts.configured, Initialized: runtime.Initialized[row.Lane], Provider: row.Provider, Model: row.Model, Schedule: row.Schedule, CredentialState: facts.credential, ConsentState: row.Consent, ConsentPurposes: row.ConsentPurposes, PendingRestart: runtime.PendingRestart, Blockers: slices.Clone(facts.blockers)}
		if lane.ConsentState == "" {
			lane.ConsentState = "not_required"
		}
		if !lane.Enabled {
			lane.Blockers = append(lane.Blockers, "disabled")
		}
		if lane.Enabled && !lane.Configured {
			lane.Blockers = append(lane.Blockers, "unconfigured")
		}
		if !storeAvailable && row.Lane != laneMediaPolicy {
			lane.Initialized = false
			lane.Blockers = append(lane.Blockers, "store_unavailable")
		}
		if lane.Enabled && !lane.Initialized {
			lane.Blockers = append(lane.Blockers, "uninitialized")
		}
		switch lane.CredentialState {
		case "missing":
			lane.Blockers = append(lane.Blockers, "credential_missing")
		case "unknown":
			lane.Blockers = append(lane.Blockers, "credential_unknown")
		}
		switch lane.ConsentState {
		case consentMissing:
			lane.Blockers = append(lane.Blockers, "consent_missing")
		case consentUnknown:
			lane.Blockers = append(lane.Blockers, "consent_unknown")
		case consentStale:
			lane.Blockers = append(lane.Blockers, "consent_stale")
		}
		if lane.Enabled && (row.Lane == laneTextSearch || row.Lane == lanePersonSearch || row.Lane == laneDocumentVectors) {
			switch runtime.VectorStatus {
			case api.VectorStatusError:
				lane.Blockers = append(lane.Blockers, "runtime_error")
			case api.VectorStatusStale:
				lane.Blockers = append(lane.Blockers, "index_stale")
			case "", api.VectorStatusDisabled, api.VectorStatusInitializing, api.VectorStatusReady:
				// Installation is reported independently above.
			}
		}
		if runtime.PendingRestart {
			lane.Blockers = append(lane.Blockers, "pending_restart")
		}
		if lane.Blockers == nil {
			lane.Blockers = []string{}
		}
		slices.Sort(lane.Blockers)
		lane.Blockers = slices.Compact(lane.Blockers)
		response.Lanes = append(response.Lanes, lane)
	}
	return response
}
