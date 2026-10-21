package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/vector"
)

func (w *Worker) upsertMeasured(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) (err error) {
	finish := measureEmbeddingWrite(ctx)
	defer func() { finish(err) }()
	return w.deps.Backend.Upsert(ctx, gen, chunks)
}

// embedDocumentsMeasured returns the completed leading prefix of inputs.
// Each provider request is saved before the next is sent, and saved output
// is reused instead of requested again.
func (w *ContextWorker) embedDocumentsMeasured(ctx context.Context, inputs []DocumentInput, page *contextEmbeddingPage) ([][][]float32, error) {
	if page.vectors == nil {
		page.vectors = make(map[[sha256.Size]byte][][]float32)
	}
	var requested []DocumentInput
	var chunks []string
	for _, input := range inputs {
		if _, saved := page.vectors[savedDocumentKey(input)]; !saved {
			requested = append(requested, input)
			chunks = append(chunks, input.Chunks...)
		}
	}
	var err error
	if len(requested) > 0 {
		finish := measureEmbeddingProvider(ctx, chunks)
		accepted := 0
		err = vector.EmbedDocumentRequests(ctx, w.deps.Client, requested, nil, func(first int, vectors [][][]float32) error {
			if first+len(vectors) > len(requested) {
				return fmt.Errorf("contextual batched vector document count mismatch: got %d, expected %d",
					first+len(vectors), len(requested))
			}
			var entries []vector.EmbeddingCheckpoint
			completed := make(map[[sha256.Size]byte][][]float32)
			for i, documentVectors := range vectors {
				input := requested[first+i]
				// A malformed document is requested again on the next pass.
				if len(documentVectors) == len(input.Chunks) {
					key := savedDocumentKey(input)
					for _, identity := range page.identities[key] {
						entries = append(entries, vector.EmbeddingCheckpoint{Owner: identity.owner, Revision: identity.revision, Key: hex.EncodeToString(key[:]), Vectors: documentVectors})
					}
					completed[key] = documentVectors
				}
				accepted += len(input.Chunks)
			}
			if err := page.checkpoints.Save(ctx, page.gen, entries); err != nil {
				return err
			}
			if len(entries) > 0 {
				jobctx.RecordProgress(ctx)
			}
			maps.Copy(page.vectors, completed)
			return nil
		})
		// embedDocuments splits or truncates a document the provider rejects
		// as too large and retries it, and a yield resumes later, so neither
		// is a failure.
		recorded := err
		if isSizeRejection(err) || errors.Is(err, jobctx.ErrYieldedToWaiter) {
			recorded = nil
		}
		finish(accepted, recorded)
	}
	vectors := make([][][]float32, 0, len(inputs))
	for _, input := range inputs {
		saved, ok := page.vectors[savedDocumentKey(input)]
		if !ok {
			break
		}
		vectors = append(vectors, saved)
	}
	return vectors, err
}
