package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

type testDraftClient struct {
	*imaplib.Client
}

func (c *testDraftClient) RemoveDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error) {
	result, err := c.Client.InspectDraft(ctx, target)
	if err != nil || result.State != imaplib.DraftRemotePresent {
		return result, err
	}
	return result, c.Client.DeleteMessage(ctx, target.Mailbox+"|"+strconv.FormatUint(uint64(target.UID), 10))
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

// draftUIDPresentOnServer reports whether a UID still fetches from the Drafts
// mailbox, read back through a second IMAP connection.
func draftUIDPresentOnServer(t *testing.T, f draftLifecycleFixture, uid emersionimap.UID) bool {
	t.Helper()
	c, err := imapclient.DialInsecure(f.config.Host+":"+strconv.Itoa(f.config.Port), nil)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = c.Select("Drafts", nil).Wait()
	require.NoError(t, err)
	var uidSet emersionimap.UIDSet
	uidSet.AddNum(uid)
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
	return found
}

// decodeDraftLifecycleEvent parses one emitted --json lifecycle result.
func decodeDraftLifecycleEvent(t *testing.T, event api.CLIRunEvent) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(event.Data), &result))
	return result
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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	events, err := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(err)
	require.Len(events, 1)
	var result map[string]any
	require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assert.Equal("active", result["lifecycle"])
	assert.Equal(float64(1), result["revision"])
	assert.Equal(imaplib.DraftRemotePresent, result["provider_status"])
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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	events, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=Updated draft body", "--json")
	require.NoError(err)
	require.Len(events, 1)

	var result map[string]any
	require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assert.Equal("replaced", result["status"])
	assert.Equal(float64(2), result["revision"])

	// Verify revision in store.
	var revision int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT revision FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&revision))
	assert.Equal(int64(2), revision)

	// Old message must be tombstoned.
	assert.True(isDraftTombstoned(t, f, f.draftID))

	// Exactly one membership row for the source.
	var cnt int
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`), f.source.ID).Scan(&cnt))
	assert.Equal(1, cnt)

	// Archived raw for the new current_message_id must contain the updated body.
	var newMessageID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT current_message_id FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&newMessageID))
	rawData, rawErr := f.store.GetMessageRawContext(t.Context(), newMessageID)
	require.NoError(rawErr, "must be able to load archived raw for new draft")
	assert.Contains(string(rawData), "Updated draft body", "archived raw must contain the new body text")
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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Recreate the mailbox so the saved receipt belongs to the old epoch.
	recreateDraftsMailbox(t, f)

	_, err := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(err)
	assert.Equal("uidvalidity_changed", err.Error())

	// Lifecycle must remain unchanged.
	var lifecycle string
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle))
	assert.Equal("active", lifecycle)
}

// TestDraftMutationRefusesChangedRemoteContent verifies that when the local
// raw digest doesn't match the remote copy, the operation returns draft_changed.
func TestDraftMutationRefusesChangedRemoteContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Replace stored raw with different bytes so SHA-256 diverges from remote.
	// Clear compression too so decoding the swapped bytes doesn't fail.
	differentRaw := []byte("From: " + testutil.IMAPTestUsername + "\r\nTo: nobody@example.com\r\nMessage-ID: <different@example.com>\r\n\r\nTotally different\r\n")
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?
	`), differentRaw, f.draftID)
	require.NoError(err)
	// Confirm the update landed.
	var cnt int
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT COUNT(*) FROM message_raw WHERE message_id = ?
	`), f.draftID).Scan(&cnt))
	require.Equal(1, cnt, "message_raw must have a row for the draft")

	_, err = f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(err)
	// Remote has original raw; local has different raw → digests differ → draft_changed.
	assert.Equal("draft_changed", err.Error())

	// Lifecycle stays active.
	var lifecycle string
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle))
	assert.Equal("active", lifecycle)
}

// TestDraftDeleteRemovesOnlyTheOwnedUID verifies that deleting a draft
// expunges only its UID and leaves a bystander message in the same mailbox.
func TestDraftDeleteRemovesOnlyTheOwnedUID(t *testing.T) {
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)

	// Append a bystander to the same mailbox.
	bystanderRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <bystander@example.com>\r\n\r\nBystander\r\n")
	bClient, err := imapclient.DialInsecure(addr, nil)
	require.NoError(err)
	defer func() { _ = bClient.Close() }()
	require.NoError(bClient.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
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
	require.NotZero(bystanderUID, "bystander must receive a UID from the server")

	// Delete only the owned draft.
	_, err = f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.NoError(err)

	// Verify the bystander UID is still present on the server.
	verifyClient, err := imapclient.DialInsecure(addr, nil)
	require.NoError(err)
	defer func() { _ = verifyClient.Close() }()
	require.NoError(verifyClient.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = verifyClient.Select("Drafts", nil).Wait()
	require.NoError(err)
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
	require.NoError(fetchCmd.Close())
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
// that the factory-injected failure counter persists across retries, which
// build a fresh client each time.
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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)

	// Expunge the draft from the IMAP server via a helper connection.
	testutil.ExpungeIMAPMessage(t, addr, "Drafts", emersionimap.UID(f.draftUID))

	events, err := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(err)
	require.Len(events, 1)
	var result map[string]any
	require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	// Provider must not report present after expunge.
	assert.NotEqual(imaplib.DraftRemotePresent, result["provider_status"])
	// Lifecycle stays active (get is read-only).
	assert.Equal("active", result["lifecycle"])

	// draft-edit after external removal must return draft_missing.
	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=should fail")
	require.Error(editErr)
	assert.Equal("draft_missing", editErr.Error(), "draft-edit must return draft_missing for externally removed draft")

	// draft-delete after external removal must complete locally (absent is idempotent).
	_, deleteErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.NoError(deleteErr, "draft-delete must complete when remote copy is already absent")

	// Lifecycle must now be discarded; no APPEND recreated the draft.
	events2, err2 := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(err2)
	var result2 map[string]any
	require.NoError(json.Unmarshal([]byte(events2[0].Data), &result2))
	assert.Equal("discarded", result2["lifecycle"], "lifecycle must be discarded after draft-delete")
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
		assert := assert.New(t)

		intent, err := parseDraftLifecycleArgs([]string{"draft-edit", "42", "--revision=3", "--body=hello"})
		require.NoError(t, err)
		assert.Equal(int64(42), intent.DraftID)
		assert.Equal(int64(3), intent.Revision)
		assert.Equal("hello", intent.Body)
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
	assert := assert.New(t)
	require := require.New(t)

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
			assert.True(err.Error() == "revision_conflict" || err.Error() == "operation_pending" || err.Error() == "sync_active",
				"unexpected error: %v", err)
		}
	}
	assert.Equal(1, successes, "exactly one edit must succeed")
	assert.Equal(1, failures, "exactly one edit must conflict")

	// Mailbox must hold exactly one draft copy.
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)
	c, err := imapclient.DialInsecure(addr, nil)
	require.NoError(err)
	defer func() { _ = c.Close() }()
	require.NoError(c.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	data, err := c.Select("Drafts", nil).Wait()
	require.NoError(err)
	assert.Equal(uint32(1), data.NumMessages, "mailbox must hold exactly one draft copy")
}

// TestDraftDeleteReportsRemoteFailureWithoutLocalLoss verifies that when
// draft-delete fails because RemoveDraft is rejected, the operation returns
// delete_failed, leaves lifecycle=active with pending_uid set, and
// leaves both the local membership/message rows and the server copy intact.
func TestDraftDeleteReportsRemoteFailureWithoutLocalLoss(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	addr := f.config.Host + ":" + strconv.Itoa(f.config.Port)

	// Capture the current UID before the delete attempt.
	var currentUID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
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
	require.Error(err)
	assert.Equal("delete_failed", err.Error())

	// Lifecycle stays active; pending_uid must be set (Begin saves old uid).
	var lifecycle string
	var pendingUID sql.NullInt64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle, pending_uid FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle, &pendingUID))
	assert.Equal("active", lifecycle)
	assert.True(pendingUID.Valid, "pending_uid must be set")

	// Membership row must still exist.
	var membershipCount int
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ? AND uid = ?
	`), f.source.ID, currentUID).Scan(&membershipCount))
	assert.Equal(1, membershipCount, "membership row must be present")

	// Message row must not be tombstoned.
	assert.False(isDraftTombstoned(t, f, f.draftID))

	// Server copy must still be present.
	c, err := imapclient.DialInsecure(addr, nil)
	require.NoError(err)
	defer func() { _ = c.Close() }()
	require.NoError(c.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, selErr := c.Select("Drafts", nil).Wait()
	require.NoError(selErr)
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
	require.NoError(fetchCmd.Close())
	assert.True(found, "server copy must still be present in delete_failed state")
}

