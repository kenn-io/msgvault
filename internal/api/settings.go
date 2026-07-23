package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
)

const settingsPath = "/api/v1/settings"

// SecretSettingState is the only representation of a secret returned to a
// browser. The configured value never crosses the API boundary.
type SecretSettingState struct {
	Configured bool `json:"configured"`
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

// Setting describes one browser-managed allowlisted config value. ReadOnly
// marks settings that are visible over HTTP but can only be changed by
// editing config.toml on the daemon host; PATCH rejects updates to them.
type Setting struct {
	Key             string              `json:"key"`
	Group           string              `json:"group"`
	Kind            string              `json:"kind"`
	Value           *SettingValue       `json:"value,omitempty"`
	Secret          *SecretSettingState `json:"secret,omitempty"`
	Options         []string            `json:"options,omitempty"`
	RestartRequired bool                `json:"restart_required"`
	Testable        bool                `json:"testable,omitempty"`
	ReadOnly        bool                `json:"read_only,omitempty"`
}

type SettingsResponse struct {
	Settings       []Setting `json:"settings"`
	PendingRestart bool      `json:"pending_restart"`
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
	Updates              []SettingUpdate `json:"updates" minItems:"1" nullable:"false"`
	ConfirmAPIKeyRestart bool            `json:"confirm_api_key_restart,omitempty"`
}

type settingDefinition struct {
	key      string
	group    string
	kind     string
	options  []string
	testable bool
	// localOnly settings are visible over HTTP but can only be changed by
	// editing config.toml on the daemon host. Used for values that select
	// daemon-side resources (such as environment variable names) which a
	// remote session must never control.
	localOnly bool
	secret    func(*config.Config) bool
	read      func(*config.Config) any
}

var settingsCatalog = []settingDefinition{
	stringSetting("web.default_search_mode", "browser", []string{exploreSearchModeFullText, exploreSearchModeSemantic, exploreSearchModeHybrid}, func(c *config.Config) string { return c.Web.DefaultSearchMode }),
	stringSetting("web.theme", "browser", []string{"system", "light", "dark"}, func(c *config.Config) string { return c.Web.Theme }),
	stringSetting("web.density", "browser", []string{"compact", "comfortable"}, func(c *config.Config) string { return c.Web.Density }),
	stringSetting("server.bind_addr", "server", nil, func(c *config.Config) string { return c.Server.BindAddr }),
	intSetting("server.api_port", "server", func(c *config.Config) int { return c.Server.APIPort }),
	secretSetting("server.api_key", "server", func(c *config.Config) bool { return c.Server.APIKey != "" }),
	boolSetting("server.allow_insecure", "server", func(c *config.Config) bool { return c.Server.AllowInsecure }),
	stringArraySetting("server.trusted_proxies", "server", func(c *config.Config) []string { return c.Server.TrustedProxies }),
	stringSetting("analytics.engine", "archive", []string{"auto", "sql", "duckdb"}, func(c *config.Config) string { return c.Analytics.Engine }),
	boolSetting("analytics.auto_build_cache", "archive", func(c *config.Config) bool { return c.Analytics.AutoBuildCache }),
	boolSetting("vector.enabled", "search", func(c *config.Config) bool { return c.Vector.Enabled }),
	stringSetting("vector.backend", "search", []string{"sqlite-vec", "pgvector"}, func(c *config.Config) string { return c.Vector.Backend }),
	stringSetting("vector.db_path", "search", nil, func(c *config.Config) string { return c.Vector.DBPath }),
	boolSetting("vector.skip_extension_create", "search", func(c *config.Config) bool { return c.Vector.SkipExtensionCreate }),
	testableStringSetting("vector.embeddings.endpoint", "search", func(c *config.Config) string { return c.Vector.Embeddings.Endpoint }),
	localOnlyStringSetting("vector.embeddings.api_key_env", "search", func(c *config.Config) string { return c.Vector.Embeddings.APIKeyEnv }),
	stringSetting("vector.embeddings.model", "search", nil, func(c *config.Config) string { return c.Vector.Embeddings.Model }),
	intSetting("vector.embeddings.dimension", "search", func(c *config.Config) int { return c.Vector.Embeddings.Dimension }),
	intSetting("vector.embeddings.batch_size", "search", func(c *config.Config) int { return c.Vector.Embeddings.BatchSize }),
	intSetting("vector.embeddings.max_retries", "search", func(c *config.Config) int { return c.Vector.Embeddings.MaxRetries }),
	intSetting("vector.embeddings.max_input_chars", "search", func(c *config.Config) int { return c.Vector.Embeddings.MaxInputChars }),
	intSetting("vector.embeddings.eta_window", "search", func(c *config.Config) int { return c.Vector.Embeddings.ETAWindow }),
	stringSetting("vector.embed.schedule.cron", "search", nil, func(c *config.Config) string { return c.Vector.Embed.Schedule.Cron }),
	boolSetting("vector.embed.schedule.run_after_sync", "search", func(c *config.Config) bool { return c.Vector.Embed.Schedule.RunAfterSync }),
	stringArraySetting("vector.embed.scope.message_types", "search", func(c *config.Config) []string { return c.Vector.Embed.Scope.MessageTypes }),
	intSetting("vector.search.rrf_k", "search", func(c *config.Config) int { return c.Vector.Search.RRFK }),
	intSetting("vector.search.k_per_signal", "search", func(c *config.Config) int { return c.Vector.Search.KPerSignal }),
	numberSetting("vector.search.subject_boost", "search", func(c *config.Config) float64 { return c.Vector.Search.SubjectBoost }),
	boolSetting("beeper.enabled", "sources", func(c *config.Config) bool { return c.Beeper.Enabled }),
	stringSetting("beeper.schedule", "sources", nil, func(c *config.Config) string { return c.Beeper.Schedule }),
	boolSetting("integrations.tasks.enabled", "integrations", func(c *config.Config) bool { return c.Integrations.Tasks.Enabled }),
	testableStringSetting("integrations.tasks.endpoint", "integrations", func(c *config.Config) string { return c.Integrations.Tasks.Endpoint }),
	secretSetting("integrations.tasks.api_key", "integrations", func(c *config.Config) bool { return c.Integrations.Tasks.APIKey != "" }),
	stringSetting("integrations.tasks.default_project", "integrations", nil, func(c *config.Config) string { return c.Integrations.Tasks.DefaultProject }),
}

func (s *Server) registerSettingsRoutes(api huma.API) {
	get := rawAPIV1Operation("getSettings", http.MethodGet, "/settings", "Get browser-managed settings")
	get.Responses = jsonResponsesFor[SettingsResponse](api)
	addSettingsETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, get, s.handleGetSettings)

	patch := rawAPIV1Operation("patchSettings", http.MethodPatch, "/settings", "Update browser-managed settings")
	patch.Parameters = append(patch.Parameters, &huma.Param{
		Name:        "If-Match",
		In:          "header",
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
	registerRawHumaRoute(api, patch, s.handlePatchSettings)
}

func addSettingsETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{
		"ETag": {
			Description: "Strong content hash for optimistic concurrency",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
}

func stringSetting(key, group string, options []string, read func(*config.Config) string) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "string", options: options, read: func(c *config.Config) any { return read(c) }}
}

func testableStringSetting(key, group string, read func(*config.Config) string) settingDefinition {
	definition := stringSetting(key, group, nil, read)
	definition.testable = true
	return definition
}

func localOnlyStringSetting(key, group string, read func(*config.Config) string) settingDefinition {
	definition := stringSetting(key, group, nil, read)
	definition.localOnly = true
	return definition
}

func intSetting(key, group string, read func(*config.Config) int) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "integer", read: func(c *config.Config) any { return read(c) }}
}

