package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
)

const (
	settingsPath                 = "/api/v1/settings"
	settingsGroupSources         = "sources"
	settingsGroupAttachments     = "attachments"
	settingsGroupEnrichment      = "enrichment"
	settingsCredentialETagHeader = "Credential-Etag" // #nosec G101 -- concurrency header, not a credential.
	settingsCredentialETagSchema = "Credential-ETag" // #nosec G101 -- schema header name, not a credential.
)

// SecretSettingState is the only representation of a secret returned to a
// browser. The configured value never crosses the API boundary.
type SecretSettingState struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty" enum:"stored,environment,none"`
}

// SettingValue is an explicit JSON union. Exactly one member is populated,
// keeping generated Go and TypeScript clients typed without exposing an
// unstructured config map.
type SettingValue struct {
	String  *string   `json:"string,omitempty"`
	Integer *int      `json:"integer,omitempty"`
	Number  *float64  `json:"number,omitempty"`
	Boolean *bool     `json:"boolean,omitempty"`
	Strings *[]string `json:"strings,omitempty"`
}

// SettingValidation lets generic Settings clients render the same basic
// input constraints enforced by the daemon. More complex cross-field rules
// remain authoritative at PATCH/PUT time.
type SettingValidation struct {
	Hint     string   `json:"hint,omitempty"`
	Required bool     `json:"required,omitempty"`
	Minimum  *float64 `json:"minimum,omitempty"`
	Maximum  *float64 `json:"maximum,omitempty"`
}

// Setting describes one browser-managed allowlisted config value. ReadOnly
// marks settings that are visible over HTTP but can only be changed by
// editing config.toml on the daemon host; PATCH rejects updates to them.
type Setting struct {
	Key             string              `json:"key"`
	Group           string              `json:"group"`
	Label           string              `json:"label"`
	Description     string              `json:"description"`
	Kind            string              `json:"kind"`
	Value           *SettingValue       `json:"value,omitempty"`
	Secret          *SecretSettingState `json:"secret,omitempty"`
	Options         []string            `json:"options,omitempty"`
	RestartRequired bool                `json:"restart_required"`
	Testable        bool                `json:"testable,omitempty"`
	ReadOnly        bool                `json:"read_only,omitempty"`
	Inherited       bool                `json:"inherited,omitempty"`
	CredentialID    string              `json:"credential_id,omitempty"`
	Validation      *SettingValidation  `json:"validation,omitempty"`
}

type SettingGroup struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type SettingsResponse struct {
	Groups                    []SettingGroup                    `json:"groups"`
	Settings                  []Setting                         `json:"settings"`
	PersonEnrichmentProviders []PersonEnrichmentProviderSetting `json:"person_enrichment_providers,omitempty"`
	CredentialETag            string                            `json:"credential_etag"`
	PendingRestart            bool                              `json:"pending_restart"`
}

type SecretSettingUpdate struct {
	Action string `json:"action" enum:"set,clear"`
	Value  string `json:"value,omitempty"`
}

type SettingUpdate struct {
	Key    string               `json:"key"`
	Value  *SettingValue        `json:"value,omitempty"`
	Secret *SecretSettingUpdate `json:"secret,omitempty"`
}

type SettingsPatchRequest struct {
	Updates []SettingUpdate `json:"updates" minItems:"1" nullable:"false"`
}

var (
	errInvalidSettingUpdate      = errors.New("invalid setting update")
	errSuppressionRollbackFailed = errors.New("generated suppression key could not be rolled back")
)

type settingDefinition struct {
	key             string
	group           string
	kind            string
	options         []string
	restartRequired bool
	// localOnly settings are visible over HTTP but can only be changed by
	// editing config.toml on the daemon host. Used for values that select
	// daemon-side resources (such as environment variable names) which a
	// remote session must never control.
	localOnly             bool
	secret                func(*config.Config) bool
	serverSecret          func(context.Context, *Server, *config.Config) bool
	credentialID          string
	credentialEndpoint    func(*config.Config) string
	credentialEnvironment func(*config.Config) string
	inherited             func(*config.Config) bool
	read                  func(*config.Config) any
}

