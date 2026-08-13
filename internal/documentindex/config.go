// Package documentindex owns the opt-in document attachment indexing policy.
package documentindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ProviderMistral = "mistral"
	ModelMistralOCR = "mistral-ocr-4-0"
	RegionMistralEU = "eu"

	RetentionUnknown  = "unknown"
	RetentionStandard = "standard"
	RetentionZDR      = "zdr"

	TrainingUnknown       = "unknown"
	TrainingDefaultOptOut = "default-opt-out"
	TrainingOptedOut      = "opted-out"

	defaultAPIKeyEnv                 = "MISTRAL_API_KEY" // #nosec G101 -- environment variable name, not a credential.
	defaultMaxFileBytes        int64 = 50 << 20
	defaultMaxPages                  = 500
	defaultMaxResponseBytes    int64 = 64 << 20
	defaultMaxNormalizedChars        = 25_000_000
	defaultMaxSpoolBytes       int64 = 512 << 20
	defaultMinFreeSpaceBytes   int64 = 1 << 30
	defaultMaxRetries                = 3
	defaultMaxPagesPerRun            = 10_000
	defaultMaxEstimatedCostUSD       = 50.0
	profilePolicyVersion             = 1
	hardMaxFileBytes           int64 = 500 << 20
	hardMaxPages                     = 5_000
	hardMaxResponseBytes       int64 = 512 << 20
	hardMaxNormalizedChars           = 100_000_000
	hardMaxSpoolBytes          int64 = 100 << 30
	hardMaxFreeSpaceBytes      int64 = 1 << 40
	hardMaxRequestTimeout            = 30 * time.Minute
	hardMaxRetries                   = 10
	hardMaxPagesPerRun               = 1_000_000
	hardMaxEstimatedCostUSD          = 1_000_000.0
)

var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// DefaultDocumentsConfig returns the complete safe policy used as the decode
// target. Decoding over populated defaults distinguishes an omitted numeric
// field from an explicit zero, which Validate must reject.
func DefaultDocumentsConfig() DocumentsConfig {
	config := DocumentsConfig{
		MaxFileBytes:              defaultMaxFileBytes,
		MaxPagesPerDocument:       defaultMaxPages,
		MaxResponseBytes:          defaultMaxResponseBytes,
		MaxNormalizedChars:        defaultMaxNormalizedChars,
		MaxSpoolBytes:             defaultMaxSpoolBytes,
		MinFreeSpaceBytes:         defaultMinFreeSpaceBytes,
		RequestTimeout:            5 * time.Minute,
		MaxRetries:                defaultMaxRetries,
		MaxPagesPerRun:            defaultMaxPagesPerRun,
		MaxEstimatedCostUSDPerRun: defaultMaxEstimatedCostUSD,
	}
	config.ApplyDefaults()
	return config
}

// AttachmentsConfig groups independently consented attachment processing
// lanes. Visual indexing is intentionally not configured through Documents.
type AttachmentsConfig struct {
	Documents DocumentsConfig `toml:"documents"`
}

// DocumentsConfig controls hosted Mistral extraction and local indexing.
// Supplying an API key or setting Enabled never records provider consent.
type DocumentsConfig struct {
	Enabled                   bool          `toml:"enabled"`
	Provider                  string        `toml:"provider"`
	Region                    string        `toml:"region"`
	APIKeyEnv                 string        `toml:"api_key_env"`
	Model                     string        `toml:"model"`
	RetentionPosture          string        `toml:"retention_posture"`
	TrainingPosture           string        `toml:"training_posture"`
	MaxFileBytes              int64         `toml:"max_file_bytes"`
	MaxPagesPerDocument       int           `toml:"max_pages_per_document"`
	MaxResponseBytes          int64         `toml:"max_response_bytes"`
	MaxNormalizedChars        int           `toml:"max_normalized_chars"`
	MaxSpoolBytes             int64         `toml:"max_spool_bytes"`
	MinFreeSpaceBytes         int64         `toml:"min_free_space_bytes"`
	RequestTimeout            time.Duration `toml:"request_timeout"`
	MaxRetries                int           `toml:"max_retries"`
	MaxPagesPerRun            int           `toml:"max_pages_per_run"`
	MaxEstimatedCostUSDPerRun float64       `toml:"max_estimated_cost_usd_per_run"`
	EstimatedCostUSDPerKUnits float64       `toml:"estimated_cost_usd_per_1000_units"`
	PricingAssumptionOn       string        `toml:"pricing_assumption_on"`
	Schedule                  string        `toml:"schedule"`
	CapabilityManifest        string        `toml:"capability_manifest"`
	Scope                     ScopeConfig   `toml:"scope"`
	Index                     IndexConfig   `toml:"index"`
	defaultsApplied           bool
}

