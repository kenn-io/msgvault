package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// draftLifecycleFixture extends draftReplyFixture by creating one registered
// IMAP draft via the existing draft-reply flow.
type draftLifecycleFixture struct {
	draftReplyFixture
	draftID     int64
	draftUID    uint32
	draftUIDVal uint32
}

func newDraftLifecycleFixture(t *testing.T) draftLifecycleFixture {
	t.Helper()
	f := newDraftReplyFixture(t)
	adapter := f.grantedAdapter()

	args := []string{
		"draft-reply",
		strconv.FormatInt(f.parentID, 10),
		"--from", testutil.IMAPTestUsername,
		"--body", "Initial draft body",
		"--json",
	}
	var events []api.CLIRunEvent
	err := adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: args}, func(ev api.CLIRunEvent) error {
		events = append(events, ev)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 1)

	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &result))
	draftID := int64(result["message_id"].(float64))
	require.Positive(t, draftID)

	var uid, uidval int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT uid, uidvalidity FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&uid, &uidval))

	return draftLifecycleFixture{
		draftReplyFixture: f,
		draftID:           draftID,
		draftUID:          uint32(uid),
		draftUIDVal:       uint32(uidval),
	}
}

func (f draftLifecycleFixture) runLifecycle(t *testing.T, args ...string) ([]api.CLIRunEvent, error) {
	t.Helper()
	adapter := f.grantedAdapter()
	var events []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args}, func(ev api.CLIRunEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

// isDraftTombstoned reports whether the message row carries a tombstone timestamp.
func isDraftTombstoned(t *testing.T, f draftLifecycleFixture, messageID int64) bool {
	t.Helper()
	var deletedAt sql.NullTime
	err := f.store.DB().QueryRow(f.store.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE id = ?
	`), messageID).Scan(&deletedAt)
	require.NoError(t, err)
	return deletedAt.Valid
}

// TestDraftGetReportsLocalAndRemoteState verifies that draft-get returns
// lifecycle==active, revision==1, and provider_status==present.
func TestDraftGetReportsLocalAndRemoteState(t *testing.T) {
	f := newDraftLifecycleFixture(t)
	events, err := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(t, err)
	require.Len(t, events, 1)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &result))
	assert.Equal(t, "active", result["lifecycle"])
	assert.Equal(t, float64(1), result["revision"])
	assert.Equal(t, imaplib.DraftRemotePresent, result["provider_status"])
}

// TestDraftDeleteRefusesNonDraftMessageInSameMailbox verifies that a message
// without an imap_drafts row returns draft_not_found without opening IMAP.
func TestDraftDeleteRefusesNonDraftMessageInSameMailbox(t *testing.T) {
	f := newDraftReplyFixture(t) // no draft created
	adapter := f.grantedAdapter()
	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.parentID, 10), "--revision=1"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(t, err)
	assert.Equal(t, "draft_not_found", err.Error())
}

// TestDraftEditReplacesRemoteCopyAndAdvancesRevision verifies that after a
// successful edit the revision advances to 2, the old message is tombstoned,
// and exactly one membership row exists for the source.
func TestDraftEditReplacesRemoteCopyAndAdvancesRevision(t *testing.T) {
	f := newDraftLifecycleFixture(t)
	events, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=Updated draft body", "--json")
	require.NoError(t, err)
	require.Len(t, events, 1)

	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &result))
	assert.Equal(t, "replaced", result["status"])
	assert.Equal(t, float64(2), result["revision"])

	// Verify revision in store.
	var revision int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT revision FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&revision))
	assert.Equal(t, int64(2), revision)

	// Old message must be tombstoned.
	assert.True(t, isDraftTombstoned(t, f, f.draftID))

	// Exactly one membership row for the source.
	var cnt int
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`), f.source.ID).Scan(&cnt))
	assert.Equal(t, 1, cnt)

	// Archived raw for the new current_message_id must contain the updated body.
	var newMessageID int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT current_message_id FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&newMessageID))
	rawData, rawErr := f.store.GetMessageRawContext(t.Context(), newMessageID)
	require.NoError(t, rawErr, "must be able to load archived raw for new draft")
	assert.Contains(t, string(rawData), "Updated draft body", "archived raw must contain the new body text")
}

