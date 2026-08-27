// Package document defines the independent attachment-document vector corpus.
package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	docembedding "go.kenn.io/docbank/document/embedding"
	"go.kenn.io/msgvault/internal/vector"
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

// SearchMode uses Docbank's shared retrieval modes.
type SearchMode = docembedding.SearchMode

const (
	SearchModeAuto     = docembedding.SearchModeAuto
	SearchModeLexical  = docembedding.SearchModeLexical
	SearchModeSemantic = docembedding.SearchModeSemantic
	SearchModeHybrid   = docembedding.SearchModeHybrid
)

// ParseSearchMode normalizes a document search mode. Omitted values normalize
// to auto; SearchService keeps auto lexical so query text leaves the process
// only on an explicit semantic or hybrid request.
func ParseSearchMode(value string) (SearchMode, error) {
	mode := docembedding.SearchMode(strings.ToLower(strings.TrimSpace(value)))
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

// EmbeddingRecipe returns the shared raw preparation recipe used by this PR.
func EmbeddingRecipe(cfg vector.Config) (docembedding.Recipe, error) {
	recipe, err := docembedding.NewRecipe(docembedding.RecipeConfig{
		Mode: docembedding.RepresentationRaw, MaxInputRunes: cfg.Embeddings.MaxInputChars,
	})
	if err != nil {
		return docembedding.Recipe{}, fmt.Errorf(
			"construct document embedding recipe from vector.embeddings.max_input_chars=%d: %w",
			cfg.Embeddings.MaxInputChars, err,
		)
	}
	return recipe, nil
}

func vectorSpaceIdentity(cfg vector.Config) (docembedding.VectorSpaceIdentity, error) {
	deploymentFingerprint, err := (docembedding.EgressIdentity{
		Purpose: docembedding.EgressDocumentEmbedding, Provider: string(cfg.Embeddings.EffectiveAPIFormat()),
		Endpoint: cfg.Embeddings.Endpoint, Model: cfg.Embeddings.Model, ModelRevision: cfg.Embeddings.Model,
	}).Fingerprint()
	if err != nil {
		return docembedding.VectorSpaceIdentity{}, fmt.Errorf("fingerprint document embedding deployment: %w", err)
	}
	return docembedding.VectorSpaceIdentity{
		Provider: string(cfg.Embeddings.EffectiveAPIFormat()), Model: cfg.Embeddings.Model,
		ModelRevision: deploymentFingerprint, Dimension: cfg.Embeddings.Dimension,
		Normalization: "provider-output-v1",
	}, nil
}

// Fingerprint returns the immutable, non-secret document-vector corpus
// identity. The canonical endpoint is represented only by a deployment hash so
// OpenAI-compatible services cannot reuse each other's vector spaces. Credential
// location, message preprocessing, and message scope remain excluded.
func Fingerprint(extractionProfileID string, cfg vector.Config) (string, error) {
	recipe, err := EmbeddingRecipe(cfg)
	if err != nil {
		return "", err
	}
	spaceIdentity, err := vectorSpaceIdentity(cfg)
	if err != nil {
		return "", err
	}
	spaceFingerprint, err := spaceIdentity.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("fingerprint document vector space: %w", err)
	}
	payload := struct {
		Version               int    `json:"version"`
		ExtractionProfileID   string `json:"extraction_profile_id"`
		RecipeFingerprint     string `json:"recipe_fingerprint"`
		VectorSpace           string `json:"vector_space_fingerprint"`
		TaskPrefixFingerprint string `json:"task_prefix_fingerprint,omitempty"`
	}{
		Version: 1, ExtractionProfileID: extractionProfileID,
		RecipeFingerprint: recipe.Fingerprint(), VectorSpace: spaceFingerprint,
		TaskPrefixFingerprint: cfg.Embeddings.TaskPrefixFingerprint(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal document vector fingerprint: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

// EgressFingerprint binds operator consent to both Msgvault's exact corpus
// policy and Docbank's canonical provider destination.
func EgressFingerprint(extractionProfileID string, cfg vector.Config) (string, error) {
	return egressFingerprint(extractionProfileID, docembedding.EgressDocumentEmbedding, cfg)
}

// QueryEgressFingerprint is purpose-separated from document-text consent.
func QueryEgressFingerprint(extractionProfileID string, cfg vector.Config) (string, error) {
	return egressFingerprint(extractionProfileID, docembedding.EgressQueryEmbedding, cfg)
}

func egressFingerprint(extractionProfileID string, purpose docembedding.EgressPurpose, cfg vector.Config) (string, error) {
	destination, err := (docembedding.EgressIdentity{
		Purpose: purpose, Provider: string(cfg.Embeddings.EffectiveAPIFormat()),
		Endpoint: cfg.Embeddings.Endpoint, Model: cfg.Embeddings.Model,
		ModelRevision: cfg.Embeddings.Model,
	}).Fingerprint()
	if err != nil {
		return "", fmt.Errorf("fingerprint document egress destination: %w", err)
	}
	generationFingerprint, err := Fingerprint(extractionProfileID, cfg)
	if err != nil {
		return "", err
	}
	payload := struct {
		Version                int    `json:"version"`
		GenerationFingerprint  string `json:"generation_fingerprint"`
		DestinationFingerprint string `json:"destination_fingerprint"`
	}{
		Version: 1, GenerationFingerprint: generationFingerprint,
		DestinationFingerprint: destination,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal document egress consent identity: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
