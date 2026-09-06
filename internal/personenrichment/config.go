package personenrichment

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	ProviderExa       = "exa"
	ProviderSixtyfour = "sixtyfour"

	defaultExaEndpoint       = "https://api.exa.ai/search"
	defaultSixtyfourEndpoint = "https://api.sixtyfour.ai/people-intelligence-async"
	defaultSixtyfourPoll     = "https://api.sixtyfour.ai/job-status"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	providerNamePattern    = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
)

// Config owns orchestration settings and a set of independently consented
// provider policies. Disabled is the safe default.
//
//nolint:recvcheck // defaults mutate while validation reads the resulting value.
type Config struct {
	Enabled           bool             `toml:"enabled"`
	Schedule          string           `toml:"schedule"`
	BatchSize         int              `toml:"batch_size"`
	LeaseDuration     time.Duration    `toml:"lease_duration"`
	SuppressionKeyEnv string           `toml:"suppression_key_env"`
	Providers         []ProviderConfig `toml:"providers"`
}

// ProviderConfig contains runtime settings and the exact outbound-data policy.
// Credential values are supplied at runtime and never enter this structure.
//
//nolint:recvcheck // defaults mutate while validation/profile construction read.
type ProviderConfig struct {
	Name                            string            `toml:"name"`
	Kind                            string            `toml:"kind"`
	Enabled                         bool              `toml:"enabled"`
	Endpoint                        string            `toml:"endpoint"`
	APIKeyEnv                       string            `toml:"api_key_env"`
	Mode                            string            `toml:"mode"`
	PollEndpoint                    string            `toml:"poll_endpoint"`
	Tier                            string            `toml:"tier"`
	NumResults                      int               `toml:"num_results"`
	AllowedIdentifiers              []IdentifierClass `toml:"allowed_identifiers"`
	TargetKeys                      []string          `toml:"target_keys"`
	AllowSensitiveTargets           bool              `toml:"allow_sensitive_targets"`
	RetentionPosture                string            `toml:"retention_posture"`
	TrainingPosture                 string            `toml:"training_posture"`
	RefreshInterval                 time.Duration     `toml:"refresh_interval"`
	RequestTimeout                  time.Duration     `toml:"request_timeout"`
	PollInterval                    time.Duration     `toml:"poll_interval"`
	MaxJobAge                       time.Duration     `toml:"max_job_age"`
	MaxRetries                      int               `toml:"max_retries"`
	MaxRequestsPerRun               int64             `toml:"max_requests_per_run"`
	MaxRequestsPerDay               int64             `toml:"max_requests_per_day"`
	MaxCostUSDMicrosPerPersonPerDay int64             `toml:"max_cost_usd_micros_per_person_per_day"`
	MaxCostUSDMicrosPerRun          int64             `toml:"max_cost_usd_micros_per_run"`
	MaxCostUSDMicrosPerDay          int64             `toml:"max_cost_usd_micros_per_day"`
}

func (c *Config) ApplyDefaults() {
	if c.Schedule == "" {
		c.Schedule = "*/15 * * * *"
	}
	if c.BatchSize == 0 {
		c.BatchSize = 25
	}
	if c.LeaseDuration == 0 {
		c.LeaseDuration = 5 * time.Minute
	}
	for i := range c.Providers {
		c.Providers[i].ApplyDefaults()
	}
}

func (c Config) Validate() error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(c.Schedule); err != nil {
		return fmt.Errorf("invalid [people.enrichment] schedule %q: %w", c.Schedule, err)
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("invalid [people.enrichment] batch_size %d: must be positive", c.BatchSize)
	}
	if c.LeaseDuration <= 0 {
		return fmt.Errorf("invalid [people.enrichment] lease_duration %s: must be positive", c.LeaseDuration)
	}
	if c.SuppressionKeyEnv != "" && !environmentNamePattern.MatchString(c.SuppressionKeyEnv) {
		return fmt.Errorf("invalid [people.enrichment] suppression_key_env %q", c.SuppressionKeyEnv)
	}

	seen := make(map[string]struct{}, len(c.Providers))
	enabled := 0
	for i := range c.Providers {
		provider := c.Providers[i]
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			return fmt.Errorf("[people.enrichment.providers.%d] name is required", i)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate provider name %q", name)
		}
		seen[name] = struct{}{}
		if provider.Enabled {
			enabled++
		}
		if err := provider.Validate(); err != nil {
			return fmt.Errorf("provider %q: %w", provider.Name, err)
		}
	}
	if !c.Enabled {
		return nil
	}
	if c.SuppressionKeyEnv == "" {
		return errors.New("[people.enrichment] suppression_key_env is required when enrichment is enabled")
	}
	if enabled == 0 {
		return errors.New("[people.enrichment] requires at least one enabled provider")
	}
	return nil
}

