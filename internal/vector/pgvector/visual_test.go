//go:build pgvector

package pgvector

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualBackendStoresSearchesAndDeletesActivePublication(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend, ctx, db := newBackendForTest(t)
	statements := []string{
		`ALTER TABLE messages ADD COLUMN received_at TIMESTAMPTZ`,
		`ALTER TABLE messages ADD COLUMN internal_date TIMESTAMPTZ`,
		`CREATE TABLE attachments (
			id BIGINT PRIMARY KEY, message_id BIGINT NOT NULL REFERENCES messages(id),
			filename TEXT, mime_type TEXT, size BIGINT, attachment_role TEXT NOT NULL
		)`,
		`CREATE TABLE visual_generations (
			id BIGINT PRIMARY KEY, state TEXT NOT NULL
		)`,
		`CREATE TABLE visual_publications (
			generation_id BIGINT NOT NULL REFERENCES visual_generations(id),
			message_id BIGINT NOT NULL REFERENCES messages(id), blob_hash TEXT NOT NULL,
			media_input_key TEXT NOT NULL, representative_attachment_id BIGINT REFERENCES attachments(id),
			current_vector_token TEXT, state TEXT NOT NULL
		)`,
		`INSERT INTO visual_generations (id, state) VALUES (7, 'active')`,
		`UPDATE messages SET source_id = 3, sent_at = CURRENT_TIMESTAMP WHERE id = 1`,
		`INSERT INTO attachments (id, message_id, filename, mime_type, size, attachment_role)
		VALUES (11, 1, 'diagram.png', 'image/png', 128, 'standalone')`,
	}
	for _, statement := range statements {
		_, err := db.Exec(statement)
		requirements.NoError(err)
	}
	_, err := db.Exec(`INSERT INTO visual_publications
			(generation_id, message_id, blob_hash, media_input_key, representative_attachment_id, current_vector_token, state)
		VALUES (7, 1, $1, 'original', 11, 'visual-token-1', 'current')`, strings.Repeat("a", 64))
	requirements.NoError(err)

	visualBackend := backend.Visual()
	vector := make([]float32, visualDimension)
	vector[0] = 1
	requirements.NoError(visualBackend.PutUnpublished(ctx, "visual-token-1", vector))

	hits, err := visualBackend.Search(ctx, visual.SearchRequest{
		GenerationID: 7, Vector: vector, Limit: 10, SourceID: 3, MIMEPrefix: "image/",
	})
	requirements.NoError(err)
	requirements.Len(hits, 1)
	assertions.Equal(visual.VectorToken("visual-token-1"), hits[0].Token)
	assertions.InDelta(1, hits[0].Score, 1e-6)

	loaded, err := visualBackend.LoadOwnerVector(ctx, 7, visual.Owner{
		MessageID: 1, BlobHash: strings.Repeat("a", 64), MediaInputKey: "original",
	})
	requirements.NoError(err)
	assertions.Equal(vector, loaded)

	requirements.NoError(visualBackend.DeleteTokens(ctx, []visual.VectorToken{"visual-token-1"}))
	hits, err = visualBackend.Search(ctx, visual.SearchRequest{GenerationID: 7, Vector: vector, Limit: 10})
	requirements.NoError(err)
	assertions.Empty(hits)
}
