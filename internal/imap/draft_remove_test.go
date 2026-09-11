package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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

	t.Run("RemoveDraft_requires_conditional_store_for_present", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		cl := newDraftTestClient(t, addr)
		r, err := cl.RemoveDraft(context.Background(), target)
		require.Error(err)
		var appendErr *DraftAppendError
		require.ErrorAs(err, &appendErr)
		assert.Equal("conditional_store_required", appendErr.Code)
		assert.Equal(DraftRemotePresent, r.State)
		inspect, inspectErr := cl.InspectDraft(context.Background(), target)
		require.NoError(inspectErr)
		assert.Equal(DraftRemotePresent, inspect.State)
	})

	t.Run("RemoveDraft_flag_missing_does_not_expunge", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		// Append without \Draft flag so RemoveDraft sees flag_missing.
		noFlagUID := appendWithFlags(t, addr, "Drafts", raw, []emersionimap.Flag{emersionimap.FlagSeen})
		require.NotZero(noFlagUID)
		flagMissingTarget := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         noFlagUID,
			RawSHA256:   digest,
		}
		cl := newDraftTestClient(t, addr)
		r, err := cl.RemoveDraft(context.Background(), flagMissingTarget)
		require.NoError(err)
		assert.Equal(DraftRemoteFlagMissing, r.State, "flag_missing must not call expungeUIDLocked")
		// Message must still be present.
		cl2 := newDraftTestClient(t, addr)
		r2, err2 := cl2.InspectDraft(context.Background(), flagMissingTarget)
		require.NoError(err2)
		assert.NotEqual(DraftRemoteAbsent, r2.State, "message must not be expunged for flag_missing")
	})

	t.Run("RemoveDraft_changed_does_not_expunge", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		// Re-append a proper draft and supply wrong digest so RemoveDraft sees changed.
		cl := newDraftTestClient(t, addr)
		reappendResult, err := cl.AppendDraft(context.Background(), "Drafts", raw)
		require.NoError(err)
		differentDigest := sha256.Sum256([]byte("different content"))
		changedTarget := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         reappendResult.UID,
			RawSHA256:   differentDigest,
		}
		cl2 := newDraftTestClient(t, addr)
		r, err := cl2.RemoveDraft(context.Background(), changedTarget)
		require.NoError(err)
		assert.Equal(DraftRemoteChanged, r.State, "changed must not call expungeUIDLocked")
		// Message must still be present.
		goodTarget := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         reappendResult.UID,
			RawSHA256:   digest,
		}
		cl3 := newDraftTestClient(t, addr)
		r2, err2 := cl3.InspectDraft(context.Background(), goodTarget)
		require.NoError(err2)
		assert.Equal(DraftRemotePresent, r2.State, "message must not be expunged for changed")
	})
}

// TestInspectDraftReportsFlagLossAndContentChange verifies flag_missing
// and changed classifications, and that RemoveDraft refuses both cases
// without expunging the message.
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

	t.Run("flag_missing_RemoveDraft_refuses", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		target := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         noFlagUID,
			RawSHA256:   digest,
		}
		cl := newDraftTestClient(t, addr)
		r, err := cl.RemoveDraft(context.Background(), target)
		require.NoError(err)
		assert.Equal(DraftRemoteFlagMissing, r.State, "RemoveDraft must refuse flag_missing without expunging")
		// Message must still be present (expungeUIDLocked was not called).
		cl2 := newDraftTestClient(t, addr)
		r2, err2 := cl2.InspectDraft(context.Background(), target)
		require.NoError(err2)
		assert.NotEqual(DraftRemoteAbsent, r2.State, "message must survive RemoveDraft refusal for flag_missing")
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

	t.Run("changed_RemoveDraft_refuses", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		differentRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft-test@example.com>\r\n\r\nDIFFERENT\r\n")
		target := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         presentResult.UID,
			RawSHA256:   sha256.Sum256(differentRaw), // intentionally wrong digest
		}
		cl := newDraftTestClient(t, addr)
		r, err := cl.RemoveDraft(context.Background(), target)
		require.NoError(err)
		assert.Equal(DraftRemoteChanged, r.State, "RemoveDraft must refuse changed without expunging")
		// Message must still be present (expungeUIDLocked was not called).
		goodTarget := DraftTarget{
			Mailbox:     "Drafts",
			UIDValidity: uidvalidity,
			UID:         presentResult.UID,
			RawSHA256:   digest, // correct digest
		}
		cl2 := newDraftTestClient(t, addr)
		r2, err2 := cl2.InspectDraft(context.Background(), goodTarget)
		require.NoError(err2)
		assert.Equal(DraftRemotePresent, r2.State, "message must survive RemoveDraft refusal for changed")
	})
}

