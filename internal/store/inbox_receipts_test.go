package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func inboxReceiptFixture(t *testing.T) (*storetest.Fixture, inboxcontrol.Receipt) {
	t.Helper()
	f := storetest.New(t)
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: f.CreateMessage("inbox-receipt-message"), ProviderID: "inbox-receipt-message"}
	inbox := true
	state := inboxcontrol.State{Target: target, Inbox: &inbox, ObservedAt: time.Now().UTC()}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
	intent, err := inboxcontrol.IntentFingerprint(request)
	require.NoError(t, err)
	plan, err := inboxcontrol.SemanticFingerprint(state)
	require.NoError(t, err)
	return f, inboxcontrol.Receipt{ID: "receipt-fixture", PrincipalID: "agent-fixture", SourceID: f.Source.ID, IdempotencyKey: "archive-fixture", IntentHash: intent, StateHash: plan, Intent: request, Before: state, Status: inboxcontrol.StatusPrepared, CreatedAt: time.Now().UTC()}
}

func TestInboxLedgerIntentCollision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	got, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.True(claimed)
	assertions.Equal(receipt.ID, got.ID)
	receipt.ID = "receipt-repeated"
	got, claimed, err = f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.False(claimed)
	assertions.Equal("receipt-fixture", got.ID)
	receipt.Intent.Operation = inboxcontrol.OpSetUnread
	receipt.IntentHash, err = inboxcontrol.IntentFingerprint(receipt.Intent)
	requirements.NoError(err)
	_, _, err = f.Store.PrepareInboxReceipt(t.Context(), receipt)
	require.ErrorIs(t, err, inboxcontrol.ErrConflict)
	other, err := f.Store.LookupInboxReceipt(t.Context(), "another-agent", receipt.SourceID, receipt.IdempotencyKey)
	requirements.NoError(err)
	assertions.Nil(other)
}

func TestInboxLedgerAcceptsBoundedUTF8KeysOnBothBackends(t *testing.T) {
	for _, key := range []string{" ", "operation\x00key", "operation\nkey", "操作"} {
		t.Run(fmt.Sprintf("%x", key), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f, receipt := inboxReceiptFixture(t)
			receipt.IdempotencyKey = key
			_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			assertions.True(claimed)
			got, err := f.Store.LookupInboxReceipt(t.Context(), receipt.PrincipalID, receipt.SourceID, key)
			requirements.NoError(err)
			requirements.NotNil(got)
			assertions.Equal(key, got.IdempotencyKey)
		})
	}
}

func TestInboxLedgerConcurrentClaim(t *testing.T) {
	f, receipt := inboxReceiptFixture(t)
	type outcome struct {
		claimed bool
		err     error
	}
	results := make(chan outcome, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			claimedReceipt := receipt
			claimedReceipt.ID = fmt.Sprintf("receipt-concurrent-%d", i)
			_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), claimedReceipt)
			results <- outcome{claimed, err}
		})
	}
	wg.Wait()
	close(results)
	claims := 0
	for result := range results {
		require.NoError(t, result.err)
		if result.claimed {
			claims++
		}
	}
	assert.Equal(t, 1, claims)
}

func TestInboxLedgerRestartNoReplay(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
	require.ErrorIs(t, f.Store.MarkInboxDispatching(t.Context(), receipt.ID), inboxcontrol.ErrConflict)
	requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusUnknown, got.Status)
	assertions.NotNil(got.DispatchedAt)
	assertions.Equal("interrupted-after-dispatch", got.FailureCode)
	require.ErrorIs(t, f.Store.MarkInboxDispatching(t.Context(), receipt.ID), inboxcontrol.ErrConflict)
	got, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.False(claimed)
	assertions.Equal(inboxcontrol.StatusUnknown, got.Status)
	requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
	still, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(got, still)
}

func TestInboxLedgerInterruptedPrepared(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusFailed, got.Status)
	assertions.Nil(got.DispatchedAt)
	assertions.Equal("interrupted-before-dispatch", got.FailureCode)
}