var settingsCatalog = []settingDefinition{
	liveStringSetting("web.default_search_mode", "browser", []string{exploreSearchModeFullText, exploreSearchModeSemantic, exploreSearchModeHybrid}, func(c *config.Config) string { return c.Web.DefaultSearchMode }),
	liveStringSetting("web.theme", "browser", []string{"system", "light", "dark"}, func(c *config.Config) string { return c.Web.Theme }),
	liveStringSetting("web.density", "browser", []string{"compact", "comfortable"}, func(c *config.Config) string { return c.Web.Density }),
	readOnlyStringSetting("server.bind_addr", "server", func(c *config.Config) string { return c.Server.BindAddr }),
	readOnlyIntSetting("server.api_port", "server", func(c *config.Config) int { return c.Server.APIPort }),
	readOnlySecretSetting("server.api_key", "server", func(c *config.Config) bool { return c.Server.APIKey != "" }),
	readOnlyBoolSetting("server.allow_insecure", "server", func(c *config.Config) bool { return c.Server.AllowInsecure }),
	readOnlyStringArraySetting("server.trusted_proxies", "server", func(c *config.Config) []string { return c.Server.TrustedProxies }),
	stringSetting("server.daemon_idle_timeout", "server", nil, func(c *config.Config) string { return c.Server.DaemonIdleTimeout.String() }),
	stringSetting("server.daemon_auto_restart", "server", []string{config.DaemonAutoRestartNewer, config.DaemonAutoRestartNever, config.DaemonAutoRestartAlways}, func(c *config.Config) string { return c.Server.DaemonAutoRestart }),
	stringSetting("analytics.engine", "archive", []string{"auto", "sql", "duckdb"}, func(c *config.Config) string { return c.Analytics.Engine }),
	boolSetting("analytics.auto_build_cache", "archive", func(c *config.Config) bool { return c.Analytics.AutoBuildCache }),
	stringSetting("analytics.min_rebuild_interval", "archive", nil, func(c *config.Config) string { return c.Analytics.MinRebuildInterval.String() }),
	stringSetting("analytics.builder_memory_limit", "archive", nil, func(c *config.Config) string { return c.Analytics.BuilderMemoryLimit }),
	intSetting("analytics.builder_threads", "archive", func(c *config.Config) int { return c.Analytics.BuilderThreads }),
	stringSetting("analytics.builder_temp_limit", "archive", nil, func(c *config.Config) string { return c.Analytics.BuilderTempLimit }),
	intSetting("sync.rate_limit_qps", "sync", func(c *config.Config) int { return c.Sync.RateLimitQPS }),
	boolSetting("log.enabled", "logging", func(c *config.Config) bool { return c.Log.Enabled }),
	stringSetting("log.level", "logging", []string{"", "debug", "info", "warn", "error"}, func(c *config.Config) string { return c.Log.Level }),
	int64Setting("log.sql_slow_ms", "logging", func(c *config.Config) int64 { return c.Log.SQLSlowMs }),
	boolSetting("log.sql_trace", "logging", func(c *config.Config) bool { return c.Log.SQLTrace }),
	boolSetting("vector.enabled", "search", func(c *config.Config) bool { return c.Vector.Enabled }),
	readOnlyStringSettingWithOptions("vector.backend", "search", []string{"sqlite-vec", "pgvector"}, func(c *config.Config) string { return c.Vector.Backend }),
	readOnlyStringSetting("vector.db_path", "search", func(c *config.Config) string { return c.Vector.DBPath }),
	readOnlyBoolSetting("vector.skip_extension_create", "search", func(c *config.Config) bool { return c.Vector.SkipExtensionCreate }),
	stringSetting("vector.embeddings.api_format", "search", []string{"openai", "voyage-contextual"}, func(c *config.Config) string {
		return string(c.Vector.Embeddings.EffectiveAPIFormat())
	}),
	stringSetting("vector.embeddings.endpoint", "search", nil, func(c *config.Config) string { return c.Vector.Embeddings.Endpoint }),
	localOnlyStringSetting("vector.embeddings.api_key_env", "search", func(c *config.Config) string { return c.Vector.Embeddings.APIKeyEnv }),
	providerCredentialSetting("vector.embeddings.api_key", "search", providercredentials.VectorEmbeddingsID,
		func(c *config.Config) string { return c.Vector.Embeddings.Endpoint },
		func(c *config.Config) string { return c.Vector.Embeddings.APIKeyEnv }),
	stringSetting("vector.embeddings.model", "search", nil, func(c *config.Config) string { return c.Vector.Embeddings.Model }),
	stringSetting("vector.embeddings.document_prefix", "search", nil, func(c *config.Config) string { return c.Vector.Embeddings.DocumentPrefix }),
	stringSetting("vector.embeddings.query_prefix", "search", nil, func(c *config.Config) string { return c.Vector.Embeddings.QueryPrefix }),
	intSetting("vector.embeddings.dimension", "search", func(c *config.Config) int { return c.Vector.Embeddings.Dimension }),
	intSetting("vector.embeddings.batch_size", "search", func(c *config.Config) int { return c.Vector.Embeddings.BatchSize }),
	stringSetting("vector.embeddings.timeout", "search", nil, func(c *config.Config) string { return c.Vector.Embeddings.Timeout.String() }),
	intSetting("vector.embeddings.max_retries", "search", func(c *config.Config) int { return c.Vector.Embeddings.MaxRetries }),
	intSetting("vector.embeddings.max_input_chars", "search", func(c *config.Config) int { return c.Vector.Embeddings.MaxInputChars }),
	intSetting("vector.embeddings.eta_window", "search", func(c *config.Config) int { return c.Vector.Embeddings.ETAWindow }),
	boolSetting("vector.people.enabled", "search", func(c *config.Config) bool { return c.Vector.People.Enabled }),
	stringSetting("vector.people.retention_posture", "search", nil, func(c *config.Config) string {
		return c.Vector.People.RetentionPosture
	}),
	stringSetting("vector.people.training_posture", "search", nil, func(c *config.Config) string {
		return c.Vector.People.TrainingPosture
	}),
	stringSetting("vector.embed.schedule.cron", "search", nil, func(c *config.Config) string { return c.Vector.Embed.Schedule.Cron }),
	boolSetting("vector.embed.schedule.run_after_sync", "search", func(c *config.Config) bool { return c.Vector.Embed.Schedule.RunAfterSync }),
	stringArraySetting("vector.embed.scope.message_types", "search", func(c *config.Config) []string { return c.Vector.Embed.Scope.MessageTypes }),
	stringArraySetting("vector.embed.scope.accounts", "search", func(c *config.Config) []string { return c.Vector.Embed.Scope.Accounts }),
	stringSetting("vector.embed.backstop_interval", "search", nil, func(c *config.Config) string { return c.Vector.Embed.BackstopInterval.String() }),
	boolSetting("vector.multimodal.enabled", "search", func(c *config.Config) bool { return c.Vector.Multimodal.Enabled }),
	stringSetting("vector.multimodal.provider", "search", []string{"voyage"}, func(c *config.Config) string { return c.Vector.Multimodal.Provider }),
	stringSetting("vector.multimodal.endpoint", "search", nil, func(c *config.Config) string { return c.Vector.Multimodal.Endpoint }),
	localOnlyStringSetting("vector.multimodal.api_key_env", "search", func(c *config.Config) string { return c.Vector.Multimodal.APIKeyEnv }),
	providerCredentialSetting("vector.multimodal.api_key", "search", providercredentials.VectorMultimodalID,
		func(c *config.Config) string { return c.Vector.Multimodal.Endpoint },
		func(c *config.Config) string { return c.Vector.Multimodal.APIKeyEnv }),
	localOnlyStringSetting("vector.multimodal.capabilities_file", "search", func(c *config.Config) string { return c.Vector.Multimodal.CapabilitiesFile }),
	stringSetting("vector.multimodal.model", "search", []string{"voyage-multimodal-3.5"}, func(c *config.Config) string { return c.Vector.Multimodal.Model }),
	intSetting("vector.multimodal.dimension", "search", func(c *config.Config) int { return c.Vector.Multimodal.Dimension }),
	intSetting("vector.multimodal.max_context_chars", "search", func(c *config.Config) int { return c.Vector.Multimodal.MaxContextChars }),
	// Configured (default-resolved) values, NOT the runtime-gated helpers:
	// those return false whenever the lane is disabled, which would show
	// default-enabled consent options as off and then silently activate
	// them when only the parent switch is turned on.
	boolSetting("vector.multimodal.include_images", "search", func(c *config.Config) bool { return c.Vector.Multimodal.ConfiguredImages() }),
	boolSetting("vector.multimodal.include_animated_gifs", "search", func(c *config.Config) bool { return c.Vector.Multimodal.ConfiguredAnimatedGIFs() }),
	boolSetting("vector.multimodal.include_video", "search", func(c *config.Config) bool { return c.Vector.Multimodal.ConfiguredVideo() }),
	boolSetting("vector.multimodal.allow_image_queries", "search", func(c *config.Config) bool { return c.Vector.Multimodal.ConfiguredImageQueries() }),
	stringArraySetting("vector.multimodal.scope.message_types", "search", func(c *config.Config) []string { return c.Vector.Multimodal.Scope.MessageTypes }),
	stringSetting("vector.multimodal.schedule.cron", "search", nil, func(c *config.Config) string { return c.Vector.Multimodal.Schedule.Cron }),
	boolSetting("vector.multimodal.schedule.run_after_sync", "search", func(c *config.Config) bool { return c.Vector.Multimodal.Schedule.RunAfterSync }),
	intSetting("vector.search.rrf_k", "search", func(c *config.Config) int { return c.Vector.Search.RRFK }),
	intSetting("vector.search.k_per_signal", "search", func(c *config.Config) int { return c.Vector.Search.KPerSignal }),
	numberSetting("vector.search.subject_boost", "search", func(c *config.Config) float64 { return c.Vector.Search.SubjectBoost }),
	intSetting("vector.search.max_page_size_hybrid", "search", func(c *config.Config) int { return c.Vector.Search.MaxPageSizeHybridClamp() }),
	configuredBoolSetting("vector.preprocess.strip_quotes", "search", func(c *config.Config) bool { return c.Vector.Preprocess.StripQuotesEnabled() }, func(c *config.Config) bool { return c.Vector.Preprocess.StripQuotes == nil }),
	configuredBoolSetting("vector.preprocess.strip_signatures", "search", func(c *config.Config) bool { return c.Vector.Preprocess.StripSignaturesEnabled() }, func(c *config.Config) bool { return c.Vector.Preprocess.StripSignatures == nil }),
	configuredBoolSetting("vector.preprocess.strip_html", "search", func(c *config.Config) bool { return c.Vector.Preprocess.StripHTMLEnabled() }, func(c *config.Config) bool { return c.Vector.Preprocess.StripHTML == nil }),
	configuredBoolSetting("vector.preprocess.strip_base64", "search", func(c *config.Config) bool { return c.Vector.Preprocess.StripBase64Enabled() }, func(c *config.Config) bool { return c.Vector.Preprocess.StripBase64 == nil }),
	configuredBoolSetting("vector.preprocess.strip_url_tracking", "search", func(c *config.Config) bool { return c.Vector.Preprocess.StripURLTrackingEnabled() }, func(c *config.Config) bool { return c.Vector.Preprocess.StripURLTracking == nil }),
	configuredBoolSetting("vector.preprocess.collapse_whitespace", "search", func(c *config.Config) bool { return c.Vector.Preprocess.CollapseWhitespaceEnabled() }, func(c *config.Config) bool { return c.Vector.Preprocess.CollapseWhitespace == nil }),
	boolSetting("beeper.enabled", settingsGroupSources, func(c *config.Config) bool { return c.Beeper.Enabled }),
	stringSetting("beeper.schedule", settingsGroupSources, nil, func(c *config.Config) string { return c.Beeper.Schedule }),
	stringArraySetting("beeper.accounts", settingsGroupSources, func(c *config.Config) []string { return c.Beeper.Accounts }),
	stringArraySetting("beeper.exclude_accounts", settingsGroupSources, func(c *config.Config) []string { return c.Beeper.ExcludeAccounts }),
	numberSetting("beeper.rate_limit_qps", settingsGroupSources, func(c *config.Config) float64 { return c.Beeper.RateLimitQPS }),
	boolSetting("slack.enabled", settingsGroupSources, func(c *config.Config) bool { return c.Slack.Enabled }),
	stringSetting("slack.schedule", settingsGroupSources, nil, func(c *config.Config) string { return c.Slack.Schedule }),
	stringArraySetting("slack.channels", settingsGroupSources, func(c *config.Config) []string { return c.Slack.Channels }),
	stringArraySetting("slack.exclude_channels", settingsGroupSources, func(c *config.Config) []string { return c.Slack.ExcludeChannels }),
	configuredBoolSetting("beeper.media", settingsGroupAttachments, func(c *config.Config) bool { return c.Beeper.MediaEnabled() }, func(c *config.Config) bool { return c.Beeper.Media == nil }),
	stringSetting("beeper.media_scope", settingsGroupAttachments, []string{"all", "direct", "none"}, func(c *config.Config) string { return effectiveMediaScope(c.Beeper.MediaScope) }),
	intSetting("beeper.media_max_participants", settingsGroupAttachments, func(c *config.Config) int { return c.Beeper.MediaMaxParticipants }),
	intSetting("beeper.max_media_mb", settingsGroupAttachments, func(c *config.Config) int { return c.Beeper.MaxMediaMB }),
	configuredBoolSetting("slack.media", settingsGroupAttachments, func(c *config.Config) bool { return c.Slack.MediaEnabled() }, func(c *config.Config) bool { return c.Slack.Media == nil }),
	stringSetting("slack.media_scope", settingsGroupAttachments, []string{"all", "direct", "none"}, func(c *config.Config) string { return effectiveMediaScope(c.Slack.MediaScope) }),
	intSetting("slack.media_max_participants", settingsGroupAttachments, func(c *config.Config) int { return c.Slack.MediaMaxParticipants }),
	intSetting("slack.max_media_mb", settingsGroupAttachments, func(c *config.Config) int { return c.Slack.MaxMediaMB }),
	configuredBoolSetting("discord.media", settingsGroupAttachments, func(c *config.Config) bool { return c.Discord.Media == nil || *c.Discord.Media }, func(c *config.Config) bool { return c.Discord.Media == nil }),
	stringSetting("discord.media_scope", settingsGroupAttachments, []string{"all", "direct", "none"}, func(c *config.Config) string { return effectiveMediaScope(c.Discord.MediaScope) }),
	intSetting("discord.media_max_participants", settingsGroupAttachments, func(c *config.Config) int { return c.Discord.MediaMaxParticipants }),
	intSetting("discord.max_media_mb", settingsGroupAttachments, func(c *config.Config) int { return c.Discord.MaxMediaMB }),
	configuredBoolSetting("teams.media", settingsGroupAttachments, func(c *config.Config) bool { return c.Teams.Media == nil || *c.Teams.Media }, func(c *config.Config) bool { return c.Teams.Media == nil }),
	stringSetting("teams.media_scope", settingsGroupAttachments, []string{"all", "direct", "none"}, func(c *config.Config) string { return effectiveMediaScope(c.Teams.MediaScope) }),
	intSetting("teams.media_max_participants", settingsGroupAttachments, func(c *config.Config) int { return c.Teams.MediaMaxParticipants }),
	intSetting("teams.max_media_mb", settingsGroupAttachments, func(c *config.Config) int { return c.Teams.MaxMediaMB }),
	stringSetting("activity.timezone", "activity", nil, func(c *config.Config) string { return c.Activity.Timezone }),
	intSetting("activity.max_direct_counterparts", "activity", func(c *config.Config) int { return c.Activity.MaxDirectCounterparts }),
	intSetting("activity.batch_size", "activity", func(c *config.Config) int { return c.Activity.BatchSize }),
	stringSetting("activity.schedule", "activity", nil, func(c *config.Config) string { return c.Activity.Schedule }),
	intSetting("backup.zstd_level", "backup", func(c *config.Config) int { return c.Backup.ZstdLevel }),
	boolSetting("people.enrichment.enabled", settingsGroupEnrichment, func(c *config.Config) bool { return c.People.Enrichment.Enabled }),
	stringSetting("people.enrichment.schedule", settingsGroupEnrichment, nil, func(c *config.Config) string { return c.People.Enrichment.Schedule }),
	intSetting("people.enrichment.batch_size", settingsGroupEnrichment, func(c *config.Config) int { return c.People.Enrichment.BatchSize }),
	stringSetting("people.enrichment.lease_duration", settingsGroupEnrichment, nil, func(c *config.Config) string { return c.People.Enrichment.LeaseDuration.String() }),
	readOnlyStringSetting("carddav.base_url", settingsGroupSources, func(c *config.Config) string { return c.CardDAV.BaseURL }),
	readOnlyStringSetting("carddav.username", settingsGroupSources, func(c *config.Config) string { return c.CardDAV.Username }),
	readOnlyStringSetting("carddav.schedule", settingsGroupSources, func(c *config.Config) string { return c.CardDAV.Schedule }),
	readOnlyBoolSetting("carddav.enabled", settingsGroupSources, func(c *config.Config) bool { return c.CardDAV.Enabled }),
	readOnlyCardDAVSecretSetting(),
	boolSetting("integrations.tasks.enabled", "integrations", func(c *config.Config) bool { return c.Integrations.Tasks.Enabled }),
	stringSetting("integrations.tasks.endpoint", "integrations", nil, func(c *config.Config) string { return c.Integrations.Tasks.Endpoint }),
	secretSetting("integrations.tasks.api_key", "integrations", func(c *config.Config) bool { return c.Integrations.Tasks.APIKey != "" }),
	stringSetting("integrations.tasks.default_project", "integrations", nil, func(c *config.Config) string { return c.Integrations.Tasks.DefaultProject }),
}