// TestDraftLifecycleCommandsRouteThroughDaemon verifies cobra command
// registration, ExactArgs(1), and required-flag enforcement.
func TestDraftLifecycleCommandsRouteThroughDaemon(t *testing.T) {
	t.Run("draft-get ExactArgs", func(t *testing.T) {
		require := require.New(t)

		cmd := newDraftGetCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		require.Equal("draft-get <draft-id>", cmd.Use)
		// Zero args: cobra should refuse (ExactArgs(1)).
		err := cmd.Args(cmd, []string{})
		require.Error(err)
		// Two args: cobra should also refuse.
		err = cmd.Args(cmd, []string{"1", "2"})
		require.Error(err)
		// One arg: OK.
		err = cmd.Args(cmd, []string{"1"})
		require.NoError(err)
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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// First attempt: inject a failing RemoveDraft to leave pending_kind='discard'.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 1) // fail first call
	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(err)
	assert.Equal("delete_failed", err.Error())

	// Verify pending state is set (revision bumped to 2 by Begin).
	var pendingKind sql.NullString
	var revision int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT pending_kind, revision FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&pendingKind, &revision))
	assert.Equal("discard", pendingKind.String)
	assert.Equal(int64(2), revision)

	// Retry with the stored current revision and a real RemoveDraft.
	evs = nil
	_, retryErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=2")
	require.NoError(retryErr, "replay of pending discard must succeed")

	// Draft must now be discarded.
	var lifecycle string
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle))
	assert.Equal("discarded", lifecycle)
}

// TestDraftReplayPendingDiscardRejectsStaleRevision verifies that
// replayPendingDiscard refuses a caller-supplied revision that does not match
// the stored current revision.
func TestDraftReplayPendingDiscardRejectsStaleRevision(t *testing.T) {
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Perform a successful edit first so revision advances to 2.
	_, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=Intermediate edit")
	require.NoError(err)

	// Leave pending_kind='discard' at revision=3 by injecting a RemoveDraft failure.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 1)
	err = adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=2"},
	}, nil)
	require.Error(err)

	// Stored revision is now 3. Both the original revision=1 and the pre-Begin
	// revision=2 are stale and must be refused; only the current value works.
	for _, stale := range []string{"--revision=1", "--revision=2"} {
		_, staleErr := f.runLifecycle(t,
			"draft-delete", strconv.FormatInt(f.draftID, 10), stale)
		require.Error(staleErr)
		assert.Equal(t, "revision_conflict", staleErr.Error(),
			"stale revision must produce revision_conflict: %s", stale)
	}
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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Inject a pending_kind='edit' with a very old pending_started_at.
	staleTime := time.Now().Add(-60 * time.Minute)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear")
	require.Error(editErr)
	assert.Equal("edit_interrupted", editErr.Error())

	// pending_kind must be cleared.
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&pendingKind))
	assert.False(pendingKind.Valid, "pending_kind must be cleared after edit_interrupted")
}

// TestDraftEditRejectsRecentPendingEdit verifies that the staleness guard in
// draft-edit returns operation_pending for a fresh pending-edit marker and
// edit_interrupted for a stale one, controlled by pending_started_at alone.
func TestDraftEditRejectsRecentPendingEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Inject a fresh pending_kind='edit' (younger than pendingEditStalenessThreshold).
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), time.Now(), f.draftID)
	require.NoError(err)

	_, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=should refuse")
	require.Error(editErr)
	assert.Equal("operation_pending", editErr.Error(),
		"fresh pending-edit marker must produce operation_pending")

	// Backdate the marker past the staleness threshold; now draft-edit must clear it.
	staleTime := time.Now().Add(-(pendingEditStalenessThreshold + time.Minute))
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts SET pending_started_at = ? WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	_, editErr = f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear")
	require.Error(editErr)
	assert.Equal("edit_interrupted", editErr.Error(),
		"stale pending-edit marker must produce edit_interrupted after clearing")
}

// TestDraftEditInterruptedMessageMatchingUID verifies that when pending_uid
// equals uid (Persist did not commit), edit_interrupted does not name any UID —
// naming the tracked UID would tell the operator to remove the live copy.
func TestDraftEditInterruptedMessageMatchingUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	staleTime := time.Now().Add(-60 * time.Minute)
	// uid == pending_uid: simulate crash between Begin and Persist.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	evs, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear", "--json")
	require.Error(editErr)
	assert.Equal("edit_interrupted", editErr.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](editErr)
	require.True(ok)
	// Must not name any UID — the tracked copy is still the live one.
	assert.NotContains(coded.Err.Error(), "UID=")

	// The refusal the client receives must name no UID either, and must carry
	// the inspect guidance rather than a removal instruction.
	require.Len(evs, 1)
	assert.Equal(cliStreamStderr, evs[0].Type)
	delivered := decodeDraftLifecycleEvent(t, evs[0])
	assert.Equal("edit_interrupted", delivered["status"])
	assert.NotContains(delivered, "uid", "no UID may be named when the tracked copy is the live one")
	assert.Equal(draftInspectInstruction, delivered["instructions"])
}