// TestRemoveDraftRefusesEpochMismatch verifies uidvalidity_changed is returned
// before any FETCH or STORE.
func TestRemoveDraftRefusesEpochMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	wrongTarget := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: result.UIDValidity + 1,
		UID:         result.UID,
		RawSHA256:   digest,
	}

	var appendErr *DraftAppendError
	cl2 := newDraftTestClient(t, addr)
	_, err = cl2.RemoveDraft(context.Background(), wrongTarget)
	require.Error(err)
	require.ErrorAs(err, &appendErr)
	assert.Equal("uidvalidity_changed", appendErr.Code)

	// The original message must still be present (no STORE happened).
	cl3 := newDraftTestClient(t, addr)
	goodTarget := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: result.UIDValidity,
		UID:         result.UID,
		RawSHA256:   digest,
	}
	r, err := cl3.InspectDraft(context.Background(), goodTarget)
	require.NoError(err)
	assert.Equal(DraftRemotePresent, r.State, "original message must still be present after epoch refusal")
}

// TestRemoveDraftIsIdempotentForAbsentUID verifies that a second RemoveDraft
// call for an already-absent UID returns absent with no error.
func TestRemoveDraftIsIdempotentForAbsentUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	target := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: result.UIDValidity,
		UID:         result.UID + 9999,
		RawSHA256:   digest,
	}

	// An absent UID remains idempotent even when the server lacks CONDSTORE.
	cl2 := newDraftTestClient(t, addr)
	r, err := cl2.RemoveDraft(context.Background(), target)
	require.NoError(err)
	assert.Equal(DraftRemoteAbsent, r.State)

	// The second remove is the same absent state.
	cl3 := newDraftTestClient(t, addr)
	r2, err := cl3.RemoveDraft(context.Background(), target)
	require.NoError(err)
	assert.Equal(DraftRemoteAbsent, r2.State)
}

