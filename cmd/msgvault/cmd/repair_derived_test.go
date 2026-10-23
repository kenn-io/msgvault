package cmd

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFormatRepairDerivedSummaryReportsMessageMetadata(t *testing.T) {
	got := formatRepairDerivedSummary("discord/example", &rederive.Summary{
		MessagesScanned:          3,
		MessageMetadataRewritten: 2,
		AttachmentsTagged:        4,
		Duration:                 1250 * time.Millisecond,
	})
	assert.Equal(t,
		"discord/example: 3 messages scanned, 2 message metadata rewritten, 0 bodies rewritten, 4 attachments tagged (1s)\n",
		got,
	)
}

func TestRepairDerivedTargetsRejectsUnknownSourceType(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "inbox@example.net")
	require.NoError(err)
	saved := repairDerivedSourceTypes
	t.Cleanup(func() { repairDerivedSourceTypes = saved })

	repairDerivedSourceTypes = []string{"gmial"}
	_, err = repairDerivedTargets(st)
	require.ErrorContains(err, `no source of type "gmial" to repair`)

	repairDerivedSourceTypes = []string{"gmail"}
	targets, err := repairDerivedTargets(st)
	require.NoError(err)
	require.Len(targets, 1)
	for _, kind := range []string{"gmail", "beeper", "discord"} {
		src, err := st.GetOrCreateSource(kind, "inbox@example.net")
		require.NoError(err)
		var out bytes.Buffer
		require.NoError(repairDerivedSource(t.Context(), &out, st, src))
		assert.Empty(t, out.String())
	}
}

func TestRepairDerivedSourceRebuildsEmail(t *testing.T) {
	for _, sourceType := range []string{"outlook", "beeper", "discord"} {
		t.Run(sourceType, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			older, err := st.GetOrCreateSource(sourceType, "older@example.net")
			require.NoError(err)
			conv, err := st.EnsureConversation(older.ID, "older", "")
			require.NoError(err)
			oldID, err := st.UpsertMessage(&store.Message{SourceID: older.ID, ConversationID: conv, SourceMessageID: "older", MessageType: "email"})
			require.NoError(err)
			require.NoError(st.UpsertMessageBody(oldID, sql.NullString{String: "stale", Valid: true}, sql.NullString{}))
			require.NoError(st.UpsertMessageRaw(oldID, []byte("Content-Type: text/plain\r\n\r\nMerged history")))
			src, err := st.GetOrCreateSource(sourceType, "repair@example.net")
			require.NoError(err)
			conv, err = st.EnsureConversation(src.ID, "old", "")
			require.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "old", MessageType: "email"})
			require.NoError(err)
			require.NoError(st.UpsertMessageRaw(id, []byte("Content-Type: text/plain\r\n\r\nRebuilt archive")))
			if _, version, ok := rederive.Lookup(sourceType); ok {
				require.NoError(st.MarkMigrationApplied(rederive.LedgerKey(sourceType, src.Identifier, version)))
			}
			require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "stale", Valid: true}, sql.NullString{}))
			var out bytes.Buffer
			if sourceType == "outlook" {
				require.NoError(repairDerivedSource(t.Context(), &out, st, src))
				assert.Contains(out.String(), "1 bodies rewritten")
			} else {
				_, _, err := rederive.RunIfStale(t.Context(), st, sourceType, src.Identifier, src.ID, nil)
				require.NoError(err)
			}
			body, err := st.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal("Rebuilt archive", body)
			_, ran, err := rederive.RunIfStale(t.Context(), st, sourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			assert.False(ran)
			if sourceType == "beeper" {
				assert.Less(oldID, id)
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: older.ID, IntoSourceID: src.ID})
				require.NoError(err)
				_, _, err = rederive.RunIfStale(t.Context(), st, sourceType, src.Identifier, src.ID, nil)
				require.NoError(err)
				body, err = st.GetMessageBodyText(oldID)
				require.NoError(err)
				assert.Equal("Merged history", body)
			}
		})
	}
}
