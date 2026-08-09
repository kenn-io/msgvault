package embed

import (
	"errors"
	"fmt"
)

// ErrDocumentTooLarge means one complete contextual document cannot fit in a
// provider request. Callers must not split the document because doing so would
// change the contextual embeddings.
var ErrDocumentTooLarge = errors.New("embed: contextual document too large")

const voyagePromptReserveUTF8BytesPerChunk = 64

// RequestLimits bounds one Voyage contextual embedding request. MaxUTF8Bytes
// is a conservative proxy for tokens. PackDocuments also reserves a fixed
// prompt allowance for every chunk.
type RequestLimits struct {
	MaxDocuments int
	MaxChunks    int
	MaxUTF8Bytes int
}

var defaultVoyageRequestLimits = RequestLimits{
	MaxDocuments: 1_000,
	MaxChunks:    16_000,
	MaxUTF8Bytes: 100_000,
}

// PackDocuments groups complete documents into requests without changing
// document or chunk order. A document that cannot fit by itself is rejected.
func PackDocuments(documents []DocumentInput, limits RequestLimits) ([][]DocumentInput, error) {
	limits = capVoyageRequestLimits(limits)
	if limits.MaxDocuments <= 0 || limits.MaxChunks <= 0 || limits.MaxUTF8Bytes <= 0 {
		return nil, errors.New("embed: request limits must be positive")
	}
	if len(documents) == 0 {
		return nil, nil
	}

	var batches [][]DocumentInput
	batchStart := 0
	batchChunks := 0
	batchBytes := 0
	for i, document := range documents {
		documentChunks := len(document.Chunks)
		if documentChunks > limits.MaxChunks {
			return nil, fmt.Errorf("%w: document %d has %d chunks; maximum is %d chunks",
				ErrDocumentTooLarge, i, documentChunks, limits.MaxChunks)
		}
		documentBytes, err := contextualDocumentBytes(document, limits.MaxUTF8Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: document %d exceeds %d UTF-8 bytes",
				ErrDocumentTooLarge, i, limits.MaxUTF8Bytes)
		}

		wouldOverflow := i > batchStart && (i-batchStart+1 > limits.MaxDocuments ||
			batchChunks+documentChunks > limits.MaxChunks ||
			batchBytes+documentBytes > limits.MaxUTF8Bytes)
		if wouldOverflow {
			batches = append(batches, documents[batchStart:i])
			batchStart = i
			batchChunks = 0
			batchBytes = 0
		}

		batchChunks += documentChunks
		batchBytes += documentBytes
	}
	batches = append(batches, documents[batchStart:])
	return batches, nil
}

// capVoyageRequestLimits makes each positive caller value a downward-only
// override of the provider-safe ceiling. Zero and negative values retain their
// existing defaulting or validation behavior at the caller.
func capVoyageRequestLimits(limits RequestLimits) RequestLimits {
	if limits.MaxDocuments > defaultVoyageRequestLimits.MaxDocuments {
		limits.MaxDocuments = defaultVoyageRequestLimits.MaxDocuments
	}
	if limits.MaxChunks > defaultVoyageRequestLimits.MaxChunks {
		limits.MaxChunks = defaultVoyageRequestLimits.MaxChunks
	}
	if limits.MaxUTF8Bytes > defaultVoyageRequestLimits.MaxUTF8Bytes {
		limits.MaxUTF8Bytes = defaultVoyageRequestLimits.MaxUTF8Bytes
	}
	return limits
}

func contextualDocumentBytes(document DocumentInput, maximum int) (int, error) {
	total := 0
	for _, chunk := range document.Chunks {
		cost := len(chunk) + voyagePromptReserveUTF8BytesPerChunk
		if cost > maximum-total {
			return 0, ErrDocumentTooLarge
		}
		total += cost
	}
	return total, nil
}
