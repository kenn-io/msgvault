package imap

import (
	"bytes"
	"context"
	"encoding/base64"
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

func TestAppendDraftEncodedSizeLimit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	raw := []byte("From: alice@example.com\r\n\r\nbody\r\n")
	limit := uint32(len(raw))
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}, emersionimap.CapAppendLimit: {}},
		AppendLimit:        &limit,
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.AppendDraft(t.Context(), "Drafts", raw)
	requirements.NoError(err)
	assertions.Equal(DraftStateCreated, result.State)

	tooLarge := append(append([]byte(nil), raw...), 'x')
	result, err = client.AppendDraft(t.Context(), "Drafts", tooLarge)
	requirements.Error(err)
	assertions.Equal(DraftStateRejected, result.State)
	assertions.Equal("message_too_large", result.Code)
}

func TestAppendDraftEncodedSizeLimitBase64Expansion(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const attachmentBytes = 64
	content := bytes.Repeat([]byte{'x'}, attachmentBytes)
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Archived",
		Body: "review", Attachments: []ForwardAttachment{{
			Filename: "payload.bin", ContentType: "application/octet-stream", Content: content,
		}},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	encoded := base64.StdEncoding.EncodeToString(content)
	wireEncoded := []byte(encoded[:76] + "\r\n" + encoded[76:])
	requirements.Contains(string(draft.Raw), string(wireEncoded))
	unencodedRaw := bytes.Replace(draft.Raw, wireEncoded, content, 1)
	assertions.Len(encoded, base64.StdEncoding.EncodedLen(attachmentBytes))
	assertions.Greater(len(draft.Raw), len(unencodedRaw))
	limit := uint32(len(unencodedRaw))
	assertions.Less(len(content), int(limit))
	assertions.Greater(len(draft.Raw), int(limit))

	addr, user := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}, emersionimap.CapAppendLimit: {}},
		AppendLimit:        &limit,
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.AppendDraft(t.Context(), "Drafts", draft.Raw)
	requirements.Error(err)
	assertions.Equal(DraftStateRejected, result.State)
	assertions.Equal("message_too_large", result.Code)
	status, err := user.Status("Drafts", &emersionimap.StatusOptions{NumMessages: true, UIDNext: true})
	requirements.NoError(err)
	requirements.NotNil(status.NumMessages)
	assertions.Zero(*status.NumMessages)
	assertions.Equal(emersionimap.UID(1), status.UIDNext)
	t.Logf("base64 expansion: unencoded attachment bytes=%d, encoded bytes=%d, unencoded raw=%d, encoded raw=%d, limit=%d, APPEND messages=%d", attachmentBytes, len(wireEncoded), len(unencodedRaw), len(draft.Raw), limit, *status.NumMessages)
}

func TestAppendDraftEncodedSizeLimitStatusFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}, emersionimap.CapAppendLimit: {}},
		StatusErrorMailbox: "Drafts",
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.AppendDraft(t.Context(), "Drafts", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	requirements.Error(err)
	assertions.Equal(DraftStateRejected, result.State)
	assertions.Equal("append_limit_unavailable", result.Code)
}

func TestAppendDraftEncodedSizeLimitNilStatus(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}, emersionimap.CapAppendLimit: {}},
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.AppendDraft(t.Context(), "Drafts", []byte("From: alice@example.com\r\n\r\nbody\r\n"))
	requirements.NoError(err)
	assertions.Equal(DraftStateCreated, result.State)
}

func TestAppendDraftEncodedSizeLimitFromStatus(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	raw := []byte("From: alice@example.com\r\n\r\nbody\r\n")
	limit := uint32(len(raw) - 1)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox:  map[string]int{"Drafts": 0},
		Caps:                emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}, emersionimap.CapAppendLimit: {}},
		StatusAppendLimit:   &limit,
		StatusAppendMailbox: "Drafts",
	})
	host, port, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	portNumber, err := strconv.Atoi(port)
	requirements.NoError(err)
	client := NewClient(&Config{Host: host, Port: portNumber, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.AppendDraft(t.Context(), "Drafts", raw)
	requirements.Error(err)
	assertions.Equal(DraftStateRejected, result.State)
	assertions.Equal("message_too_large", result.Code)
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
