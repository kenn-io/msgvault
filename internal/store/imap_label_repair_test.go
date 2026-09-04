package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// seedTwoInboxMessages puts two messages in INBOX via a normal (Reset-free)
// membership apply, matching how a real sync would record them.
func seedTwoInboxMessages(t *testing.T, f imapMembershipFixture) (int64, int64) {
	t.Helper()
	messageID1 := f.createMessage(t, "repair-labels-1", "<repair-labels-1@example.com>")
	messageID2 := f.createMessage(t, "repair-labels-2", "<repair-labels-2@example.com>")
	require.NoError(t, f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 3, HighestModSeq: 1},
		Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 1, UID: 1, SourceMessageID: "repair-labels-1"},
			{Mailbox: "INBOX", UIDValidity: 1, UID: 2, SourceMessageID: "repair-labels-2"},
		},
	}}))
	return messageID1, messageID2
}

// TestRepairIMAPSourceLabels_RemovesStrayAddOnlyLabel reproduces the gap
// described in issue #748: an add-only label merge leaves a label that no
// imap_message_memberships row backs, and nothing else ever revisits it.
func TestRepairIMAPSourceLabels_RemovesStrayAddOnlyLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID1, messageID2 := seedTwoInboxMessages(t, f)

	strayLabelID, err := f.store.EnsureLabel(f.source.ID, "stray-source-label", "Stray", "user")
	require.NoError(err)
	changed, err := f.store.ReconcileMessageLabels(messageID1, []int64{strayLabelID}, false)
	require.NoError(err)
	require.True(changed)
	require.Equal([]string{"INBOX", "Stray"}, messageLabels(t, f.store, messageID1))

	summary, err := f.store.RepairIMAPSourceLabels(context.Background(), f.source.ID, true)
	require.NoError(err)
	assert.Equal(2, summary.Scanned)
	assert.Equal(1, summary.Changed)
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, messageID1))
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, messageID2))
}

// TestRepairIMAPSourceLabels_DryRunDoesNotWrite asserts a dry run reports
// what would change without persisting it.
func TestRepairIMAPSourceLabels_DryRunDoesNotWrite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID1, _ := seedTwoInboxMessages(t, f)

	strayLabelID, err := f.store.EnsureLabel(f.source.ID, "stray-source-label", "Stray", "user")
	require.NoError(err)
	_, err = f.store.ReconcileMessageLabels(messageID1, []int64{strayLabelID}, false)
	require.NoError(err)

	summary, err := f.store.RepairIMAPSourceLabels(context.Background(), f.source.ID, false)
	require.NoError(err)
	assert.Equal(2, summary.Scanned)
	assert.Equal(1, summary.Changed)
	assert.Equal([]string{"INBOX", "Stray"}, messageLabels(t, f.store, messageID1))
}

// TestRepairIMAPSourceLabels_NoopWhenLabelsAlreadyMatch asserts a second
// apply run over already-consistent labels changes nothing.
func TestRepairIMAPSourceLabels_NoopWhenLabelsAlreadyMatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	seedTwoInboxMessages(t, f)

	summary, err := f.store.RepairIMAPSourceLabels(context.Background(), f.source.ID, true)
	require.NoError(err)
	assert.Equal(2, summary.Scanned)
	assert.Equal(0, summary.Changed)
}

// TestRepairIMAPSourceLabels_CancellationStopsTheLoop catches a repair that
// keeps processing every message regardless of context cancellation. The
// per-message test hook cancels after the first message, so a second message
// being processed proves cancellation was not honored between messages.
func TestRepairIMAPSourceLabels_CancellationStopsTheLoop(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID1, _ := seedTwoInboxMessages(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var processed []int64
	restore := f.store.SetIMAPLabelRepairPerMessageHookForTest(func(messageID int64) {
		processed = append(processed, messageID)
		cancel()
	})
	defer restore()

	summary, err := f.store.RepairIMAPSourceLabels(ctx, f.source.ID, true)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(store.IMAPLabelRepairSummary{}, summary)
	assert.Equal([]int64{messageID1}, processed)
}

// TestRepairIMAPSourceLabels_DryRunCancellationIsNotMaskedAsSuccess catches
// the specific failure mode roborev flagged on PR #754: the dry-run rollback
// sentinel returning as a nil error even when the context was cancelled
// mid-repair, which would report a cancelled run as a completed dry run.
func TestRepairIMAPSourceLabels_DryRunCancellationIsNotMaskedAsSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID1, _ := seedTwoInboxMessages(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var processed []int64
	restore := f.store.SetIMAPLabelRepairPerMessageHookForTest(func(messageID int64) {
		processed = append(processed, messageID)
		cancel()
	})
	defer restore()

	summary, err := f.store.RepairIMAPSourceLabels(ctx, f.source.ID, false)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(store.IMAPLabelRepairSummary{}, summary)
	assert.Equal([]int64{messageID1}, processed)
}
