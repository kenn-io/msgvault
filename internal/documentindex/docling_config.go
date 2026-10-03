package documentindex

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
)

// ApplyConfiguredProviderDefaults replaces prefilled Mistral settings only
// when the TOML file omitted them. Explicit values are validated unchanged.
func (c *DocumentsConfig) ApplyConfiguredProviderDefaults(isDefined func(string) bool) {
	if c.Provider != ProviderDocling {
		return
	}
	for _, field := range []struct {
		name     string
		value    *string
		fallback string
	}{
		{"api_key_env", &c.APIKeyEnv, ""},
		{"model", &c.Model, ModelDocling},
		{"region", &c.Region, string(document.RenditionTrustOperatorNetwork)},
		{"retention_posture", &c.RetentionPosture, OperatorControlled},
		{"training_posture", &c.TrainingPosture, OperatorControlled},
	} {
		if !isDefined(field.name) {
			*field.value = field.fallback
		}
	}
	if !isDefined("request_timeout") {
		c.RequestTimeout = 30 * time.Second
	}
}

func (c *DocumentsConfig) validateDocling() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.String() != c.Endpoint || u.User != nil || u.Opaque != "" || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(c.Endpoint, "#") || strings.TrimSpace(c.Endpoint) != c.Endpoint {
		return errors.New("attachments.documents.endpoint: must be an HTTP(S) origin without credentials, path, query, or fragment")
	}
	if u.Scheme == "http" && !isDoclingLoopbackHost(u.Hostname()) {
		if !isDoclingPrivateHost(u.Hostname()) {
			return errors.New("attachments.documents.endpoint: public endpoints must use HTTPS; plain HTTP is allowed only for localhost or a private or link-local IP address")
		}
		if c.APIKeyEnv != "" {
			return errors.New("attachments.documents.api_key_env: an API key requires HTTPS unless the endpoint is on loopback")
		}
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value <= 0 || value > 65535 {
			return errors.New("attachments.documents.endpoint: invalid port")
		}
	}
	if c.Model != ModelDocling {
		return fmt.Errorf("attachments.documents.model: must be pinned to %q", ModelDocling)
	}
	if c.Region != string(document.RenditionTrustOperatorNetwork) {
		return errors.New("attachments.documents.region: Docling requires operator_network")
	}
	if c.APIKeyEnv != "" && !envNamePattern.MatchString(c.APIKeyEnv) {
		return errors.New("attachments.documents.api_key_env: invalid environment variable name")
	}
	if c.RetentionPosture != OperatorControlled {
		return errors.New("attachments.documents.retention_posture: Docling requires operator-controlled")
	}
	if c.TrainingPosture != OperatorControlled {
		return errors.New("attachments.documents.training_posture: Docling requires operator-controlled")
	}
	if c.TotalTimeout <= 0 || c.TotalTimeout > 24*time.Hour {
		return errors.New("attachments.documents.total_timeout: must be positive and at most 24h")
	}
	if c.PollInterval <= 0 || c.PollInterval > c.TotalTimeout {
		return errors.New("attachments.documents.poll_interval: must be positive and at most total_timeout")
	}
	if c.MaxPollAttempts <= 0 || c.MaxPollAttempts > 10_000 {
		return errors.New("attachments.documents.max_poll_attempts: must be between 1 and 10000")
	}
	if c.Conversion.CSV.Enabled {
		return errors.New("attachments.documents.conversion.csv: Docling accepts native CSV; PDF conversion is Mistral-only")
	}
	return nil
}

func isDoclingLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Only IP literals qualify: DNS could resolve a hostname to a public address.
func isDoclingPrivateHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// doclingFormats limits original-file uploads to supported document formats.
// Visual/media indexing remains a separately authorized lane.
func doclingFormats() []mistral.CandidateFormat {
	var formats []mistral.CandidateFormat
	for _, format := range mistral.CandidateFormats() {
		if slices.Contains([]string{"pdf", "docx", "pptx", "xlsx", "txt", "markdown", "csv"}, format.ID) {
			formats = append(formats, format)
		}
	}
	return append(formats, mistral.CandidateFormat{ID: "html", Family: "text", MediaType: "text/html", UnitKind: "document"})
}

func (c *DocumentsConfig) doclingInputPolicy() (ResolvedInputPolicy, error) {
	if err := c.Validate(); err != nil {
		return ResolvedInputPolicy{}, err
	}
	policy := ResolvedInputPolicy{Routes: make(map[string]InputRoute)}
	for _, format := range doclingFormats() {
		policy.AllowedMediaTypes = append(policy.AllowedMediaTypes, format.MediaType)
		policy.Routes[format.MediaType] = InputRoute{Format: format}
	}
	slices.Sort(policy.AllowedMediaTypes)
	return policy, nil
}