// TestDraftEditRejectsStaleRevision verifies that a second edit at the same
// revision returns revision_conflict.
func TestDraftEditRejectsStaleRevision(t *testing.T) {
	f := newDraftLifecycleFixture(t)
	// First edit succeeds: revision 1 → 2.
	_, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=First edit")
	require.NoError(t, err)

	// Second edit at same revision must fail.
	_, err = f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=Second attempt")
	require.Error(t, err)
	assert.Equal(t, "revision_conflict", err.Error())
}

// TestDraftMutationRefusesAfterUIDValidityChange verifies that when the stored
// uidvalidity does not match the server epoch, the operation returns
// uidvalidity_changed and lifecycle stays active.
func TestDraftMutationRefusesAfterUIDValidityChange(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Corrupt the stored uidvalidity so InspectDraft detects an epoch mismatch.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts SET uidvalidity = uidvalidity + 9999 WHERE draft_id = ?
	`), f.draftID)
	require.NoError(t, err)

	_, err = f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(t, err)
	assert.Equal(t, "uidvalidity_changed", err.Error())

	// Lifecycle must remain unchanged.
	var lifecycle string
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle))
	assert.Equal(t, "active", lifecycle)
}

// TestDraftMutationRefusesChangedRemoteContent verifies that when the local
// raw digest doesn't match the remote copy, the operation returns draft_changed.
func TestDraftMutationRefusesChangedRemoteContent(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Replace stored raw with different bytes so SHA-256 diverges from remote.
	// Clear compression too so decoding the swapped bytes doesn't fail.
	differentRaw := []byte("From: " + testutil.IMAPTestUsername + "\r\nTo: nobody@example.com\r\nMessage-ID: <different@example.com>\r\n\r\nTotally different\r\n")
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?
	`), differentRaw, f.draftID)
	require.NoError(t, err)
	// Confirm the update landed.
	var cnt int
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT COUNT(*) FROM message_raw WHERE message_id = ?
	`), f.draftID).Scan(&cnt))
	require.Equal(t, 1, cnt, "message_raw must have a row for the draft")

	_, err = f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(t, err)
	// Remote has original raw; local has different raw → digests differ → draft_changed.
	assert.Equal(t, "draft_changed", err.Error())

	// Lifecycle stays active.
	var lifecycle string
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle))
	assert.Equal(t, "active", lifecycle)
}

// TestDraftDeleteRemovesOnlyTheOwnedUID verifies that deleting a draft
// expunges only its UID and leaves a bystander message in the same mailbox.
func TestDraftDeleteRemovesOnlyTheOwnedUID(t *testing.T) {
	f := newDraftLifecycleFixture(t)
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)

	// Append a bystander to the same mailbox.
	bystanderRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <bystander@example.com>\r\n\r\nBystander\r\n")
	bClient, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = bClient.Close() }()
	require.NoError(t, bClient.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	appendCmd := bClient.Append("Drafts", int64(len(bystanderRaw)), &emersionimap.AppendOptions{
		Flags: []emersionimap.Flag{emersionimap.FlagDraft},
	})
	done := make(chan struct{})
	var bystanderUID emersionimap.UID
	go func() {
		defer close(done)
		_, _ = io.WriteString(appendCmd, string(bystanderRaw))
		_ = appendCmd.Close()
		data, waitErr := appendCmd.Wait()
		if waitErr == nil && data != nil {
			bystanderUID = data.UID
		}
	}()
	<-done
	require.NotZero(t, bystanderUID, "bystander must receive a UID from the server")

	// Delete only the owned draft.
	_, err = f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.NoError(t, err)

	// Verify the bystander UID is still present on the server.
	verifyClient, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = verifyClient.Close() }()
	require.NoError(t, verifyClient.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = verifyClient.Select("Drafts", nil).Wait()
	require.NoError(t, err)
	var uidSet emersionimap.UIDSet
	uidSet.AddNum(bystanderUID)
	fetchCmd := verifyClient.Fetch(uidSet, &emersionimap.FetchOptions{UID: true})
	found := false
	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			if _, ok := item.(imapclient.FetchItemDataUID); ok {
				found = true
			}
		}
	}
	require.NoError(t, fetchCmd.Close())
	assert.True(t, found, "bystander UID must remain after deleting only the owned draft")
}

// failOnNthRemove wraps a real IMAP client and injects a RemoveDraft failure
// until the shared counter exceeds failBefore.  All other methods are delegated
// to the embedded client unchanged.
type failOnNthRemove struct {
	*imaplib.Client
	mu         sync.Mutex
	counter    *int // shared across factory calls to survive re-creation
	failBefore int  // fail when the call number is <= failBefore
}

func (f *failOnNthRemove) RemoveDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error) {
	f.mu.Lock()
	*f.counter++
	n := *f.counter
	f.mu.Unlock()
	if n <= f.failBefore {
		return imaplib.DraftInspectResult{}, &imaplib.DraftAppendError{
			State: imaplib.DraftStateRemoteUnknown,
			Code:  "remote_unknown",
			Err:   errors.New("injected RemoveDraft failure"),
		}
	}
	return f.Client.RemoveDraft(ctx, target)
}

// sharedRemoveCounter holds state that must outlive a single IMAP client so
// that the factory-injected failure counter persists across --resume calls.
type sharedRemoveCounter struct{ n int }

// makeFaultyFactory returns a draftClientFactory that wraps the real IMAP
// client and fails RemoveDraft on the first failBefore calls (counted across
// all clients created from this factory).
func (f draftLifecycleFixture) makeFaultyFactory(counter *sharedRemoveCounter, failBefore int) func(context.Context, *store.Source) (draftClient, error) {
	return func(ctx context.Context, src *store.Source) (draftClient, error) {
		return &failOnNthRemove{
			Client:     imaplib.NewClient(f.config, testutil.IMAPTestPassword),
			counter:    &counter.n,
			failBefore: failBefore,
		}, nil
	}
}

// TestDraftGetReportsExternalRemoval verifies that draft-get reports a
// non-present provider_status when the UID was expunged externally, and that
// lifecycle stays active in the local store.
func TestDraftGetReportsExternalRemoval(t *testing.T) {
	f := newDraftLifecycleFixture(t)
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)

	// Expunge the draft from the IMAP server via a helper connection.
	testutil.ExpungeIMAPMessage(t, addr, "Drafts", emersionimap.UID(f.draftUID))

	events, err := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(t, err)
	require.Len(t, events, 1)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &result))
	// Provider must not report present after expunge.
	assert.NotEqual(t, imaplib.DraftRemotePresent, result["provider_status"])
	// Lifecycle stays active (get is read-only).
	assert.Equal(t, "active", result["lifecycle"])

	// draft-edit after external removal must return draft_missing.
	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=should fail")
	require.Error(t, editErr)
	assert.Equal(t, "draft_missing", editErr.Error(), "draft-edit must return draft_missing for externally removed draft")

	// draft-delete after external removal must complete locally (absent is idempotent).
	_, deleteErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.NoError(t, deleteErr, "draft-delete must complete when remote copy is already absent")

	// Lifecycle must now be discarded; no APPEND recreated the draft.
	events2, err2 := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(t, err2)
	var result2 map[string]any
	require.NoError(t, json.Unmarshal([]byte(events2[0].Data), &result2))
	assert.Equal(t, "discarded", result2["lifecycle"], "lifecycle must be discarded after draft-delete")
}

// TestParseDraftLifecycleArgs is a pure unit test for the hand-written arg parser.
func TestParseDraftLifecycleArgs(t *testing.T) {
	t.Run("draft-get valid", func(t *testing.T) {
		intent, err := parseDraftLifecycleArgs([]string{"draft-get", "42"})
		require.NoError(t, err)
		assert.Equal(t, int64(42), intent.DraftID)
		assert.Equal(t, "draft-get", intent.Command)
	})

	t.Run("draft-edit missing revision", func(t *testing.T) {
		_, err := parseDraftLifecycleArgs([]string{"draft-edit", "42", "--body=hi"})
		require.Error(t, err)
		// code is "invalid_args"; the specific message is in the wrapped err
		assert.Equal(t, "invalid_args", err.Error())
	})

	t.Run("draft-edit missing body", func(t *testing.T) {
		_, err := parseDraftLifecycleArgs([]string{"draft-edit", "42", "--revision=1"})
		require.Error(t, err)
		assert.Equal(t, "invalid_args", err.Error())
	})

	t.Run("draft-edit with all flags", func(t *testing.T) {
		intent, err := parseDraftLifecycleArgs([]string{"draft-edit", "42", "--revision=3", "--body=hello"})
		require.NoError(t, err)
		assert.Equal(t, int64(42), intent.DraftID)
		assert.Equal(t, int64(3), intent.Revision)
		assert.Equal(t, "hello", intent.Body)
	})

	t.Run("draft-delete with revision", func(t *testing.T) {
		intent, err := parseDraftLifecycleArgs([]string{"draft-delete", "42", "--revision=1"})
		require.NoError(t, err)
		assert.Equal(t, int64(1), intent.Revision)
	})

	t.Run("unknown flag rejected", func(t *testing.T) {
		_, err := parseDraftLifecycleArgs([]string{"draft-get", "42", "--unknown"})
		require.Error(t, err)
		assert.Equal(t, "invalid_args", err.Error())
	})

	t.Run("non-integer draft ID rejected", func(t *testing.T) {
		_, err := parseDraftLifecycleArgs([]string{"draft-get", "not-a-number"})
		require.Error(t, err)
	})

	t.Run("zero draft ID rejected", func(t *testing.T) {
		_, err := parseDraftLifecycleArgs([]string{"draft-get", "0"})
		require.Error(t, err)
	})
}

// TestDraftLifecycleEnvAndCwdRejected verifies that env or cwd in the request
// is rejected with invalid_args, as lifecycle commands are env-free.
func TestDraftLifecycleEnvAndCwdRejected(t *testing.T) {
	adapter := &storeAPIAdapter{}
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-get", "1"},
		Env:  map[string]string{"EDITOR": "vim"},
	}, nil)
	require.Error(t, err)
	assert.Equal(t, "invalid_args", err.Error())
}

// appendRawToServer appends raw bytes with \Draft flag to a mailbox on the
// in-memory IMAP server and returns the assigned UID.
func appendRawToServer(t *testing.T, addr, mailbox string, raw []byte) emersionimap.UID {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	appendCmd := c.Append(mailbox, int64(len(raw)), &emersionimap.AppendOptions{
		Flags: []emersionimap.Flag{emersionimap.FlagDraft},
	})
	done := make(chan struct{})
	var uid emersionimap.UID
	go func() {
		defer close(done)
		_, _ = io.Copy(appendCmd, bytes.NewReader(raw))
		_ = appendCmd.Close()
		data, waitErr := appendCmd.Wait()
		if waitErr == nil && data != nil {
			uid = data.UID
		}
	}()
	<-done
	require.NotZero(t, uid)
	return uid
}

// TestDraftEditRejectsStaleRevision/concurrent: two concurrent edits at the same
// starting revision; exactly one commits, the other gets revision_conflict or
// operation_pending; the mailbox holds exactly one draft copy (row 6).
func TestDraftEditConcurrentRevisionConflict(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = f.runLifecycle(t,
				"draft-edit", strconv.FormatInt(f.draftID, 10),
				"--revision=1", "--body=Concurrent edit "+strconv.Itoa(idx))
		}(i)
	}
	wg.Wait()

	// Exactly one must succeed and one must fail with revision_conflict or
	// operation_pending.
	successes, failures := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else {
			failures++
			assert.True(t, err.Error() == "revision_conflict" || err.Error() == "operation_pending" || err.Error() == "sync_active",
				"unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, successes, "exactly one edit must succeed")
	assert.Equal(t, 1, failures, "exactly one edit must conflict")

	// Mailbox must hold exactly one draft copy.
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)
	c, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	data, err := c.Select("Drafts", nil).Wait()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), data.NumMessages, "mailbox must hold exactly one draft copy")
}

// TestDraftDeleteReportsRemoteFailureWithoutLocalLoss verifies that when
// draft-delete fails because RemoveDraft is rejected, the operation returns
// delete_failed, leaves lifecycle=active with pending_uid set, and
// leaves both the local membership/message rows and the server copy intact.
func TestDraftDeleteReportsRemoteFailureWithoutLocalLoss(t *testing.T) {
	f := newDraftLifecycleFixture(t)
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)

	// Capture the current UID before the delete attempt.
	var currentUID int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT uid FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&currentUID))

	// Inject a factory that always fails RemoveDraft so that draft-delete leaves
	// the pending state without touching the remote or local data.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 999) // always fail

	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(t, err)
	assert.Equal(t, "delete_failed", err.Error())

	// Lifecycle stays active; pending_uid must be set (Begin saves old uid).
	var lifecycle string
	var pendingUID sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle, pending_uid FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle, &pendingUID))
	assert.Equal(t, "active", lifecycle)
	assert.True(t, pendingUID.Valid, "pending_uid must be set")

	// Membership row must still exist.
	var membershipCount int
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ? AND uid = ?
	`), f.source.ID, currentUID).Scan(&membershipCount))
	assert.Equal(t, 1, membershipCount, "membership row must be present")

	// Message row must not be tombstoned.
	assert.False(t, isDraftTombstoned(t, f, f.draftID))

	// Server copy must still be present.
	c, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, selErr := c.Select("Drafts", nil).Wait()
	require.NoError(t, selErr)
	var uidSet emersionimap.UIDSet
	uidSet.AddNum(emersionimap.UID(currentUID))
	fetchCmd := c.Fetch(uidSet, &emersionimap.FetchOptions{UID: true})
	found := false
	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			if _, ok := item.(imapclient.FetchItemDataUID); ok {
				found = true
			}
		}
	}
	require.NoError(t, fetchCmd.Close())
	assert.True(t, found, "server copy must still be present in delete_failed state")
}