func (s *Server) registerSettingsRoutes(api huma.API) {
	get := rawAPIV1Operation("getSettings", http.MethodGet, "/settings", "Get browser-managed settings")
	get.Responses = jsonResponsesFor[SettingsResponse](api)
	addSettingsETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addSettingsCredentialETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, get, s.handleGetSettings)

	patch := rawAPIV1Operation("patchSettings", http.MethodPatch, "/settings", "Update browser-managed settings")
	patch.Parameters = append(patch.Parameters, &huma.Param{
		Name:        ifMatchHeaderName,
		In:          headerParamLocation,
		Description: "Strong ETag returned by the latest settings read",
		Required:    true,
		Schema:      &huma.Schema{Type: huma.TypeString},
	})
	patch.RequestBody = jsonRequestBodyFor[SettingsPatchRequest](api)
	patch.Responses = jsonResponsesFor[SettingsResponse](api)
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusConflict,
		http.StatusPreconditionFailed,
		http.StatusPreconditionRequired,
		http.StatusUnprocessableEntity,
	} {
		patch.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addSettingsCredentialETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, patch, s.handlePatchSettings)
	s.registerProviderCredentialSettingsRoutes(api)
	s.registerPersonEnrichmentSettingsRoute(api)
}

func addSettingsETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{
		etagHeaderName: {
			Description: "Strong content hash for optimistic concurrency",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
}

func addSettingsCredentialETagHeader(response *huma.Response) {
	if response.Headers == nil {
		response.Headers = map[string]*huma.Param{}
	}
	response.Headers[settingsCredentialETagSchema] = &huma.Param{
		Description: "Strong content hash for the independent provider credential store",
		Schema:      &huma.Schema{Type: huma.TypeString},
	}
}

func stringSetting(key, group string, options []string, read func(*config.Config) string) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "string", options: options, restartRequired: true, read: func(c *config.Config) any { return read(c) }}
}