// TestDraftEditInterruptedMessageDifferentUID verifies that when pending_uid
// differs from uid (Persist committed), edit_interrupted names pending_uid as
// the stale removable copy.
func TestDraftEditInterruptedMessageDifferentUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	staleTime := time.Now().Add(-60 * time.Minute)
	// Record the original uid, then advance uid to simulate Persist committing.
	var origUID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(
		`SELECT uid FROM imap_drafts WHERE draft_id = ?`), f.draftID).Scan(&origUID))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1,
		    uid = 99999
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	evs, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear", "--json")
	require.Error(editErr)
	assert.Equal("edit_interrupted", editErr.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](editErr)
	require.True(ok)
	// Must name pending_uid (the stale pre-edit copy) in the message.
	assert.Contains(coded.Err.Error(), fmt.Sprintf("UID=%d", origUID))
	// Must not name the post-Persist uid (the tracked live copy).
	assert.NotContains(coded.Err.Error(), "UID=99999")

	// The client is told the same thing: the cause is a daemon-log field, so
	// the UID and the removal instruction have to arrive on the event stream.
	require.Len(evs, 1)
	assert.Equal(cliStreamStderr, evs[0].Type)
	delivered := decodeDraftLifecycleEvent(t, evs[0])
	assert.Equal("edit_interrupted", delivered["status"])
	assert.Equal(float64(origUID), delivered["uid"])
	assert.Equal(draftRemoveStaleInstruction, delivered["instructions"])
}

// draftGetRevision runs draft-get and returns the revision it reports, which
// is the stored current revision with no arithmetic.
func draftGetRevision(t *testing.T, f draftLifecycleFixture) int64 {
	t.Helper()
	events, err := f.runLifecycle(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	require.NoError(t, err)
	require.Len(t, events, 1)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &result))
	revision, ok := result["revision"].(float64)
	require.True(t, ok, "draft-get must report a revision")
	return int64(revision)
}

// storedDraftRevision reads the revision directly from the ownership row.
func storedDraftRevision(t *testing.T, f draftLifecycleFixture) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT revision FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&revision))
	return revision
}

// TestDraftEditInterruptedReloadsAndEdits drives the whole caller-level
// recovery sequence for an interrupted edit: the recovery call reports
// edit_interrupted, the caller re-reads the draft, and the revision that read
// reports is accepted by the next edit.
func TestDraftEditInterruptedReloadsAndEdits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Simulate a crash between Begin and Finish: pending marker set, revision
	// already advanced to 2, marker older than the staleness threshold.
	staleTime := time.Now().Add(-(pendingEditStalenessThreshold + time.Minute))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	// 1. The recovery call clears the marker and refuses the requested edit.
	//    What the client receives is the code plus the streamed result: the
	//    coded error's cause is logged by the daemon and never sent, so the
	//    recovery guidance is asserted on the emitted event.
	evs, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1",
		"--body=Body that must not be applied", "--json")
	require.Error(editErr)
	assert.Equal("edit_interrupted", editErr.Error())
	require.Len(evs, 1, "the refusal must reach the client as an event")
	assert.Equal(cliStreamStderr, evs[0].Type)
	delivered := decodeDraftLifecycleEvent(t, evs[0])
	assert.Equal("edit_interrupted", delivered["status"])
	assert.Equal(draftInspectInstruction, delivered["instructions"],
		"the client must receive recovery guidance, not the code alone")
	_, hasDeliveredRevision := delivered["revision"]
	assert.False(hasDeliveredRevision, "a refusal must carry no retry revision")
	coded, ok := errors.AsType[*api.CLIRunCodedError](editErr)
	require.True(ok)
	assert.Contains(coded.Err.Error(), "was not applied",
		"recovery must not read as a promise that the requested body landed")

	// 2. The caller re-reads. draft-get reports the stored current revision.
	reloaded := draftGetRevision(t, f)
	assert.Equal(storedDraftRevision(t, f), reloaded,
		"draft-get must report the stored revision with no arithmetic")
	assert.Equal(int64(2), reloaded)

	// 3. The reloaded revision is accepted by the next edit.
	events, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision="+strconv.FormatInt(reloaded, 10), "--body=Recovered body", "--json")
	require.NoError(err, "edit at the reloaded revision must succeed")
	require.Len(events, 1)
	var result map[string]any
	require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assert.Equal("replaced", result["status"])
	assert.Equal(float64(3), result["revision"], "successful edit reports the committed revision")

	// The new body is what the archive holds.
	var newMessageID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT current_message_id FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&newMessageID))
	rawData, rawErr := f.store.GetMessageRawContext(t.Context(), newMessageID)
	require.NoError(rawErr)
	assert.Contains(string(rawData), "Recovered body")
	assert.NotContains(string(rawData), "Body that must not be applied")
}

// TestDraftDeleteInterruptedReloadsAndCompletes drives the caller-level
// recovery sequence for an interrupted discard: the failure carries no
// revision, the caller re-reads, and the reloaded revision completes the
// discard.
func TestDraftDeleteInterruptedReloadsAndCompletes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// 1. Fail the first RemoveDraft so the discard is left pending.
	counter := &sharedRemoveCounter{}
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = f.makeFaultyFactory(counter, 1)
	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1", "--json"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(err)
	assert.Equal("delete_failed", err.Error())
	require.NotEmpty(evs, "delete_failed must emit an event")

	var failOutput map[string]any
	require.NoError(json.Unmarshal([]byte(evs[0].Data), &failOutput))
	_, hasRevision := failOutput["revision"]
	assert.False(hasRevision, "a failure result must carry no retry revision")
	assert.Equal(draftInspectInstruction, failOutput["instructions"],
		"the failure must carry numberless reload-and-inspect guidance")

	// 2. The caller re-reads. draft-get reports the stored current revision
	//    even while the pending marker is set.
	reloaded := draftGetRevision(t, f)
	assert.Equal(storedDraftRevision(t, f), reloaded,
		"draft-get must report the stored revision while a marker exists")
	assert.Equal(int64(2), reloaded)

	// 3. The reloaded revision completes the discard.
	_, retryErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision="+strconv.FormatInt(reloaded, 10))
	require.NoError(retryErr, "delete at the reloaded revision must complete")

	var lifecycle string
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle, pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle, &pendingKind))
	assert.Equal("discarded", lifecycle)
	assert.False(pendingKind.Valid, "pending marker must be cleared")
}

