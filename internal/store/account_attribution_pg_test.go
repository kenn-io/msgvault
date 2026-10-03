package store_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const pgWaitBudget = 5 * time.Second

func requirePostgreSQLStore(t *testing.T) *store.Store {
	t.Helper()
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL lock-order regression; set MSGVAULT_TEST_DB")
	}
	return st
}

// waitForLockWait returns the backend waiting on a lock while running a
// statement that contains fragment.
func waitForLockWait(t *testing.T, st *store.Store, fragment, message string) int {
	t.Helper()
	var pid int
	var err error
	require.Eventually(t, func() bool {
		err = st.DB().QueryRowContext(context.Background(), `
			SELECT COALESCE(MIN(pid), 0) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND POSITION($1 IN query) > 0`, fragment).Scan(&pid)
		return err == nil && pid > 0
	}, pgWaitBudget, 10*time.Millisecond, message)
	require.NoError(t, err)
	return pid
}

func holdsRelationLock(t *testing.T, st *store.Store, pid int, relation string) bool {
	t.Helper()
	var held bool
	require.NoError(t, st.DB().QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE l.pid = $1 AND c.relname = $2 AND l.granted)`, pid, relation).Scan(&held))
	return held
}

// holdIdentityRow takes the identity revision row the way identity writers
// do and keeps it until release runs.
func holdIdentityRow(t *testing.T, st *store.Store) func() {
	t.Helper()
	ctx := context.Background()
	_, err := st.DB().ExecContext(ctx, `INSERT INTO archive_metadata (key, value) VALUES ('identity_revision', '0') ON CONFLICT (key) DO NOTHING`)
	require.NoError(t, err)
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE archive_metadata SET value = value WHERE key = 'identity_revision'`)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Commit() }) }
	t.Cleanup(release)
	return release
}

func waitResult(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(pgWaitBudget):
		require.FailNow(t, what+" did not finish")
		return nil
	}
}

func assertNoDeadlock(t *testing.T, err error) {
	t.Helper()
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		assert.NotEqual(t, "40P01", pgErr.Code, "deadlock detected")
	}
	assert.NoError(t, err)
}

func scopedSync(t *testing.T, st *store.Store, sourceID int64) *store.Store {
	t.Helper()
	runID, err := st.StartSync(sourceID, "full")
	require.NoError(t, err)
	return st.ScopedToSync(sourceID, runID)
}

func TestAccountAttributionConcurrentSourceSyncsPG(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := requirePostgreSQLStore(t)
	a := newAttrFixtureOn(t, st, "gmail", "a@example.net")
	b := newAttrFixtureOn(t, st, "gmail", "b@example.net")

	reached := map[int64]chan struct{}{a.source.ID: make(chan struct{}), b.source.ID: make(chan struct{})}
	release := map[int64]chan struct{}{a.source.ID: make(chan struct{}), b.source.ID: make(chan struct{})}
	var once sync.Map
	restore := st.SetAttributionAfterLockHookForTest(func(sources []int64) {
		if len(sources) != 1 {
			return
		}
		if _, loaded := once.LoadOrStore(sources[0], true); loaded {
			return
		}
		if ch, ok := reached[sources[0]]; ok {
			close(ch)
			<-release[sources[0]]
		}
	})
	defer restore()
	releaseAll := sync.OnceFunc(func() {
		close(release[a.source.ID])
		close(release[b.source.ID])
	})
	t.Cleanup(releaseAll)

	persist := func(f *attrFixture, scoped *store.Store, key string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := scoped.PersistMessageContext(context.Background(), &store.MessagePersistData{
				Message:    &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: key, MessageType: "email"},
				RawMIME:    []byte("To: alias@example.org\r\n\r\nbody"),
				Recipients: []store.RecipientSet{f.recipientSet("to", []string{"alias@example.org"}, true)},
			})
			done <- err
		}()
		return done
	}
	aDone := persist(a, scopedSync(t, st, a.source.ID), "a-1")
	<-reached[a.source.ID]
	bDone := persist(b, scopedSync(t, st, b.source.ID), "b-1")
	select {
	case <-reached[b.source.ID]:
	case <-time.After(pgWaitBudget):
		require.FailNow("source B's persist must pass its entry locks while A holds its own")
	}

	addDone := make(chan error, 1)
	go func() { addDone <- st.AddAccountIdentity(a.source.ID, "alias@example.org", "manual") }()
	waitForLockWait(t, st, "UPDATE archive_metadata SET value = value",
		"the identity add must wait for both attribution readers")

	releaseAll()
	assertNoDeadlock(t, waitResult(t, aDone, "source A persist"))
	assertNoDeadlock(t, waitResult(t, bDone, "source B persist"))
	assertNoDeadlock(t, waitResult(t, addDone, "identity add"))
	ids := searchIDs(t, st, "received:alias@example.org")
	require.Len(ids, 1)
	address, _ := attribution(t, st, ids[0])
	assert.Equal("alias@example.org", address.String)
}