func liveStringSetting(key, group string, options []string, read func(*config.Config) string) settingDefinition {
	definition := stringSetting(key, group, options, read)
	definition.restartRequired = false
	return definition
}

func localOnlyStringSetting(key, group string, read func(*config.Config) string) settingDefinition {
	definition := stringSetting(key, group, nil, read)
	definition.localOnly = true
	return definition
}

func readOnlyStringSetting(key, group string, read func(*config.Config) string) settingDefinition {
	return localOnlyStringSetting(key, group, read)
}

func readOnlyStringSettingWithOptions(key, group string, options []string, read func(*config.Config) string) settingDefinition {
	definition := stringSetting(key, group, options, read)
	definition.localOnly = true
	return definition
}

func readOnlyBoolSetting(key, group string, read func(*config.Config) bool) settingDefinition {
	definition := boolSetting(key, group, read)
	definition.localOnly = true
	return definition
}

func readOnlyIntSetting(key, group string, read func(*config.Config) int) settingDefinition {
	definition := intSetting(key, group, read)
	definition.localOnly = true
	return definition
}

func readOnlyStringArraySetting(key, group string, read func(*config.Config) []string) settingDefinition {
	definition := stringArraySetting(key, group, read)
	definition.localOnly = true
	return definition
}

func readOnlySecretSetting(key, group string, configured func(*config.Config) bool) settingDefinition {
	definition := secretSetting(key, group, configured)
	definition.localOnly = true
	return definition
}

