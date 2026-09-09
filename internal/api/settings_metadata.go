package api

import (
	"errors"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/config"
)

// settingMetadata is the user-facing copy for one setting. The label names
// the setting, the description says what it does in one plain sentence, and
// the section places it inside its group. Format and range rules belong in
// settingsValidation so they are not repeated here.
// Section IDs that repeat across the catalog.
const (
	sectionProvider = "provider"
	sectionVisual   = "visual"
)

type settingMetadata struct {
	label       string
	description string
	section     string
}

// settingsGroups are the categories a settings client shows. Groups with
// sections list them in display order; every setting in such a group names
// one of them.
var settingsGroups = []SettingGroup{
	{
		ID: "browser", Label: "Appearance",
		Description: "How the web app looks and which search mode it opens in.",
	},
	{
		ID: "server", Label: "Daemon",
		Description: "How the background daemon listens, stays alive, and logs.",
		Sections: []SettingSection{
			{ID: "listener", Label: "Listener and access"},
			{ID: "lifecycle", Label: "Lifecycle"},
			{ID: "logging", Label: "Logging"},
		},
	},
	{
		ID: "archive", Label: "Archive",
		Description: "The analytics cache, activity history, and backups.",
		Sections: []SettingSection{
			{ID: "analytics", Label: "Analytics cache", Description: "Aggregate views read a Parquet cache that the daemon rebuilds when it goes stale."},
			{ID: "activity", Label: "Activity history", Description: "Sorts events into calendar days for the activity views."},
			{ID: "backups", Label: "Backups"},
		},
	},
	{
		ID: "search", Label: "Search",
		Description: "Semantic search, the embedding provider, and visual attachment indexing.",
		Sections: []SettingSection{
			{ID: "semantic", Label: "Semantic search"},
			{ID: sectionProvider, Label: "Text embedding provider", Description: "Save endpoint changes before storing a credential."},
			{ID: "schedule", Label: "Embedding schedule and scope"},
			{ID: "people", Label: "Person embeddings", Description: "Sends consented person fields to the text embedding provider."},
			{ID: sectionVisual, Label: "Visual attachment search", Description: "Sends images to a hosted provider. Semantic search must also be on."},
			{ID: "ranking", Label: "Hybrid ranking", Description: "How full-text and semantic results combine."},
			{ID: "preprocess", Label: "Text preprocessing", Description: "What to strip from message text before embedding it."},
		},
	},
	{
		ID: settingsGroupSources, Label: "Sources",
		Description: "Sync schedules and filters for chat and contact sources.",
		Sections: []SettingSection{
			{ID: "shared", Label: "All sources"},
			{ID: sourceTypeBeeper, Label: "Beeper"},
			{ID: sourceTypeSlack, Label: "Slack"},
			{ID: "carddav", Label: "CardDAV", Description: "Change these in the CardDAV account category."},
		},
	},
	{
		ID: settingsGroupAttachments, Label: "Attachments",
		Description: "Which chat attachments future syncs download. Existing files are not fetched, removed, or re-checked.",
		Sections: []SettingSection{
			{ID: sourceTypeBeeper, Label: "Beeper"},
			{ID: sourceTypeSlack, Label: "Slack"},
			{ID: "discord", Label: "Discord"},
			{ID: "teams", Label: "Teams"},
		},
	},
	{
		ID: settingsGroupEnrichment, Label: "Person enrichment",
		Description: "Look up more about people through providers you have set up and consented to.",
	},
	{
		ID: "integrations", Label: "Integrations",
		Description: "Optional connections to outside services.",
	},
}