// ScopeConfig limits extraction to selected message families. Empty includes
// every source whose attachment role is authoritatively standalone.
type ScopeConfig struct {
	MessageTypes []string `toml:"message_types"`
}

// IndexConfig describes the local index shape. The first release requires
// lexical plaintext because it has no vector-only serving path yet.
// Embeddings require a separate explicit opt-in and are implemented only
// after the text-vector publication contract lands.
type IndexConfig struct {
	Lexical        *bool                    `toml:"lexical"`
	StoreChunkText *bool                    `toml:"store_chunk_text"`
	Embeddings     DocumentEmbeddingsConfig `toml:"embeddings"`
}

type DocumentEmbeddingsConfig struct {
	Enabled bool   `toml:"enabled"`
	Profile string `toml:"profile"`
}

// ApplyDefaults restores safe v1 settings after TOML decoding. Pointer
// booleans preserve an explicit false.
func (c *DocumentsConfig) ApplyDefaults() {
	if !c.defaultsApplied {
		if c.Provider == "" {
			c.Provider = ProviderMistral
		}
		if c.Region == "" {
			c.Region = RegionMistralEU
		}
		if c.APIKeyEnv == "" {
			c.APIKeyEnv = defaultAPIKeyEnv
		}
		if c.Model == "" {
			c.Model = ModelMistralOCR
		}
		if c.RetentionPosture == "" {
			c.RetentionPosture = RetentionUnknown
		}
		if c.TrainingPosture == "" {
			c.TrainingPosture = TrainingUnknown
		}
		if c.Index.Lexical == nil {
			value := true
			c.Index.Lexical = &value
		}
		if c.Index.StoreChunkText == nil {
			value := true
			c.Index.StoreChunkText = &value
		}
		if c.Index.Embeddings.Profile == "" {
			c.Index.Embeddings.Profile = "vector.embeddings"
		}
		c.defaultsApplied = true
	}
	for i := range c.Scope.MessageTypes {
		c.Scope.MessageTypes[i] = strings.ToLower(strings.TrimSpace(c.Scope.MessageTypes[i]))
	}
	slices.Sort(c.Scope.MessageTypes)
	c.Scope.MessageTypes = slices.Compact(c.Scope.MessageTypes)
}

func (c *DocumentsConfig) LexicalEnabled() bool {
	return c.Index.Lexical != nil && *c.Index.Lexical
}

func (c *DocumentsConfig) StoresChunkText() bool {
	return c.Index.StoreChunkText != nil && *c.Index.StoreChunkText
}

// MaxDocumentsWithinRunBudget reserves the full per-document unit allowance
// before any hosted request. Container unit counts are not authoritative until
// Mistral responds, so this is a conservative scheduling guard rather than a
// billing guarantee for an individual container.
func (c *DocumentsConfig) MaxDocumentsWithinRunBudget(requested int) (int, error) {
	if requested <= 0 || requested > 10_000 || c.MaxPagesPerDocument <= 0 || c.MaxPagesPerRun <= 0 {
		return 0, errors.New("document run budget has invalid bounds")
	}
	limit := min(requested, c.MaxPagesPerRun/c.MaxPagesPerDocument)
	if c.EstimatedCostUSDPerKUnits > 0 {
		costPerDocument := float64(c.MaxPagesPerDocument) * c.EstimatedCostUSDPerKUnits / 1000
		limit = min(limit, int(math.Floor(c.MaxEstimatedCostUSDPerRun/costPerDocument)))
	}
	if limit <= 0 {
		return 0, errors.New("document run budget is smaller than one worst-case document request")
	}
	return limit, nil
}