// TestDraftLifecycleCommandsRouteThroughDaemon verifies cobra command
// registration, ExactArgs(1), and required-flag enforcement.
func TestDraftLifecycleCommandsRouteThroughDaemon(t *testing.T) {
	t.Run("draft-get ExactArgs", func(t *testing.T) {
		cmd := newDraftGetCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		require.Equal(t, "draft-get <draft-id>", cmd.Use)
		// Zero args: cobra should refuse (ExactArgs(1)).
		err := cmd.Args(cmd, []string{})
		require.Error(t, err)
		// Two args: cobra should also refuse.
		err = cmd.Args(cmd, []string{"1", "2"})
		require.Error(t, err)
		// One arg: OK.
		err = cmd.Args(cmd, []string{"1"})
		require.NoError(t, err)
	})

	t.Run("draft-edit requires revision", func(t *testing.T) {
		cmd := newDraftEditCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		// RunE must return a usageErr when --revision is absent.
		err := cmd.RunE(cmd, []string{"1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "revision")
	})

	t.Run("draft-edit requires body", func(t *testing.T) {
		cmd := newDraftEditCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		require.NoError(t, cmd.Flags().Set("revision", "1"))
		err := cmd.RunE(cmd, []string{"1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "body")
	})

	t.Run("draft-delete requires revision", func(t *testing.T) {
		cmd := newDraftDeleteCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		err := cmd.RunE(cmd, []string{"1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "revision")
	})
}

// TestDraftReplayPendingDiscard verifies that draft-delete resumes a pending
// discard when the previous attempt left pending_kind='discard' set.
func TestDraftReplayPendingDiscard(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// First attempt: inject a failing RemoveDraft to leave pending_kind='discard'.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 1) // fail first call
	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(t, err)
	assert.Equal(t, "delete_failed", err.Error())

	// Verify pending state is set (revision bumped to 2 by Begin).
	var pendingKind sql.NullString
	var revision int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT pending_kind, revision FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&pendingKind, &revision))
	assert.Equal(t, "discard", pendingKind.String)
	assert.Equal(t, int64(2), revision)

	// Retry with --revision=1 (pre-Begin revision) and real RemoveDraft.
	evs = nil
	_, retryErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.NoError(t, retryErr, "replay of pending discard must succeed")

	// Draft must now be discarded.
	var lifecycle string
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle))
	assert.Equal(t, "discarded", lifecycle)
}

// TestDraftReplayPendingDiscardRejectsStaleRevision verifies that
// replayPendingDiscard refuses a caller-supplied revision that does not match
// the current pre-Begin revision (draft.Revision-1).
func TestDraftReplayPendingDiscardRejectsStaleRevision(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Perform a successful edit first so revision advances to 2.
	_, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=Intermediate edit")
	require.NoError(t, err)

	// Leave pending_kind='discard' at revision=3 by injecting a RemoveDraft failure.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 1)
	err = adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=2"},
	}, nil)
	require.Error(t, err)

	// Retry with the original stale revision=1 (pre-Begin was 2, so 1 != 3-1=2) must fail.
	_, staleErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(t, staleErr)
	assert.Equal(t, "revision_conflict", staleErr.Error(), "stale revision must produce revision_conflict")
}

