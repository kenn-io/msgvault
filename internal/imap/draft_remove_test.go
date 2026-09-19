package imap

import (
	"net"
	"strconv"
	"testing"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftRemoveExactUID(t *testing.T) {
	requirements := require.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
	})
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	defer func() { _ = client.Close() }()
	receipt, err := client.AppendDraft(t.Context(), "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\nbody\r\n"))
	requirements.NoError(err)
	requirements.NotZero(receipt.UID)
	observation, err := client.InspectDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
	requirements.NoError(err)
	requirements.True(observation.Present)
	requirements.True(observation.Draft)
	removed, err := client.RemoveDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
	requirements.NoError(err)
	requirements.True(removed.Complete)
	requirements.False(removed.Present)
}
