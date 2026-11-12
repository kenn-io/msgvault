package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func TestVoiceTranscriptEligibilitySearchRebuildDisableAndDelete(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newBeeperMediaFixture(t)
	st := f.Store
	audio := addBeeperAudio(t, st, f.Source.ID, f.ConvID, "voice", strings.Repeat("a", 64))
	mapping := audio.mapping("destination", "revision", "processing")
	mapping.TranscriptSHA256 = ""
	retainAudio(t, st, mapping, "occurrence")
	require.NoError(st.SetVoiceTranscriptSearch(t.Context(), true))
	pending, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(err)
	assert.Empty(pending)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE beeper_media_deliveries SET phase='done', coverage_state='transcribed' WHERE processing_key=?`), "processing")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE beeper_media_occurrences SET transcript_sha256=? WHERE attachment_id=?`), strings.Repeat("b", 64), audio.attachmentID)
	require.NoError(err)
	supplied, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(err)
	assert.Empty(supplied)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE beeper_media_occurrences SET transcript_sha256='' WHERE attachment_id=?`), audio.attachmentID)
	require.NoError(err)
	require.NoError(st.UpsertMessageBodyContext(t.Context(), audio.messageID, sql.NullString{String: "caption", Valid: true}, sql.NullString{}))
	mappings, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(err)
	require.Len(mappings, 1)
	require.NoError(st.SaveVoiceTranscript(t.Context(), mappings[0], "dinner Thursday"))
	mappingsAfter, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(err)
	assert.Empty(mappingsAfter)
	find := func() []int64 {
		ids, _, err := st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"dinner", "Thursday"}}, 10)
		require.NoError(err)
		return ids
	}
	assert.Equal([]int64{audio.messageID}, find())
	phrase := func() []int64 {
		ids, _, err := st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"dinner Thursday"}}, 10)
		require.NoError(err)
		return ids
	}
	bodySearcher, ok := query.NewEngine(st.DB(), st.IsPostgreSQL()).(query.MessageBodySearcher)
	require.True(ok)
	bodyHits, err := bodySearcher.SearchMessageBodies(t.Context(), &search.Query{TextTerms: []string{"dinner"}}, 10, 0)
	require.NoError(err)
	assert.Empty(bodyHits.Messages)
	longBody := strings.Repeat("padding ", 69000)
	require.NoError(st.UpsertMessageBodyContext(t.Context(), audio.messageID, sql.NullString{String: longBody, Valid: true}, sql.NullString{}))
	require.NoError(st.IndexStoredMessageContext(t.Context(), audio.messageID))
	assert.Equal([]int64{audio.messageID}, find())
	assert.Equal([]int64{audio.messageID}, phrase())
	_, err = st.RebuildFTSContext(t.Context(), nil)
	require.NoError(err)
	assert.Equal([]int64{audio.messageID}, find())
	assert.Equal([]int64{audio.messageID}, phrase())
	require.NoError(st.SetVoiceTranscriptSearch(t.Context(), false))
	assert.Empty(find())
	require.NoError(st.SaveVoiceTranscript(t.Context(), mappings[0], "dinner Thursday"))
	assert.Empty(find())
	require.NoError(st.SetVoiceTranscriptSearch(t.Context(), true))
	mappings, err = st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(err)
	require.Len(mappings, 1)
	require.NoError(st.SaveVoiceTranscript(t.Context(), mappings[0], ""))
	emptyMappings, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(err)
	assert.Empty(emptyMappings)
	assert.Empty(find())
	require.NoError(st.SaveVoiceTranscript(t.Context(), mappings[0], "dinner Thursday"))
	assert.Equal([]int64{audio.messageID}, find())
	// Saved speech belongs to the message and survives attachment removal.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM attachments WHERE id=?`), audio.attachmentID)
	require.NoError(err)
	require.NoError(st.IndexStoredMessageContext(t.Context(), audio.messageID))
	assert.Equal([]int64{audio.messageID}, find())
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM messages WHERE id=?`), audio.messageID)
	require.NoError(err)
	assert.Empty(find())
}