// DoclingPolicyJSON returns the exact non-secret processing and disclosure
// identity. Disk-spool settings and Mistral retry policy do not apply to Docling.
func (c *DocumentsConfig) DoclingPolicyJSON() ([]byte, error) {
	input, err := c.doclingInputPolicy()
	if err != nil {
		return nil, err
	}
	return c.doclingPolicyJSON(input.AllowedMediaTypes)
}

func (c *DocumentsConfig) doclingPolicyJSON(allowedMediaTypes []string) ([]byte, error) {
	if c.Provider != ProviderDocling {
		return nil, errors.New("docling policy requires provider=docling")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	normalization, err := document.NewNormalizePolicy(c.MaxNormalizedChars)
	if err != nil {
		return nil, fmt.Errorf("configure document normalization: %w", err)
	}
	formats := slices.Clone(allowedMediaTypes)
	slices.Sort(formats)
	formats = slices.Compact(formats)
	scope := slices.Clone(c.Scope.MessageTypes)
	slices.Sort(scope)
	scope = slices.Compact(scope)
	payload := struct {
		Version           int                              `json:"version"`
		Provider          string                           `json:"provider"`
		Endpoint          string                           `json:"endpoint"`
		Model             string                           `json:"model"`
		TrustBoundary     string                           `json:"trust_boundary"`
		Retention         string                           `json:"retention"`
		Training          string                           `json:"training"`
		CredentialBinding string                           `json:"credential_binding"`
		Formats           []string                         `json:"formats"`
		MaxFileBytes      int64                            `json:"max_file_bytes"`
		MaxResponseBytes  int64                            `json:"max_response_bytes"`
		MaxUnits          int                              `json:"max_units"`
		MaxUnitsPerRun    int                              `json:"max_units_per_run"`
		RequestTimeout    int64                            `json:"request_timeout_nanos"`
		TotalTimeout      int64                            `json:"total_timeout_nanos"`
		PollInterval      int64                            `json:"poll_interval_nanos"`
		MaxPollAttempts   int                              `json:"max_poll_attempts"`
		MessageTypes      []string                         `json:"message_types"`
		IncludeInline     bool                             `json:"include_inline"`
		Normalization     document.NormalizePolicyIdentity `json:"normalization"`
		Lexical           bool                             `json:"lexical"`
		StoreChunkText    bool                             `json:"store_chunk_text"`
		AdapterVersion    string                           `json:"adapter_version"`
	}{
		Version: 1, Provider: ProviderDocling, Endpoint: c.Endpoint, Model: c.Model,
		TrustBoundary: c.Region, Retention: c.RetentionPosture, Training: c.TrainingPosture,
		CredentialBinding: c.APIKeyEnv, Formats: formats, MaxFileBytes: c.MaxFileBytes,
		MaxResponseBytes: c.MaxResponseBytes, MaxUnits: c.MaxPagesPerDocument,
		MaxUnitsPerRun: c.MaxPagesPerRun, RequestTimeout: int64(c.RequestTimeout),
		TotalTimeout: int64(c.TotalTimeout), PollInterval: int64(c.PollInterval), MaxPollAttempts: c.MaxPollAttempts,
		MessageTypes: scope, IncludeInline: c.Scope.IncludeInline, Normalization: normalization.Identity(),
		Lexical: c.LexicalEnabled(), StoreChunkText: c.StoresChunkText(),
		AdapterVersion: "docbank:a212ec3d2e4a:docling-documents-v1",
	}
	return json.Marshal(payload, json.Deterministic(true))
}

// DoclingDescriptor binds the adapter contract to the configured operator
// origin, limits and disclosure policy without contacting the server.
func (c *DocumentsConfig) DoclingDescriptor() (document.RenditionDescriptor, error) {
	input, err := c.doclingInputPolicy()
	if err != nil {
		return document.RenditionDescriptor{}, err
	}
	fingerprint, err := c.ProfileFingerprint(mistral.CapabilityManifest{}, input.AllowedMediaTypes)
	if err != nil {
		return document.RenditionDescriptor{}, err
	}
	var formats []document.RenditionFormatCapability
	for _, format := range doclingFormats() {
		formats = append(formats, document.RenditionFormatCapability{MediaFamily: format.Family, MediaType: format.MediaType, InputKind: document.RenditionInputOriginalFile})
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: ModelDocling, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fingerprint, TrustBoundary: document.RenditionTrustOperatorNetwork,
		SupportedFormats: formats, ReturnsMarkdown: true, ReturnsStructured: true,
		ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	if err != nil {
		return document.RenditionDescriptor{}, fmt.Errorf("configure Docling descriptor: %w", err)
	}
	return descriptor, nil
}
