package api

import (
	"errors"
	"strings"
)

type settingMetadata struct {
	label       string
	description string
}

var settingsGroups = []SettingGroup{
	{ID: "browser", Label: "Web appearance", Description: "Browser preferences applied without restarting the daemon."},
	{ID: "server", Label: "Daemon", Description: "Daemon lifecycle settings. Listener and authentication bootstrap values are host-managed and read-only."},
	{ID: "archive", Label: "Analytics", Description: "Analytics engine and bounded cache-builder resources."},
	{ID: "sync", Label: "Sync", Description: "Shared source synchronization limits."},
	{ID: "logging", Label: "Logging", Description: "Persistent diagnostics. SQL tracing can produce high-volume logs with statement metadata."},
	{ID: "search", Label: "Search and embeddings", Description: "Vector search, text embeddings, and visual Voyage embeddings."},
	{ID: settingsGroupSources, Label: "Sources", Description: "Safe schedules and filters for configured archive sources."},
	{ID: settingsGroupAttachments, Label: "Attachment downloads", Description: "Controls future attachment downloads only. Changes do not fetch, remove, or re-evaluate existing files."},
	{ID: "activity", Label: "Activity", Description: "Dated activity projection schedule and bounded batch settings."},
	{ID: "backup", Label: "Backups", Description: "Portable backup compression settings."},
	{ID: settingsGroupEnrichment, Label: "Person enrichment", Description: "Global orchestration and independently consented providers. Provider policies are keyed by stable name."},
	{ID: "integrations", Label: "Integrations", Description: "Optional outbound integrations."},
}

