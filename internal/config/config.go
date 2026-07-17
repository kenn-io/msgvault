// Package config handles loading and managing msgvault configuration.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/vector"
)

const (
	AnalyticsEngineAuto   = "auto"
	AnalyticsEngineSQL    = "sql"
	AnalyticsEngineDuckDB = "duckdb"

	DaemonAutoRestartNewer  = "newer"
	DaemonAutoRestartNever  = "never"
	DaemonAutoRestartAlways = "always"
)

// ChatConfig holds chat/LLM configuration.
type ChatConfig struct {
	Server     string `toml:"server"`      // Ollama server URL
	Model      string `toml:"model"`       // Model name
	MaxResults int    `toml:"max_results"` // Top-K messages to retrieve
}

// AnalyticsConfig controls daemon-side analytics engine selection.
type AnalyticsConfig struct {
	Engine         string `toml:"engine"`           // auto, sql, or duckdb
	AutoBuildCache bool   `toml:"auto_build_cache"` // Build stale/missing Parquet cache before using DuckDB
}

func (a *AnalyticsConfig) ApplyDefaults() {
	a.Engine = strings.ToLower(strings.TrimSpace(a.Engine))
	if a.Engine == "" {
		a.Engine = AnalyticsEngineAuto
	}
}

func (a *AnalyticsConfig) Validate() error {
	switch a.Engine {
	case AnalyticsEngineAuto, AnalyticsEngineSQL, AnalyticsEngineDuckDB:
		return nil
	default:
		return fmt.Errorf("invalid [analytics] engine %q (want %q, %q, or %q)",
			a.Engine,
			AnalyticsEngineAuto,
			AnalyticsEngineSQL,
			AnalyticsEngineDuckDB)
	}
}

// ServerConfig holds HTTP API server configuration.
type ServerConfig struct {
	APIPort           int           `toml:"api_port"`            // HTTP server port; 0 (the default) auto-selects an open port at daemon startup and clients discover it via the daemon runtime record. Set api_port explicitly for a stable port (e.g. remote/NAS deployments).
	BindAddr          string        `toml:"bind_addr"`           // Bind address (default: 127.0.0.1)
	APIKey            string        `toml:"api_key"`             // API authentication key
	AllowInsecure     bool          `toml:"allow_insecure"`      // Allow unauthenticated non-loopback access
	CORSOrigins       []string      `toml:"cors_origins"`        // Allowed CORS origins (empty = disabled)
	CORSCredentials   bool          `toml:"cors_credentials"`    // Allow credentials in CORS
	CORSMaxAge        int           `toml:"cors_max_age"`        // Preflight cache duration in seconds
	DaemonIdleTimeout time.Duration `toml:"daemon_idle_timeout"` // Background daemon idle timeout (0 disables)
	DaemonAutoRestart string        `toml:"daemon_auto_restart"` // never, newer, or always
}

func (s *ServerConfig) ApplyDefaults() {
	s.DaemonAutoRestart = strings.ToLower(strings.TrimSpace(s.DaemonAutoRestart))
	if s.DaemonAutoRestart == "" {
		s.DaemonAutoRestart = DaemonAutoRestartNewer
	}
}

func (s *ServerConfig) Validate() error {
	switch s.DaemonAutoRestart {
	case DaemonAutoRestartNewer, DaemonAutoRestartNever, DaemonAutoRestartAlways:
		return nil
	default:
		return fmt.Errorf("invalid [server] daemon_auto_restart %q (want %q, %q, or %q)",
			s.DaemonAutoRestart,
			DaemonAutoRestartNewer,
			DaemonAutoRestartNever,
			DaemonAutoRestartAlways)
	}
}

