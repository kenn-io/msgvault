package embed

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

const contextualChatMessageType = "beeper"

// SourceVersion identifies the source state used to assemble a document.
type SourceVersion struct {
	MessageID    int64
	LastModified any
}

// MetadataVersion identifies the conversation metadata used to assemble a
// document.
type MetadataVersion struct {
	ConversationID int64
	Digest         string
}

// AffectedScope selects documents affected by a source change.
type AffectedScope struct {
	Kind           string
	ConversationID int64
	UTCStart       time.Time
	UTCEnd         time.Time
	MessageID      int64
	MessageIDStart int64
	MessageIDEnd   int64
	Undated        bool
}

// OwnedChunk is embedding text with its source ownership and offsets.
type OwnedChunk struct {
	MessageID       int64
	ChunkIndex      int
	Text            string
	SourceCharStart int
	SourceCharEnd   int
	SourceBasis     vector.SourceBasis
	Truncated       bool
}

// Document is one revisioned semantic unit and its owned chunks.
type Document struct {
	Key             string
	Kind            string
	ScopeKey        string
	Revision        string
	SourceSequence  int64
	Versions        []SourceVersion
	MetadataVersion MetadataVersion
	Chunks          []OwnedChunk
}

// DocumentInput is one document's ordered embedding inputs.
type DocumentInput = vector.DocumentInput

// SemanticClient separates query embedding from document embedding while
// preserving document boundaries. EmbedDocuments can return a completed
// leading document prefix together with an error; callers must retain that
// prefix and retry only the unfinished suffix.
type SemanticClient interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocuments(ctx context.Context, documents []DocumentInput) ([][][]float32, error)
}