var settingsMetadata = map[string]settingMetadata{
	"web.default_search_mode":                   {"Default search mode", "Initial search mode used by the Web interface."},
	"web.theme":                                 {"Theme", "Web color theme. Applied without a daemon restart."},
	"web.density":                               {"Density", "Web layout density. Applied without a daemon restart."},
	"server.bind_addr":                          {"Bind address", "Host-managed listener address; remote settings clients cannot change it."},
	"server.api_port":                           {"API port", "Host-managed listener port; 0 asks the daemon to select a port."},
	"server.api_key":                            {"API key", "Host-managed authentication bootstrap secret. Its value is never returned."},
	"server.allow_insecure":                     {"Allow insecure access", "Host-managed authentication boundary; remote settings clients cannot change it."},
	"server.trusted_proxies":                    {"Trusted proxies", "Host-managed proxy trust boundary; remote settings clients cannot change it."},
	"server.daemon_idle_timeout":                {"Idle timeout", "How long an idle background daemon waits before stopping; 0 disables the timeout."},
	"server.daemon_auto_restart":                {"Automatic restart", "Policy used when a compatible daemon executable changes."},
	"analytics.engine":                          {"Analytics engine", "Engine used for aggregate analytics queries; auto selects the best available engine."},
	"analytics.auto_build_cache":                {"Build stale analytics cache", "Build the analytics cache automatically when a query finds it stale."},
	"analytics.min_rebuild_interval":            {"Minimum cache rebuild interval", "Minimum time between automatic analytics cache rebuilds."},
	"analytics.builder_memory_limit":            {"Cache-builder memory limit", "Optional memory limit applied while building the analytics cache."},
	"analytics.builder_threads":                 {"Cache-builder threads", "Maximum analytics cache-builder threads; 0 uses the engine default."},
	"analytics.builder_temp_limit":              {"Cache-builder temporary storage limit", "Optional temporary-storage limit applied while building the analytics cache."},
	"sync.rate_limit_qps":                       {"Sync requests per second", "Positive shared request-rate limit used by source synchronization."},
	"log.enabled":                               {"Persistent logs", "Write structured logs to the daemon log directory."},
	"log.level":                                 {"Log level", "Minimum persistent log severity; an empty value uses the built-in default."},
	"log.sql_slow_ms":                           {"Slow SQL threshold", "Warn when a SQL statement exceeds this many milliseconds; 0 uses the built-in default."},
	"log.sql_trace":                             {"Trace every SQL statement", "High-volume diagnostic mode that logs every SQL statement. Enable only while debugging."},
	"vector.enabled":                            {"Vector search master switch", "Master gate for text semantic indexing and search. Visual embeddings have an additional lane gate."},
	"vector.backend":                            {"Vector backend", "Host-managed storage backend; changing it requires local migration decisions."},
	"vector.db_path":                            {"Vector database path", "Host-managed filesystem/database location."},
	"vector.skip_extension_create":              {"Skip extension creation", "Host-managed database privilege setting."},
	"vector.embeddings.api_format":              {"Text embedding API format", "Request and response format used by the configured text embedding provider."},
	"vector.embeddings.endpoint":                {"Text embedding endpoint", "API root used for text embedding requests. Save endpoint changes before storing a credential."},
	"vector.embeddings.api_key_env":             {"Text embedding credential environment variable", "Host-managed environment variable used when no stored text embedding credential exists."},
	"vector.embeddings.api_key":                 {"Text embedding API key", "Write-only credential for the text embedding endpoint. Stored credentials override the configured environment variable."},
	"vector.embeddings.model":                   {"Text embedding model", "Provider model identifier included in the embedding generation fingerprint."},
	"vector.embeddings.document_prefix":         {"Document embedding prefix", "Optional provider-specific prefix prepended to text when indexing documents."},
	"vector.embeddings.query_prefix":            {"Query embedding prefix", "Optional provider-specific prefix prepended to text when embedding search queries."},
	"vector.embeddings.dimension":               {"Text embedding dimension", "Vector dimension returned by the configured text embedding model."},
	"vector.embeddings.batch_size":              {"Text embedding batch size", "Maximum number of inputs sent in one text embedding request."},
	"vector.embeddings.timeout":                 {"Text embedding timeout", "Maximum duration allowed for one text embedding provider request."},
	"vector.embeddings.max_retries":             {"Text embedding retries", "Maximum transient request retries; 0 uses the built-in default."},
	"vector.embeddings.max_input_chars":         {"Maximum text embedding input", "Maximum characters sent for one text embedding input."},
	"vector.embeddings.eta_window":              {"Text embedding ETA window", "Recent progress samples used to estimate completion time."},
	"vector.people.enabled":                     {"Embed curated person fields", "Allow the explicitly consented person fields to use the text embedding provider when the vector master gate is enabled."},
	"vector.people.retention_posture":           {"Person embedding retention posture", "Operator assertion describing provider retention for curated person embeddings."},
	"vector.people.training_posture":            {"Person embedding training posture", "Operator assertion describing provider training use for curated person embeddings."},
	"vector.embed.schedule.cron":                {"Text embedding schedule", "Five-field cron schedule for background text embedding; empty disables the schedule."},
	"vector.embed.schedule.run_after_sync":      {"Embed text after sync", "Run text embedding after a successful source synchronization."},
	"vector.embed.scope.message_types":          {"Text embedding message types", "Optional message-type allowlist for text embedding; empty uses all supported types."},
	"vector.embed.scope.accounts":               {"Text embedding accounts", "Optional account-ID allowlist for text embedding; empty uses all accounts."},
	"vector.embed.backstop_interval":            {"Text embedding backstop interval", "Maximum interval between background embedding checks when no schedule or sync trigger runs."},
	"vector.multimodal.enabled":                 {"Visual embedding lane", "Additional gate for hosted visual attachment indexing; the vector master gate must also be enabled."},
	"vector.multimodal.provider":                {"Visual embedding provider", "Hosted provider used for visual attachment embeddings."},
	"vector.multimodal.endpoint":                {"Visual embedding endpoint", "Pinned API root used for visual embedding requests. Save endpoint changes before storing a credential."},
	"vector.multimodal.api_key_env":             {"Visual embedding credential environment variable", "Host-managed environment variable used when no stored visual embedding credential exists."},
	"vector.multimodal.api_key":                 {"Voyage API key", "Write-only credential for visual Voyage embeddings. Stored credentials override the configured environment variable."},
	"vector.multimodal.capabilities_file":       {"Visual capability manifest", "Host-managed path to the locally probed provider capability manifest."},
	"vector.multimodal.model":                   {"Visual embedding model", "Provider model identifier included in the visual embedding generation fingerprint."},
	"vector.multimodal.dimension":               {"Visual embedding dimension", "Vector dimension required by the pinned visual embedding model."},
	"vector.multimodal.max_context_chars":       {"Maximum visual context", "Maximum normalized owning-message characters sent with one visual attachment."},
	"vector.multimodal.include_images":          {"Index still images", "Allow supported still-image attachments to be sent to the visual embedding provider."},
	"vector.multimodal.include_animated_gifs":   {"Index animated GIFs", "Allow animated GIF attachments only when the provider capability manifest permits them."},
	"vector.multimodal.include_video":           {"Index videos", "Allow bounded supported video attachments to be sent to the visual embedding provider."},
	"vector.multimodal.allow_image_queries":     {"Allow image queries", "Allow bounded query images to be sent to the visual embedding provider."},
	"vector.multimodal.scope.message_types":     {"Visual embedding message types", "Optional message-type allowlist for visual attachment indexing."},
	"vector.multimodal.schedule.cron":           {"Visual embedding schedule", "Five-field cron schedule for background visual indexing; empty disables the schedule."},
	"vector.multimodal.schedule.run_after_sync": {"Embed visuals after sync", "Run visual attachment indexing after a successful source synchronization."},
	"vector.search.rrf_k":                       {"Hybrid RRF constant", "Reciprocal-rank-fusion constant used to combine hybrid search signals."},
	"vector.search.k_per_signal":                {"Hybrid candidates per signal", "Candidate count retained from each hybrid search signal."},
	"vector.search.subject_boost":               {"Subject match boost", "Non-negative hybrid-ranking weight applied to subject matches."},
	"vector.search.max_page_size_hybrid":        {"Maximum hybrid page size", "Maximum page size accepted for hybrid search; 0 disables this clamp."},
	"vector.preprocess.strip_quotes":            {"Strip quoted replies", "Remove quoted reply blocks before text embedding when enabled."},
	"vector.preprocess.strip_signatures":        {"Strip signatures", "Remove detected message signatures before text embedding when enabled."},
	"vector.preprocess.strip_html":              {"Strip HTML", "Remove HTML markup before text embedding when enabled."},
	"vector.preprocess.strip_base64":            {"Strip base64 payloads", "Remove embedded base64 payloads before text embedding when enabled."},
	"vector.preprocess.strip_url_tracking":      {"Strip URL tracking parameters", "Remove common tracking parameters from URLs before text embedding when enabled."},
	"vector.preprocess.collapse_whitespace":     {"Collapse whitespace", "Normalize repeated whitespace before text embedding when enabled."},
	"people.enrichment.enabled":                 {"Enable person enrichment", "Global gate. At least one named provider and a durable suppression key must be available before enablement."},
	"people.enrichment.schedule":                {"Enrichment schedule", "Five-field cron schedule for person enrichment."},
	"people.enrichment.batch_size":              {"Enrichment batch size", "Positive number of people leased per enrichment run."},
	"people.enrichment.lease_duration":          {"Enrichment lease", "Positive duration for enrichment work leases."},
	"beeper.enabled":                            {"Scheduled Beeper sync", "Enable scheduled synchronization for configured Beeper accounts."},
	"beeper.schedule":                           {"Beeper sync schedule", "Five-field cron schedule for Beeper synchronization; empty disables the schedule."},
	"beeper.accounts":                           {"Included Beeper accounts", "Beeper account IDs to include; empty includes all accounts not explicitly excluded."},
	"beeper.exclude_accounts":                   {"Excluded Beeper accounts", "Beeper account IDs skipped during synchronization."},
	"beeper.rate_limit_qps":                     {"Beeper requests per second", "Non-negative request-rate limit; 0 uses the provider default."},
	"slack.enabled":                             {"Scheduled Slack sync", "Enable scheduled synchronization for configured Slack channels."},
	"slack.schedule":                            {"Slack sync schedule", "Five-field cron schedule for Slack synchronization; empty disables the schedule."},
	"slack.channels":                            {"Included Slack channels", "Channel names to include; direct messages are never filtered by this list."},
	"slack.exclude_channels":                    {"Excluded Slack channels", "Channel names to skip."},
	"beeper.media":                              {"Download Beeper attachments", "Provider default is enabled; changes affect future downloads only."},
	"slack.media":                               {"Download Slack files", "Provider default is enabled; changes affect future downloads only."},
	"discord.media":                             {"Download Discord attachments", "Provider default is enabled; changes affect future downloads only."},
	"teams.media":                               {"Download Teams attachments", "Provider default is enabled; changes affect future downloads only."},
	"beeper.media_scope":                        {"Beeper attachment scope", "Choose all conversations, direct conversations only, or none for future downloads."},
	"slack.media_scope":                         {"Slack attachment scope", "Choose all conversations, direct conversations only, or none for future downloads."},
	"discord.media_scope":                       {"Discord attachment scope", "Choose all conversations, direct conversations only, or none for future downloads."},
	"teams.media_scope":                         {"Teams attachment scope", "Choose all conversations, direct conversations only, or none for future downloads."},
	"beeper.media_max_participants":             {"Beeper participant limit", "Skip future attachment downloads in conversations over this participant count; 0 means no participant limit."},
	"slack.media_max_participants":              {"Slack participant limit", "Skip future attachment downloads in conversations over this participant count; 0 means no participant limit."},
	"discord.media_max_participants":            {"Discord participant limit", "Skip future attachment downloads in conversations over this participant count; 0 means no participant limit."},
	"teams.media_max_participants":              {"Teams participant limit", "Skip future attachment downloads in conversations over this participant count; 0 means no participant limit."},
	"beeper.max_media_mb":                       {"Beeper maximum attachment size", "Maximum future attachment size in MiB; 0 uses the Beeper default of 100 MiB."},
	"slack.max_media_mb":                        {"Slack maximum attachment size", "Maximum future attachment size in MiB; 0 uses the Slack default of 100 MiB."},
	"discord.max_media_mb":                      {"Discord maximum attachment size", "Maximum future attachment size in MiB; 0 uses the Discord default of 50 MiB."},
	"teams.max_media_mb":                        {"Teams maximum attachment size", "Maximum future attachment size in MiB; 0 uses the Teams default of 100 MiB."},
	"activity.timezone":                         {"Activity timezone", "IANA timezone used to group events into local calendar dates."},
	"activity.max_direct_counterparts":          {"Maximum direct counterparts", "Bounded number of direct-message counterparts included in one activity projection pass."},
	"activity.batch_size":                       {"Activity batch size", "Bounded number of source records processed in one activity projection batch."},
	"activity.schedule":                         {"Activity projection schedule", "Five-field cron schedule for dated activity projection; empty disables the schedule."},
	"backup.zstd_level":                         {"Backup compression level", "Zstandard compression level for portable backups; 0 uses the encoder default."},
	"carddav.base_url":                          {"CardDAV base URL", "Current CardDAV server URL. Change it through the dedicated CardDAV account workflow."},
	"carddav.username":                          {"CardDAV username", "Current CardDAV account name. Change it through the dedicated CardDAV account workflow."},
	"carddav.schedule":                          {"CardDAV sync schedule", "Current CardDAV sync schedule. Change it through the dedicated CardDAV account workflow."},
	"carddav.enabled":                           {"CardDAV synchronization", "Current CardDAV synchronization state. Change it through the dedicated CardDAV account workflow."},
	"carddav.password":                          {"CardDAV password", "Write-only CardDAV credential managed through the dedicated CardDAV account workflow."},
	"integrations.tasks.enabled":                {"Task integration", "Enable the configured provider-neutral outbound task integration."},
	"integrations.tasks.endpoint":               {"Task integration endpoint", "HTTPS, loopback HTTP, owner-controlled Unix socket, or local discovery endpoint."},
	"integrations.tasks.api_key":                {"Task integration API key", "Write-only bearer credential used only by the daemon for the task integration."},
	"integrations.tasks.default_project":        {"Default task project", "Project used by default for task creation and lookup."},
}