// TestDraftDeleteRejectsStaleRevision verifies the ordinary stale-revision
// path for draft-delete: a revision the draft has moved past is a
// reload-and-retry conflict and mutates nothing.
func TestDraftDeleteRejectsStaleRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// An ordinary edit advances the revision to 2.
	_, err := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=First edit")
	require.NoError(err)

	_, staleErr := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.Error(staleErr)
	assert.Equal("revision_conflict", staleErr.Error())

	var lifecycle string
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle, pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle, &pendingKind))
	assert.Equal("active", lifecycle)
	assert.False(pendingKind.Valid, "a refused delete must not claim the draft")
	assert.Equal(int64(2), storedDraftRevision(t, f))

	// The reloaded revision is accepted.
	_, err = f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10),
		"--revision="+strconv.FormatInt(draftGetRevision(t, f), 10))
	require.NoError(err)
}

// TestDraftLifecycleOnDiscardedDraft verifies that an already-discarded draft
// is classified before any IMAP connection is opened: a current-revision
// delete reports discarded, an edit refuses with draft_missing, and a stale
// revision stays a reload-and-retry conflict. None of them reports a pending
// operation that does not exist.
func TestDraftLifecycleOnDiscardedDraft(t *testing.T) {
	f := newDraftLifecycleFixture(t)

	// Discard the draft through the ordinary path.
	_, err := f.runLifecycle(t,
		"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1")
	require.NoError(t, err)
	discardedRevision := storedDraftRevision(t, f)
	require.Equal(t, int64(2), discardedRevision)
	current := strconv.FormatInt(discardedRevision, 10)

	// A factory that fails loudly if any command opens an IMAP connection.
	newAdapter := func(opened *bool) *storeAPIAdapter {
		adapter := f.grantedAdapter()
		adapter.draftLifecycleClientFactory = func(context.Context, *store.Source) (draftClient, error) {
			*opened = true
			return nil, errors.New("must not open IMAP connection for a discarded draft")
		}
		return adapter
	}

	t.Run("delete at the current revision reports discarded", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		opened := false
		var evs []api.CLIRunEvent
		err := newAdapter(&opened).runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
			Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=" + current, "--json"},
		}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
		require.NoError(err)
		require.Len(evs, 1)
		var result map[string]any
		require.NoError(json.Unmarshal([]byte(evs[0].Data), &result))
		assert.Equal("discarded", result["status"])
		assert.Equal("discarded", result["lifecycle"])
		assert.False(opened, "a discarded draft must not open an IMAP connection")
	})

	t.Run("edit at the current revision refuses draft_missing", func(t *testing.T) {
		opened := false
		err := newAdapter(&opened).runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
			Args: []string{"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=" + current, "--body=too late"},
		}, nil)
		require.Error(t, err)
		assert.Equal(t, "draft_missing", err.Error())
		assert.False(t, opened, "a discarded draft must not open an IMAP connection")
	})

	t.Run("a stale revision stays a reload-and-retry conflict", func(t *testing.T) {
		for _, command := range []string{"draft-delete", "draft-edit"} {
			opened := false
			args := []string{command, strconv.FormatInt(f.draftID, 10), "--revision=1"}
			if command == "draft-edit" {
				args = append(args, "--body=too late")
			}
			err := newAdapter(&opened).runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args}, nil)
			require.Error(t, err)
			assert.Equal(t, "revision_conflict", err.Error(), "command %s", command)
			assert.False(t, opened, "a discarded draft must not open an IMAP connection")
		}
	})
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
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.Len(evs, 1)
	var result map[string]any
	require.NoError(json.Unmarshal([]byte(evs[0].Data), &result))
	assert.Equal("not_checked", result["provider_status"])
	assert.False(imapOpened, "IMAP must not be opened when grant mailbox mismatches")
}

// failRemoveWithPlainError wraps a real IMAP client and fails RemoveDraft with
// an error that is not a *imaplib.DraftAppendError, which is the arm the coded
// classification does not cover.
type failRemoveWithPlainError struct {
	*imaplib.Client
}

func (c *failRemoveWithPlainError) RemoveDraft(context.Context, imaplib.DraftTarget) (imaplib.DraftInspectResult, error) {
	return imaplib.DraftInspectResult{}, errors.New("injected non-coded RemoveDraft failure")
}