// TestDraftEditReturnsPendingOperationForPendingDiscard verifies that
// draft-edit returns operation_pending when pending_kind='discard' is set.
func TestDraftEditReturnsPendingOperationForPendingDiscard(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Inject pending_kind='discard' directly in the DB (simulating a crashed delete).
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'discard', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), time.Now(), f.draftID)
	require.NoError(t, err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=should fail")
	require.Error(t, editErr)
	assert.Equal(t, "operation_pending", editErr.Error())
}

// TestDraftDeleteReturnsPendingOperationForPendingEdit verifies that
// draft-delete returns operation_pending when pending_kind='edit' is set.
func TestDraftDeleteReturnsPendingOperationForPendingEdit(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Inject pending_kind='edit' directly in the DB (simulating a crashed edit).
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), time.Now(), f.draftID)
	require.NoError(t, err)

	_, deleteErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(t, deleteErr)
	assert.Equal(t, "operation_pending", deleteErr.Error())
}

// TestDraftEditClearsStaleInterruptedEdit verifies that draft-edit returns
// edit_interrupted (and clears the stale pending marker) when pending_kind='edit'
// has a pending_started_at older than pendingEditStalenessThreshold.
func TestDraftEditClearsStaleInterruptedEdit(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Inject a pending_kind='edit' with a very old pending_started_at.
	staleTime := time.Now().Add(-60 * time.Minute)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(t, err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear")
	require.Error(t, editErr)
	assert.Equal(t, "edit_interrupted", editErr.Error())

	// pending_kind must be cleared.
	var pendingKind sql.NullString
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&pendingKind))
	assert.False(t, pendingKind.Valid, "pending_kind must be cleared after edit_interrupted")
}

