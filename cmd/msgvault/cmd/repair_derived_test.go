package cmd

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"

	"go.kenn.io/msgvault/internal/rederive"
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

func TestRepairDerivedSourceDerivesEmailLinkage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "repair@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Repair")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "mail", MessageType: "email"}, RawMIME: []byte("Message-ID: <repair@example.test>\r\n\r\n")})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM email_link_key_state WHERE message_id = ?`), id)
	require.NoError(err)
	var out bytes.Buffer
	require.NoError(repairDerivedSource(t.Context(), &out, st, src))
	assert.Contains(out.String(), "email linkage derived for 1 messages")
	var outcome string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT outcome FROM email_link_key_state WHERE message_id = ?`), id).Scan(&outcome))
	assert.Equal("complete", outcome)
}