// TestDraftEditReportsOldCopyRemainsWhenRemovalFailsAfterPersist covers the
// partial outcome an edit reaches when its APPEND and its local persist both
// commit and only the removal of the pre-edit copy fails. The local record is
// complete, so the result must say the edit was applied and the old copy
// remains — the inverse of remote_accepted_local_failed — on both the coded
// and the non-coded arm.
func TestDraftEditReportsOldCopyRemainsWhenRemovalFailsAfterPersist(t *testing.T) {
	testCases := []struct {
		name    string
		factory func(f draftLifecycleFixture) func(context.Context, *store.Source) (draftClient, error)
	}{
		{
			name: "coded RemoveDraft failure",
			factory: func(f draftLifecycleFixture) func(context.Context, *store.Source) (draftClient, error) {
				return f.makeFaultyFactory(&sharedRemoveCounter{}, 999)
			},
		},
		{
			name: "non-coded RemoveDraft failure",
			factory: func(f draftLifecycleFixture) func(context.Context, *store.Source) (draftClient, error) {
				return func(context.Context, *store.Source) (draftClient, error) {
					return &failRemoveWithPlainError{
						Client: imaplib.NewClient(f.config, testutil.IMAPTestPassword),
					}, nil
				}
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := newDraftLifecycleFixture(t)
			adapter := f.grantedAdapter()
			adapter.draftLifecycleClientFactory = testCase.factory(f)

			var evs []api.CLIRunEvent
			err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
				Args: []string{"draft-edit", strconv.FormatInt(f.draftID, 10),
					"--revision=1", "--body=Body that did land", "--json"},
			}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
			require.Error(err)
			assert.Equal("edit_applied_old_copy_remains", err.Error())

			// The status the client receives must match the returned code and
			// must name the leftover pre-edit copy.
			require.Len(evs, 1, "a partial operation must report a result")
			assert.Equal(cliStreamStderr, evs[0].Type)
			result := decodeDraftLifecycleEvent(t, evs[0])
			assert.Equal("edit_applied_old_copy_remains", result["status"])
			assert.Equal(draftRemoveStaleInstruction, result["instructions"])
			assert.Equal(float64(f.draftUID), result["uid"],
				"the leftover pre-edit copy must be named to the operator")
			_, hasRevision := result["revision"]
			assert.False(hasRevision, "a partial result must carry no retry revision")

			// The local record is complete: it points at the new copy.
			var currentMessageID, newUID int64
			var lifecycle string
			var pendingKind sql.NullString
			require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
				SELECT current_message_id, uid, lifecycle, pending_kind
				FROM imap_drafts WHERE draft_id = ?
			`), f.draftID).Scan(&currentMessageID, &newUID, &lifecycle, &pendingKind))
			assert.NotEqual(f.draftID, currentMessageID,
				"current_message_id must point at the new copy")
			assert.NotEqual(int64(f.draftUID), newUID, "uid must point at the new copy")
			assert.Equal("active", lifecycle)
			assert.Equal("edit", pendingKind.String,
				"the pending marker stays set for the next draft-edit")

			rawData, rawErr := f.store.GetMessageRawContext(t.Context(), currentMessageID)
			require.NoError(rawErr)
			assert.Contains(string(rawData), "Body that did land",
				"the archived raw must hold the body this call applied")

			// Both copies are in the mailbox: only the removal failed.
			assert.True(draftUIDPresentOnServer(t, f, emersionimap.UID(f.draftUID)),
				"the pre-edit copy must still be on the server")
			assert.True(draftUIDPresentOnServer(t, f, emersionimap.UID(newUID)),
				"the new copy must be on the server")

			// draft-delete does not retry the removal: it refuses a pending
			// edit and redirects to draft-edit.
			_, deleteErr := f.runLifecycle(t, "draft-delete", strconv.FormatInt(f.draftID, 10),
				"--revision="+strconv.FormatInt(storedDraftRevision(t, f), 10))
			require.Error(deleteErr)
			assert.Equal("operation_pending", deleteErr.Error())
		})
	}
}

// TestDraftRefusalInstructionsReachTheCLIRunClient drives the real
// handleCLIRun path end to end. CLIRunCodedError.Error() is the code alone and
// handleCLIRun streams exactly that, so anything the operator must act on
// reaches them only if it travels as a CLI event.
func TestDraftRefusalInstructionsReachTheCLIRunClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Simulate a crash after Persist committed: pending_uid names the pre-edit
	// copy and uid names the new one, so the refusal can identify a stale copy.
	origUID := int64(f.draftUID)
	staleTime := time.Now().Add(-(pendingEditStalenessThreshold + time.Minute))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1, uid = 99999
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(fmt.Sprintf(`[data]
data_dir = %q

[server]
api_key = %q
`, dataDir, "draft-boundary-secret")), 0o600))
	serverCfg, err := config.Load(configPath, "")
	require.NoError(err)

	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: serverCfg,
		Store:  f.grantedAdapter(),
		Logger: slog.New(slog.DiscardHandler),
	})
	body, err := json.Marshal(api.CLIRunRequest{Args: []string{
		"draft-edit", strconv.FormatInt(f.draftID, 10),
		"--revision=1", "--body=Body that must not be applied", "--json",
	}})
	require.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "draft-boundary-secret")
	response := httptest.NewRecorder()

	daemon.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	var stderr, streamedError string
	decoder := json.NewDecoder(response.Body)
	for decoder.More() {
		var event api.CLIRunEvent
		require.NoError(decoder.Decode(&event))
		switch event.Type {
		case cliStreamStderr:
			stderr += event.Data
		case "error":
			streamedError = event.Error
		}
	}

	// The error the client sees is the bare code: the cause never crosses.
	assert.Equal("edit_interrupted", streamedError)
	require.NotEmpty(stderr, "the refusal must stream a result the client can read")

	var delivered map[string]any
	require.NoError(json.Unmarshal([]byte(stderr), &delivered))
	assert.Equal("edit_interrupted", delivered["status"])
	assert.Equal(float64(origUID), delivered["uid"],
		"the removable stale copy must be named across the daemon boundary")
	assert.Equal(draftRemoveStaleInstruction, delivered["instructions"],
		"the recovery instruction must cross the daemon boundary")
	assert.NotContains(stderr, "Body that must not be applied",
		"a refusal must not echo the supplied body")
}

// countingRemoveClient wraps a real IMAP client and counts RemoveDraft calls
// across every client the factory hands out.
type countingRemoveClient struct {
	*imaplib.Client
	mu      *sync.Mutex
	removes *int
}

func (c *countingRemoveClient) RemoveDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error) {
	c.mu.Lock()
	*c.removes++
	c.mu.Unlock()
	return c.Client.RemoveDraft(ctx, target)
}

// TestConcurrentDraftDeletePendingDiscard verifies that two concurrent
// draft-delete calls against a pending-discard state issue exactly one remote
// removal, with no intermediate window between lock release and re-acquire.
// The loser either blocks on the lock and is refused, or arrives after the
// replay committed and reports the already-discarded draft without opening a
// connection; neither outcome removes anything a second time.
func TestConcurrentDraftDeletePendingDiscard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Simulate a prior Begin by injecting pending_kind='discard' and advancing the
	// revision to 2, exactly as BeginIMAPDraftOperationContext would leave it.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'discard', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1
		WHERE draft_id = ?
	`), time.Now().Add(-time.Minute), f.draftID)
	require.NoError(err)

	var removeMu sync.Mutex
	removes := 0
	factory := func(context.Context, *store.Source) (draftClient, error) {
		return &countingRemoveClient{
			Client:  imaplib.NewClient(f.config, testutil.IMAPTestPassword),
			mu:      &removeMu,
			removes: &removes,
		}, nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Both callers present the stored current revision, which is what
			// draft-get reports while the marker is set.
			adapter := f.grantedAdapter()
			adapter.draftLifecycleClientFactory = factory
			errs[idx] = adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
				Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=2"},
			}, nil)
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, e := range errs {
		if e == nil {
			successes++
			continue
		}
		// sync_active: the loser blocked on the lock while the winner held it.
		// revision_conflict: the loser's revision no longer matches.
		assert.True(e.Error() == "revision_conflict" || e.Error() == "sync_active" ||
			e.Error() == "draft_not_found",
			"unexpected error from losing caller: %v", e)
	}
	assert.GreaterOrEqual(successes, 1, "the pending discard must complete")

	removeMu.Lock()
	observedRemoves := removes
	removeMu.Unlock()
	assert.Equal(1, observedRemoves,
		"exactly one caller may act on the pending discard; the other must not reach RemoveDraft")

	var lifecycle string
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle, pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle, &pendingKind))
	assert.Equal("discarded", lifecycle)
	assert.False(pendingKind.Valid, "pending marker must be cleared exactly once")
}

// recreateDraftsMailbox deletes and recreates the Drafts mailbox and returns the
// new UIDVALIDITY. DELETE followed by CREATE is how a mailbox's epoch changes,
// and it is the one epoch change the in-tree fake models. Every UID recorded
// under the old epoch then names a different message, or no message at all.
func recreateDraftsMailbox(t *testing.T, f draftLifecycleFixture) uint32 {
	t.Helper()
	client, err := imapclient.DialInsecure(f.config.Host+":"+strconv.Itoa(f.config.Port), nil)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	require.NoError(t, client.Delete("Drafts").Wait())
	require.NoError(t, client.Create("Drafts", nil).Wait())
	selected, err := client.Select("Drafts", nil).Wait()
	require.NoError(t, err)
	require.NotEqual(t, f.draftUIDVal, selected.UIDValidity,
		"recreating the mailbox must change its UIDVALIDITY")
	return selected.UIDValidity
}

