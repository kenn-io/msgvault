package imap

import (
	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestValidateArchiveMailboxUsesExactSelectableExistingMailbox(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServerWithSpecialUse(t,
		map[string]int{"INBOX": 1, "Saved Mail": 1, "Container": 0},
		map[string][]imapapi.MailboxAttr{"Container": {imapapi.MailboxAttrNoSelect}})
	client := newTestClient(t, addr)
	require.NoError(t, client.ValidateArchiveMailbox(t.Context(), "Saved Mail"))
	for _, invalid := range []string{"Missing Mail", "Container", "INBOX", "inbox", "", "Saved\nMail", string([]byte{0xff})} {
		err := client.ValidateArchiveMailbox(t.Context(), invalid)
		require.Error(t, err, "mailbox %q", invalid)
		if err != nil {
			assert.ErrorContains(t, err, "archive mailbox")
		}
	}
}