func TestInboxLedgerRecoveryAfterSourceRemoval(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dispatched bool
		status     inboxcontrol.Status
		code       string
	}{
		{"prepared", false, inboxcontrol.StatusFailed, "interrupted-before-dispatch"},
		{"dispatching", true, inboxcontrol.StatusUnknown, "interrupted-after-dispatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			if tc.dispatched {
				requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
			}
			_, _, err = f.Store.RemoveSourceSerialized(t.Context(), receipt.SourceID)
			requirements.NoError(err)
			_, err = f.Store.GetSourceByID(receipt.SourceID)
			requirements.ErrorIs(err, store.ErrSourceNotFound)

			requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()), "retained receipts must not prevent startup after source removal")
			got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
			requirements.NoError(err)
			requirements.NotNil(got)
			assertions.Equal(tc.status, got.Status)
			assertions.Equal(tc.code, got.FailureCode)
			assertions.NotNil(got.FinishedAt)
			requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
			repeated, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			assertions.False(claimed)
			assertions.Equal(got, repeated, "source removal must not erase or replay dispatch evidence")
		})
	}
}

func TestInboxLedgerRecoveryPreservesActiveDispatcher(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
	execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), receipt.SourceID)
	requirements.NoError(err)
	t.Cleanup(func() { _ = execution.Release() })
	requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusDispatching, got.Status, "recovery must not interrupt a worker that still owns the source")
	requirements.NoError(execution.Release())
	requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
	got, err = f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusUnknown, got.Status)
}

func TestInboxLedgerFinishPreservesDispatchAndRejectsOverwrite(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
	after := receipt.Before
	after.Inbox = new(false)
	requirements.NoError(f.Store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusVerified, &after, ""))
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, got.Status)
	assertions.NotNil(got.DispatchedAt)
	assertions.NotNil(got.FinishedAt)
	assertions.False(*got.After.Inbox)
	assertions.ErrorIs(f.Store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusUnknown, nil, "lost-response"), inboxcontrol.ErrConflict)
}

func TestInboxLedgerPersistsAcrossReopen(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	testutil.SkipIfPostgres(t, "SQLite file reopen; backend-neutral recovery covered above")
	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
	var sequence int
	var name, path string
	requirements.NoError(f.Store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	requirements.NoError(f.Store.Close())
	reopened, err := store.OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	requirements.NoError(reopened.RecoverInboxReceipts(t.Context()))
	got, claimed, err := reopened.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.False(claimed)
	assertions.Equal(inboxcontrol.StatusUnknown, got.Status)
	assertions.NotNil(got.DispatchedAt)
}

func TestInboxLedgerRecordsIMAPMoveMappingBeforeReadback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	original := receipt.Before.Target
	original.SourceType, original.Mailbox, original.UIDValidity, original.UID = "imap", "INBOX", 7, 11
	receipt.Before.Target = original
	receipt.Intent.Target = &original
	var err error
	receipt.IntentHash, err = inboxcontrol.IntentFingerprint(receipt.Intent)
	requirements.NoError(err)
	receipt.StateHash, err = inboxcontrol.SemanticFingerprint(receipt.Before)
	requirements.NoError(err)
	_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.True(claimed)
	requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))

	destination := original
	destination.Mailbox, destination.UIDValidity, destination.UID = "Archive", 9, 4
	mapping := inboxcontrol.State{Target: destination, ObservedAt: time.Now().UTC()}
	requirements.NoError(f.Store.RecordInboxReceiptMapping(t.Context(), receipt.ID, mapping))
	stored, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	requirements.NotNil(stored.After)
	assertions.Equal(mapping, *stored.After)
	assertions.Equal(inboxcontrol.StatusDispatching, stored.Status)

	requirements.NoError(f.Store.RecoverInboxReceipts(t.Context()))
	stored, err = f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusUnknown, stored.Status)
	requirements.NotNil(stored.After)
	assertions.Equal(destination, stored.After.Target)
}

func TestInboxLedgerRejectsMismatchedFinalObservation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
	after := receipt.Before
	after.Target.ItemID += 100
	err = f.Store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusVerified, &after, "")
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	got, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusDispatching, got.Status)
}

func TestInboxLedgerRejectsCapabilityReadAsMutation(t *testing.T) {
	f, receipt := inboxReceiptFixture(t)
	target := receipt.Before.Target
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	receipt.Intent = inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source, DryRun: true}
	receipt.Before.Target, receipt.Before.Source = inboxcontrol.Target{}, source
	var err error
	receipt.IntentHash, err = inboxcontrol.IntentFingerprint(receipt.Intent)
	require.NoError(t, err)
	receipt.StateHash, err = inboxcontrol.SemanticFingerprint(receipt.Before)
	require.NoError(t, err)
	_, _, err = f.Store.PrepareInboxReceipt(t.Context(), receipt)
	assert.ErrorIs(t, err, inboxcontrol.ErrInvalid)
}