// TestDraftEditRejectsRecentPendingEdit verifies that the staleness guard in
// draft-edit returns operation_pending for a fresh pending-edit marker and
// edit_interrupted for a stale one, controlled by pending_started_at alone.
func TestDraftEditRejectsRecentPendingEdit(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Inject a fresh pending_kind='edit' (younger than pendingEditStalenessThreshold).
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), time.Now(), f.draftID)
	require.NoError(t, err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=should refuse")
	require.Error(t, editErr)
	assert.Equal(t, "operation_pending", editErr.Error(),
		"fresh pending-edit marker must produce operation_pending")

	// Backdate the marker past the staleness threshold; now draft-edit must clear it.
	staleTime := time.Now().Add(-(pendingEditStalenessThreshold + time.Minute))
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts SET pending_started_at = ? WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(t, err)

	_, editErr = f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear")
	require.Error(t, editErr)
	assert.Equal(t, "edit_interrupted", editErr.Error(),
		"stale pending-edit marker must produce edit_interrupted after clearing")
}

// TestDraftEditInterruptedMessageMatchingUID verifies that when pending_uid
// equals uid (Persist did not commit), edit_interrupted does not name any UID —
// naming the tracked UID would tell the operator to remove the live copy.
func TestDraftEditInterruptedMessageMatchingUID(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	staleTime := time.Now().Add(-60 * time.Minute)
	// uid == pending_uid: simulate crash between Begin and Persist.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(t, err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear")
	require.Error(t, editErr)
	assert.Equal(t, "edit_interrupted", editErr.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](editErr)
	require.True(t, ok)
	// Must not name any UID — the tracked copy is still the live one.
	assert.NotContains(t, coded.Err.Error(), "UID=")
}

