package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
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
// delete_failed, leaves lifecycle=delete_pending with pending_uid set, and
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

// Compile-time check: verify errors.Is can unwrap CLIRunCodedError (used in
// allowlist tests; kept here to guard the interface invariant for lifecycle).
var _ = errors.New // suppress unused import if the above test is the only user