// Endpoint returns the one documented v1 regional endpoint currently shipped.
// More regions require an explicit documented-host addition and new consent.
func (c *DocumentsConfig) Endpoint() (string, error) {
	switch c.Region {
	case RegionMistralEU:
		return "https://api.mistral.ai/v1/ocr", nil
	default:
		return "", fmt.Errorf("attachments.documents.region: unknown region %q (supported: %q)", c.Region, RegionMistralEU)
	}
}

// Validate checks the complete effective policy without resolving credentials.
// Disabled configurations are validated too so enabling later cannot expose
// bytes under an already-invalid policy.
func (c *DocumentsConfig) Validate() error {
	if c.Provider != ProviderMistral {
		return fmt.Errorf("attachments.documents.provider: must be %q", ProviderMistral)
	}
	if c.Model != ModelMistralOCR {
		return fmt.Errorf("attachments.documents.model: must be pinned to %q", ModelMistralOCR)
	}
	if _, err := c.Endpoint(); err != nil {
		return err
	}
	if !envNamePattern.MatchString(c.APIKeyEnv) {
		return fmt.Errorf("attachments.documents.api_key_env: invalid environment variable name %q", c.APIKeyEnv)
	}
	if !slices.Contains([]string{RetentionUnknown, RetentionStandard, RetentionZDR}, c.RetentionPosture) {
		return fmt.Errorf("attachments.documents.retention_posture: invalid value %q", c.RetentionPosture)
	}
	if !slices.Contains([]string{TrainingUnknown, TrainingDefaultOptOut, TrainingOptedOut}, c.TrainingPosture) {
		return fmt.Errorf("attachments.documents.training_posture: invalid value %q", c.TrainingPosture)
	}
	positive := []struct {
		name  string
		value int64
	}{
		{name: "max_file_bytes", value: c.MaxFileBytes},
		{name: "max_pages_per_document", value: int64(c.MaxPagesPerDocument)},
		{name: "max_response_bytes", value: c.MaxResponseBytes},
		{name: "max_normalized_chars", value: int64(c.MaxNormalizedChars)},
		{name: "max_spool_bytes", value: c.MaxSpoolBytes},
		{name: "min_free_space_bytes", value: c.MinFreeSpaceBytes},
		{name: "request_timeout", value: int64(c.RequestTimeout)},
		{name: "max_retries", value: int64(c.MaxRetries)},
		{name: "max_pages_per_run", value: int64(c.MaxPagesPerRun)},
	}
	for _, field := range positive {
		if field.value <= 0 {
			return fmt.Errorf("attachments.documents.%s: must be positive", field.name)
		}
	}
	bounded := []struct {
		name  string
		value int64
		limit int64
	}{
		{name: "max_file_bytes", value: c.MaxFileBytes, limit: hardMaxFileBytes},
		{name: "max_pages_per_document", value: int64(c.MaxPagesPerDocument), limit: hardMaxPages},
		{name: "max_response_bytes", value: c.MaxResponseBytes, limit: hardMaxResponseBytes},
		{name: "max_normalized_chars", value: int64(c.MaxNormalizedChars), limit: hardMaxNormalizedChars},
		{name: "max_spool_bytes", value: c.MaxSpoolBytes, limit: hardMaxSpoolBytes},
		{name: "min_free_space_bytes", value: c.MinFreeSpaceBytes, limit: hardMaxFreeSpaceBytes},
		{name: "request_timeout", value: int64(c.RequestTimeout), limit: int64(hardMaxRequestTimeout)},
		{name: "max_retries", value: int64(c.MaxRetries), limit: hardMaxRetries},
		{name: "max_pages_per_run", value: int64(c.MaxPagesPerRun), limit: hardMaxPagesPerRun},
	}
	for _, field := range bounded {
		if field.value > field.limit {
			return fmt.Errorf("attachments.documents.%s: exceeds hard safety limit", field.name)
		}
	}
	if c.MaxSpoolBytes < c.MaxFileBytes {
		return errors.New("attachments.documents.max_spool_bytes: must be at least max_file_bytes")
	}
	if math.IsNaN(c.MaxEstimatedCostUSDPerRun) || math.IsInf(c.MaxEstimatedCostUSDPerRun, 0) || c.MaxEstimatedCostUSDPerRun <= 0 {
		return errors.New("attachments.documents.max_estimated_cost_usd_per_run: must be finite and positive")
	}
	if c.MaxEstimatedCostUSDPerRun > hardMaxEstimatedCostUSD {
		return errors.New("attachments.documents.max_estimated_cost_usd_per_run: exceeds hard safety limit")
	}
	if math.IsNaN(c.EstimatedCostUSDPerKUnits) || math.IsInf(c.EstimatedCostUSDPerKUnits, 0) ||
		c.EstimatedCostUSDPerKUnits < 0 {
		return errors.New("attachments.documents.estimated_cost_usd_per_1000_units: must be finite and nonnegative")
	}
	if (c.EstimatedCostUSDPerKUnits == 0) != (c.PricingAssumptionOn == "") {
		return errors.New("attachments.documents pricing assumption requires both estimated_cost_usd_per_1000_units and pricing_assumption_on")
	}
	if c.PricingAssumptionOn != "" {
		if _, err := time.Parse(time.DateOnly, c.PricingAssumptionOn); err != nil {
			return errors.New("attachments.documents.pricing_assumption_on: must use YYYY-MM-DD")
		}
	}
	if c.Schedule != "" {
		if strings.TrimSpace(c.CapabilityManifest) == "" {
			return errors.New("attachments.documents.schedule requires capability_manifest")
		}
		if c.EstimatedCostUSDPerKUnits <= 0 {
			return errors.New("attachments.documents.schedule requires an explicit pricing assumption")
		}
	}
	if !c.LexicalEnabled() || !c.StoresChunkText() {
		return errors.New("attachments.documents.index.lexical and store_chunk_text must both be true until document vector publication lands")
	}
	if c.Index.Embeddings.Enabled {
		return errors.New("attachments.documents.index.embeddings.enabled is not available until document vector publication lands")
	}
	if slices.Contains(c.Scope.MessageTypes, "") {
		return errors.New("attachments.documents.scope.message_types contains an empty value")
	}
	return nil
}