func numberSetting(key, group string, read func(*config.Config) float64) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "number", read: func(c *config.Config) any { return read(c) }}
}

func boolSetting(key, group string, read func(*config.Config) bool) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "boolean", read: func(c *config.Config) any { return read(c) }}
}

func stringArraySetting(key, group string, read func(*config.Config) []string) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "string_array", read: func(c *config.Config) any { return read(c) }}
}

func secretSetting(key, group string, configured func(*config.Config) bool) settingDefinition {
	return settingDefinition{key: key, group: group, kind: "secret", secret: configured}
}

func settingsDefinitionByKey() map[string]settingDefinition {
	result := make(map[string]settingDefinition, len(settingsCatalog))
	for _, definition := range settingsCatalog {
		result[definition.key] = definition
	}
	return result
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	snapshot, cfg, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, buildSettingsResponse(cfg, s.settingsPendingRestart.Load()))
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	ifMatches := r.Header.Values("If-Match")
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
		writeError(w, http.StatusInternalServerError, "settings_read_failed", err.Error())
		return
	}
	edits, changesAPIKey, err := settingsEdits(current, request.Updates)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if changesAPIKey && !request.ConfirmAPIKeyRestart {
		writeError(w, http.StatusBadRequest, "api_key_restart_confirmation_required",
			"Changing the API key requires confirmation because it takes effect after restart")
		return
	}

	editor := s.settingsConfigEditor
	if editor == nil {
		editor = config.EditConfigFile
	}
	snapshot, err := editor(s.cfg.ConfigFilePath(), ifMatches[0], edits)
	if err != nil {
		if errors.Is(err, config.ErrConfigChanged) {
			s.settingsPendingRestart.Store(true)
		}
		switch {
		case errors.Is(err, config.ErrConfigChanged):
			writeError(w, http.StatusInternalServerError, "settings_write_failed",
				"Settings changed, but the write did not complete cleanly; restart is required")
		case errors.Is(err, config.ErrConfigConflict):
			writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		case errors.Is(err, config.ErrAmbiguousConfigTarget), errors.Is(err, config.ErrUnsafeConfigTarget):
			writeError(w, http.StatusConflict, "settings_edit_rejected", err.Error())
		case errors.Is(err, config.ErrInvalidConfigCandidate):
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "settings_write_failed", "Could not write settings")
		}
		return
	}
	// A nil editor error means the candidate is already the committed config.
	// Record that fact before decoding the response snapshot so a subsequent
	// load failure cannot make the daemon report a false non-pending state.
	s.settingsPendingRestart.Store(true)
	loaded, err := config.LoadConfigFile(snapshot, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, buildSettingsResponse(loaded, true))
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