// TestDraftEditInterruptedNamesNoUIDAfterEpochChange is the destructive case the
// pending receipt alone cannot rule out. pending_uid differs from uid, so the
// marker says Persist committed and the pre-edit copy is the removable one — but
// the mailbox epoch moved after the marker was written, so that UID now names an
// unrelated message. The recovery must name no UID at all and must describe the
// copy instead, because the operator acts on a named UID by deleting it.
func TestDraftEditInterruptedNamesNoUIDAfterEpochChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)

	// Post-Persist marker shape: pending_uid names the pre-edit copy, uid names
	// the replacement, and the marker is older than the staleness threshold.
	origUID := int64(f.draftUID)
	staleTime := time.Now().Add(-(pendingEditStalenessThreshold + time.Minute))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts
		SET pending_kind = 'edit', pending_uid = uid, pending_uidvalidity = uidvalidity,
		    pending_started_at = ?, revision = revision + 1, uid = 99999
		WHERE draft_id = ?
	`), staleTime, f.draftID)
	require.NoError(err)

	// The epoch changes after the marker was written. A bystander then takes the
	// pre-edit copy's UID under the new epoch, so naming that UID would tell the
	// operator to delete a message this draft never owned.
	recreateDraftsMailbox(t, f)
	bystanderRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\n" +
		"Subject: Not a draft\r\nMessage-ID: <bystander-epoch@example.com>\r\n\r\nBystander\r\n")
	bystanderUID := appendRawToServer(t, f.config.Host+":"+strconv.Itoa(f.config.Port), "Drafts", bystanderRaw)
	require.Equal(origUID, int64(bystanderUID),
		"the bystander must take the pre-edit copy's UID under the new epoch")

	evs, editErr := f.runLifecycle(t,
		"draft-edit", strconv.FormatInt(f.draftID, 10), "--revision=1", "--body=after clear", "--json")
	require.Error(editErr)
	assert.Equal("edit_interrupted", editErr.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](editErr)
	require.True(ok)
	assert.NotContains(coded.Err.Error(), "UID=",
		"an unverifiable copy must not be named in the logged cause either")

	require.Len(evs, 1)
	assert.Equal(cliStreamStderr, evs[0].Type)
	delivered := decodeDraftLifecycleEvent(t, evs[0])
	assert.Equal("edit_interrupted", delivered["status"])
	assert.NotContains(delivered, "uid",
		"a UID whose epoch moved names a different message and must not be reported")
	assert.NotContains(evs[0].Data, strconv.FormatInt(origUID, 10),
		"the bystander's UID must appear nowhere in the delivered result")
	assert.Equal(draftLocateDuplicateInstruction, delivered["instructions"])

	// The recovery is still a recovery: the marker is cleared and nothing was
	// removed from the mailbox.
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&pendingKind))
	assert.False(pendingKind.Valid, "the interrupted marker must still be cleared")
	assert.True(draftUIDPresentOnServer(t, f, emersionimap.UID(bystanderUID)),
		"the bystander must survive the recovery untouched")
}

// failRemoveUnverifiableCopy fails RemoveDraft and makes every InspectDraft
// after the first report an epoch change. The first call is the pre-Begin
// ownership check the edit path makes before it claims; the later one is the
// re-verification of the pre-edit copy after the removal failed. This is what a
// mailbox recreated between the claim and the removal looks like to the daemon.
type failRemoveUnverifiableCopy struct {
	*imaplib.Client
	mu       sync.Mutex
	inspects int
}

func (c *failRemoveUnverifiableCopy) InspectDraft(
	ctx context.Context, target imaplib.DraftTarget,
) (imaplib.DraftInspectResult, error) {
	c.mu.Lock()
	c.inspects++
	first := c.inspects == 1
	c.mu.Unlock()
	if first {
		return c.Client.InspectDraft(ctx, target)
	}
	return imaplib.DraftInspectResult{}, &imaplib.DraftAppendError{
		State: imaplib.DraftStateRejected,
		Code:  "uidvalidity_changed",
		Err:   errors.New("injected epoch change before re-verification"),
	}
}

func (c *failRemoveUnverifiableCopy) RemoveDraft(
	context.Context, imaplib.DraftTarget,
) (imaplib.DraftInspectResult, error) {
	return imaplib.DraftInspectResult{}, &imaplib.DraftAppendError{
		State: imaplib.DraftStateRemoteUnknown,
		Code:  "remote_unknown",
		Err:   errors.New("injected RemoveDraft failure"),
	}
}

// TestDraftEditNamesNoUIDWhenLeftoverCopyCannotBeVerified covers the same rule
// on the post-persist arm: the edit applied, the removal of the pre-edit copy
// failed, and the copy could not be re-verified live. The result still has to
// report the partial outcome, but it must name no UID.
func TestDraftEditNamesNoUIDWhenLeftoverCopyCannotBeVerified(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = func(context.Context, *store.Source) (draftClient, error) {
		return &failRemoveUnverifiableCopy{
			Client: imaplib.NewClient(f.config, testutil.IMAPTestPassword),
		}, nil
	}

	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-edit", strconv.FormatInt(f.draftID, 10),
			"--revision=1", "--body=Body that did land", "--json"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(err)
	assert.Equal("edit_applied_old_copy_remains", err.Error())

	require.Len(evs, 1)
	result := decodeDraftLifecycleEvent(t, evs[0])
	assert.Equal("edit_applied_old_copy_remains", result["status"])
	assert.NotContains(result, "uid",
		"a leftover copy that cannot be verified live must not be named")
	assert.Equal(draftLocateDuplicateInstruction, result["instructions"])
	assert.NotContains(evs[0].Data, "Body that did land",
		"a partial result must not echo the supplied body")

	// The edit itself still committed: the local record points at the new copy.
	var currentMessageID int64
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT current_message_id, pending_kind FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&currentMessageID, &pendingKind))
	assert.NotEqual(f.draftID, currentMessageID)
	assert.Equal("edit", pendingKind.String)
}

// failInspectClient wraps a real IMAP client and fails every InspectDraft with a
// caller-supplied error.
type failInspectClient struct {
	*imaplib.Client
	inspectErr error
}

func (c *failInspectClient) InspectDraft(
	context.Context, imaplib.DraftTarget,
) (imaplib.DraftInspectResult, error) {
	return imaplib.DraftInspectResult{}, c.inspectErr
}

// TestDraftInspectFailureDeliversRecoveryInstructions covers the pre-Begin
// InspectDraft failure arms. CLIRunCodedError carries only its code across the
// daemon boundary, so a code returned bare reaches the caller as a bare code
// with nothing to act on, even though both of these codes are actionable: one
// says every recorded UID now names something else, the other says the mailbox
// state the operation depends on was never established.
func TestDraftInspectFailureDeliversRecoveryInstructions(t *testing.T) {
	epochChange := func() error {
		return &imaplib.DraftAppendError{State: imaplib.DraftStateRejected,
			Code: "uidvalidity_changed", Err: errors.New("injected epoch change")}
	}
	unknownRemote := func() error {
		return &imaplib.DraftAppendError{State: imaplib.DraftStateRemoteUnknown,
			Code: "remote_unknown", Err: errors.New("injected network failure")}
	}
	testCases := []struct {
		name        string
		command     string
		code        string
		instruction string
		inspectErr  error
	}{
		{"edit after an epoch change", "draft-edit", "uidvalidity_changed", draftReloadInstruction, epochChange()},
		{"delete after an epoch change", "draft-delete", "uidvalidity_changed", draftReloadInstruction, epochChange()},
		{"edit with an unknown remote", "draft-edit", "remote_unknown", draftInspectInstruction, unknownRemote()},
		{"delete with an unknown remote", "draft-delete", "remote_unknown", draftInspectInstruction, unknownRemote()},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := newDraftLifecycleFixture(t)
			adapter := f.grantedAdapter()
			adapter.draftLifecycleClientFactory = func(context.Context, *store.Source) (draftClient, error) {
				return &failInspectClient{
					Client:     imaplib.NewClient(f.config, testutil.IMAPTestPassword),
					inspectErr: testCase.inspectErr,
				}, nil
			}

			args := []string{testCase.command, strconv.FormatInt(f.draftID, 10), "--revision=1", "--json"}
			if testCase.command == "draft-edit" {
				args = append(args, "--body=never applied")
			}
			var evs []api.CLIRunEvent
			err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args},
				func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
			require.Error(err)
			assert.Equal(testCase.code, err.Error())

			require.Len(evs, 1, "an actionable refusal must reach the caller as an event")
			assert.Equal(cliStreamStderr, evs[0].Type)
			delivered := decodeDraftLifecycleEvent(t, evs[0])
			assert.Equal(testCase.code, delivered["status"])
			assert.Equal(testCase.instruction, delivered["instructions"])
			assert.NotContains(delivered, "uid", "a refusal before any claim names no copy")
			_, hasRevision := delivered["revision"]
			assert.False(hasRevision, "a refusal must carry no retry revision")

			// Nothing was claimed: the refusal is before Begin.
			var lifecycle string
			var revision int64
			var pendingKind sql.NullString
			require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
				SELECT lifecycle, revision, pending_kind FROM imap_drafts WHERE draft_id = ?
			`), f.draftID).Scan(&lifecycle, &revision, &pendingKind))
			assert.Equal("active", lifecycle)
			assert.Equal(int64(1), revision)
			assert.False(pendingKind.Valid)
		})
	}
}