func readOnlyCardDAVSecretSetting() settingDefinition {
	return settingDefinition{
		key: "carddav.password", group: settingsGroupSources, kind: "secret", localOnly: true,
		serverSecret: func(ctx context.Context, s *Server, c *config.Config) bool {
			return s != nil && s.cardDAV != nil &&
				s.cardDAV.passwordConfigured(ctx, c.CardDAV.BaseURL, c.CardDAV.Username)
		},
	}
}

func intSetting(key, group string, read func(*config.Config) int) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "integer", restartRequired: true, read: func(c *config.Config) any { return read(c) }}
}

func int64Setting(key, group string, read func(*config.Config) int64) settingDefinition {
	return intSetting(key, group, func(c *config.Config) int { return int(read(c)) })
}

func numberSetting(key, group string, read func(*config.Config) float64) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "number", restartRequired: true, read: func(c *config.Config) any { return read(c) }}
}

func boolSetting(key, group string, read func(*config.Config) bool) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "boolean", restartRequired: true, read: func(c *config.Config) any { return read(c) }}
}

func configuredBoolSetting(
	key, group string,
	read func(*config.Config) bool,
	inherited func(*config.Config) bool,
) settingDefinition {
	definition := boolSetting(key, group, read)
	definition.inherited = inherited
	return definition
}

func stringArraySetting(key, group string, read func(*config.Config) []string) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "string_array", restartRequired: true, read: func(c *config.Config) any { return read(c) }}
}

func secretSetting(key, group string, configured func(*config.Config) bool) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "secret", restartRequired: true, secret: configured}
}

func providerCredentialSetting(
	key, group, credentialID string,
	endpoint, environment func(*config.Config) string,
) settingDefinition {
	return settingDefinition{
		key: key, group: group, kind: "secret", restartRequired: true,
		credentialID: credentialID, credentialEndpoint: endpoint, credentialEnvironment: environment,
	}
}

func effectiveMediaScope(value string) string {
	if strings.TrimSpace(value) == "" {
		return "all"
	}
	return value
}