func TestAccountAttributionScopedPersistVsSourceRemovalPG(t *testing.T) {
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "gmail", "persist@example.net")
	victim := newAttrFixtureOn(t, st, "gmail", "victim@example.net")
	f.confirm("persist@example.net")
	scoped := scopedSync(t, st, f.source.ID)

	release := holdIdentityRow(t, st)
	persistDone := make(chan error, 1)
	go func() {
		_, err := scoped.PersistMessageContext(context.Background(), &store.MessagePersistData{
			Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "p-1", MessageType: "email"},
			RawMIME: []byte("Delivered-To: persist@example.net\r\n\r\nbody"),
		})
		persistDone <- err
	}()
	pid := waitForLockWait(t, st, "FOR SHARE", "the scoped persist must wait at its identity share lock")
	assert.False(t, holdsRelationLock(t, st, pid, "sync_runs"), "the waiting persist must not hold sync_runs")

	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(context.Background(), victim.source.ID)
		removeDone <- err
	}()
	require.Eventually(t, func() bool {
		var waiting int
		return st.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND POSITION('archive_metadata' IN query) > 0`).Scan(&waiting) == nil && waiting >= 2
	}, pgWaitBudget, 10*time.Millisecond, "source removal must wait for the identity row")
	release()
	assertNoDeadlock(t, waitResult(t, persistDone, "scoped persist"))
	assertNoDeadlock(t, waitResult(t, removeDone, "source removal"))
	assert.Len(t, searchIDs(t, st, "received:persist@example.net"), 1)
}

func TestAccountAttributionPersistVsIMAPLabelRepairPG(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "imap", "imaps://owner%40example.net@mail.example.net:993")
	f.confirm("owner@example.net")
	id := f.persist(attrMail{raw: "Delivered-To: owner@example.net\r\n\r\nbody", sourceMsgKey: "imap-1"})
	require.NoError(st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 3, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 3, UID: 1, SourceMessageID: "imap-1"}},
	}}))
	archive, err := st.EnsureLabel(f.source.ID, "Archive", "Archive", "user")
	require.NoError(err)

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetIMAPLabelRepairPerMessageHookForTest(func(int64) {
		pauseOnce.Do(func() {
			close(paused)
			<-resume
		})
	})
	defer restore()
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)

	repairDone := make(chan error, 1)
	go func() {
		_, err := st.RepairIMAPSourceLabels(context.Background(), f.source.ID, true)
		repairDone <- err
	}()
	<-paused
	var repairPID int
	require.Eventually(func() bool {
		return st.DB().QueryRowContext(context.Background(), `
			SELECT COALESCE(MIN(l.pid), 0) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE c.relname = 'sources' AND l.mode = 'RowExclusiveLock' AND l.granted
			  AND l.pid <> pg_backend_pid()`).Scan(&repairPID) == nil && repairPID > 0
	}, pgWaitBudget, 10*time.Millisecond)

	persistDone := make(chan error, 1)
	go func() {
		_, err := st.PersistMessageContext(context.Background(), &store.MessagePersistData{
			Message:  &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "imap-1", MessageType: "email"},
			RawMIME:  []byte("Delivered-To: owner@example.net\r\n\r\nbody"),
			LabelIDs: []int64{archive},
		})
		persistDone <- err
	}()
	persistPID := waitForLockWait(t, st, "UPDATE sources SET updated_at", "the persist must wait on the source row")
	var blockers []int64
	rows, err := st.DB().QueryContext(context.Background(), `SELECT unnest(pg_blocking_pids($1))`, persistPID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pid int64
		require.NoError(rows.Scan(&pid))
		blockers = append(blockers, pid)
	}
	require.NoError(rows.Err())
	assert.Equal([]int64{int64(repairPID)}, blockers, "only the label repair blocks the persist")

	resumeOnce()
	assertNoDeadlock(t, waitResult(t, repairDone, "label repair"))
	assertNoDeadlock(t, waitResult(t, persistDone, "persist"))
	labels, err := st.MessageLabelIDsContext(t.Context(), id)
	require.NoError(err)
	assert.Equal([]int64{archive}, labels, "the later persist's labels win")
	assert.Equal([]int64{id}, searchIDs(t, st, "received:owner@example.net"))
	assert.Equal([]int64{id}, searchIDs(t, st, "account:owner@example.net"))
}

func TestAccountAttributionSentRoleChangeVsPersistPG(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "imap", "imaps://owner%40example.net@mail.example.net:993")
	f.confirm("owner@example.net")
	labels, err := st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{"Outbox": {Name: "Outbox", Type: "user"}})
	require.NoError(err)
	outbox := labels["Outbox"]

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetAttributionAfterLockHookForTest(func(sources []int64) {
		if slices.Contains(sources, f.source.ID) {
			pauseOnce.Do(func() {
				close(paused)
				<-resume
			})
		}
	})
	defer restore()
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)

	persistDone := make(chan error, 1)
	go func() {
		_, err := st.PersistMessageContext(context.Background(), &store.MessagePersistData{
			Message:    &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "out-1", MessageType: "email"},
			RawMIME:    []byte("From: owner@example.net\r\n\r\nbody"),
			Recipients: []store.RecipientSet{f.recipientSet("from", []string{"owner@example.net"}, true)},
			LabelIDs:   []int64{outbox},
		})
		persistDone <- err
	}()
	<-paused
	roleDone := make(chan error, 1)
	go func() {
		_, err := st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
			"Outbox": {Name: "Outbox", Type: "user", SystemRole: store.LabelSystemRoleSent},
		})
		roleDone <- err
	}()
	waitForLockWait(t, st, "UPDATE sources SET updated_at", "the Sent role change must wait on the source row")
	resumeOnce()
	assertNoDeadlock(t, waitResult(t, persistDone, "persist"))
	assertNoDeadlock(t, waitResult(t, roleDone, "Sent role change"))
	ids := searchIDs(t, st, "account:owner@example.net")
	require.Len(ids, 1)
	assert.Empty(searchIDs(t, st, "received:owner@example.net"))
}

func TestAccountAttributionScopedParticipantMergeVsSourceRemovalPG(t *testing.T) {
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "beeper", "beeper-account")
	victim := newAttrFixtureOn(t, st, "gmail", "victim@example.net")
	oldID := f.participant("old@example.org")
	newID := f.participant("new@example.org")
	other := f.participant("third@example.org")
	scoped := scopedSync(t, st, f.source.ID)

	release := holdIdentityRow(t, st)
	mergeDone := make(chan error, 1)
	go func() {
		if err := scoped.MergeParticipants(oldID, newID); err != nil {
			mergeDone <- err
			return
		}
		mergeDone <- scoped.SetParticipantIdentifier(other, store.PhoneIdentifierType, "+15555550100")
	}()
	pid := waitForLockWait(t, st, "archive_metadata", "the scoped merge must wait for the identity row")
	assert.False(t, holdsRelationLock(t, st, pid, "sync_runs"), "the waiting merge must not hold sync_runs")
	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(context.Background(), victim.source.ID)
		removeDone <- err
	}()
	release()
	assertNoDeadlock(t, waitResult(t, mergeDone, "participant merge"))
	assertNoDeadlock(t, waitResult(t, removeDone, "source removal"))
}

func TestAccountAttributionIdentityAndDraftEntriesVsSourceRemovalPG(t *testing.T) {
	require := require.New(t)
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "gmail", "alice@example.com")
	victim := newAttrFixtureOn(t, st, "gmail", "victim@example.net")
	f.confirm("alice@example.com")
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}
	receipt := store.GmailDraftReceipt{SourceID: f.source.ID, GmailDraftID: "d-1", GmailMessageID: "gm-1", ThreadID: "t-1"}
	draft, err := st.PersistGmailDraftContext(t.Context(), receipt, participants, gmailTestBuild(f.source.ID, f.conv, receipt, []byte("old")))
	require.NoError(err)
	_, err = st.ClaimGmailDraftContext(t.Context(), draft.DraftID, 1, store.GmailDraftOperationEdit, []byte("new"))
	require.NoError(err)
	require.NoError(st.RecordGmailDraftOutcomeContext(t.Context(), draft.DraftID, 1, "accepted_local_failed", "gm-2"))
	scoped := scopedSync(t, st, f.source.ID)

	release := holdIdentityRow(t, st)
	mergeDone := make(chan error, 1)
	go func() {
		_, err := scoped.MergeConfirmedAccountIdentitySignalsContext(context.Background(), f.source.ID,
			[]store.IdentityConfirmation{{Identifier: "alice@example.com", Signals: []string{"profile"}}})
		mergeDone <- err
	}()
	mergePID := waitForLockWait(t, st, "archive_metadata", "the identity merge must wait for the identity row")
	publishDone := make(chan error, 1)
	go func() {
		replacement := store.GmailDraftReceipt{SourceID: f.source.ID, GmailDraftID: "d-1", GmailMessageID: "gm-2", ThreadID: "t-1"}
		_, err := st.PublishGmailDraftReplacementContext(context.Background(), draft.DraftID, 1, "gm-2", participants,
			gmailTestBuild(f.source.ID, f.conv, replacement, []byte("new")))
		publishDone <- err
	}()
	publishPID := waitForLockWait(t, st, "FOR SHARE", "the draft publication must wait at its identity share lock")
	for _, pid := range []int{mergePID, publishPID} {
		assert.False(t, holdsRelationLock(t, st, pid, "sync_runs"))
		assert.False(t, holdsRelationLock(t, st, pid, "gmail_drafts"))
	}
	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(context.Background(), victim.source.ID)
		removeDone <- err
	}()
	release()
	assertNoDeadlock(t, waitResult(t, mergeDone, "identity merge"))
	assertNoDeadlock(t, waitResult(t, publishDone, "draft publication"))
	assertNoDeadlock(t, waitResult(t, removeDone, "source removal"))
}

func TestAccountAttributionOrdinaryDraftsTakeNoIdentityLockPG(t *testing.T) {
	require := require.New(t)
	st := requirePostgreSQLStore(t)
	beeper := newAttrFixtureOn(t, st, "beeper", "beeper-account")
	bdraft, err := st.CreateBeeperDraftContext(t.Context(), beeper.source.ID, "!chat:example.org", "hello")
	require.NoError(err)
	g := newAttrFixtureOn(t, st, "gmail", "alice@example.com")
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}
	receipt := store.GmailDraftReceipt{SourceID: g.source.ID, GmailDraftID: "d-1", GmailMessageID: "gm-1", ThreadID: "t-1"}
	gdraft, err := st.PersistGmailDraftContext(t.Context(), receipt, participants, gmailTestBuild(g.source.ID, g.conv, receipt, []byte("old")))
	require.NoError(err)

	release := holdIdentityRow(t, st)
	defer release()
	done := make(chan error, 2)
	go func() {
		_, err := st.FinishBeeperDraftContext(context.Background(), bdraft.DraftID, bdraft.Revision, "hello")
		done <- err
	}()
	go func() {
		_, err := st.ClaimGmailDraftContext(context.Background(), gdraft.DraftID, 1, store.GmailDraftOperationDelete, nil)
		done <- err
	}()
	assert.NoError(t, waitResult(t, done, "Beeper or Gmail draft write"))
	assert.NoError(t, waitResult(t, done, "Beeper or Gmail draft write"))
}

func TestAccountAttributionRetypeDuringRefreshPG(t *testing.T) {
	require := require.New(t)
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "mbox", "archive-1")
	f.confirm("work@example.org")
	id := f.persist(attrMail{raw: "To: work@example.org\r\n\r\nbody", to: []string{"work@example.org"}, sourceMsgKey: "retype"})

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetAccountAttributionAfterReadHookForTest(func(messageID int64) {
		if messageID == id {
			pauseOnce.Do(func() {
				close(paused)
				<-resume
			})
		}
	})
	defer restore()
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- st.RefreshAccountAttributionForTest(context.Background(), id) }()
	<-paused
	_, err := st.UpsertMessage(&store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "retype", MessageType: "chat",
	})
	require.NoError(err)
	resumeOnce()
	require.NoError(waitResult(t, refreshDone, "paused refresh"))
	address, path := attribution(t, st, id)
	assert.Equal(t, sql.NullString{}, address)
	assert.Equal(t, sql.NullString{}, path, "a refresh that read the old type must not restore the account")
}
