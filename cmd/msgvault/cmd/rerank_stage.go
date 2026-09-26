//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/rerank"
)

// newRerankStage builds the [vector.rerank] stage the hybrid engine runs for
// requests that opt in. The default endpoint is a hosted provider, so a
// missing credential is refused here instead of failing every search with a
// 401.
func newRerankStage(cfg vector.Config, mainStore *store.Store, apiKey string) (*hybrid.RerankStage, error) {
	if cfg.Rerank.Endpoint == vector.DefaultRerankEndpoint && apiKey == "" {
		return nil, fmt.Errorf("no API key: set %s or store a %q provider credential",
			cfg.Rerank.APIKeyEnv, "vector.rerank")
	}
	client, err := rerank.NewCohereClient(rerank.CohereOptions{
		Endpoint: cfg.Rerank.Endpoint,
		APIKey:   apiKey,
		Model:    cfg.Rerank.Model,
		Timeout:  cfg.Rerank.Timeout,
	})
	if err != nil {
		return nil, err
	}
	return &hybrid.RerankStage{
		Reranker:   client,
		Texts:      rerankCandidateTexts(mainStore, embeddingPreprocessConfig(cfg), cfg.Rerank.MaxCandidateChars),
		Model:      cfg.Rerank.Model,
		Candidates: cfg.Rerank.Candidates,
	}, nil
}

// rerankCandidateTexts loads each candidate's subject and body through the
// same preprocessing the embedder applies, capped at maxChars characters, so
// the reranker scores the text the index was built from rather than quoted
// replies and signatures.
func rerankCandidateTexts(mainStore *store.Store, preprocess embed.PreprocessConfig, maxChars int) hybrid.CandidateTexts {
	return func(ctx context.Context, ids []int64) ([]string, error) {
		texts := make([]string, len(ids))
		for i, id := range ids {
			message, err := mainStore.GetMessageContext(ctx, id)
			if errors.Is(err, store.ErrMessageNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", id, err)
			}
			body := embed.HydrationBodyText(message.MessageType, message.BodyText, message.BodyHTML)
			texts[i], _ = embed.Preprocess(message.Subject, body, maxChars, preprocess)
		}
		return texts, nil
	}
}