func settingsDefinitionByKey() map[string]settingDefinition {
	result := make(map[string]settingDefinition, len(settingsCatalog))
	for _, definition := range settingsCatalog {
		result[definition.key] = definition
	}
	return result
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	snapshot, cfg, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	if err := validateSafePublicSettingsEndpoints(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	credentials, err := providercredentials.Read(cfg.TokensDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	response, err := s.buildSettingsResponse(r.Context(), cfg, credentials, s.settingsPendingRestart.Load())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set(settingsCredentialETagHeader, credentials.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	ifMatches := r.Header.Values(ifMatchHeaderName)
	if len(ifMatches) != 1 || strings.TrimSpace(ifMatches[0]) == "" {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return
	}
	var request SettingsPatchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid settings request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid settings request")
		return
	}
	if len(request.Updates) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "At least one settings update is required")
		return
	}
	_, current, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	credentials, err := providercredentials.Read(current.TokensDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	edits, restartRequired, err := settingsEdits(current, request.Updates)
	if err != nil {
		if errors.Is(err, errInvalidSettingUpdate) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", "One or more settings are invalid")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	suppressionEdits, updatedCredentials, generatedSuppression, err :=
		prepareFirstEnrichmentEnable(current, request.Updates, credentials)
	if err != nil {
		writeFirstEnrichmentEnableError(w, err)
		return
	}
	credentials = updatedCredentials
	edits = append(edits, suppressionEdits...)

	snapshot, err := s.commitSettingsEdits(ifMatches[0], edits, generatedSuppression, current.TokensDir())
	if err != nil {
		if errors.Is(err, config.ErrConfigChanged) && restartRequired {
			s.settingsPendingRestart.Store(true)
		}
		s.writeSettingsConfigError(w, err)
		return
	}
	// A nil editor error means the candidate is already the committed config.
	// Record that fact before decoding the response snapshot so a subsequent
	// load failure cannot make the daemon report a false non-pending state.
	if restartRequired {
		s.settingsPendingRestart.Store(true)
	}
	loaded, err := config.LoadConfigFile(snapshot, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	credentials, err = deleteStoredCredentials(current.TokensDir(), credentials,
		staleStoredCredentialIDs(loaded, credentials))
	if err != nil {
		writeProviderCredentialError(w, err)
		return
	}
	response, err := s.buildSettingsResponse(r.Context(), loaded, credentials, s.settingsPendingRestart.Load())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set(settingsCredentialETagHeader, credentials.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func writeFirstEnrichmentEnableError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSuppressionUnavailable):
		writeError(w, http.StatusUnprocessableEntity, "suppression_key_unavailable",
			"Person enrichment requires a valid host suppression key")
	case errors.Is(err, providercredentials.ErrConflict):
		writeError(w, http.StatusPreconditionFailed, "credential_conflict",
			"Provider credentials changed; reload settings and retry")
	default:
		writeError(w, http.StatusInternalServerError, "credential_store_unavailable",
			"Provider credential store is unavailable")
	}
}

// commitSettingsEdits writes the candidate config file. When this PATCH
// generated a suppression key for the first enrichment enable and the write
// provably did not reach the committed file, the generated key is removed
// again so the credential store does not keep a secret config never adopted.
func (s *Server) commitSettingsEdits(
	ifMatch string, edits []config.Edit, generatedSuppression, tokensDir string,
) (config.ConfigFile, error) {
	editor := s.settingsConfigEditor
	if editor == nil {
		editor = config.EditConfigFile
	}
	snapshot, err := editor(s.cfg.ConfigFilePath(), ifMatch, edits)
	if err == nil {
		return snapshot, nil
	}
	if generatedSuppression != "" && !errors.Is(err, config.ErrConfigChanged) {
		if _, rollbackErr := providercredentials.DeleteSuppressionIfValue(
			tokensDir, generatedSuppression,
		); rollbackErr != nil {
			return config.ConfigFile{}, fmt.Errorf("%w: %w", errSuppressionRollbackFailed, rollbackErr)
		}
	}
	return config.ConfigFile{}, err
}

func (s *Server) writeSettingsConfigError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSuppressionRollbackFailed):
		writeError(w, http.StatusInternalServerError, "credential_store_unavailable",
			"Provider credential store is unavailable")
	case errors.Is(err, config.ErrConfigChanged):
		writeError(w, http.StatusInternalServerError, "settings_write_failed",
			"Settings changed, but the write did not complete cleanly")
	case errors.Is(err, config.ErrConfigConflict):
		writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
	case errors.Is(err, config.ErrAmbiguousConfigTarget), errors.Is(err, config.ErrUnsafeConfigTarget):
		writeError(w, http.StatusConflict, "settings_edit_rejected", "Settings could not be edited safely")
	case errors.Is(err, config.ErrInvalidConfigCandidate):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "One or more settings are invalid")
	default:
		writeError(w, http.StatusInternalServerError, "settings_write_failed", "Could not write settings")
	}
}

func (s *Server) readPersistedSettings() (config.ConfigFile, *config.Config, error) {
	snapshot, err := config.ReadConfigFile(s.cfg.ConfigFilePath())
	if err != nil {
		return config.ConfigFile{}, nil, err
	}
	if !snapshot.Exists {
		return snapshot, config.NewDefaultConfig(), nil
	}
	loaded, err := config.LoadConfigFile(snapshot, "")
	if err != nil {
		return config.ConfigFile{}, nil, err
	}
	return snapshot, loaded, nil
}

func (s *Server) buildSettingsResponse(
	ctx context.Context,
	cfg *config.Config,
	credentials providercredentials.Snapshot,
	pendingRestart bool,
) (SettingsResponse, error) {
	settings := make([]Setting, 0, len(settingsCatalog))
	for _, definition := range settingsCatalog {
		metadata := metadataForSetting(definition.key)
		setting := Setting{
			Key:             definition.key,
			Group:           definition.group,
			Label:           metadata.label,
			Description:     metadata.description,
			Kind:            definition.kind,
			Options:         definition.options,
			RestartRequired: definition.restartRequired,
			ReadOnly:        definition.localOnly,
			CredentialID:    definition.credentialID,
			Validation:      validationForSetting(definition.key),
		}
		if definition.inherited != nil {
			setting.Inherited = definition.inherited(cfg)
		}
		if definition.credentialID != "" {
			_, state, err := credentials.Resolve(definition.credentialID,
				definition.credentialEndpoint(cfg), definition.credentialEnvironment(cfg), osLookupEnv)
			if errors.Is(err, providercredentials.ErrOriginMismatch) {
				state = providercredentials.State{Configured: false, Source: providercredentials.SourceNone}
			} else if err != nil {
				return SettingsResponse{}, err
			}
			setting.Secret = &SecretSettingState{Configured: state.Configured, Source: string(state.Source)}
		} else if definition.serverSecret != nil {
			setting.Secret = &SecretSettingState{Configured: definition.serverSecret(ctx, s, cfg)}
		} else if definition.secret != nil {
			setting.Secret = &SecretSettingState{Configured: definition.secret(cfg)}
		} else {
			setting.Value = settingValue(definition.kind, definition.read(cfg))
		}
		settings = append(settings, setting)
	}
	providers, err := personEnrichmentProviderSettings(cfg, credentials)
	if err != nil {
		return SettingsResponse{}, err
	}
	return SettingsResponse{
		Groups: append([]SettingGroup(nil), settingsGroups...), Settings: settings,
		PersonEnrichmentProviders: providers, CredentialETag: credentials.ETag,
		PendingRestart: pendingRestart,
	}, nil
}