// TestSelectedEpochBelongsToTheSelectionThatRemoves pins the invariant that
// makes RemoveDraft's epoch guard sound when selectMailbox short-circuits on an
// already-selected mailbox, which it does whenever one client inspects and then
// removes. The guard compares target.UIDValidity against c.selectedUIDValidity,
// and that field is only ever written in two shapes: set together with
// c.selectedMailbox from a SELECT response on the live connection
// (client.go:540-541, qresync.go:189-190), or zeroed together with it on every
// path that can invalidate a selection — connect (client.go:336-337), reconnect
// (client.go:499-500), a network error inside withConn (client.go:523-524), and
// Close (client.go:1844-1845).
//
// A cache hit therefore requires a non-empty c.selectedMailbox, which only the
// SELECT that also published the epoch can produce. UIDVALIDITY is reported at
// SELECT and does not change underneath a selected mailbox, so the cached value
// is the epoch of the selection the removal actually runs in, not an epoch
// carried over from some earlier one.
//
// Written to be read instead of re-derived: a reviewer asking whether a stale
// epoch can survive into a removal is asking whether this test can fail.
func TestSelectedEpochBelongsToTheSelectionThatRemoves(t *testing.T) {
	caps := emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {},
		emersionimap.CapUIDPlus:   {},
	}
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               caps,
	})

	// liveEpoch reads the mailbox's current UIDVALIDITY over an independent
	// connection, so the assertions compare against the server and not against
	// another copy of the client's own cache.
	liveEpoch := func(t *testing.T) uint32 {
		t.Helper()
		probe, err := imapclient.DialInsecure(addr, nil)
		require.NoError(t, err)
		defer func() { _ = probe.Close() }()
		require.NoError(t, probe.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
		selected, err := probe.Select("Drafts", nil).Wait()
		require.NoError(t, err)
		return selected.UIDValidity
	}

	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)

	t.Run("a cached selection carries that selection's own epoch", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		cl := newDraftTestClient(t, addr)
		appended, err := cl.AppendDraft(context.Background(), "Drafts", raw)
		require.NoError(err)
		target := DraftTarget{
			Mailbox: "Drafts", UIDValidity: appended.UIDValidity,
			UID: appended.UID, RawSHA256: digest,
		}

		// The inspection establishes the selection. This is the ordinary edit
		// and delete shape: one client, inspect then remove.
		inspected, err := cl.InspectDraft(context.Background(), target)
		require.NoError(err)
		require.Equal(DraftRemotePresent, inspected.State)
		require.Equal("Drafts", cl.selectedMailbox,
			"the inspection must leave the mailbox selected, so the removal hits the cache")
		require.Equal(liveEpoch(t), cl.selectedUIDValidity,
			"the cached epoch must be the one the server reported at SELECT")

		// The removal reuses that selection and validates against its epoch.
		removed, err := cl.RemoveDraft(context.Background(), target)
		require.Error(err)
		var appendErr *DraftAppendError
		require.ErrorAs(err, &appendErr)
		assert.Equal("conditional_store_required", appendErr.Code)
		assert.Equal(DraftRemotePresent, removed.State)
		assert.Equal(liveEpoch(t), cl.selectedUIDValidity,
			"reusing the selection cannot move the epoch it validated against")
	})

	t.Run("every invalidating path clears the mailbox and the epoch together", func(t *testing.T) {
		for _, invalidate := range []struct {
			name string
			run  func(t *testing.T, cl *Client)
		}{
			{
				name: "a network error inside withConn",
				run: func(t *testing.T, cl *Client) {
					err := cl.withConn(context.Background(), func(*imapclient.Client) error {
						return io.ErrUnexpectedEOF
					})
					require.Error(t, err)
				},
			},
			{
				name: "reconnect",
				run: func(t *testing.T, cl *Client) {
					cl.mu.Lock()
					defer cl.mu.Unlock()
					require.NoError(t, cl.reconnect(context.Background()))
				},
			},
			{
				name: "Close",
				run: func(t *testing.T, cl *Client) {
					_ = cl.Close()
				},
			},
		} {
			t.Run(invalidate.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				cl := newDraftTestClient(t, addr)
				appended, err := cl.AppendDraft(context.Background(), "Drafts", raw)
				require.NoError(err)
				_, err = cl.InspectDraft(context.Background(), DraftTarget{
					Mailbox: "Drafts", UIDValidity: appended.UIDValidity,
					UID: appended.UID, RawSHA256: digest,
				})
				require.NoError(err)
				require.NotEmpty(cl.selectedMailbox)
				require.NotZero(cl.selectedUIDValidity)

				invalidate.run(t, cl)

				assert.Empty(cl.selectedMailbox,
					"an invalidated connection must leave no mailbox cached to short-circuit on")
				assert.Zero(cl.selectedUIDValidity,
					"an invalidated connection must leave no epoch behind to validate against")
			})
		}
	})

	t.Run("a changed epoch is only ever observed through a new SELECT", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		cl := newDraftTestClient(t, addr)
		appended, err := cl.AppendDraft(context.Background(), "Drafts", raw)
		require.NoError(err)
		target := DraftTarget{
			Mailbox: "Drafts", UIDValidity: appended.UIDValidity,
			UID: appended.UID, RawSHA256: digest,
		}
		_, err = cl.InspectDraft(context.Background(), target)
		require.NoError(err)
		require.Equal(appended.UIDValidity, cl.selectedUIDValidity)

		// Recreate the mailbox, which is how the epoch changes, then drop the
		// connection the way any of the invalidating paths above would.
		recreate, err := imapclient.DialInsecure(addr, nil)
		require.NoError(err)
		require.NoError(recreate.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
		require.NoError(recreate.Delete("Drafts").Wait())
		require.NoError(recreate.Create("Drafts", nil).Wait())
		_ = recreate.Close()
		require.NotEqual(appended.UIDValidity, liveEpoch(t),
			"recreating the mailbox must change the epoch")
		require.NoError(cl.Close())

		// The next removal re-SELECTs, so it validates against the new epoch and
		// refuses rather than acting on a UID recorded under the old one.
		_, err = cl.RemoveDraft(context.Background(), target)
		require.Error(err)
		appendErr, ok := errors.AsType[*DraftAppendError](err)
		require.True(ok)
		assert.Equal("uidvalidity_changed", appendErr.Code)
		assert.Equal(liveEpoch(t), cl.selectedUIDValidity,
			"the refusal must have compared against the epoch of its own SELECT")
	})
}
