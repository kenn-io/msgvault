//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func rerankTestConfig() vector.Config {
	cfg := vector.Config{Enabled: true, Rerank: vector.RerankConfig{Enabled: true}}
	cfg.ApplyDefaults()
	return cfg
}

func TestNewRerankStageRefusesHostedEndpointWithoutKey(t *testing.T) {
	cfg := rerankTestConfig()

	_, err := newRerankStage(cfg, testutil.NewTestStore(t), "")
	require.Error(t, err)
	require.ErrorContains(t, err, "OPENROUTER_API_KEY")

	stage, err := newRerankStage(cfg, testutil.NewTestStore(t), "sk-or-test")
	require.NoError(t, err)
	assert.Equal(t, "cohere/rerank-4-pro", stage.Model)
	assert.Equal(t, 50, stage.Candidates)
}

func TestNewRerankStageAllowsKeylessLocalEndpoint(t *testing.T) {
	cfg := vector.Config{Enabled: true, Rerank: vector.RerankConfig{
		Enabled: true, Endpoint: "http://127.0.0.1:8000/v1", Model: "local-reranker",
	}}
	cfg.ApplyDefaults()

	stage, err := newRerankStage(cfg, testutil.NewTestStore(t), "")
	require.NoError(t, err)
	assert.Equal(t, "local-reranker", stage.Model)
}

func TestRerankCandidateTextsUseEmbeddingPreprocessing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "user@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "thread-1", "email_thread", "Parcel")
	require.NoError(err)
	sentAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "parcel-1",
		MessageType:     "email",
		Subject:         sql.NullString{String: "Parcel delivery", Valid: true},
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(messageID, sql.NullString{
		String: "Your parcel arrives Tuesday.\n\nOn Mon, Sep 1, 2026, Sender wrote:\n> quoted earlier message",
		Valid:  true,
	}, sql.NullString{}))

	cfg := rerankTestConfig()
	texts := rerankCandidateTexts(st, embeddingPreprocessConfig(cfg), 4000)
	got, err := texts(context.Background(), []int64{messageID, 999_999})
	require.NoError(err)
	require.Len(got, 2)
	assert.Contains(got[0], "Parcel delivery", "subject is included")
	assert.Contains(got[0], "Your parcel arrives Tuesday.")
	assert.NotContains(got[0], "quoted earlier message", "quoted replies are stripped like the embedder does")
	assert.Empty(got[1], "a message that no longer exists yields no text")

	capped := rerankCandidateTexts(st, embeddingPreprocessConfig(cfg), 10)
	got, err = capped(context.Background(), []int64{messageID})
	require.NoError(err)
	assert.Len([]rune(got[0]), 10, "max_candidate_chars caps each candidate")
}
