package embed

import (
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

// IMPORTANT: changes in preprocess.Preprocess that shift output for an
// unchanged Config must be paired with a preprocessVersion bump in
// internal/vector/config.go.

// PreprocessConfig controls pre-embedding transformations.
type PreprocessConfig = preprocess.Config

// Preprocess produces the normalized string fed to the embedder.
func Preprocess(subject, body string, maxChars int, cfg PreprocessConfig) (string, bool) {
	return preprocess.Preprocess(subject, body, maxChars, cfg)
}

// PreprocessConfigFor derives the worker's preprocessing settings.
func PreprocessConfigFor(cfg vector.Config) PreprocessConfig {
	chunkPolicy := EmbeddingChunkPolicy(cfg.Embeddings.MaxInputChars)
	return PreprocessConfig{
		StripQuotes: cfg.Preprocess.StripQuotesEnabled(), StripSignatures: cfg.Preprocess.StripSignaturesEnabled(),
		StripHTML: cfg.Preprocess.StripHTMLEnabled(), StripBase64: cfg.Preprocess.StripBase64Enabled(),
		StripURLTracking: cfg.Preprocess.StripURLTrackingEnabled(), CollapseWhitespace: cfg.Preprocess.CollapseWhitespaceEnabled(),
		MaxBodyRunes: chunkPolicy.MaxBodyRunes,
	}
}
