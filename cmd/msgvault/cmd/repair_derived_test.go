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
}

func TestRepairDerivedSourceRebuildsEmail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "repair@example.net")
	require.NoError(err)
	var out bytes.Buffer
	require.NoError(repairDerivedSource(t.Context(), &out, st, src))
	assert.Empty(out.String())
	conv, err := st.EnsureConversation(src.ID, "old", "")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "old", MessageType: "email"})
	require.NoError(err)
	require.NoError(st.UpsertMessageRaw(id, []byte("Content-Type: text/plain\r\n\r\nRebuilt archive")))
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "stale", Valid: true}, sql.NullString{}))
	require.NoError(repairDerivedSource(t.Context(), &out, st, src))
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Rebuilt archive", body)
}