// credentialBinding ties an endpoint setting to the credential that gets sent
// to it. When the endpoint's origin changes, the stored credential must not
// silently follow: it is cleared unless the same PATCH explicitly provides a
// replacement, so a retained secret can never be replayed to a new
// destination after restart.
type credentialBinding struct {
	endpointKey     string
	credentialKey   string
	currentEndpoint func(*config.Config) string
	credentialSet   func(*config.Config) bool
}

var credentialBindings = []credentialBinding{
	{
		endpointKey:     "integrations.tasks.endpoint",
		credentialKey:   "integrations.tasks.api_key",
		currentEndpoint: func(c *config.Config) string { return c.Integrations.Tasks.Endpoint },
		credentialSet:   func(c *config.Config) bool { return c.Integrations.Tasks.APIKey != "" },
	},
}

// storedCredentialBinding ties an endpoint setting to the write-only provider
// credential bound to its origin in the credential store. When a PATCH moves
// the endpoint to a different origin, the stored credential is deleted in the
// same request so it can never be replayed to the new destination.
type storedCredentialBinding struct {
	credentialID    string
	currentEndpoint func(*config.Config) string
}

var storedCredentialBindings = []storedCredentialBinding{
	{
		credentialID:    providercredentials.VectorEmbeddingsID,
		currentEndpoint: func(c *config.Config) string { return c.Vector.Embeddings.Endpoint },
	},
	{
		credentialID:    providercredentials.VectorMultimodalID,
		currentEndpoint: func(c *config.Config) string { return c.Vector.Multimodal.Endpoint },
	},
}

// staleStoredCredentialIDs lists the stored provider credentials that are
// bound to an origin other than the committed endpoint. Deciding from the
// committed state rather than from this request's edits makes severing
// idempotent: a credential left behind by an interrupted earlier cleanup, or
// by a host edit of config.toml, is removed by the next settings write.
func staleStoredCredentialIDs(committed *config.Config, credentials providercredentials.Snapshot) []string {
	var ids []string
	for _, binding := range storedCredentialBindings {
		if storedCredentialOriginMismatch(credentials, binding.credentialID, binding.currentEndpoint(committed)) {
			ids = append(ids, binding.credentialID)
		}
	}
	for _, id := range credentials.StoredPersonEnrichmentIDs() {
		binding, ok := providerCredentialBindingForID(committed, id)
		if !ok || storedCredentialOriginMismatch(credentials, id, binding.endpoint) {
			ids = append(ids, id)
		}
	}
	return ids
}

// storedCredentialOriginMismatch reports whether a stored credential exists
// for id but is bound to an origin other than endpoint's.
func storedCredentialOriginMismatch(credentials providercredentials.Snapshot, id, endpoint string) bool {
	if !credentials.Stored(id) {
		return false
	}
	_, _, err := credentials.Resolve(id, endpoint, "", nil)
	return errors.Is(err, providercredentials.ErrOriginMismatch)
}

// deleteStoredCredentials removes each stored credential from the store,
// threading the store ETag through successive writes.
func deleteStoredCredentials(
	tokensDir string, credentials providercredentials.Snapshot, ids []string,
) (providercredentials.Snapshot, error) {
	for _, id := range ids {
		if !credentials.Stored(id) {
			continue
		}
		next, err := providercredentials.Delete(tokensDir, credentials.ETag, id)
		if err != nil {
			return credentials, err
		}
		credentials = next
	}
	return credentials, nil
}

// endpointOrigin reduces an endpoint to the destination that would receive
// credentials: scheme plus host for URLs with a host, the socket or opaque
// path otherwise, and the trimmed raw value when it is not a URL. Values that
// cannot be proven to name the same destination compare as different, which
// errs toward clearing the credential.
func endpointOrigin(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" {
		return trimmed
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.Host != "" {
		return scheme + "://" + strings.ToLower(parsed.Host)
	}
	return scheme + "://" + parsed.Opaque + parsed.Path
}

// credentialSeveranceEdits returns the extra edits that clear stored
// credentials whose endpoint origin is being changed by this PATCH without an
// explicit credential update alongside it.
func credentialSeveranceEdits(current *config.Config, edits []config.Edit) []config.Edit {
	byKey := make(map[string]any, len(edits))
	for _, edit := range edits {
		byKey[edit.Key] = edit.Value
	}
	var severance []config.Edit
	for _, binding := range credentialBindings {
		endpointValue, endpointEdited := byKey[binding.endpointKey]
		if !endpointEdited {
			continue
		}
		endpoint, ok := endpointValue.(string)
		if !ok || endpointOrigin(endpoint) == endpointOrigin(binding.currentEndpoint(current)) {
			continue
		}
		if _, credentialEdited := byKey[binding.credentialKey]; credentialEdited {
			continue
		}
		if !binding.credentialSet(current) {
			continue
		}
		severance = append(severance, config.Edit{Key: binding.credentialKey, Value: ""})
	}
	return severance
}