// ResolveAPIKey is called only by an explicit provider operation. Merely
// loading configuration never resolves or validates the secret.
func (c *DocumentsConfig) ResolveAPIKey() (string, error) {
	value, ok := os.LookupEnv(c.APIKeyEnv)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("document extraction requires nonempty environment variable %s", c.APIKeyEnv)
	}
	return value, nil
}

// ProfileFingerprint binds consent and immutable extraction output to every
// policy field that can change uploaded bytes, output, or privacy posture.
func (c *DocumentsConfig) ProfileFingerprint(allowedMediaTypes []string) (string, error) {
	encoded, err := c.ProfilePolicyJSON(allowedMediaTypes)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

// ProfilePolicyJSON is the canonical non-secret policy persisted with an
// immutable extraction profile. Its digest is the consent fingerprint.
func (c *DocumentsConfig) ProfilePolicyJSON(allowedMediaTypes []string) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	mediaTypes := slices.Clone(allowedMediaTypes)
	slices.Sort(mediaTypes)
	mediaTypes = slices.Compact(mediaTypes)
	endpoint, err := c.Endpoint()
	if err != nil {
		return nil, err
	}
	normalizePolicy := DefaultNormalizePolicy(c.MaxNormalizedChars)
	payload := struct {
		Version                   int      `json:"version"`
		Provider                  string   `json:"provider"`
		Endpoint                  string   `json:"endpoint"`
		Model                     string   `json:"model"`
		Retention                 string   `json:"retention"`
		Training                  string   `json:"training"`
		MaxFileBytes              int64    `json:"max_file_bytes"`
		MaxPagesPerDocument       int      `json:"max_pages_per_document"`
		MaxResponseBytes          int64    `json:"max_response_bytes"`
		MaxNormalizedChars        int      `json:"max_normalized_chars"`
		MaxSpoolBytes             int64    `json:"max_spool_bytes"`
		MinFreeSpaceBytes         int64    `json:"min_free_space_bytes"`
		RequestTimeoutNanos       int64    `json:"request_timeout_nanos"`
		MaxRetries                int      `json:"max_retries"`
		MaxPagesPerRun            int      `json:"max_pages_per_run"`
		MaxEstimatedCostUSDPerRun float64  `json:"max_estimated_cost_usd_per_run"`
		MessageTypes              []string `json:"message_types"`
		AllowedMediaTypes         []string `json:"allowed_media_types"`
		Lexical                   bool     `json:"lexical"`
		StoreChunkText            bool     `json:"store_chunk_text"`
		ExtractHeader             bool     `json:"extract_header"`
		ExtractFooter             bool     `json:"extract_footer"`
		NormalizationVersion      int      `json:"normalization_version"`
		MaxUnitChars              int      `json:"max_unit_chars"`
		MaxSourceUnitBytes        int      `json:"max_source_unit_bytes"`
		MaxMetadataSourceBytes    int      `json:"max_metadata_source_bytes"`
		MaxLinkChars              int      `json:"max_link_chars"`
		MaxChunkRunes             int      `json:"max_chunk_runes"`
		ChunkOverlap              int      `json:"chunk_overlap"`
		MaxChunks                 int      `json:"max_chunks"`
	}{
		Version: profilePolicyVersion, Provider: c.Provider, Endpoint: endpoint,
		Model: c.Model, Retention: c.RetentionPosture, Training: c.TrainingPosture,
		MaxFileBytes: c.MaxFileBytes, MaxPagesPerDocument: c.MaxPagesPerDocument,
		MaxResponseBytes: c.MaxResponseBytes, MaxNormalizedChars: c.MaxNormalizedChars,
		MaxSpoolBytes: c.MaxSpoolBytes, MinFreeSpaceBytes: c.MinFreeSpaceBytes,
		RequestTimeoutNanos: int64(c.RequestTimeout), MaxRetries: c.MaxRetries,
		MaxPagesPerRun:            c.MaxPagesPerRun,
		MaxEstimatedCostUSDPerRun: c.MaxEstimatedCostUSDPerRun,
		MessageTypes:              slices.Clone(c.Scope.MessageTypes), AllowedMediaTypes: mediaTypes,
		Lexical: c.LexicalEnabled(), StoreChunkText: c.StoresChunkText(),
		ExtractHeader: true, ExtractFooter: true,
		NormalizationVersion: normalizationPolicyVersion,
		MaxUnitChars:         normalizePolicy.MaxUnitChars, MaxSourceUnitBytes: normalizePolicy.MaxSourceUnitBytes,
		MaxMetadataSourceBytes: normalizePolicy.MaxMetadataSourceBytes, MaxLinkChars: normalizePolicy.MaxLinkChars,
		MaxChunkRunes: normalizePolicy.MaxChunkRunes, ChunkOverlap: normalizePolicy.ChunkOverlap,
		MaxChunks: normalizePolicy.MaxChunks,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode document extraction profile: %w", err)
	}
	return encoded, nil
}