var settingsValidation = map[string]SettingValidation{
	"server.api_port":                numberValidation(0, new(float64(65_535)), "0 asks the daemon to select an available port."),
	"server.daemon_idle_timeout":     {Hint: "Go duration such as 30s, 15m, or 2h; 0 disables the idle timeout.", Required: true},
	"analytics.min_rebuild_interval": {Hint: "Go duration such as 15m or 2h; 0 allows immediate rebuilds.", Required: true},
	"analytics.builder_memory_limit": {Hint: "Optional positive size such as 512MiB or 2GB."},
	"analytics.builder_threads":      numberValidation(0, nil, "0 uses the analytics engine default."),
	"analytics.builder_temp_limit":   {Hint: "Optional positive size such as 1GiB or 10GB."},
	"sync.rate_limit_qps":            numberValidation(1, nil, "Positive requests-per-second limit."),
	"log.sql_slow_ms":                numberValidation(0, nil, "0 uses the built-in threshold."),

	"vector.embeddings.endpoint": {
		Hint: "Absolute HTTP or HTTPS URL without credentials, query, or fragment.", Required: true,
	},
	"vector.embeddings.model":           {Hint: "Provider model identifier used in the vector generation fingerprint.", Required: true},
	"vector.embeddings.dimension":       numberValidation(1, nil, "Positive embedding vector dimension."),
	"vector.embeddings.batch_size":      numberValidation(1, nil, "Positive provider request batch size."),
	"vector.embeddings.timeout":         {Hint: "Positive Go duration such as 30s or 2m.", Required: true},
	"vector.embeddings.max_retries":     numberValidation(0, nil, "0 uses the built-in retry default."),
	"vector.embeddings.max_input_chars": numberValidation(1, nil, "Maximum characters sent for one embedding input."),
	"vector.embeddings.eta_window":      numberValidation(1, nil, "Positive rolling window used for progress estimates."),
	"vector.people.retention_posture":   {Hint: "Explicit provider data-retention posture.", Required: true},
	"vector.people.training_posture":    {Hint: "Explicit provider model-training posture.", Required: true},
	"vector.embed.schedule.cron":        {Hint: "Five-field cron expression; leave empty to disable scheduled embedding."},
	"vector.embed.backstop_interval":    {Hint: "Go duration; 0 uses the default and a negative duration disables the backstop.", Required: true},
	"vector.multimodal.endpoint": {
		Hint: "Pinned Voyage HTTPS API root without credentials, query, or fragment.", Required: true,
	},
	"vector.multimodal.model":             {Hint: "Pinned visual embedding model identifier.", Required: true},
	"vector.multimodal.dimension":         numberValidation(1024, new(float64(1024)), "Voyage visual embeddings require 1024 dimensions."),
	"vector.multimodal.max_context_chars": numberValidation(1, nil, "Positive maximum context characters per visual document."),
	"vector.multimodal.schedule.cron":     {Hint: "Five-field cron expression; leave empty to disable scheduled visual embedding."},
	"vector.search.rrf_k":                 numberValidation(1, nil, "Positive reciprocal-rank-fusion constant."),
	"vector.search.k_per_signal":          numberValidation(1, nil, "Positive candidates retained per search signal."),
	"vector.search.subject_boost":         numberValidation(0, nil, "Non-negative subject-match weight."),
	"vector.search.max_page_size_hybrid":  numberValidation(0, nil, "0 disables the hybrid-result page-size clamp."),

	"beeper.schedule":                {Hint: "Five-field cron expression; leave empty to disable scheduled sync."},
	"slack.schedule":                 {Hint: "Five-field cron expression; leave empty to disable scheduled sync."},
	"beeper.rate_limit_qps":          numberValidation(0, nil, "0 uses the Beeper provider default."),
	"beeper.media_max_participants":  numberValidation(0, nil, "0 means no participant limit."),
	"slack.media_max_participants":   numberValidation(0, nil, "0 means no participant limit."),
	"discord.media_max_participants": numberValidation(0, nil, "0 means no participant limit."),
	"teams.media_max_participants":   numberValidation(0, nil, "0 means no participant limit."),
	"beeper.max_media_mb":            numberValidation(0, nil, "0 uses the Beeper default of 100 MiB."),
	"slack.max_media_mb":             numberValidation(0, nil, "0 uses the Slack default of 100 MiB."),
	"discord.max_media_mb":           numberValidation(0, nil, "0 uses the Discord default of 50 MiB."),
	"teams.max_media_mb":             numberValidation(0, nil, "0 uses the Teams default of 100 MiB."),

	"activity.timezone":                {Hint: "UTC or an IANA timezone such as America/New_York.", Required: true},
	"activity.max_direct_counterparts": numberValidation(1, new(float64(10_000)), "Bounded direct-counterpart count."),
	"activity.batch_size":              numberValidation(1, new(float64(10_000)), "Bounded projection batch size."),
	"activity.schedule":                {Hint: "Five-field cron expression; leave empty to disable scheduled projection."},
	"backup.zstd_level":                numberValidation(0, new(float64(19)), "0 uses the backup encoder default; explicit levels are 1 through 19."),
	"people.enrichment.schedule":       {Hint: "Required five-field cron expression.", Required: true},
	"people.enrichment.batch_size":     numberValidation(1, nil, "Positive number of people leased per run."),
	"people.enrichment.lease_duration": {Hint: "Positive Go duration such as 5m or 1h.", Required: true},
	"integrations.tasks.endpoint":      {Hint: "Optional HTTPS URL, loopback HTTP URL, or owner-controlled Unix socket."},
}

