package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	docbankdocument "go.kenn.io/docbank/document"
)

// The raw representation recipe was removed from Docbank after v0.14. This
// file keeps its recipe identity and chunk formatting so document vector
// generations fingerprinted by earlier builds stay valid.

const RepresentationRaw RepresentationMode = "raw"

const (
	recipeVersion        = 1
	inputFormatVersion   = 1
	defaultMaxInputRunes = 8_000
	// The filename and title bounds never shaped an input because msgvault
	// supplies no document context. They remain part of the canonical recipe
	// JSON so existing fingerprints keep matching.
	recipeMaxFilenameRunes = 256
	recipeMaxTitleRunes    = 512
	recipeMaxHeadingRunes  = 512
)

type RepresentationMode string

type RecipeConfig struct {
	Mode          RepresentationMode
	MaxInputRunes int
}

type RecipeValues struct {
	Version            int                `json:"version"`
	InputFormatVersion int                `json:"input_format_version"`
	Mode               RepresentationMode `json:"mode"`
	MaxInputRunes      int                `json:"max_input_runes"`
	MaxFilenameRunes   int                `json:"max_filename_runes"`
	MaxTitleRunes      int                `json:"max_title_runes"`
	MaxHeadingRunes    int                `json:"max_heading_runes"`
}

type Recipe struct {
	values RecipeValues
	digest string
}

func NewRecipe(config RecipeConfig) (Recipe, error) {
	if config.Mode == "" {
		config.Mode = RepresentationRaw
	}
	if config.MaxInputRunes == 0 {
		config.MaxInputRunes = defaultMaxInputRunes
	}
	if config.Mode != RepresentationRaw {
		return Recipe{}, fmt.Errorf("unsupported embedding representation mode %q", config.Mode)
	}
	if config.MaxInputRunes < 1 {
		return Recipe{}, errors.New("embedding recipe bounds must be positive")
	}
	values := RecipeValues{
		Version: recipeVersion, InputFormatVersion: inputFormatVersion, Mode: config.Mode,
		MaxInputRunes: config.MaxInputRunes, MaxFilenameRunes: recipeMaxFilenameRunes,
		MaxTitleRunes: recipeMaxTitleRunes, MaxHeadingRunes: recipeMaxHeadingRunes,
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return Recipe{}, fmt.Errorf("encode embedding recipe: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return Recipe{values: values, digest: hex.EncodeToString(digest[:])}, nil
}

func (r Recipe) Values() RecipeValues { return r.values }

func (r Recipe) CanonicalJSON() ([]byte, error) {
	if r.digest == "" {
		return nil, errors.New("embedding recipe is invalid; use NewRecipe")
	}
	return json.Marshal(r.values)
}

func (r Recipe) Fingerprint() string { return r.digest }

// RawEmbeddingInput is the provider text for one normalized chunk.
type RawEmbeddingInput struct {
	ChunkKey      string
	ChunkChecksum string
	Text          string
}

// RawEmbeddingInputs formats every chunk of a validated normalized document
// under the raw recipe: an optional bounded heading line, a source locator,
// and the chunk text truncated to the recipe's rune limit.
func RawEmbeddingInputs(normalized docbankdocument.NormalizedDocument, recipe Recipe) ([]RawEmbeddingInput, error) {
	if err := docbankdocument.ValidateNormalizedDocument(normalized); err != nil {
		return nil, fmt.Errorf("validate normalized document: %w", err)
	}
	if len(normalized.Chunks) == 0 {
		return nil, errors.New("normalized document has no chunks")
	}
	if recipe.digest == "" {
		return nil, errors.New("embedding recipe is invalid; use NewRecipe")
	}
	inputs := make([]RawEmbeddingInput, 0, len(normalized.Chunks))
	for _, chunk := range normalized.Chunks {
		sourcePrefix := "Source: " + formatLocator(normalized, chunk.Spans) + "\nContent:\n"
		heading := boundedHeadingContext(
			chunk.HeadingPath, recipe.values.MaxHeadingRunes,
			recipe.values.MaxInputRunes-utf8.RuneCountInString(sourcePrefix)-1,
		)
		text, err := boundedInput(heading+sourcePrefix, chunk.Text, recipe.values.MaxInputRunes)
		if err != nil {
			return nil, fmt.Errorf("format normalized chunk %q: %w", chunk.Key, err)
		}
		inputs = append(inputs, RawEmbeddingInput{
			ChunkKey: chunk.Key, ChunkChecksum: chunk.Checksum, Text: text,
		})
	}
	return inputs, nil
}

func formatLocator(normalized docbankdocument.NormalizedDocument, spans []docbankdocument.ChunkSpan) string {
	if len(spans) == 0 {
		return "document"
	}
	first, last := spans[0].UnitIndex, spans[len(spans)-1].UnitIndex
	kind := normalized.UnitKind
	if kind == "" {
		kind = "unit"
	}
	if first == last {
		return fmt.Sprintf("%s %d", kind, first+1)
	}
	return fmt.Sprintf("%ss %d-%d", kind, first+1, last+1)
}

func boundedHeadingContext(path []string, maxHeadingRunes, availableRunes int) string {
	if len(path) == 0 {
		return ""
	}
	const headingPrefix = "Heading: "
	textLimit := min(maxHeadingRunes, availableRunes-utf8.RuneCountInString(headingPrefix+"\n"))
	if textLimit < 1 {
		return ""
	}
	return headingPrefix + truncateRunes(strings.Join(path, " > "), textLimit) + "\n"
}

func boundedInput(prefix, body string, limit int) (string, error) {
	prefixRunes := utf8.RuneCountInString(prefix)
	if prefixRunes >= limit {
		return "", errors.New("embedding input context consumes the configured rune limit")
	}
	return prefix + truncateRunes(body, limit-prefixRunes), nil
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