// TestDraftEditInterruptedMessageDifferentUID verifies that when pending_uid
// differs from uid (Persist committed), edit_interrupted names pending_uid as
// the stale removable copy.
func TestDraftEditInterruptedMessageDifferentUID(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	staleTime := time.Now().Add(-60 * time.Minute)
	// Record the original uid, then advance uid to simulate Persist committing.
	var origUID int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(
		`SELECT uid FROM imap_drafts WHERE draft_id = ?`), f.draftID).Scan(&origUID))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1,
		    uid = 99999
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(t, err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear")
	require.Error(t, editErr)
	assert.Equal(t, "edit_interrupted", editErr.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](editErr)
	require.True(t, ok)
	// Must name pending_uid (the stale pre-edit copy) in the message.
	assert.Contains(t, coded.Err.Error(), fmt.Sprintf("UID=%d", origUID))
	// Must not name the post-Persist uid (the tracked live copy).
	assert.NotContains(t, coded.Err.Error(), "UID=99999")
}

// TestDraftDeleteRevisionRoundTrip verifies that the revision in delete_failed
// JSON output is the value the retry must pass as --revision. A consumer that
// reads revision off the failure event and retries with it must succeed.
func TestDraftDeleteRevisionRoundTrip(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Fail the first RemoveDraft call to produce delete_failed.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 1)

	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1", "--json"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(t, err)
	assert.Equal(t, "delete_failed", err.Error())
	require.NotEmpty(t, evs, "delete_failed must emit an event")

	// Read revision from the emitted JSON — this is what a --json consumer would do.
	var failOutput map[string]any
	require.NoError(t, json.Unmarshal([]byte(evs[0].Data), &failOutput))
	revisionVal, ok := failOutput["revision"].(float64)
	require.True(t, ok, "emitted JSON must contain revision field")
	retryRevision := strconv.FormatInt(int64(revisionVal), 10)

	// Retry with the emitted revision; must succeed.
	_, retryErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision="+retryRevision)
	require.NoError(t, retryErr, "retry with emitted revision must succeed")
}