func buildSettingsResponse(cfg *config.Config, pendingRestart bool) SettingsResponse {
	settings := make([]Setting, 0, len(settingsCatalog))
	for _, definition := range settingsCatalog {
		setting := Setting{
			Key:             definition.key,
			Group:           definition.group,
			Kind:            definition.kind,
			Options:         definition.options,
			RestartRequired: true,
			Testable:        definition.testable,
			ReadOnly:        definition.localOnly,
		}
		if definition.secret != nil {
			setting.Secret = &SecretSettingState{Configured: definition.secret(cfg)}
		} else {
			setting.Value = settingValue(definition.kind, definition.read(cfg))
		}
		settings = append(settings, setting)
	}
	return SettingsResponse{Settings: settings, PendingRestart: pendingRestart}
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
	{
		endpointKey:     "vector.embeddings.endpoint",
		credentialKey:   "vector.embeddings.api_key_env",
		currentEndpoint: func(c *config.Config) string { return c.Vector.Embeddings.Endpoint },
		credentialSet:   func(c *config.Config) bool { return c.Vector.Embeddings.APIKeyEnv != "" },
	},
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
	changesAPIKey := false
	for _, update := range updates {
		definition, ok := definitions[update.Key]
		if !ok {
			return nil, false, fmt.Errorf("setting %q is not browser-managed", update.Key)
		}
		if definition.localOnly {
			return nil, false, fmt.Errorf(
				"setting %q names an environment variable on the machine running msgvault; edit config.toml on that machine to change it",
				update.Key)
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
		if update.Key == "server.api_key" {
			changesAPIKey = true
		}
		edits = append(edits, config.Edit{Key: update.Key, Value: value})
	}
	edits = append(edits, credentialSeveranceEdits(current, edits)...)
	return edits, changesAPIKey, nil
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