func numberValidation(minimum float64, maximum *float64, hint string) SettingValidation {
	return SettingValidation{Hint: hint, Minimum: new(minimum), Maximum: maximum}
}

func validationForSetting(key string) *SettingValidation {
	validation, ok := settingsValidation[key]
	if !ok {
		return nil
	}
	return &validation
}

func validateSettingBounds(key string, value any) error {
	validation := validationForSetting(key)
	if validation == nil || (validation.Minimum == nil && validation.Maximum == nil) {
		return nil
	}
	var number float64
	switch typed := value.(type) {
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case float64:
		number = typed
	default:
		return nil
	}
	if validation.Minimum != nil && number < *validation.Minimum {
		return errors.New("below minimum")
	}
	if validation.Maximum != nil && number > *validation.Maximum {
		return errors.New("above maximum")
	}
	return nil
}

func metadataForSetting(key string) settingMetadata {
	if metadata, ok := settingsMetadata[key]; ok {
		return metadata
	}
	last := key
	if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
		last = key[dot+1:]
	}
	words := strings.Fields(strings.ReplaceAll(last, "_", " "))
	for index := range words {
		if words[index] != "" {
			words[index] = strings.ToUpper(words[index][:1]) + words[index][1:]
		}
	}
	label := strings.Join(words, " ")
	return settingMetadata{label: label, description: "Configures " + strings.ReplaceAll(key, "_", " ") + "."}
}