func settingsEdits(current *config.Config, updates []SettingUpdate) ([]config.Edit, bool, error) {
	definitions := settingsDefinitionByKey()
	seen := make(map[string]struct{}, len(updates))
	edits := make([]config.Edit, 0, len(updates))
	restartRequired := false
	for _, update := range updates {
		definition, ok := definitions[update.Key]
		if !ok {
			return nil, false, fmt.Errorf("setting %q is not browser-managed", update.Key)
		}
		if definition.localOnly {
			return nil, false, fmt.Errorf("setting %q is host-managed and cannot be changed through remote Settings", update.Key)
		}
		if definition.credentialID != "" {
			return nil, false, fmt.Errorf("setting %q must use the provider credential endpoint", update.Key)
		}
		if _, duplicate := seen[update.Key]; duplicate {
			return nil, false, fmt.Errorf("setting %q is updated more than once", update.Key)
		}
		seen[update.Key] = struct{}{}
		var value any
		if definition.secret != nil {
			if update.Secret == nil || update.Value != nil {
				return nil, false, fmt.Errorf("setting %q must use a secret action", update.Key)
			}
			switch update.Secret.Action {
			case "set":
				if update.Secret.Value == "" {
					return nil, false, fmt.Errorf("setting %q cannot be set to an empty secret", update.Key)
				}
				value = update.Secret.Value
			case "clear":
				if update.Secret.Value != "" {
					return nil, false, fmt.Errorf("setting %q clear action cannot include a value", update.Key)
				}
				value = ""
			default:
				return nil, false, fmt.Errorf("setting %q has an invalid secret action", update.Key)
			}
		} else {
			if update.Secret != nil || update.Value == nil {
				return nil, false, fmt.Errorf("setting %q requires a value", update.Key)
			}
			converted, err := convertSettingValue(definition.kind, update.Value)
			if err != nil {
				return nil, false, fmt.Errorf("setting %q: %w", update.Key, err)
			}
			value = converted
		}
		if err := validateSettingUpdate(update.Key, value, definition.options); err != nil {
			return nil, false, fmt.Errorf("%w: %s", errInvalidSettingUpdate, update.Key)
		}
		edits = append(edits, config.Edit{Key: update.Key, Value: value})
		restartRequired = restartRequired || definition.restartRequired
	}
	edits = append(edits, credentialSeveranceEdits(current, edits)...)
	return edits, restartRequired, nil
}

func validateSettingUpdate(key string, value any, options []string) error {
	if len(options) > 0 {
		text, ok := value.(string)
		if !ok {
			return errors.New("option must be a string")
		}
		if !slices.Contains(options, text) {
			return errors.New("unsupported option")
		}
	}
	if err := validateSettingBounds(key, value); err != nil {
		return err
	}
	switch key {
	case "sync.rate_limit_qps":
		integer, ok := value.(int)
		if !ok || integer <= 0 {
			return errors.New("must be positive")
		}
	case "log.sql_slow_ms", "beeper.media_max_participants", "beeper.max_media_mb",
		"slack.media_max_participants", "slack.max_media_mb", "discord.media_max_participants",
		"discord.max_media_mb", "teams.media_max_participants", "teams.max_media_mb",
		"vector.search.max_page_size_hybrid":
		integer, ok := value.(int)
		if !ok || integer < 0 {
			return errors.New("must be non-negative")
		}
	case "beeper.rate_limit_qps":
		number, ok := value.(float64)
		if !ok || number < 0 {
			return errors.New("must be non-negative")
		}
	case "vector.embeddings.endpoint", "vector.multimodal.endpoint":
		endpoint, ok := value.(string)
		if !ok {
			return errors.New("endpoint must be a string")
		}
		if _, err := providercredentials.EndpointOrigin(endpoint); err != nil {
			return err
		}
	case "vector.embeddings.timeout", "server.daemon_idle_timeout", "analytics.min_rebuild_interval",
		"people.enrichment.lease_duration":
		text, ok := value.(string)
		if !ok {
			return errors.New("duration must be a string")
		}
		duration, err := time.ParseDuration(text)
		if err != nil || duration < 0 || (key != "server.daemon_idle_timeout" && key != "analytics.min_rebuild_interval" && duration == 0) {
			return errors.New("invalid duration")
		}
	case "vector.embed.backstop_interval":
		text, ok := value.(string)
		if !ok {
			return errors.New("duration must be a string")
		}
		if _, err := time.ParseDuration(text); err != nil {
			return errors.New("invalid duration")
		}
	}
	return nil
}

func settingValue(kind string, value any) *SettingValue {
	result := &SettingValue{}
	switch kind {
	case "string":
		if typed, ok := value.(string); ok {
			result.String = &typed
		}
	case "boolean":
		if typed, ok := value.(bool); ok {
			result.Boolean = &typed
		}
	case "integer":
		if typed, ok := value.(int); ok {
			result.Integer = &typed
		}
	case "number":
		if typed, ok := value.(float64); ok {
			result.Number = &typed
		}
	case "string_array":
		if typed, ok := value.([]string); ok {
			stringsValue := append([]string{}, typed...)
			result.Strings = &stringsValue
		}
	}
	return result
}

func convertSettingValue(kind string, value *SettingValue) (any, error) {
	if value == nil {
		return nil, errors.New("value is required")
	}
	populated := 0
	if value.String != nil {
		populated++
	}
	if value.Integer != nil {
		populated++
	}
	if value.Number != nil {
		populated++
	}
	if value.Boolean != nil {
		populated++
	}
	if value.Strings != nil {
		populated++
	}
	if populated != 1 {
		return nil, errors.New("value must contain exactly one typed member")
	}
	switch kind {
	case "string":
		if value.String == nil {
			return nil, errors.New("value must be a string")
		}
		return *value.String, nil
	case "boolean":
		if value.Boolean == nil {
			return nil, errors.New("value must be a boolean")
		}
		return *value.Boolean, nil
	case "integer":
		if value.Integer == nil {
			return nil, errors.New("value must be an integer")
		}
		return *value.Integer, nil
	case "number":
		if value.Number == nil || math.IsInf(*value.Number, 0) || math.IsNaN(*value.Number) {
			return nil, errors.New("value must be a finite number")
		}
		return *value.Number, nil
	case "string_array":
		if value.Strings == nil {
			return nil, errors.New("value must be an array of strings")
		}
		return *value.Strings, nil
	default:
		return nil, errors.New("unsupported setting type")
	}
}
