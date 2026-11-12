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

func TestVoiceTranscriptSearchLifecycle(t *testing.T) {
	f := newBeeperMediaFixture(t)
	st := f.Store
	audio := addBeeperAudio(t, st, f.Source.ID, f.ConvID, "voice", strings.Repeat("a", 64))
	mapping := audio.mapping("destination", "revision", "processing")
	mapping.TranscriptSHA256 = ""
	retainAudio(t, st, mapping, "occurrence")
	require.NoError(t, st.SetVoiceTranscriptSearch(t.Context(), true))
	pending, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(t, err)
	assert.Empty(t, pending)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE beeper_media_deliveries SET phase='done', coverage_state='transcribed' WHERE processing_key=?`), "processing")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE beeper_media_occurrences SET transcript_sha256=? WHERE attachment_id=?`), strings.Repeat("b", 64), audio.attachmentID)
	require.NoError(t, err)
	supplied, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(t, err)
	assert.Empty(t, supplied)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE beeper_media_occurrences SET transcript_sha256='' WHERE attachment_id=?`), audio.attachmentID)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBodyContext(t.Context(), audio.messageID, sql.NullString{String: "caption", Valid: true}, sql.NullString{}))
	mappings, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(t, err)
	require.Len(t, mappings, 1)
	require.NoError(t, st.UpsertMessageRawWithFormat(audio.messageID, []byte(strings.Replace(string(audio.raw), `"attachments":`, `"text":"changed caption","attachments":`, 1)), "beeper_json"))
	require.NoError(t, st.SaveVoiceTranscript(t.Context(), mappings[0], "dinner Thursday"))
	mappingsAfter, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(t, err)
	assert.Empty(t, mappingsAfter)
	find := func() []int64 {
		ids, _, err := st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"dinner", "Thursday"}}, 10)
		require.NoError(t, err)
		return ids
	}
	assert.Equal(t, []int64{audio.messageID}, find())
	metadata, err := st.GetSearchMetadata(t.Context(), []int64{audio.messageID}, search.Parse("caption"), true)
	require.NoError(t, err)
	if !st.IsPostgreSQL() {
		assert.Contains(t, metadata[audio.messageID].MatchSnippet, "caption")
	}
	bodySearcher, ok := query.NewEngine(st.DB(), st.IsPostgreSQL()).(query.MessageBodySearcher)
	require.True(t, ok)
	bodyHits, err := bodySearcher.SearchMessageBodies(t.Context(), &search.Query{TextTerms: []string{"dinner"}}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, bodyHits.Messages)
	longBody := strings.Repeat("padding ", 69000)
	require.NoError(t, st.UpsertMessageBodyContext(t.Context(), audio.messageID, sql.NullString{String: longBody, Valid: true}, sql.NullString{}))
	require.NoError(t, st.IndexStoredMessageContext(t.Context(), audio.messageID))
	assert.Equal(t, []int64{audio.messageID}, find())
	_, err = st.RebuildFTSContext(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, []int64{audio.messageID}, find())
	require.NoError(t, st.SetVoiceTranscriptSearch(t.Context(), false))
	assert.Empty(t, find())
	require.NoError(t, st.SaveVoiceTranscript(t.Context(), mappings[0], "dinner Thursday"))
	assert.Empty(t, find())
	require.NoError(t, st.SetVoiceTranscriptSearch(t.Context(), true))
	mappings, err = st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(t, err)
	require.Len(t, mappings, 1)
	require.NoError(t, st.SaveVoiceTranscript(t.Context(), mappings[0], ""))
	emptyMappings, err := st.VoiceTranscriptMappings(t.Context(), "destination", 0, 8)
	require.NoError(t, err)
	assert.Empty(t, emptyMappings)
	assert.Empty(t, find())
	require.NoError(t, st.SaveVoiceTranscript(t.Context(), mappings[0], "dinner Thursday"))
	assert.Equal(t, []int64{audio.messageID}, find())
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM attachments WHERE id=?`), audio.attachmentID)
	require.NoError(t, err)
	require.NoError(t, st.IndexStoredMessageContext(t.Context(), audio.messageID))
	assert.Equal(t, []int64{audio.messageID}, find())
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM messages WHERE id=?`), audio.messageID)
	require.NoError(t, err)
	assert.Empty(t, find())
}