var settingsMetadata = map[string]settingMetadata{
	"web.default_search_mode": {"Default search mode", "Search mode the web app opens with.", ""},
	"web.theme":               {"Theme", "Light, dark, or follow the system.", ""},
	"web.density":             {"Density", "Spacing of tables and toolbars.", ""},

	"server.bind_addr":           {"Bind address", "Address the daemon listens on.", "listener"},
	"server.api_port":            {"API port", "Port the daemon listens on.", "listener"},
	"server.api_key":             {"API key", "Key that remote clients and browser logins use. Its value is never shown.", "listener"},
	"server.allow_insecure":      {"Allow insecure access", "Allow connections from other machines without an API key.", "listener"},
	"server.trusted_proxies":     {"Trusted proxies", "IP addresses or ranges allowed to forward HTTPS details for a request.", "listener"},
	"server.daemon_idle_timeout": {"Idle timeout", "How long a background daemon waits with nothing to do before it stops.", "lifecycle"},
	"server.daemon_auto_restart": {"Automatic restart", "When the CLI restarts a running daemon after its binary changes.", "lifecycle"},
	"log.enabled":                {"Persistent logs", "Write structured logs to the daemon log directory.", "logging"},
	"log.level":                  {"Log level", "Lowest severity written to the log. Empty uses the default.", "logging"},
	"log.sql_slow_ms":            {"Slow SQL threshold", "Log a warning when a SQL statement takes longer than this many milliseconds.", "logging"},
	"log.sql_trace":              {"Trace every SQL statement", "Log every SQL statement. Produces a lot of output, so turn it on only while debugging.", "logging"},

	"analytics.engine":                 {"Analytics engine", "Engine for aggregate queries. Auto picks the best one available.", "analytics"},
	"analytics.auto_build_cache":       {"Rebuild stale cache automatically", "Rebuild the analytics cache when a query finds it out of date.", "analytics"},
	"analytics.min_rebuild_interval":   {"Minimum rebuild interval", "Shortest gap between automatic cache rebuilds.", "analytics"},
	"analytics.builder_memory_limit":   {"Builder memory limit", "Memory the cache builder may use.", "analytics"},
	"analytics.builder_threads":        {"Builder threads", "Threads the cache builder may use.", "analytics"},
	"analytics.builder_temp_limit":     {"Builder temporary storage limit", "Disk space the cache builder may use for temporary files.", "analytics"},
	"activity.timezone":                {"Timezone", "Timezone used to sort events into calendar days.", "activity"},
	"activity.max_direct_counterparts": {"Direct conversation limit", "Conversations with more other people than this count as broadcasts, not direct activity.", "activity"},
	"activity.batch_size":              {"Batch size", "Records processed per projection batch.", "activity"},
	"activity.schedule":                {"Schedule", "When the activity projection runs.", "activity"},
	"backup.zstd_level":                {"Compression level", "Zstandard level for portable backups.", "backups"},

	"vector.enabled":               {"Semantic search", "Index message text with an embedding provider so semantic and hybrid search work.", "semantic"},
	"vector.backend":               {"Vector backend", "Where embeddings are stored.", "semantic"},
	"vector.db_path":               {"Vector database path", "Override the vector database location.", "semantic"},
	"vector.skip_extension_create": {"Skip extension creation", "Use a vector extension an administrator already installed.", "semantic"},

	"vector.embeddings.api_format":      {"Text embedding API format", "Request format the provider expects.", sectionProvider},
	"vector.embeddings.endpoint":        {"Text embedding endpoint", "Base URL of the embedding API.", sectionProvider},
	"vector.embeddings.api_key_env":     {"Text embedding key variable", "Environment variable the daemon reads when no stored credential exists.", sectionProvider},
	"vector.embeddings.api_key":         {"Text embedding API key", "Stored key for the endpoint. It overrides the environment variable.", sectionProvider},
	"vector.embeddings.model":           {"Text embedding model", "Model identifier. Changing it starts a new index generation.", sectionProvider},
	"vector.embeddings.document_prefix": {"Document prefix", "Text added in front of each document before embedding. Some providers need one.", sectionProvider},
	"vector.embeddings.query_prefix":    {"Query prefix", "Text added in front of each search query before embedding.", sectionProvider},
	"vector.embeddings.dimension":       {"Text embedding dimension", "Vector size the model returns.", sectionProvider},
	"vector.embeddings.batch_size":      {"Text embedding batch size", "Inputs sent per request.", sectionProvider},
	"vector.embeddings.timeout":         {"Text embedding timeout", "Longest wait for one provider request.", sectionProvider},
	"vector.embeddings.max_retries":     {"Text embedding retries", "How many times a failed request is retried.", sectionProvider},
	"vector.embeddings.max_input_chars": {"Maximum input characters", "Longest text sent as one input.", sectionProvider},
	"vector.embeddings.eta_window":      {"Progress window", "Recent samples used to estimate time remaining.", sectionProvider},

	"vector.embed.schedule.cron":           {"Embedding schedule", "When background embedding runs.", "schedule"},
	"vector.embed.schedule.run_after_sync": {"Embed after sync", "Run embedding after each successful source sync.", "schedule"},
	"vector.embed.scope.message_types":     {"Embedded message types", "Only embed these message types. Empty means all.", "schedule"},
	"vector.embed.scope.accounts":          {"Embedded accounts", "Only embed these account IDs. Empty means all.", "schedule"},
	"vector.embed.backstop_interval":       {"Backstop interval", "Longest gap between embedding checks when neither the schedule nor a sync triggers one.", "schedule"},

	"vector.people.enabled":           {"Embed person fields", "Send the consented fields of each person to the text embedding provider. Semantic search must be on.", "people"},
	"vector.people.retention_posture": {"Provider retention statement", "Your statement of how long the provider keeps person data.", "people"},
	"vector.people.training_posture":  {"Provider training statement", "Your statement of whether the provider trains on person data.", "people"},

	"vector.multimodal.enabled":                 {"Visual attachment search", "Index images and videos with a hosted visual model. Semantic search must also be on.", sectionVisual},
	"vector.multimodal.provider":                {"Visual embedding provider", "Hosted provider for visual embeddings.", sectionVisual},
	"vector.multimodal.endpoint":                {"Visual embedding endpoint", "Base URL of the visual embedding API.", sectionVisual},
	"vector.multimodal.api_key_env":             {"Visual embedding key variable", "Environment variable the daemon reads when no stored credential exists.", sectionVisual},
	"vector.multimodal.api_key":                 {"Visual embedding API key", "Stored Voyage key. It overrides the environment variable.", sectionVisual},
	"vector.multimodal.capabilities_file":       {"Capability manifest", "Path to the provider capability file probed on this host.", sectionVisual},
	"vector.multimodal.model":                   {"Visual embedding model", "Visual model identifier. Changing it starts a new index generation.", sectionVisual},
	"vector.multimodal.dimension":               {"Visual embedding dimension", "Vector size the visual model returns.", sectionVisual},
	"vector.multimodal.max_context_chars":       {"Maximum context characters", "Longest excerpt of the owning message sent with each attachment.", sectionVisual},
	"vector.multimodal.include_images":          {"Index still images", "Send still images to the provider.", sectionVisual},
	"vector.multimodal.include_animated_gifs":   {"Index animated GIFs", "Send animated GIFs when the capability manifest allows them.", sectionVisual},
	"vector.multimodal.include_video":           {"Index videos", "Send videos within the size limit to the provider.", sectionVisual},
	"vector.multimodal.allow_image_queries":     {"Allow image queries", "Let searches send a query image to the provider.", sectionVisual},
	"vector.multimodal.scope.message_types":     {"Visual message types", "Only index attachments from these message types. Empty means all.", sectionVisual},
	"vector.multimodal.schedule.cron":           {"Visual indexing schedule", "When background visual indexing runs.", sectionVisual},
	"vector.multimodal.schedule.run_after_sync": {"Index visuals after sync", "Run visual indexing after each successful source sync.", sectionVisual},

	"vector.search.rrf_k":                {"RRF constant", "Reciprocal rank fusion constant used to merge result lists.", "ranking"},
	"vector.search.k_per_signal":         {"Candidates per signal", "Results each signal contributes before merging.", "ranking"},
	"vector.search.subject_boost":        {"Subject boost", "Extra weight for matches in the subject line.", "ranking"},
	"vector.search.max_page_size_hybrid": {"Maximum hybrid page size", "Largest page a hybrid search returns.", "ranking"},

	"vector.preprocess.strip_quotes":        {"Strip quoted replies", "Remove quoted earlier messages before embedding.", "preprocess"},
	"vector.preprocess.strip_signatures":    {"Strip signatures", "Remove detected signatures before embedding.", "preprocess"},
	"vector.preprocess.strip_html":          {"Strip HTML", "Remove HTML markup before embedding.", "preprocess"},
	"vector.preprocess.strip_base64":        {"Strip base64 payloads", "Remove embedded base64 data before embedding.", "preprocess"},
	"vector.preprocess.strip_url_tracking":  {"Strip URL tracking parameters", "Remove common tracking parameters from links before embedding.", "preprocess"},
	"vector.preprocess.collapse_whitespace": {"Collapse whitespace", "Squeeze repeated spaces and blank lines before embedding.", "preprocess"},

	"sync.rate_limit_qps":     {"Requests per second", "Request rate limit shared by all source syncs.", "shared"},
	"beeper.enabled":          {"Scheduled Beeper sync", "Sync Beeper accounts on the schedule below.", sourceTypeBeeper},
	"beeper.schedule":         {"Beeper schedule", "When Beeper sync runs.", sourceTypeBeeper},
	"beeper.accounts":         {"Included Beeper accounts", "Beeper account IDs to sync. Empty means all that are not excluded.", sourceTypeBeeper},
	"beeper.exclude_accounts": {"Excluded Beeper accounts", "Beeper account IDs to skip.", sourceTypeBeeper},
	"beeper.rate_limit_qps":   {"Beeper requests per second", "Request rate limit for Beeper.", sourceTypeBeeper},
	"slack.enabled":           {"Scheduled Slack sync", "Sync Slack channels on the schedule below.", sourceTypeSlack},
	"slack.schedule":          {"Slack schedule", "When Slack sync runs.", sourceTypeSlack},
	"slack.channels":          {"Included Slack channels", "Channel names to sync. Direct messages are always included.", sourceTypeSlack},
	"slack.exclude_channels":  {"Excluded Slack channels", "Channel names to skip.", sourceTypeSlack},
	"carddav.base_url":        {"CardDAV server URL", "Server this archive syncs contacts with.", "carddav"},
	"carddav.username":        {"CardDAV username", "Account used to sign in to the CardDAV server.", "carddav"},
	"carddav.schedule":        {"CardDAV schedule", "When contact sync runs.", "carddav"},
	"carddav.enabled":         {"Scheduled CardDAV sync", "Whether contact sync runs on the schedule.", "carddav"},
	"carddav.password":        {"CardDAV password", "Sign-in secret for the CardDAV server. Its value is never shown.", "carddav"},

	"beeper.media":                   {"Download Beeper attachments", "Download attachments from future Beeper syncs.", sourceTypeBeeper},
	"beeper.media_scope":             {"Beeper conversations", "Which Beeper conversations to download attachments from.", sourceTypeBeeper},
	"beeper.media_max_participants":  {"Beeper participant limit", "Skip Beeper conversations with more people than this.", sourceTypeBeeper},
	"beeper.max_media_mb":            {"Beeper maximum size", "Largest Beeper attachment to download, in MiB.", sourceTypeBeeper},
	"slack.media":                    {"Download Slack files", "Download files from future Slack syncs.", sourceTypeSlack},
	"slack.media_scope":              {"Slack conversations", "Which Slack conversations to download files from.", sourceTypeSlack},
	"slack.media_max_participants":   {"Slack participant limit", "Skip Slack conversations with more people than this.", sourceTypeSlack},
	"slack.max_media_mb":             {"Slack maximum size", "Largest Slack file to download, in MiB.", sourceTypeSlack},
	"discord.media":                  {"Download Discord attachments", "Download attachments from future Discord syncs.", "discord"},
	"discord.media_scope":            {"Discord conversations", "Which Discord conversations to download attachments from.", "discord"},
	"discord.media_max_participants": {"Discord participant limit", "Skip Discord conversations with more people than this.", "discord"},
	"discord.max_media_mb":           {"Discord maximum size", "Largest Discord attachment to download, in MiB.", "discord"},
	"teams.media":                    {"Download Teams attachments", "Download attachments from future Teams syncs.", "teams"},
	"teams.media_scope":              {"Teams conversations", "Which Teams conversations to download attachments from.", "teams"},
	"teams.media_max_participants":   {"Teams participant limit", "Skip Teams conversations with more people than this.", "teams"},
	"teams.max_media_mb":             {"Teams maximum size", "Largest Teams attachment to download, in MiB.", "teams"},

	"people.enrichment.enabled":        {"Enable person enrichment", "Run enrichment. Needs at least one provider policy and a durable suppression key.", ""},
	"people.enrichment.schedule":       {"Enrichment schedule", "When enrichment runs.", ""},
	"people.enrichment.batch_size":     {"Enrichment batch size", "People handled per run.", ""},
	"people.enrichment.lease_duration": {"Enrichment lease", "How long one run holds a batch before another run may take it.", ""},

	"integrations.tasks.enabled":         {"Task integration", "Send tasks to the configured task service.", ""},
	"integrations.tasks.endpoint":        {"Task endpoint", "Where the task service listens.", ""},
	"integrations.tasks.api_key":         {"Task API key", "Bearer key the daemon sends to the task service. Its value is never shown.", ""},
	"integrations.tasks.default_project": {"Default task project", "Project used when creating or looking up tasks.", ""},
}