// countingAppendClient wraps a real IMAP client and records every AppendDraft
// call, so a test can prove a refusal landed before the mailbox was touched.
type countingAppendClient struct {
	*imaplib.Client
	mu      *sync.Mutex
	appends *int
}

func (c *countingAppendClient) AppendDraft(
	ctx context.Context, mailbox string, raw []byte,
) (imaplib.DraftAppendResult, error) {
	c.mu.Lock()
	*c.appends++
	c.mu.Unlock()
	return c.Client.AppendDraft(ctx, mailbox, raw)
}

// replaceDraftArchivedRaw overwrites the archived RFC822 bytes of the draft's
// current message. GetIMAPDraftContext projects from_address from the sender
// participant row rather than from these bytes, so the draft keeps its
// confirmed identity and the substituted header matters only where the edit
// path actually reads it: ReplaceDraftBody, which copies From verbatim.
func replaceDraftArchivedRaw(t *testing.T, f draftLifecycleFixture, raw []byte) {
	t.Helper()
	var currentMessageID int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
		SELECT current_message_id FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&currentMessageID))
	result, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?
	`), raw, currentMessageID)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "the draft must have archived raw bytes to replace")
}

// TestDraftEditRefusesDraftWhoseFromResolvesToNoAddress covers the archived
// draft whose From header survives net/mail — which is all ReplaceDraftBody
// requires of it — but resolves to zero addresses once enmime parses the
// recomposed bytes. A null group is exactly that header.
//
// The persist builder indexes Parsed.From[0], and it runs only after Begin has
// claimed the draft and AppendDraft has landed the second copy, so a refusal
// arriving there would leave two copies on the server, no local persist, a
// pending marker, and an unrun sync-lock release. The refusal therefore has to
// happen while the composed bytes are still the only thing that exists, which
// is what the append count and the untouched ownership row prove here.
func TestDraftEditRefusesDraftWhoseFromResolvesToNoAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftLifecycleFixture(t)
	replaceDraftArchivedRaw(t, f, []byte(
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n"+
			"From: Undisclosed recipients:;\r\n"+
			"To: parent@example.com\r\n"+
			"Subject: Re: Parent\r\n"+
			"Message-ID: <null-group-from@example.test>\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=\"utf-8\"\r\n"+
			"\r\n"+
			"Initial draft body\r\n"))

	var appends int
	refreshesBefore := len(*f.refreshed)
	adapter := f.grantedAdapter()
	adapter.draftLifecycleClientFactory = func(context.Context, *store.Source) (draftClient, error) {
		return &countingAppendClient{
			Client:  imaplib.NewClient(f.config, testutil.IMAPTestPassword),
			mu:      new(sync.Mutex),
			appends: &appends,
		}, nil
	}

	var evs []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-edit", strconv.FormatInt(f.draftID, 10),
			"--revision=1", "--body=Body that must not be appended", "--json"},
	}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
	require.Error(err)
	assert.Equal("invalid_reply_metadata", err.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](err)
	require.True(ok)
	assert.ErrorContains(coded.Err, "exactly one From address")
	assert.NotContains(coded.Err.Error(), "Body that must not be appended",
		"a refusal must not echo the supplied body")
	assert.Empty(evs, "a refusal with nothing to recover reports no result")

	// The refusal lands before the mailbox is touched at all.
	assert.Zero(appends, "no copy may be appended for a draft that cannot be persisted")
	assert.True(draftUIDPresentOnServer(t, f, emersionimap.UID(f.draftUID)),
		"the original copy must still be the one the mailbox holds")

	// The ownership row is untouched: no claim, no marker, no revision bump.
	var lifecycle string
	var revision, currentMessageID int64
	var pendingKind sql.NullString
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT lifecycle, revision, pending_kind, current_message_id
		FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&lifecycle, &revision, &pendingKind, &currentMessageID))
	assert.Equal("active", lifecycle)
	assert.Equal(int64(1), revision, "the refusal must leave the original revision")
	assert.False(pendingKind.Valid, "no pending marker may survive a pre-claim refusal")
	assert.Equal(f.draftID, currentMessageID)
	assert.Len(*f.refreshed, refreshesBefore, "a refused edit refreshes no cache")
}

// bumpRevisionAfterRemove wraps a real IMAP client, performs the real removal,
// and then advances imap_drafts.revision so the Finish that follows finds its
// CAS no longer matching. It is the seam for the one outcome where the server
// copy is already expunged and only the local record is behind: the expunge is
// genuine, and only the local completion fails.
type bumpRevisionAfterRemove struct {
	*imaplib.Client
	store   *store.Store
	draftID int64
	t       *testing.T
}

func (c *bumpRevisionAfterRemove) RemoveDraft(
	ctx context.Context, target imaplib.DraftTarget,
) (imaplib.DraftInspectResult, error) {
	result, err := (&testDraftClient{Client: c.Client}).RemoveDraft(ctx, target)
	if err != nil {
		return result, err
	}
	_, execErr := c.store.DB().Exec(c.store.Rebind(`
		UPDATE imap_drafts SET revision = revision + 1 WHERE draft_id = ?
	`), c.draftID)
	require.NoError(c.t, execErr)
	return result, nil
}

// TestDraftDeleteReportsRemoteDeletedLocalFailed covers both sites that emit
// remote_deleted_local_failed: the ordinary draft-delete whose Finish fails
// after the expunge, and the pending-discard replay's equivalent.
//
// This is the only post-expunge state where the mail is already destroyed and
// only the local record is behind, so draftCompleteDiscardInstruction is the
// guidance the operator most needs. A CLIRunCodedError carries only its code
// across the daemon boundary, so the instruction reaches them only as a CLI
// event; the logged cause never crosses. The recovery it names — another
// draft-delete at the reloaded revision — is available only while the pending
// marker survives, so the row must still carry it.
func TestDraftDeleteReportsRemoteDeletedLocalFailed(t *testing.T) {
	testCases := []struct {
		name string
		// leaveState prepares the draft and returns the revision the failing
		// draft-delete must supply.
		leaveState func(t *testing.T, f draftLifecycleFixture) int64
	}{
		{
			name:       "ordinary delete",
			leaveState: func(*testing.T, draftLifecycleFixture) int64 { return 1 },
		},
		{
			name: "pending discard replay",
			leaveState: func(t *testing.T, f draftLifecycleFixture) int64 {
				// Fail the first RemoveDraft so the claim survives as
				// pending_kind='discard' and the retry replays it.
				adapter := f.grantedAdapter()
				adapter.draftLifecycleClientFactory = f.makeFaultyFactory(&sharedRemoveCounter{}, 1)
				err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
					Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10), "--revision=1"},
				}, nil)
				require.Error(t, err)
				require.Equal(t, "delete_failed", err.Error())
				var pendingKind sql.NullString
				require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`
					SELECT pending_kind FROM imap_drafts WHERE draft_id = ?
				`), f.draftID).Scan(&pendingKind))
				require.Equal(t, "discard", pendingKind.String)
				return storedDraftRevision(t, f)
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := newDraftLifecycleFixture(t)
			revision := testCase.leaveState(t, f)
			refreshesBefore := len(*f.refreshed)

			adapter := f.grantedAdapter()
			adapter.draftLifecycleClientFactory = func(context.Context, *store.Source) (draftClient, error) {
				return &bumpRevisionAfterRemove{
					Client:  imaplib.NewClient(f.config, testutil.IMAPTestPassword),
					store:   f.store,
					draftID: f.draftID,
					t:       t,
				}, nil
			}

			var evs []api.CLIRunEvent
			err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
				Args: []string{"draft-delete", strconv.FormatInt(f.draftID, 10),
					"--revision=" + strconv.FormatInt(revision, 10), "--json"},
			}, func(ev api.CLIRunEvent) error { evs = append(evs, ev); return nil })
			require.Error(err)
			assert.Equal("remote_deleted_local_failed", err.Error())
			coded, ok := errors.AsType[*api.CLIRunCodedError](err)
			require.True(ok)
			assert.ErrorContains(coded.Err, "revision_conflict")

			// The code alone is what the daemon streams as the error, so the
			// instruction is actionable only if it travels as an event.
			assert.NotContains(err.Error(), draftCompleteDiscardInstruction)
			require.Len(evs, 1, "a partial operation must report a result")
			assert.Equal(cliStreamStderr, evs[0].Type)
			delivered := decodeDraftLifecycleEvent(t, evs[0])
			assert.Equal("remote_deleted_local_failed", delivered["status"])
			assert.Equal(draftCompleteDiscardInstruction, delivered["instructions"])
			assert.NotContains(delivered, "uid",
				"no live check ran here, so no copy may be named")
			assert.NotContains(delivered, "operation_ref",
				"the expunged copy's coordinates name nothing that still exists")
			_, hasRevision := delivered["revision"]
			assert.False(hasRevision, "a partial result must carry no retry revision")

			// The server copy really is gone: only the local record is behind.
			assert.False(draftUIDPresentOnServer(t, f, emersionimap.UID(f.draftUID)),
				"the expunge must have landed before Finish was attempted")

			// The pending marker survives, which is what makes the instruction's
			// recovery — another draft-delete at the reloaded revision — available.
			var lifecycle string
			var pendingKind sql.NullString
			var pendingUID sql.NullInt64
			require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
				SELECT lifecycle, pending_kind, pending_uid FROM imap_drafts WHERE draft_id = ?
			`), f.draftID).Scan(&lifecycle, &pendingKind, &pendingUID))
			assert.Equal("active", lifecycle, "Finish never committed, so lifecycle is unchanged")
			assert.Equal("discard", pendingKind.String, "the pending discard must survive")
			assert.True(pendingUID.Valid, "the pending receipt must survive with the marker")
			assert.Len(*f.refreshed, refreshesBefore, "a failed completion refreshes no cache")

			// The documented recovery works: draft-delete at the reloaded
			// revision completes the discard against an already-absent copy.
			_, retryErr := f.runLifecycle(t, "draft-delete", strconv.FormatInt(f.draftID, 10),
				"--revision="+strconv.FormatInt(storedDraftRevision(t, f), 10))
			require.NoError(retryErr, "the instructed recovery must complete the discard")
			require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
				SELECT lifecycle FROM imap_drafts WHERE draft_id = ?
			`), f.draftID).Scan(&lifecycle))
			assert.Equal("discarded", lifecycle)
		})
	}
}
