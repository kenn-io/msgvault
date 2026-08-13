package mistral

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	CapabilitySchemaVersion   = 2
	probeFixtureContract      = 1
	requestFingerprintVersion = 2
	mediaTypePDF              = "application/pdf"
	ProbeStatusPassed         = "passed"
	ProbeStatusRejected       = "provider_rejected"
	ProbeStatusFailed         = "probe_failed"
)

// CandidateFormat is a documented non-visual format that must be tested
// against the stateless endpoint. Documentation alone never makes it eligible.
type CandidateFormat struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	MediaType string `json:"media_type"`
	UnitKind  string `json:"unit_kind"`
}

var candidateFormats = []CandidateFormat{
	{ID: "pdf", Family: "pdf", MediaType: mediaTypePDF, UnitKind: "page"},
	{ID: "docx", Family: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", UnitKind: "page"},
	{ID: "doc", Family: "word", MediaType: "application/msword", UnitKind: "page"},
	{ID: "odt", Family: "word", MediaType: "application/vnd.oasis.opendocument.text", UnitKind: "page"},
	{ID: "rtf", Family: "word", MediaType: "application/rtf", UnitKind: "page"},
	{ID: "pptx", Family: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", UnitKind: "slide"},
	{ID: "ppt", Family: "presentation", MediaType: "application/vnd.ms-powerpoint", UnitKind: "slide"},
	{ID: "xlsx", Family: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", UnitKind: "sheet"},
	{ID: "xls", Family: "spreadsheet", MediaType: "application/vnd.ms-excel", UnitKind: "sheet"},
	{ID: "ods", Family: "spreadsheet", MediaType: "application/vnd.oasis.opendocument.spreadsheet", UnitKind: "sheet"},
	{ID: "numbers", Family: "spreadsheet", MediaType: "application/vnd.apple.numbers", UnitKind: "sheet"},
	{ID: "csv", Family: "spreadsheet", MediaType: "text/csv", UnitKind: "record"},
	{ID: "epub", Family: "ebook", MediaType: "application/epub+zip", UnitKind: "spine"},
	{ID: "txt", Family: "text", MediaType: "text/plain", UnitKind: "section"},
	{ID: "markdown", Family: "text", MediaType: "text/markdown", UnitKind: "section"},
	{ID: "rst", Family: "text", MediaType: "text/x-rst", UnitKind: "section"},
	{ID: "latex", Family: "text", MediaType: "application/x-tex", UnitKind: "section"},
	{ID: "json", Family: "structured", MediaType: "application/json", UnitKind: "record"},
	{ID: "jsonl", Family: "structured", MediaType: "application/x-ndjson", UnitKind: "record"},
	{ID: "xml", Family: "structured", MediaType: "application/xml", UnitKind: "record"},
	{ID: "yaml", Family: "structured", MediaType: "application/yaml", UnitKind: "record"},
	{ID: "go", Family: "source", MediaType: "text/x-go", UnitKind: "section"},
	{ID: "python", Family: "source", MediaType: "text/x-python", UnitKind: "section"},
	{ID: "javascript", Family: "source", MediaType: "text/javascript", UnitKind: "section"},
	{ID: "eml", Family: "mail", MediaType: "message/rfc822", UnitKind: "message"},
	{ID: "msg", Family: "mail", MediaType: "application/vnd.ms-outlook", UnitKind: "message"},
}

// CandidateFormats returns a defensive copy in stable probe order.
func CandidateFormats() []CandidateFormat {
	return slices.Clone(candidateFormats)
}

func CandidateFormatByID(id string) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}

func CandidateFormatByMediaType(mediaType string) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.MediaType == mediaType {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}

// CapabilityManifest is safe to commit: it contains no filenames, extracted
// content, raw provider responses, keys, URLs supplied by users, or full source
// hashes.
type CapabilityManifest struct {
	SchemaVersion        int                `json:"schema_version"`
	ProbeFixtureContract int                `json:"probe_fixture_contract"`
	ObservedOn           string             `json:"observed_on"`
	Endpoint             string             `json:"endpoint"`
	Region               string             `json:"region"`
	RequestedModel       string             `json:"requested_model"`
	MaxPages             int                `json:"max_pages"`
	Results              []CapabilityResult `json:"results"`
}

type CapabilityResult struct {
	FormatID           string `json:"format_id"`
	Family             string `json:"family"`
	MediaType          string `json:"media_type"`
	UnitKind           string `json:"unit_kind"`
	Status             string `json:"status"`
	ReasonCode         string `json:"reason_code,omitempty"`
	FixtureDigest      string `json:"fixture_digest"`
	RequestFingerprint string `json:"request_fingerprint"`
	ReturnedModel      string `json:"returned_model,omitempty"`
	UnitCount          int    `json:"unit_count,omitempty"`
	UnitsProcessed     int    `json:"units_processed,omitempty"`
	ProviderBytes      *int64 `json:"provider_bytes,omitempty"`
}

// ValidateComplete rejects partial, duplicated, reordered, or privacy-unsafe
// manifests before they can become runtime upload authority.
func (m CapabilityManifest) ValidateComplete() error {
	if m.SchemaVersion != CapabilitySchemaVersion {
		return fmt.Errorf("mistral capability manifest schema must be %d", CapabilitySchemaVersion)
	}
	if m.ProbeFixtureContract != probeFixtureContract {
		return fmt.Errorf("mistral capability manifest fixture contract must be %d", probeFixtureContract)
	}
	observed, err := time.Parse(time.DateOnly, m.ObservedOn)
	if err != nil || observed.After(time.Now().UTC().Add(24*time.Hour)) {
		return errors.New("mistral capability manifest has invalid observation date")
	}
	if m.Endpoint != defaultEndpoint || m.Region != "eu" || m.RequestedModel != defaultModel {
		return errors.New("mistral capability manifest endpoint, region, or model is not pinned")
	}
	if m.MaxPages <= 0 || m.MaxPages > 5_000 {
		return errors.New("mistral capability manifest has invalid page limit")
	}
	if len(m.Results) != len(candidateFormats) {
		return fmt.Errorf("mistral capability manifest has %d results, want %d", len(m.Results), len(candidateFormats))
	}
	for i, result := range m.Results {
		candidate := candidateFormats[i]
		if result.FormatID != candidate.ID || result.Family != candidate.Family ||
			result.MediaType != candidate.MediaType || result.UnitKind != candidate.UnitKind {
			return fmt.Errorf("mistral capability manifest result %d does not match candidate %q", i, candidate.ID)
		}
		if !slices.Contains([]string{ProbeStatusPassed, ProbeStatusRejected, ProbeStatusFailed}, result.Status) {
			return fmt.Errorf("mistral capability manifest result %q has invalid status", result.FormatID)
		}
		if len(result.FixtureDigest) != 16 || !lowerHex(result.FixtureDigest) {
			return fmt.Errorf("mistral capability manifest result %q has invalid fixture digest", result.FormatID)
		}
		if len(result.RequestFingerprint) != 64 || !lowerHex(result.RequestFingerprint) {
			return fmt.Errorf("mistral capability manifest result %q has invalid request fingerprint", result.FormatID)
		}
		options := DefaultOptions()
		if candidate.Family == "pdf" {
			options.Pages = fmt.Sprintf("0-%d", m.MaxPages-1)
		}
		if result.RequestFingerprint != requestFingerprint(candidate, options) {
			return fmt.Errorf("mistral capability manifest result %q has mismatched request fingerprint", result.FormatID)
		}
		if result.Status == ProbeStatusPassed {
			if result.ReasonCode != "" || result.ReturnedModel != m.RequestedModel ||
				result.UnitCount <= 0 || result.UnitsProcessed != result.UnitCount {
				return fmt.Errorf("mistral capability manifest passing result %q is incomplete", result.FormatID)
			}
		} else if result.ReasonCode == "" || result.ReturnedModel != "" || result.UnitCount != 0 ||
			result.UnitsProcessed != 0 || result.ProviderBytes != nil {
			return fmt.Errorf("mistral capability manifest non-passing result %q is not scrubbed", result.FormatID)
		}
	}
	return nil
}

// ProbeFixtureSentinel returns the exact synthetic phrase that a private
// fixture must contain for one candidate format. The authenticated probe only
// passes a format when Mistral returns this phrase from that format's bytes.
func ProbeFixtureSentinel(formatID string) (string, error) {
	if _, ok := CandidateFormatByID(formatID); !ok {
		return "", fmt.Errorf("unknown Mistral probe format %q", formatID)
	}
	return "msgvault probe " + formatID + " cedar 7319", nil
}

// AllowedMediaTypes returns only formats proven by this exact complete
// manifest. Failed and provider-rejected candidates remain visible but cannot
// be uploaded by product code.
func (m CapabilityManifest) AllowedMediaTypes() ([]string, error) {
	if err := m.ValidateComplete(); err != nil {
		return nil, err
	}
	allowed := make([]string, 0, len(m.Results))
	for _, result := range m.Results {
		if result.Status == ProbeStatusPassed {
			allowed = append(allowed, result.MediaType)
		}
	}
	return allowed, nil
}

type fingerprintPayload struct {
	Version   int             `json:"version"`
	Endpoint  string          `json:"endpoint"`
	Model     string          `json:"model"`
	Candidate CandidateFormat `json:"candidate"`
	Options   Options         `json:"options"`
}

func requestFingerprint(candidate CandidateFormat, options Options) string {
	payload, err := json.Marshal(fingerprintPayload{
		Version: requestFingerprintVersion, Endpoint: defaultEndpoint, Model: defaultModel, Candidate: candidate, Options: options,
	})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func shortFixtureDigest(fullSHA256 string) (string, error) {
	if len(fullSHA256) != sha256.Size*2 || !lowerHex(fullSHA256) {
		return "", errors.New("fixture SHA-256 must be lowercase hexadecimal")
	}
	return fullSHA256[:16], nil
}

func lowerHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