// settingsValidation carries format and range rules. A hint says how to
// write a valid value; it never restates what the setting does, and it
// never spells out a bound that Minimum, Maximum, or Off already carry.
var settingsValidation = map[string]SettingValidation{
	"server.api_port":                withOff(numberRange(1, 65_535), "The daemon picks a free port", "8080"),
	"server.daemon_idle_timeout":     withOffValue(durationValidation("30s, 15m, or 2h"), "0s", "Never stops for being idle", "20m"),
	"analytics.min_rebuild_interval": withOffValue(durationValidation("15m or 2h"), "0s", "Rebuilds can run back to back", "15m"),
	"analytics.builder_memory_limit": withOffValue(sizeValidation("512MiB or 2GB"), "", "No limit", "512MiB"),
	"analytics.builder_threads":      withOff(atLeast(1), "Engine default", "4"),
	"analytics.builder_temp_limit":   withOffValue(sizeValidation("1GiB or 10GB"), "", "No limit", "1GiB"),
	"sync.rate_limit_qps":            atLeast(1),
	"log.sql_slow_ms":                withOff(atLeast(1), "Built-in threshold of 100 ms", "100"),

	"vector.embeddings.endpoint": {
		Hint: "HTTP or HTTPS URL without credentials, query, or fragment.", Required: true,
	},
	"vector.embeddings.model":      {Required: true},
	"vector.embeddings.dimension":  atLeast(1),
	"vector.embeddings.batch_size": atLeast(1),
	"vector.embeddings.timeout":    {Hint: "Duration such as 30s or 2m.", Required: true},
	// Loading the config turns a stored 0 into 3, so zero cannot be a
	// persistent off state here; the control shows the effective value.
	"vector.embeddings.max_retries":     atLeast(1),
	"vector.embeddings.max_input_chars": atLeast(1),
	"vector.embeddings.eta_window":      atLeast(1),
	"vector.people.retention_posture":   {Required: true},
	"vector.people.training_posture":    {Required: true},
	"vector.embed.schedule.cron":        cronValidation(false),
	"vector.embed.backstop_interval":    {Hint: "Duration. 0 uses the default. A negative value disables the backstop.", Required: true},
	"vector.multimodal.endpoint": {
		Hint: "Only https://api.voyageai.com/v1 is accepted.", Required: true,
	},
	"vector.multimodal.model":             {Required: true},
	"vector.multimodal.dimension":         withHint(numberRange(1024, 1024), "Voyage visual models require 1024."),
	"vector.multimodal.max_context_chars": atLeast(1),
	"vector.multimodal.schedule.cron":     cronValidation(false),
	"vector.search.rrf_k":                 atLeast(1),
	"vector.search.k_per_signal":          atLeast(1),
	"vector.search.subject_boost":         atLeast(0),
	"vector.search.max_page_size_hybrid":  withOff(atLeast(1), "No limit", "200"),

	"beeper.schedule": cronValidation(false),
	"slack.schedule":  cronValidation(false),
	// A float has no "greater than zero" bound a number control can carry,
	// so the on range starts at 0.1 requests per second.
	"beeper.rate_limit_qps":          withOff(atLeast(0.1), "Provider default", "5"),
	"beeper.media_max_participants":  withOff(atLeast(1), "No limit", "20"),
	"slack.media_max_participants":   withOff(atLeast(1), "No limit", "20"),
	"discord.media_max_participants": withOff(atLeast(1), "No limit", "20"),
	"teams.media_max_participants":   withOff(atLeast(1), "No limit", "20"),
	"beeper.max_media_mb":            mediaSizeOff("Beeper", config.DefaultChatMaxMediaBytes),
	"slack.max_media_mb":             mediaSizeOff("Slack", config.DefaultChatMaxMediaBytes),
	"discord.max_media_mb":           mediaSizeOff("Discord", config.DefaultDiscordMaxMediaBytes),
	"teams.max_media_mb":             mediaSizeOff("Teams", config.DefaultChatMaxMediaBytes),

	"activity.timezone":                {Hint: "UTC or an IANA name such as America/New_York.", Required: true},
	"activity.max_direct_counterparts": numberRange(1, 10_000),
	"activity.batch_size":              numberRange(1, 10_000),
	"activity.schedule":                cronValidation(false),
	"backup.zstd_level":                withOff(numberRange(1, 19), "Encoder default", "3"),
	"people.enrichment.schedule":       cronValidation(true),
	"people.enrichment.batch_size":     atLeast(1),
	"people.enrichment.lease_duration": {Hint: "Duration such as 5m or 1h.", Required: true},
	"integrations.tasks.endpoint":      {Hint: "HTTPS URL, loopback HTTP URL, or a Unix socket you own."},
}

