package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"strconv"
	"testing"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

const testDraftRaw = "From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft-test@example.com>\r\n\r\nTest draft body\r\n"

func newDraftTestClient(t *testing.T, addr string) *Client {
	requirements := require.New(t)

	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	return NewClient(&Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
}

func appendWithFlags(t *testing.T, addr, mailbox string, raw []byte, flags []emersionimap.Flag) uint32 {
	requirements := require.New(t)

	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	requirements.NoError(err)
	defer func() { _ = client.Close() }()
	requirements.NoError(client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	command := client.Append(mailbox, int64(len(raw)), &emersionimap.AppendOptions{Flags: flags})
	_, err = io.Copy(command, bytes.NewReader(raw))
	requirements.NoError(err)
	requirements.NoError(command.Close())
	data, err := command.Wait()
	requirements.NoError(err)
	requirements.NotNil(data)
	return uint32(data.UID)
}

func TestInspectDraftClassifiesRemoteStateInIsolation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps: emersionimap.CapSet{
			emersionimap.CapIMAP4rev1: {},
			emersionimap.CapUIDPlus:   {},
		},
	})
	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)
	client := newDraftTestClient(t, addr)
	appendResult, err := client.AppendDraft(context.Background(), "Drafts", raw)
	requirements.NoError(err)
	target := DraftTarget{
		Mailbox: "Drafts", UIDValidity: appendResult.UIDValidity,
		UID: appendResult.UID, RawSHA256: digest,
	}

	t.Run("present", func(t *testing.T) {
		result, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), target)
		requirements.NoError(err)
		assertions.Equal(DraftRemotePresent, result.State)
		assertions.Equal(appendResult.UIDValidity, result.UIDValidity)
	})
	t.Run("absent", func(t *testing.T) {
		absent := target
		absent.UID++
		result, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), absent)
		requirements.NoError(err)
		assertions.Equal(DraftRemoteAbsent, result.State)
	})
	t.Run("epoch guard", func(t *testing.T) {
		wrongEpoch := target
		wrongEpoch.UIDValidity++
		_, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), wrongEpoch)
		requirements.Error(err)
		var appendErr *DraftAppendError
		requirements.ErrorAs(err, &appendErr)
		assertions.Equal("uidvalidity_changed", appendErr.Code)
	})
}

func TestInspectDraftReportsFlagLossAndContentChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps: emersionimap.CapSet{
			emersionimap.CapIMAP4rev1: {},
			emersionimap.CapUIDPlus:   {},
		},
	})
	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)
	flagMissingUID := appendWithFlags(t, addr, "Drafts", raw, []emersionimap.Flag{emersionimap.FlagSeen})
	client := newDraftTestClient(t, addr)
	appendResult, err := client.AppendDraft(context.Background(), "Drafts", raw)
	requirements.NoError(err)

	flagMissing, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), DraftTarget{
		Mailbox: "Drafts", UIDValidity: appendResult.UIDValidity,
		UID: flagMissingUID, RawSHA256: digest,
	})
	requirements.NoError(err)
	assertions.Equal(DraftRemoteFlagMissing, flagMissing.State)

	different := sha256.Sum256([]byte("different content"))
	changed, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), DraftTarget{
		Mailbox: "Drafts", UIDValidity: appendResult.UIDValidity,
		UID: appendResult.UID, RawSHA256: different,
	})
	requirements.NoError(err)
	assertions.Equal(DraftRemoteChanged, changed.State)
	assertions.Equal(digest, changed.RawSHA256)
}