// TestDraftEditRefusesWrongGrantMailbox verifies that draft-edit returns
// draft_disabled when the policy mailbox does not match the draft's mailbox,
// and does so without opening any IMAP connection.
func TestDraftEditRefusesWrongGrantMailbox(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	var imapOpened bool
	adapter := &storeAPIAdapter{
		store:       f.store,
		draftPolicy: []config.IMAPDraftSource{{SourceID: f.source.ID, Enabled: true, Mailbox: "WrongMailbox"}},
		draftLifecycleClientFactory: func(context.Context, *store.Source) (draftClient, error) {
			imapOpened = true
			return nil, errors.New("must not open IMAP connection for mismatched mailbox")
		},
	}
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=test"},
	}, nil)
	require.Error(t, err)
	assert.Equal(t, "draft_disabled", err.Error())
	assert.False(t, imapOpened, "IMAP must not be opened when grant mailbox mismatches")
}

// TestDraftDeleteRefusesWrongGrantMailbox verifies that draft-delete returns
// draft_disabled when the policy mailbox does not match the draft's mailbox,
// and does so without opening any IMAP connection.
func TestDraftDeleteRefusesWrongGrantMailbox(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	var imapOpened bool
	adapter := &storeAPIAdapter{
		store:       f.store,
		draftPolicy: []config.IMAPDraftSource{{SourceID: f.source.ID, Enabled: true, Mailbox: "WrongMailbox"}},
		draftLifecycleClientFactory: func(context.Context, *store.Source) (draftClient, error) {
			imapOpened = true
			return nil, errors.New("must not open IMAP connection for mismatched mailbox")
		},
	}
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1"},
	}, nil)
	require.Error(t, err)
	assert.Equal(t, "draft_disabled", err.Error())
	assert.False(t, imapOpened, "IMAP must not be opened when grant mailbox mismatches")
}