func (c *ProviderConfig) ApplyDefaults() {
	if c.RequestTimeout == 0 {
		c.RequestTimeout = time.Minute
	}
	if c.PollInterval == 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.MaxJobAge == 0 {
		c.MaxJobAge = 15 * time.Minute
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 5
	}
	switch c.Kind {
	case ProviderExa:
		if c.Endpoint == "" {
			c.Endpoint = defaultExaEndpoint
		}
		if c.APIKeyEnv == "" {
			c.APIKeyEnv = "EXA_API_KEY"
		}
		if c.Mode == "" {
			c.Mode = "people"
		}
		if c.NumResults == 0 {
			c.NumResults = 1
		}
	case ProviderSixtyfour:
		if c.Endpoint == "" {
			c.Endpoint = defaultSixtyfourEndpoint
		}
		if c.PollEndpoint == "" {
			c.PollEndpoint = defaultSixtyfourPoll
		}
		if c.APIKeyEnv == "" {
			c.APIKeyEnv = "SIXTYFOUR_API_KEY"
		}
	}
}

func (c ProviderConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	return c.validatePolicy(true)
}

// CredentialEndpoint returns the endpoint origin that may receive this
// provider's credential. Asynchronous providers must keep every credential-
// bearing endpoint on that same origin.
func (c ProviderConfig) CredentialEndpoint() (string, error) {
	endpoint, err := validateHTTPSEndpoint("endpoint", c.Endpoint)
	if err != nil {
		return "", err
	}
	switch c.Kind {
	case ProviderExa:
		if c.PollEndpoint != "" {
			return "", errors.New("exa poll_endpoint must be empty")
		}
	case ProviderSixtyfour:
		pollEndpoint, err := validateHTTPSEndpoint("poll_endpoint", c.PollEndpoint)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(endpoint.Scheme, pollEndpoint.Scheme) ||
			!strings.EqualFold(endpoint.Host, pollEndpoint.Host) {
			return "", errors.New("sixtyfour endpoint and poll_endpoint must use the same origin")
		}
	default:
		return "", fmt.Errorf("kind must be %q or %q", ProviderExa, ProviderSixtyfour)
	}
	return c.Endpoint, nil
}

