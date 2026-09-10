package imap

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAppendDraft(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts*2026": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.AppendDraft(t.Context(), "Drafts*2026", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	requirements.NoError(err)
	assertions.Equal(DraftStateCreated, result.State)
	assertions.Positive(result.UID)
	assertions.Positive(result.UIDValidity)
}

func TestAppendDraftRequiresUIDPlus(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}},
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.AppendDraft(t.Context(), "Drafts", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	requirements.Error(err)
	assertions.Equal("uidplus_required", result.Code)
}

func TestAppendDraftAcceptedWithoutReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		ReceiptlessAppend:  true,
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.AppendDraft(t.Context(), "Drafts", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	requirements.Error(err)
	assertions.Equal("accepted_unidentified", result.Code)
}

func TestAppendDraftCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := NewClient(&Config{Host: "127.0.0.1", Port: 1, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	result, err := client.AppendDraft(ctx, "Drafts", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	require.Error(t, err)
	assert.Equal(t, "cancelled", result.Code)
}

func TestAppendDraftConnectionFailureIsRejected(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	client := NewClient(&Config{Host: "127.0.0.1", Port: 1, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	result, err := client.AppendDraft(ctx, "Drafts", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	requirements.Error(err)
	assertions.Equal(DraftStateRejected, result.State)
	assertions.Equal("connection_failed", result.Code)
	assertions.Equal("connection_failed", err.Error())
}

func TestValidateDraftMailbox(t *testing.T) {
	for _, value := range []string{"Drafts*2026", " mailbox "} {
		require.NoError(t, ValidateDraftMailbox(value))
	}
	for _, value := range []string{"", "  ", "Drafts\r\nX: bad"} {
		require.Error(t, ValidateDraftMailbox(value))
	}
}