// TestDraftGetReportsNotCheckedForWrongGrantMailbox verifies that draft-get
// returns provider_status=not_checked when the policy mailbox does not match
// the draft's mailbox, without opening any IMAP connection.
func TestDraftGetReportsNotCheckedForWrongGrantMailbox(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	var imapOpened bool
	adapter := &storeAPIAdapter{
		store:       f.store,
		draftPolicy: []config.IMAPDraftSource{{SourceID: f.source.ID, Enabled: true, Mailbox: "WrongMailbox"}},
		draftLifecycleClientFactory: func(context.Context, *store.Source) (draftClient, error) {
			imapOpened = true
			return nil, errors.New("must not open IMAP connection for mismatched mailbox")
		},
	}
	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-get", strconv.FormatInt(f.draftID, 10), "--json"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.NoError(t, err)
	require.Len(t, evs, 1)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(evs[0].Data), &result))
	assert.Equal(t, "not_checked", result["provider_status"])
	assert.False(t, imapOpened, "IMAP must not be opened when grant mailbox mismatches")
}

// TestConcurrentDraftDeletePendingDiscard verifies that two concurrent
// draft-delete calls against a pending-discard state produce exactly one
// completion and one refusal, with no intermediate window between lock release
// and re-acquire.
func TestConcurrentDraftDeletePendingDiscard(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Simulate a prior Begin by injecting pending_kind='discard' and advancing the
	// revision to 2, exactly as BeginIMAPDraftOperationContext would leave it.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'discard', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), time.Now().Add(-time.Minute), f.draftID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Both callers present revision=1 (the pre-Begin revision stored in intent).
			_, errs[idx] = f.runLifecycle(t,
				"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
		}(i)
	}
	wg.Wait()

	successes, failures := 0, 0
	for _, e := range errs {
		if e == nil {
			successes++
		} else {
			failures++
			// sync_active: second caller blocked on the lock while first held it.
			// operation_pending: second caller got the lock after first finished;
			//   BeginIMAPDraftOperationContext returns operation_pending when
			//   lifecycle='discarded' (store imap_draft_lifecycle.go:148).
			// revision_conflict: second caller's revision no longer matches.
			assert.True(t,
				e.Error() == "revision_conflict" || e.Error() == "sync_active" ||
					e.Error() == "operation_pending" || e.Error() == "draft_not_found",
				"unexpected error from losing caller: %v", e)
		}
	}
	assert.Equal(t, 1, successes, "exactly one delete must complete")
	assert.Equal(t, 1, failures, "exactly one delete must be refused")
}
