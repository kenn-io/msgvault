// Package document defines the independent attachment-document vector corpus.
package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
)

const (
	fingerprintPolicyVersion = 2
	embeddingProfile         = "vector.embeddings"
	flatBoundaryPolicy       = "flat-input-v1"
	contextualBoundaryPolicy = "isolated-chunk-v1"
)

// GenerationID identifies one document-vector generation.
type GenerationID int64

// GenerationState describes a document-vector generation's lifecycle state.
type GenerationState string

const (
	GenerationBuilding GenerationState = "building"
	GenerationActive   GenerationState = "active"
	GenerationRetired  GenerationState = "retired"
)

// GenerationSpec is the immutable identity of one document-vector corpus
// generation. Its Fingerprint is derived from the extraction profile and
// embedding policy that determine the stored vectors.
type GenerationSpec struct {
	Fingerprint         string
	ExtractionProfileID string
	EmbeddingProfile    string
	Model               string
	Dimension           int
}

// SearchMode selects the retrieval signal used for document search.
type SearchMode string

const (
	SearchModeAuto     SearchMode = "auto"
	SearchModeLexical  SearchMode = "lexical"
	SearchModeSemantic SearchMode = "semantic"
	SearchModeHybrid   SearchMode = "hybrid"
)

// ParseSearchMode normalizes a document search mode. An omitted mode selects
// automatic lexical/vector routing.
func ParseSearchMode(value string) (SearchMode, error) {
	mode := SearchMode(strings.ToLower(strings.TrimSpace(value)))
	if mode == "" {
		return SearchModeAuto, nil
	}
	switch mode {
	case SearchModeAuto, SearchModeLexical, SearchModeSemantic, SearchModeHybrid:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported document search mode %q", value)
	}
}

// Fingerprint returns the immutable, non-secret document-vector corpus
// identity. It deliberately excludes endpoint and credential location because
// neither changes the vectors, and excludes message preprocessing/scope
// because this corpus contains normalized document chunks rather than messages.
func Fingerprint(extractionProfileID string, cfg vector.Config) string {
	payload := struct {
		Version             int                       `json:"version"`
		ExtractionProfileID string                    `json:"extraction_profile_id"`
		EmbeddingProfile    string                    `json:"embedding_profile"`
		APIFormat           vector.EmbeddingAPIFormat `json:"api_format"`
		Model               string                    `json:"model"`
		Dimension           int                       `json:"dimension"`
		MaxInputChars       int                       `json:"max_input_chars"`
		BoundaryPolicy      string                    `json:"boundary_policy"`
	}{
		Version:             fingerprintPolicyVersion,
		ExtractionProfileID: extractionProfileID,
		EmbeddingProfile:    embeddingProfile,
		APIFormat:           cfg.Embeddings.EffectiveAPIFormat(),
		Model:               cfg.Embeddings.Model,
		Dimension:           cfg.Embeddings.Dimension,
		MaxInputChars:       cfg.Embeddings.MaxInputChars,
		BoundaryPolicy:      documentBoundaryPolicy(cfg),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal document vector fingerprint: %v", err))
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func documentBoundaryPolicy(cfg vector.Config) string {
	if cfg.Embeddings.EffectiveAPIFormat() == vector.APIFormatVoyageContextual {
		// Each extracted chunk is a stable contextual document. Grouping chunks
		// claimed in one worker run would make embeddings depend on batch timing.
		return contextualBoundaryPolicy
	}
	return flatBoundaryPolicy
}

// EgressFingerprint binds operator consent to the corpus policy and canonical
// provider destination without making endpoint changes invalidate reusable
// vectors. Credentials and URL query parameters are excluded from the durable
// destination identity and are never stored.
func EgressFingerprint(extractionProfileID string, cfg vector.Config) (string, error) {
	endpoint, err := url.Parse(cfg.Embeddings.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return "", errors.New("document embedding endpoint is invalid")
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	endpoint.Host = strings.ToLower(endpoint.Host)
	endpoint.User = nil
	endpoint.RawQuery = ""
	endpoint.ForceQuery = false
	endpoint.Fragment = ""
	payload := struct {
		Version           int    `json:"version"`
		CorpusFingerprint string `json:"corpus_fingerprint"`
		Destination       string `json:"destination"`
	}{
		Version:           1,
		CorpusFingerprint: Fingerprint(extractionProfileID, cfg),
		Destination:       endpoint.String(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal document vector egress fingerprint: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