// IsLoopback returns true if the bind address is a loopback address.
// Handles the full 127.0.0.0/8 range, IPv6 ::1, and "localhost".
func (s *ServerConfig) IsLoopback() bool {
	addr := s.BindAddr
	if addr == "" || addr == "localhost" {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// ValidateSecure returns an error if the server is configured insecurely
// without an explicit opt-in via allow_insecure.
func (s *ServerConfig) ValidateSecure() error {
	if !s.IsLoopback() && s.APIKey == "" && !s.AllowInsecure {
		return fmt.Errorf("refusing to start: bind address %q is not loopback and no api_key is set\n\n"+
			"Set [server] api_key in config.toml, or set allow_insecure = true to override", s.BindAddr)
	}
	return nil
}

// AccountSchedule defines sync schedule for a single account.
type AccountSchedule struct {
	Email    string `toml:"email"`    // Gmail account email
	Schedule string `toml:"schedule"` // Cron expression (e.g., "0 2 * * *" for 2am daily)
	Enabled  bool   `toml:"enabled"`  // Whether scheduled sync is active
}

type SynctechSMSConfig struct {
	Sources []SynctechSMSSource `toml:"sources"`
}

type SynctechSMSSource struct {
	Name               string `toml:"name"`
	Enabled            bool   `toml:"enabled"`
	Backend            string `toml:"backend"` // "local" or "drive"
	Path               string `toml:"path"`
	FolderID           string `toml:"folder_id"`
	GoogleAccount      string `toml:"google_account"`
	OwnerPhone         string `toml:"owner_phone"`
	Schedule           string `toml:"schedule"`
	IncludeSMS         bool   `toml:"include_sms"`
	IncludeMMS         bool   `toml:"include_mms"`
	IncludeCalls       bool   `toml:"include_calls"`
	IncludeAttachments bool   `toml:"include_attachments"`
	StableAfter        string `toml:"stable_after"`
	OAuthApp           string `toml:"oauth_app"`
}

// RemoteConfig holds configuration for a remote msgvault server.
// Used by export-token to remember the NAS/server destination.
type RemoteConfig struct {
	URL           string `toml:"url"`            // Remote server URL (e.g., http://nas:8080)
	APIKey        string `toml:"api_key"`        // API key for authentication
	AllowInsecure bool   `toml:"allow_insecure"` // Allow HTTP (insecure) for trusted networks
}

// IdentityConfig holds the user's curated identity addresses.
type IdentityConfig struct {
	Addresses []string `toml:"addresses"`
}

// BackupConfig holds default settings for `msgvault backup` (spec Section
// 10). Repo lets `--repo` be omitted on every invocation; ZstdLevel tunes
// the pack compression level (0 keeps kit/pack's own default).
type BackupConfig struct {
	Repo      string `toml:"repo"`       // Default snapshot repository directory
	ZstdLevel int    `toml:"zstd_level"` // 0 (default) or 1-19
}

// Validate enforces the zstd compression level range: 0 (meaning "use
// kit/pack's default") or 1-19, matching the range the zstd encoder
// actually accepts.
func (b *BackupConfig) Validate() error {
	if b.ZstdLevel == 0 || (b.ZstdLevel >= 1 && b.ZstdLevel <= 19) {
		return nil
	}
	return fmt.Errorf("invalid [backup] zstd_level %d (want 0 or 1-19)", b.ZstdLevel)
}

type Config struct {
	Data        DataConfig         `toml:"data"`
	Log         LogConfig          `toml:"log"`
	OAuth       OAuthConfig        `toml:"oauth"`
	Microsoft   MicrosoftConfig    `toml:"microsoft"`
	Sync        SyncConfig         `toml:"sync"`
	Chat        ChatConfig         `toml:"chat"`
	Server      ServerConfig       `toml:"server"`
	Analytics   AnalyticsConfig    `toml:"analytics"`
	Remote      RemoteConfig       `toml:"remote"`
	Vector      vector.Config      `toml:"vector"`
	Identity    IdentityConfig     `toml:"identity"`
	Accounts    []AccountSchedule  `toml:"accounts"`
	SynctechSMS SynctechSMSConfig  `toml:"synctech_sms"`
	GCal        []GCalSource       `toml:"gcal"`
	Beeper      BeeperConfig       `toml:"beeper"`
	Granola     []GranolaSource    `toml:"granola"`
	Circleback  []CirclebackSource `toml:"circleback"`
	Backup      BackupConfig       `toml:"backup"`

	// Computed paths (not from config file)
	HomeDir    string `toml:"-"`
	configPath string // resolved path to the loaded config file
}

// LogConfig holds logging configuration. File logging is opt-in:
// set enabled = true or dir = "..." to write structured JSON logs
// to disk. Without either, msgvault only writes to stderr (which
// is the default behavior users already expect). The --log-file
// CLI flag also enables file logging for a single run.
type LogConfig struct {
	// Dir is the directory where log files live. Empty means
	// "<data dir>/logs". Setting this implicitly enables file
	// logging.
	Dir string `toml:"dir"`

	// Level overrides the default logging level. Accepted values
	// are "debug", "info", "warn", "error". Empty means "info"
	// (or "debug" when --verbose is passed).
	Level string `toml:"level"`

	// Enabled turns on persistent file logging. When false (the
	// default), the CLI only writes to stderr. Set to true, or
	// set dir, to opt in to durable on-disk logs.
	Enabled bool `toml:"enabled"`

	// SQLSlowMs is the threshold above which any individual SQL
	// query is logged at WARN regardless of the main level.
	// Zero means "use the built-in default" (100 ms). Set to a
	// very large value to effectively disable slow logging.
	SQLSlowMs int64 `toml:"sql_slow_ms"`

	// SQLTrace, when true, logs every SQL query at INFO level
	// with statement text, arg count, duration, and error. This
	// is voluminous — leave off in normal use and flip it on
	// (via config or --log-sql) only when debugging.
	SQLTrace bool `toml:"sql_trace"`
}

// DataConfig holds data storage configuration.
type DataConfig struct {
	DataDir     string `toml:"data_dir"`
	DatabaseURL string `toml:"database_url"`
}

// OAuthApp holds configuration for a named OAuth application.
type OAuthApp struct {
	ClientSecrets     string `toml:"client_secrets"`
	ServiceAccountKey string `toml:"service_account_key"`
}

// OAuthConfig holds OAuth configuration.
type OAuthConfig struct {
	ClientSecrets     string              `toml:"client_secrets"`
	ServiceAccountKey string              `toml:"service_account_key"`
	Apps              map[string]OAuthApp `toml:"apps"`
}

// ClientSecretsFor returns the client secrets path for the given app name.
// Empty name returns the default. Non-empty name looks up Apps[name].
func (o *OAuthConfig) ClientSecretsFor(name string) (string, error) {
	if name == "" {
		if o.ClientSecrets == "" {
			return "", errors.New("OAuth client secrets not configured.\n\n" +
				"Set [oauth] client_secrets in config.toml, or use --oauth-app <name>")
		}
		return o.ClientSecrets, nil
	}
	app, ok := o.Apps[name]
	if !ok {
		return "", fmt.Errorf("OAuth app %q not configured. Add it to config.toml:\n\n"+
			"  [oauth.apps.%s]\n"+
			"  client_secrets = \"/path/to/client_secret.json\"", name, name)
	}
	if app.ClientSecrets == "" {
		return "", fmt.Errorf("OAuth app %q has no client_secrets path configured", name)
	}
	return app.ClientSecrets, nil
}

// ServiceAccountKeyFor returns the service account key path for the given app name.
// Empty name returns the default. Non-empty name looks up Apps[name].
// Returns "" if no service account key is configured for the given app.
func (o *OAuthConfig) ServiceAccountKeyFor(name string) string {
	if name == "" {
		return o.ServiceAccountKey
	}
	if app, ok := o.Apps[name]; ok {
		return app.ServiceAccountKey
	}
	return ""
}

// HasAnyConfig returns true if any OAuth configuration exists
// (default or named apps).
func (o *OAuthConfig) HasAnyConfig() bool {
	if o.ClientSecrets != "" || o.ServiceAccountKey != "" {
		return true
	}
	for _, app := range o.Apps {
		if app.ClientSecrets != "" || app.ServiceAccountKey != "" {
			return true
		}
	}
	return false
}

// MicrosoftConfig holds Microsoft 365 / Azure AD OAuth configuration.
type MicrosoftConfig struct {
	ClientID string `toml:"client_id"`
	TenantID string `toml:"tenant_id"`
}

// EffectiveTenantID returns the tenant ID, defaulting to "common"
// (multi-tenant, works for personal + org accounts).
func (c *MicrosoftConfig) EffectiveTenantID() string {
	if c.TenantID == "" {
		return "common"
	}
	return c.TenantID
}

// SyncConfig holds sync-related configuration.
type SyncConfig struct {
	RateLimitQPS int `toml:"rate_limit_qps"`
}

// DefaultHome returns the default msgvault home directory.
// Respects MSGVAULT_HOME environment variable and expands ~ in its value.
func DefaultHome() string {
	if h := os.Getenv("MSGVAULT_HOME"); h != "" {
		return expandPath(h)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".msgvault"
	}
	return filepath.Join(home, ".msgvault")
}

// NewDefaultConfig returns a configuration with default values.
func NewDefaultConfig() *Config {
	homeDir := DefaultHome()
	cfg := &Config{
		HomeDir: homeDir,
		Data: DataConfig{
			DataDir: homeDir,
		},
		Sync: SyncConfig{
			RateLimitQPS: 5,
		},
		Chat: ChatConfig{
			Server:     "http://localhost:11434",
			Model:      "gpt-oss-128k",
			MaxResults: 20,
		},
		Server: ServerConfig{
			APIPort:           0,
			BindAddr:          "127.0.0.1",
			DaemonIdleTimeout: 20 * time.Minute,
			DaemonAutoRestart: DaemonAutoRestartNewer,
		},
		Analytics: AnalyticsConfig{
			Engine:         AnalyticsEngineAuto,
			AutoBuildCache: true,
		},
		Accounts:    []AccountSchedule{},
		SynctechSMS: SynctechSMSConfig{Sources: []SynctechSMSSource{}},
		GCal:        []GCalSource{},
	}
	cfg.Vector.ApplyDefaults()
	cfg.Server.ApplyDefaults()
	return cfg
}

// Load reads the configuration from the specified file.
// If path is empty, uses the default location (~/.msgvault/config.toml),
// which is optional (missing file returns defaults).
// If path is explicitly provided, the file must exist.
//
// homeDir overrides the home directory (equivalent to MSGVAULT_HOME).
// When set, config.toml is loaded from homeDir unless path is also set.
func Load(path, homeDir string) (*Config, error) {
	explicit := path != ""

	cfg := NewDefaultConfig()

	// --home overrides the default home directory, just like MSGVAULT_HOME.
	if homeDir != "" {
		homeDir = expandPath(homeDir)
		cfg.HomeDir = homeDir
		cfg.Data.DataDir = homeDir
	}

	if !explicit {
		path = filepath.Join(cfg.HomeDir, "config.toml")
	} else {
		// Expand ~ for explicit paths (e.g. --config "~/.msgvault/config.toml"
		// where the shell didn't expand it, or on Windows where ~ is never expanded).
		path = expandPath(path)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if explicit {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		// Default config file is optional
		return cfg, nil
	}

	cfg.configPath = path

	// When --config points to a custom location without --home,
	// derive HomeDir and default DataDir from the config file's parent
	// directory so that tokens, database, attachments, etc. live alongside
	// the config.
	if explicit && homeDir == "" {
		cfg.HomeDir = filepath.Dir(path)
		cfg.Data.DataDir = cfg.HomeDir
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if strings.Contains(err.Error(), "invalid escape") ||
			strings.Contains(err.Error(), "hexadecimal digits after") {
			return nil, fmt.Errorf("decode config: %w -- hint: Windows paths in TOML must use "+
				"forward slashes (C:/Games/msgvault) or single quotes ('C:\\Games\\msgvault')", err)
		}
		return nil, fmt.Errorf("decode config: %w", err)
	}

	// Expand ~ in paths
	cfg.Data.DataDir = expandPath(cfg.Data.DataDir)
	cfg.Log.Dir = expandPath(cfg.Log.Dir)
	cfg.OAuth.ClientSecrets = expandPath(cfg.OAuth.ClientSecrets)
	cfg.OAuth.ServiceAccountKey = expandPath(cfg.OAuth.ServiceAccountKey)
	cfg.Vector.DBPath = expandPath(cfg.Vector.DBPath)
	cfg.Backup.Repo = expandPath(cfg.Backup.Repo)
	for name, app := range cfg.OAuth.Apps {
		app.ClientSecrets = expandPath(app.ClientSecrets)
		app.ServiceAccountKey = expandPath(app.ServiceAccountKey)
		cfg.OAuth.Apps[name] = app
	}

	// When --config is used, resolve relative paths against the config file's
	// directory so behavior doesn't depend on the working directory.
	if explicit {
		cfg.Data.DataDir = resolveRelative(cfg.Data.DataDir, cfg.HomeDir)
		cfg.Log.Dir = resolveRelative(cfg.Log.Dir, cfg.HomeDir)
		cfg.OAuth.ClientSecrets = resolveRelative(cfg.OAuth.ClientSecrets, cfg.HomeDir)
		cfg.OAuth.ServiceAccountKey = resolveRelative(cfg.OAuth.ServiceAccountKey, cfg.HomeDir)
		cfg.Vector.DBPath = resolveRelative(cfg.Vector.DBPath, cfg.HomeDir)
		cfg.Backup.Repo = resolveRelative(cfg.Backup.Repo, cfg.HomeDir)
		for name, app := range cfg.OAuth.Apps {
			app.ClientSecrets = resolveRelative(app.ClientSecrets, cfg.HomeDir)
			app.ServiceAccountKey = resolveRelative(app.ServiceAccountKey, cfg.HomeDir)
			cfg.OAuth.Apps[name] = app
		}
	}

	// Re-apply numeric defaults over any zero-valued vector fields that
	// survived decode (e.g. `max_retries = 0` or an omitted timeout).
	// Preprocess booleans are *bool so pointer-nil still means "default";
	// an explicit false in the file stays false.
	cfg.Vector.ApplyDefaults()
	cfg.Server.ApplyDefaults()
	if err := cfg.Server.Validate(); err != nil {
		return nil, err
	}
	cfg.Analytics.ApplyDefaults()
	if err := cfg.Analytics.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.Backup.Validate(); err != nil {
		return nil, err
	}
	cfg.applySynctechSMSDefaults()
	cfg.applyGCalDefaults()
	cfg.applyMeetingSourceDefaults()
	if err := cfg.validateMeetingSources(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) applySynctechSMSDefaults() {
	for i := range c.SynctechSMS.Sources {
		src := &c.SynctechSMS.Sources[i]
		if src.Backend == "" {
			src.Backend = "local"
		}
		if !src.IncludeSMS && !src.IncludeMMS && !src.IncludeCalls {
			src.IncludeSMS = true
			src.IncludeMMS = true
			src.IncludeCalls = true
		}
		if src.StableAfter == "" {
			src.StableAfter = "10m"
		}
	}
}

// DatabaseDSN returns the database connection string or file path.
func (c *Config) DatabaseDSN() string {
	if c.Data.DatabaseURL != "" {
		return c.Data.DatabaseURL
	}
	return filepath.Join(c.Data.DataDir, "msgvault.db")
}

// DatabasePath returns the on-disk SQLite filesystem path for backup
// operations (VACUUM INTO, copies). It accepts the plain filesystem
// path and the SQLite "file:" URI form, decoding any percent-encoded
// bytes (e.g. "file:/var/lib/my%20vault.db" -> "/var/lib/my vault.db")
// and dropping the URI query string. Returns an error for non-file
// DSNs (e.g. "postgres://..."), which the SQLite-only backup helpers
// cannot operate on.
func (c *Config) DatabasePath() (string, error) {
	dsn := c.DatabaseDSN()
	if strings.HasPrefix(dsn, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse file: URI %q: %w", dsn, err)
		}
		// SQLite accepts both file:/abs/path (Path) and file:rel/path
		// (Opaque) shapes. url.Parse decodes percent-encoding for Path
		// but NOT for Opaque, so a relative file: URI like
		// "file:my%20vault.db" leaves the encoding intact in u.Opaque
		// and the on-disk filename never matches. PathUnescape handles
		// the relative-form case explicitly.
		path := u.Path
		if path == "" {
			decoded, err := url.PathUnescape(u.Opaque)
			if err != nil {
				return "", fmt.Errorf("decode file: URI opaque part %q: %w", u.Opaque, err)
			}
			path = decoded
		}
		if path == "" {
			return "", fmt.Errorf("empty file: URI in database DSN: %q", dsn)
		}
		return path, nil
	}
	if strings.Contains(dsn, "://") {
		// postgres://, mysql://, etc. — non-file DSN; backup is
		// SQLite-specific and the caller can't operate on these.
		return "", fmt.Errorf(
			"backup operations require a SQLite filesystem DSN; "+
				"got non-file DSN %q (set [data].database_url to a "+
				"plain filesystem path or file: URI)", dsn,
		)
	}
	return dsn, nil
}

// AttachmentsDir returns the path to the attachments directory.
func (c *Config) AttachmentsDir() string {
	return filepath.Join(c.Data.DataDir, "attachments")
}

// TokensDir returns the path to the OAuth tokens directory.
func (c *Config) TokensDir() string {
	return filepath.Join(c.Data.DataDir, "tokens")
}

// AnalyticsDir returns the path to the Parquet analytics directory.
func (c *Config) AnalyticsDir() string {
	return filepath.Join(c.Data.DataDir, "analytics")
}

// LogsDir returns the path to the logs directory. Uses [log].dir
// from config when set; otherwise falls back to <data_dir>/logs.
func (c *Config) LogsDir() string {
	if c.Log.Dir != "" {
		return c.Log.Dir
	}
	return filepath.Join(c.Data.DataDir, "logs")
}

// EnsureHomeDir creates the msgvault home directory if it doesn't exist.
func (c *Config) EnsureHomeDir() error {
	return fileutil.SecureMkdirAll(c.HomeDir, 0700)
}

// ConfigFilePath returns the path to the config file.
// If a config was loaded (including via --config), returns the actual path used.
// Otherwise returns the default location based on HomeDir.
func (c *Config) ConfigFilePath() string {
	if c.configPath != "" {
		return c.configPath
	}
	return filepath.Join(c.HomeDir, "config.toml")
}

// Save writes the current configuration to disk atomically.
// Uses temp file + rename to prevent partial writes on crash.
// Enforces 0600 permissions regardless of existing file mode.
func (c *Config) Save() error {
	path := c.ConfigFilePath()

	// Resolve symlinks so atomic rename replaces the target, not
	// the symlink itself. EvalSymlinks fails on dangling symlinks
	// (target doesn't exist yet), so fall back to Readlink.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if target, lErr := os.Readlink(path); lErr == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}

	// Ensure home directory exists
	if err := c.EnsureHomeDir(); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmp.Name()

	// Clean up temp file on any failure path
	success := false
	defer func() {
		if !success {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("set config file permissions: %w", err)
	}

	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync config file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename config file: %w", err)
	}

	success = true
	return nil
}

// ScheduledAccounts returns accounts with scheduling enabled.
func (c *Config) ScheduledAccounts() []AccountSchedule {
	var scheduled []AccountSchedule
	for _, acc := range c.Accounts {
		if acc.Enabled && acc.Schedule != "" {
			scheduled = append(scheduled, acc)
		}
	}
	return scheduled
}

// GetAccountSchedule returns the schedule for a specific account email.
// Returns nil if the account is not configured for scheduling.
// The returned value is a copy, so mutations won't affect the config.
func (c *Config) GetAccountSchedule(email string) *AccountSchedule {
	for i := range c.Accounts {
		if c.Accounts[i].Email == email {
			acc := c.Accounts[i]
			return &acc
		}
	}
	return nil
}

// BeeperConfig configures the Beeper Desktop archive source ([beeper] table).
// A single block, not a slice: the Beeper Desktop API is loopback-only, so
// there is exactly one instance per machine and the daemon must be co-located
// with it.
type BeeperConfig struct {
	// URL is the Beeper Desktop API base URL. Empty means the default
	// loopback address (http://localhost:23373).
	URL string `toml:"url"`
	// Enabled gates the daemon scheduler job.
	Enabled bool `toml:"enabled"`
	// Schedule is a 5-field cron expression; empty = not daemon-scheduled.
	Schedule string `toml:"schedule"`
	// Accounts is an accountID include filter (empty = sync all accounts).
	Accounts []string `toml:"accounts"`
	// ExcludeAccounts skips specific accountIDs — e.g. ["whatsapp"] when the
	// native import-whatsapp path already archives that network.
	ExcludeAccounts []string `toml:"exclude_accounts"`
	// RateLimitQPS bounds request rate against the local Beeper Desktop app
	// (0 = default 20).
	RateLimitQPS float64 `toml:"rate_limit_qps"`
	// Media toggles attachment download (nil/absent = enabled).
	Media *bool `toml:"media"`
	// MaxMediaMB caps individual attachment downloads in MiB (0 = 100).
	MaxMediaMB int `toml:"max_media_mb"`
}

// MediaEnabled reports whether attachment download is on (default true).
func (b BeeperConfig) MediaEnabled() bool {
	return b.Media == nil || *b.Media
}

// MaxMediaBytes returns the per-attachment download cap in bytes.
func (b BeeperConfig) MaxMediaBytes() int64 {
	if b.MaxMediaMB > 0 {
		return int64(b.MaxMediaMB) << 20
	}
	return 100 << 20
}

// AccountIncluded reports whether a Beeper accountID passes the
// include/exclude filters.
func (b BeeperConfig) AccountIncluded(accountID string) bool {
	if slices.Contains(b.ExcludeAccounts, accountID) {
		return false
	}
	if len(b.Accounts) == 0 {
		return true
	}
	return slices.Contains(b.Accounts, accountID)
}

// GCalSource is one configured Google Calendar sync target. Each entry is a
// top-level [[gcal]] table.
type GCalSource struct {
	Name      string   `toml:"name"`      // identifier for sync-calendar <name>; defaults to Email
	Email     string   `toml:"email"`     // the OAuth account = token key
	OAuthApp  string   `toml:"oauth_app"` // optional named OAuth app
	Calendars []string `toml:"calendars"` // optional calendarId filter; empty = owner+writer
	Schedule  string   `toml:"schedule"`  // 5-field cron; empty = not daemon-scheduled
	Enabled   bool     `toml:"enabled"`
}

// applyGCalDefaults normalizes [[gcal]] entries: a source with no name takes its
// email, so `sync-calendar <email>` resolves it.
func (c *Config) applyGCalDefaults() {
	for i := range c.GCal {
		if c.GCal[i].Name == "" {
			c.GCal[i].Name = c.GCal[i].Email
		}
	}
}

// GetGCalSource returns the configured calendar source matching name or email
// (case-insensitive), or nil.
func (c *Config) GetGCalSource(name string) *GCalSource {
	for _, src := range c.GCal {
		if strings.EqualFold(src.Name, name) || strings.EqualFold(src.Email, name) {
			cp := src
			return &cp
		}
	}
	return nil
}

// ScheduledGCalSources returns enabled calendar sources with a cron schedule.
func (c *Config) ScheduledGCalSources() []GCalSource {
	var out []GCalSource
	for _, src := range c.GCal {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}

// GranolaSource is one configured Granola account. Each entry is a top-level
// [[granola]] table.
type GranolaSource struct {
	Identifier   string `toml:"identifier"`    // stable source label for add-/sync-granola; defaults to "default" for a single entry
	AccountEmail string `toml:"account_email"` // primary account identity
	APIKey       string `toml:"api_key"`       // grn_… key from the desktop app's settings (Business plan)
	Schedule     string `toml:"schedule"`      // 5-field cron; empty = not daemon-scheduled
	Enabled      bool   `toml:"enabled"`
}

// EffectiveAccountEmail returns the normalized primary identity configured
// for this source.
func (s GranolaSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("granola", s.Identifier, s.AccountEmail)
}

// CirclebackSource is one configured Circleback account. Each entry is a
// top-level [[circleback]] table. Authentication is OAuth (add-circleback);
// no secret lives in the config file.
type CirclebackSource struct {
	Identifier   string `toml:"identifier"`    // stable source label for add-/sync-circleback; defaults to "default" for a single entry
	AccountEmail string `toml:"account_email"` // primary account identity
	Endpoint     string `toml:"endpoint"`      // MCP endpoint override; empty = production
	Schedule     string `toml:"schedule"`      // 5-field cron; empty = not daemon-scheduled
	Enabled      bool   `toml:"enabled"`
}

// EffectiveAccountEmail returns the normalized primary identity configured
// for this source.
func (s CirclebackSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("circleback", s.Identifier, s.AccountEmail)
}

func effectiveMeetingAccountEmail(kind, identifier, configured string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if strings.TrimSpace(configured) != "" {
		if email, ok := normalizedMeetingAccountEmail(configured); ok {
			return email, nil
		}
		return "", fmt.Errorf("[[%s]] identifier %q has invalid account_email %q; preserve the identifier and set account_email to the account's email address",
			kind, identifier, configured)
	}
	return "", fmt.Errorf("[[%s]] identifier %q requires account_email; preserve identifier = %q and add account_email = \"you@example.com\"",
		kind, identifier, identifier)
}

func normalizedMeetingAccountEmail(value string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(value))
	if email == "" || strings.ContainsAny(email, " \t\r\n") {
		return "", false
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, email) {
		return "", false
	}
	return email, true
}

// applyMeetingSourceDefaults normalizes [[granola]]/[[circleback]] entries: a
// single entry with no identifier becomes "default" so the CLI argument can
// be omitted in the common one-account case.
func (c *Config) applyMeetingSourceDefaults() {
	if len(c.Granola) == 1 && c.Granola[0].Identifier == "" {
		c.Granola[0].Identifier = "default"
	}
	if len(c.Circleback) == 1 && c.Circleback[0].Identifier == "" {
		c.Circleback[0].Identifier = "default"
	}
}

// validateMeetingSources rejects [[granola]]/[[circleback]] lists with empty
// or duplicate identifiers — the identifier keys the source row and token
// file, so a collision would silently merge two accounts.
func (c *Config) validateMeetingSources() error {
	check := func(kind string, ids []string) error {
		seen := map[string]bool{}
		for _, id := range ids {
			key := strings.ToLower(id)
			if key == "" {
				return fmt.Errorf("[[%s]]: every entry needs an identifier when more than one is configured", kind)
			}
			if seen[key] {
				return fmt.Errorf("[[%s]]: duplicate identifier %q", kind, id)
			}
			seen[key] = true
		}
		return nil
	}
	granolaIDs := make([]string, len(c.Granola))
	for i, s := range c.Granola {
		granolaIDs[i] = s.Identifier
	}
	if err := check("granola", granolaIDs); err != nil {
		return err
	}
	for i := range c.Granola {
		email, err := c.Granola[i].EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if strings.TrimSpace(c.Granola[i].AccountEmail) != "" {
			c.Granola[i].AccountEmail = email
		}
	}
	circlebackIDs := make([]string, len(c.Circleback))
	for i, s := range c.Circleback {
		circlebackIDs[i] = s.Identifier
	}
	if err := check("circleback", circlebackIDs); err != nil {
		return err
	}
	for i := range c.Circleback {
		email, err := c.Circleback[i].EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if strings.TrimSpace(c.Circleback[i].AccountEmail) != "" {
			c.Circleback[i].AccountEmail = email
		}
	}
	return nil
}

// GetGranolaSource returns the configured Granola source matching identifier
// (case-insensitive), or nil.
func (c *Config) GetGranolaSource(identifier string) *GranolaSource {
	for _, src := range c.Granola {
		if strings.EqualFold(src.Identifier, identifier) {
			cp := src
			return &cp
		}
	}
	return nil
}

// ScheduledGranolaSources returns enabled Granola sources with a cron schedule.
func (c *Config) ScheduledGranolaSources() []GranolaSource {
	var out []GranolaSource
	for _, src := range c.Granola {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}

// GetCirclebackSource returns the configured Circleback source matching
// identifier (case-insensitive), or nil.
func (c *Config) GetCirclebackSource(identifier string) *CirclebackSource {
	for _, src := range c.Circleback {
		if strings.EqualFold(src.Identifier, identifier) {
			cp := src
			return &cp
		}
	}
	return nil
}

// ScheduledCirclebackSources returns enabled Circleback sources with a cron schedule.
func (c *Config) ScheduledCirclebackSources() []CirclebackSource {
	var out []CirclebackSource
	for _, src := range c.Circleback {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}

func (c *Config) GetSynctechSMSSource(name string) *SynctechSMSSource {
	for _, src := range c.SynctechSMS.Sources {
		if strings.EqualFold(src.Name, name) {
			cp := src
			return &cp
		}
	}
	return nil
}

func (c *Config) ScheduledSynctechSMSSources() []SynctechSMSSource {
	var out []SynctechSMSSource
	for _, src := range c.SynctechSMS.Sources {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}

// MkTempDir creates a temporary directory with fallback logic for restricted
// environments (e.g. Windows where %TEMP% may be inaccessible due to
// permissions, antivirus, or group policy).
//
// It tries the following locations in order:
//  1. Each directory in preferredDirs (if any)
//  2. The system default temp directory (os.TempDir())
//  3. A "tmp" subdirectory under the msgvault home directory (~/.msgvault/tmp/)
//
// The first successful location is used. If all locations fail, the error
// from the system temp dir attempt is returned along with the final fallback error.
func MkTempDir(pattern string, preferredDirs ...string) (string, error) {
	// Try preferred directories first
	for _, base := range preferredDirs {
		if base == "" {
			continue
		}
		dir, err := os.MkdirTemp(base, pattern)
		if err == nil {
			secureTempDir(dir)
			return dir, nil
		}
	}

	// Try system temp dir
	dir, sysErr := os.MkdirTemp("", pattern)
	if sysErr == nil {
		secureTempDir(dir)
		return dir, nil
	}

	// Fallback: use ~/.msgvault/tmp/
	fallbackBase := filepath.Join(DefaultHome(), "tmp")
	if err := fileutil.SecureMkdirAll(fallbackBase, 0700); err != nil {
		return "", fmt.Errorf("create temp dir: %w (fallback also failed: %w)", sysErr, err)
	}
	dir, err := os.MkdirTemp(fallbackBase, pattern)
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w (fallback also failed: %w)", sysErr, err)
	}
	secureTempDir(dir)
	return dir, nil
}

// secureTempDir applies owner-only permissions to a temp directory created by
// os.MkdirTemp, which uses default permissions. On Windows, this also sets an
// owner-only DACL. Failures are logged but non-fatal.
func secureTempDir(dir string) {
	if err := fileutil.SecureChmod(dir, 0700); err != nil {
		slog.Warn("failed to secure temp directory permissions", "path", dir, "err", err)
	}
}

// resolveRelative makes a relative path absolute by joining it with base.
// Absolute paths and empty strings are returned unchanged.
func resolveRelative(path, base string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

// expandPath expands ~ to the user's home directory.
// Only expands paths that are exactly "~" or start with "~/".
// It also strips surrounding single or double quotes, which Windows CMD
// passes through literally (unlike Unix shells which strip them).
func expandPath(path string) string {
	if path == "" {
		return path
	}
	// Strip surrounding quotes left by Windows CMD (e.g. --home 'C:\Users\foo').
	// Only on Windows — Unix shells strip quotes before the process sees them,
	// and literal quote characters in Unix paths are valid (if unusual).
	if runtime.GOOS == "windows" && len(path) >= 2 &&
		((path[0] == '\'' && path[len(path)-1] == '\'') ||
			(path[0] == '"' && path[len(path)-1] == '"')) {
		path = path[1 : len(path)-1]
	}
	if path == "~" || strings.HasPrefix(path, "~"+string(os.PathSeparator)) || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		if path == "~" {
			return home
		}
		// Trim leading slashes from the suffix to handle cases like "~//foo"
		suffix := path[2:]
		for len(suffix) > 0 && (suffix[0] == '/' || suffix[0] == os.PathSeparator) {
			suffix = suffix[1:]
		}
		return filepath.Join(home, suffix)
	}
	return path
}