func atLeast(minimum float64) SettingValidation {
	return SettingValidation{Minimum: new(minimum)}
}

func numberRange(minimum, maximum float64) SettingValidation {
	return SettingValidation{Minimum: new(minimum), Maximum: new(maximum)}
}

func durationValidation(examples string) SettingValidation {
	return SettingValidation{Hint: "Duration such as " + examples + ".", Required: true}
}

func sizeValidation(examples string) SettingValidation {
	return SettingValidation{Hint: "Size such as " + examples + "."}
}

const settingFormatCron = "cron"

func cronValidation(required bool) SettingValidation {
	return SettingValidation{Format: settingFormatCron, Required: required}
}

// mediaSizeOff describes a per-attachment size cap whose zero means the
// provider default, derived from the same constant the sync path applies.
func mediaSizeOff(provider string, defaultBytes int64) SettingValidation {
	mib := strconv.FormatInt(defaultBytes>>20, 10)
	return withOff(atLeast(1), provider+" default of "+mib+" MiB", mib)
}

func withHint(validation SettingValidation, hint string) SettingValidation {
	validation.Hint = hint
	return validation
}

// withOff marks zero as the value that switches a numeric setting off. The
// range the validation carried becomes the on range; Minimum widens to
// include zero so clients that ignore Off still accept a stored zero.
func withOff(validation SettingValidation, label, suggest string) SettingValidation {
	validation = withOffValue(validation, "0", label, suggest)
	if validation.Minimum != nil && *validation.Minimum > 0 {
		validation.Off.OnMinimum = validation.Minimum
		validation.Minimum = new(float64(0))
	}
	return validation
}

func withOffValue(validation SettingValidation, value, label, suggest string) SettingValidation {
	validation.Off = &SettingOff{Value: value, Label: label, Suggest: suggest}
	return validation
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
	if validation.Off != nil {
		if off, err := strconv.ParseFloat(validation.Off.Value, 64); err == nil && off == number {
			return nil
		}
		if validation.Off.OnMinimum != nil && number < *validation.Off.OnMinimum {
			return errors.New("below minimum")
		}
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
