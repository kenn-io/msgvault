package imap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReviewFilteredDeletedMailboxRelocatesSnapshot(t *testing.T) {
	for _, mode := range []string{"deleted selected", "listed skipped", "absent skipped", "outside saved allow list"} {
		t.Run(mode, func(t *testing.T) {
			addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Drafts": 0, "Sent": 0, "Trash": 0})
			testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("Message-ID: <same@example.com>\r\n\r\ndraftobsoleteword\r\n"))
			testutil.AppendIMAPRawMessage(t, user, "Sent", []byte("Message-ID: <same@example.com>\r\n\r\nsentfinalword\r\n"))
			first := newTestClient(t, addr)
			listAllMessages(t, first)
			saved := first.ObservedFolderStates()
			require.NoError(t, first.Close())
			if mode != "listed skipped" {
				require.NoError(t, user.Delete("Drafts"))
			}
			exclude := []string{"Trash"}
			if mode == "listed skipped" || mode == "absent skipped" {
				exclude = append(exclude, "Drafts")
			}
			client := newTestClient(t, addr, WithFolderStates(saved), WithFolderFilter(nil, exclude), WithFilteredDeletionReconciliation(true),
				WithRelocationCandidateLoader(func(_ context.Context, lost []string) ([]RelocationCandidate, error) {
					assert.Equal(t, []string{"Drafts|1"}, lost)
					return []RelocationCandidate{{Target: gmail.MessageRelocationTarget{InternalID: 1, SourceID: 1, SourceMessageID: "Drafts|1", RFC822MessageID: "same@example.com"}, Mailbox: "Sent", UIDValidity: saved["Sent"].UIDValidity, UID: 1}}, nil
				}))
			if mode == "outside saved allow list" {
				client.config.Folders = []string{"INBOX", "Sent"}
			}
			ids := listAllMessages(t, client)
			target, ok := client.MessageRelocationTarget("Sent|1")
			if mode != "deleted selected" {
				assert.False(t, ok)
				assert.Empty(t, ids)
				return
			}
			require.True(t, ok)
			assert.Equal(t, "Drafts|1", target.SourceMessageID)
			assert.Equal(t, "Sent|1", target.NewSourceMessageID)
			assert.Equal(t, []string{"Sent|1"}, ids)
			raw, err := client.GetMessageRaw(t.Context(), "Sent|1")
			require.NoError(t, err)
			assert.Contains(t, string(raw.Raw), "sentfinalword")
		})
	}
}