func (c ProviderConfig) validatePolicy(enforceGuaranteedCost bool) error {
	if c.Name == "" || c.Name != strings.TrimSpace(c.Name) || !providerNamePattern.MatchString(c.Name) {
		return errors.New("name must be a CLI-safe token using only letters, digits, '.', '_', ':', or '-'")
	}
	if c.Kind != ProviderExa && c.Kind != ProviderSixtyfour {
		return fmt.Errorf("kind must be %q or %q", ProviderExa, ProviderSixtyfour)
	}
	if _, err := c.CredentialEndpoint(); err != nil {
		return err
	}
	if c.APIKeyEnv == "" || !environmentNamePattern.MatchString(c.APIKeyEnv) {
		return fmt.Errorf("invalid api_key_env %q", c.APIKeyEnv)
	}
	if err := validateAllowedIdentifiers(c.AllowedIdentifiers); err != nil {
		return err
	}
	if err := validateTargetKeys(c.TargetKeys); err != nil {
		return err
	}
	if err := validatePosture("retention", c.RetentionPosture); err != nil {
		return err
	}
	if err := validatePosture("training", c.TrainingPosture); err != nil {
		return err
	}
	for _, duration := range []struct {
		name  string
		value time.Duration
	}{
		{"refresh_interval", c.RefreshInterval},
		{"request_timeout", c.RequestTimeout},
		{"poll_interval", c.PollInterval},
		{"max_job_age", c.MaxJobAge},
	} {
		if duration.value <= 0 {
			return fmt.Errorf("%s must be positive", duration.name)
		}
	}
	if c.MaxRetries < 0 {
		return errors.New("max_retries must be non-negative")
	}
	if c.MaxRequestsPerRun <= 0 {
		return errors.New("max_requests_per_run must be positive")
	}
	if c.MaxRequestsPerDay <= 0 {
		return errors.New("max_requests_per_day must be positive")
	}
	if c.MaxCostUSDMicrosPerPersonPerDay < 0 || c.MaxCostUSDMicrosPerRun < 0 ||
		c.MaxCostUSDMicrosPerDay < 0 {
		return errors.New("hard cost caps must be non-negative")
	}
	if enforceGuaranteedCost && c.hasHardCostCap() && !providerGuaranteesChargeBound(c.Kind) {
		return fmt.Errorf("hard cost caps unsupported by %s; use request caps", c.Kind)
	}

	switch c.Kind {
	case ProviderExa:
		if c.Mode != "people" && c.Mode != "deep" && c.Mode != "deep-reasoning" {
			return fmt.Errorf("invalid Exa mode %q", c.Mode)
		}
		if c.Tier != "" {
			return errors.New("exa tier must be empty")
		}
		if c.NumResults != 1 {
			return errors.New("exa num_results must be exactly 1")
		}
	case ProviderSixtyfour:
		if strings.TrimSpace(c.Tier) == "" {
			return errors.New("sixtyfour tier is required")
		}
		if c.Mode != "" {
			return errors.New("sixtyfour mode must be empty")
		}
		if !sixtyfourAllowedIdentifiersCanBindIdentity(c.AllowedIdentifiers) {
			return errors.New("sixtyfour allowed_identifiers must include both name and current_company for verified response binding")
		}
	}
	return nil
}

func (c ProviderConfig) hasHardCostCap() bool {
	return c.MaxCostUSDMicrosPerPersonPerDay > 0 || c.MaxCostUSDMicrosPerRun > 0 ||
		c.MaxCostUSDMicrosPerDay > 0
}

func providerGuaranteesChargeBound(_ string) bool {
	// Zero hard-cost caps select request-cap-only enforcement. They do not
	// assert an unlimited monetary budget or manufacture a provider guarantee.
	return false
}

func validateAllowedIdentifiers(values []IdentifierClass) error {
	if len(values) == 0 {
		return errors.New("allowed_identifiers must not be empty")
	}
	seen := make(map[IdentifierClass]struct{}, len(values))
	for _, value := range values {
		if !validIdentifierClass(value) {
			return fmt.Errorf("allowed_identifiers contains unsupported class %q", value)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("allowed_identifiers contains duplicate %q", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateTargetKeys(values []string) error {
	if len(values) == 0 || len(values) > maxDurableAttemptTargets {
		return fmt.Errorf("target_keys count must be in [1,%d]", maxDurableAttemptTargets)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return errors.New("target_keys must contain non-empty keys")
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("target_keys contains duplicate %q", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func sixtyfourAllowedIdentifiersCanBindIdentity(values []IdentifierClass) bool {
	seen := make(map[IdentifierClass]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	_, hasName := seen[IdentifierName]
	_, hasCompany := seen[IdentifierCurrentCompany]
	return hasName && hasCompany
}

func validatePosture(name, value string) error {
	if strings.TrimSpace(value) == "" || strings.EqualFold(strings.TrimSpace(value), "unknown") {
		return fmt.Errorf("%s_posture must be explicit", name)
	}
	return nil
}

func validateHTTPSEndpoint(name, raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid %s %q", name, raw)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s must use HTTPS", name)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%s must not contain credentials", name)
	}
	if parsed.RawQuery != "" {
		return nil, fmt.Errorf("%s must not contain a query", name)
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("%s must not contain a fragment", name)
	}
	return parsed, nil
}

func canonicalEndpoint(endpoint *url.URL) string {
	cloned := *endpoint
	cloned.Path = strings.TrimRight(cloned.Path, "/")
	return cloned.String()
}
