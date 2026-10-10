package embed

import (
	"context"

	"go.kenn.io/msgvault/internal/vector"
)

func (w *Worker) upsertMeasured(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) (err error) {
	finish := measureEmbeddingWrite(ctx)
	defer func() { finish(err) }()
	return w.deps.Backend.Upsert(ctx, gen, chunks)
}

func (w *ContextWorker) embedDocumentsMeasured(ctx context.Context, inputs []DocumentInput) ([][][]float32, error) {
	var chunks []string
	for _, input := range inputs {
		chunks = append(chunks, input.Chunks...)
	}
	finish := measureEmbeddingProvider(ctx, chunks)
	vectors, err := w.deps.Client.EmbedDocuments(ctx, inputs)
	// A packed request can fail after earlier ones succeed; the worker
	// publishes the returned prefix, so its chunks count as accepted.
	accepted := 0
	for _, input := range inputs[:min(len(vectors), len(inputs))] {
		accepted += len(input.Chunks)
	}
	finish(accepted, err)
	return vectors, err
}
