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
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return NewClient(&Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
}

// appendWithFlags appends a message to a mailbox with specific flags.
func appendWithFlags(t *testing.T, addr, mailbox string, raw []byte, flags []emersionimap.Flag) uint32 {
	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	cmd := client.Append(mailbox, int64(len(raw)), &emersionimap.AppendOptions{Flags: flags})
	done := make(chan struct{})
	var appendData *emersionimap.AppendData
	var appendErr error
	go func() {
		defer close(done)
		_, appendErr = io.Copy(cmd, bytes.NewReader(raw))
		if appendErr == nil {
			appendErr = cmd.Close()
		}
		if appendErr == nil {
			appendData, appendErr = cmd.Wait()
		}
	}()
	<-done
	require.NoError(t, appendErr)
	if appendData == nil {
		t.Fatal("appendWithFlags: no data returned")
	}
	return uint32(appendData.UID)
}

// TestInspectDraftClassifiesRemoteStateInIsolation verifies each of the four
// remote states: present, absent, flag_missing, and changed.
func TestInspectDraftClassifiesRemoteStateInIsolation(t *testing.T) {
	caps := emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {},
		emersionimap.CapUIDPlus:   {},
	}
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               caps,
	})

	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)

	// Append the draft.
	cl := newDraftTestClient(t, addr)
	result, err := cl.AppendDraft(context.Background(), "Drafts", raw)
	require.NoError(t, err)
	uid := result.UID
	uidvalidity := result.UIDValidity

	target := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: uidvalidity,
		UID:         uid,
		RawSHA256:   digest,
	}

	t.Run("present", func(t *testing.T) {
		cl := newDraftTestClient(t, addr)
		r, err := cl.InspectDraft(context.Background(), target)
		require.NoError(t, err)
		assert.Equal(t, DraftRemotePresent, r.State)
		assert.Equal(t, uidvalidity, r.UIDValidity)
	})

	t.Run("absent", func(t *testing.T) {
		cl := newDraftTestClient(t, addr)
		absentTarget := target
		absentTarget.UID = uid + 9999
		r, err := cl.InspectDraft(context.Background(), absentTarget)
		require.NoError(t, err)
		assert.Equal(t, DraftRemoteAbsent, r.State)
	})

	t.Run("epoch_guard_before_fetch", func(t *testing.T) {
		cl := newDraftTestClient(t, addr)
		wrongEpochTarget := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity + 1,
			UID:         uid,
			RawSHA256:   digest,
		}
		_, err := cl.InspectDraft(context.Background(), wrongEpochTarget)
		require.Error(t, err)
		var appendErr *DraftAppendError
		require.ErrorAs(t, err, &appendErr)
		assert.Equal(t, "uidvalidity_changed", appendErr.Code)
	})

	t.Run("RemoveDraft_only_for_present", func(t *testing.T) {
		// Re-append since present test above didn't remove.
		cl := newDraftTestClient(t, addr)
		r, err := cl.RemoveDraft(context.Background(), target)
		require.NoError(t, err)
		assert.Equal(t, DraftRemotePresent, r.State)

		// Now absent - second call should return absent with no error.
		cl2 := newDraftTestClient(t, addr)
		r2, err2 := cl2.RemoveDraft(context.Background(), target)
		require.NoError(t, err2)
		assert.Equal(t, DraftRemoteAbsent, r2.State)
	})
}

// TestInspectDraftReportsFlagLossAndContentChange verifies flag_missing
// and changed classifications.
func TestInspectDraftReportsFlagLossAndContentChange(t *testing.T) {
	caps := emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {},
		emersionimap.CapUIDPlus:   {},
	}
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               caps,
	})

	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)

	// Append with \Seen only (no \Draft).
	noFlagUID := appendWithFlags(t, addr, "Drafts", raw, []emersionimap.Flag{emersionimap.FlagSeen})
	require.NotZero(t, noFlagUID)

	// We need the UIDValidity; get it from a fresh inspect that we know is absent.
	cl := newDraftTestClient(t, addr)
	presentResult, err := cl.AppendDraft(context.Background(), "Drafts", raw)
	require.NoError(t, err)
	uidvalidity := presentResult.UIDValidity

	t.Run("flag_missing", func(t *testing.T) {
		target := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         noFlagUID,
			RawSHA256:   digest,
		}
		cl := newDraftTestClient(t, addr)
		r, err := cl.InspectDraft(context.Background(), target)
		require.NoError(t, err)
		assert.Equal(t, DraftRemoteFlagMissing, r.State)
	})

	t.Run("changed", func(t *testing.T) {
		// The draft just appended with AppendDraft has the right UID but
		// we supply a different digest.
		differentRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft-test@example.com>\r\n\r\nDIFFERENT\r\n")
		target := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         presentResult.UID,
			RawSHA256:   sha256.Sum256(differentRaw), // intentionally wrong
		}
		cl := newDraftTestClient(t, addr)
		r, err := cl.InspectDraft(context.Background(), target)
		require.NoError(t, err)
		assert.Equal(t, DraftRemoteChanged, r.State)
		// The actual digest from the server matches original.
		assert.Equal(t, digest, r.RawSHA256)
	})
}

// TestRemoveDraftRefusesEpochMismatch verifies uidvalidity_changed is returned
// before any FETCH or STORE.
func TestRemoveDraftRefusesEpochMismatch(t *testing.T) {
	caps := emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {},
		emersionimap.CapUIDPlus:   {},
	}
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               caps,
	})

	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)

	cl := newDraftTestClient(t, addr)
	result, err := cl.AppendDraft(context.Background(), "Drafts", raw)
	require.NoError(t, err)

	wrongTarget := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: result.UIDValidity + 1,
		UID:         result.UID,
		RawSHA256:   digest,
	}

	var appendErr *DraftAppendError
	cl2 := newDraftTestClient(t, addr)
	_, err = cl2.RemoveDraft(context.Background(), wrongTarget)
	require.Error(t, err)
	require.ErrorAs(t, err, &appendErr)
	assert.Equal(t, "uidvalidity_changed", appendErr.Code)

	// The original message must still be present (no STORE happened).
	cl3 := newDraftTestClient(t, addr)
	goodTarget := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: result.UIDValidity,
		UID:         result.UID,
		RawSHA256:   digest,
	}
	r, err := cl3.InspectDraft(context.Background(), goodTarget)
	require.NoError(t, err)
	assert.Equal(t, DraftRemotePresent, r.State, "original message must still be present after epoch refusal")
}

// TestRemoveDraftIsIdempotentForAbsentUID verifies that a second RemoveDraft
// call for an already-absent UID returns absent with no error.
func TestRemoveDraftIsIdempotentForAbsentUID(t *testing.T) {
	caps := emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {},
		emersionimap.CapUIDPlus:   {},
	}
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               caps,
	})

	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)

	cl := newDraftTestClient(t, addr)
	result, err := cl.AppendDraft(context.Background(), "Drafts", raw)
	require.NoError(t, err)

	target := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: result.UIDValidity,
		UID:         result.UID,
		RawSHA256:   digest,
	}

	// First remove: present.
	cl2 := newDraftTestClient(t, addr)
	r, err := cl2.RemoveDraft(context.Background(), target)
	require.NoError(t, err)
	assert.Equal(t, DraftRemotePresent, r.State)

	// Second remove: absent, no error.
	cl3 := newDraftTestClient(t, addr)
	r2, err := cl3.RemoveDraft(context.Background(), target)
	require.NoError(t, err)
	assert.Equal(t, DraftRemoteAbsent, r2.State)
}
