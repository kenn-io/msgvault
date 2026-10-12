package cmd

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInboxIMAPSyncConversionPreservesFlagsPresence(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "known empty"}[known], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("imap", "imaps://synthetic@example.test@imap.example.test:993")
			requirements.NoError(err)
			conversation, err := st.EnsureConversation(source.ID, "synthetic", "Synthetic flags")
			requirements.NoError(err)
			_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "synthetic-message", ConversationID: conversation, MessageType: "email"})
			requirements.NoError(err)
			var flags []string
			if known {
				flags = []string{}
			}
			run, err := st.StartSync(source.ID, "full")
			requirements.NoError(err)
			requirements.NoError(st.CompleteSync(run, "synthetic-cursor"))
			requirements.NoError(applyIMAPMailboxDeltas(t.Context(), st, source, run, []imaplib.MailboxDelta{{Mailbox: "INBOX", State: imaplib.FolderState{UIDValidity: 17, UIDNext: 2}}}, []imaplib.MembershipObservation{{Mailbox: "INBOX", UIDValidity: 17, UID: 1, CanonicalSourceMessageID: "synthetic-message", Flags: flags}}))
			var encoded string
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=?`), source.ID).Scan(&encoded))
			var got []string
			requirements.NoError(json.Unmarshal([]byte(encoded), &got))
			assertions.Equal(flags, got)
			assertions.Equal(!known, got == nil)
		})
	}
}
