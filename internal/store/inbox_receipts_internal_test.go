package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestPrepareInboxReceiptReturnsInsertedClaimWithoutCancelableLookup(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, err := OpenForTest(filepath.Join(t.TempDir(), "inbox-receipts.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "receipt-thread", "Receipt")
	requirements.NoError(err)
	itemID, err := st.UpsertMessage(&Message{SourceID: source.ID, SourceMessageID: "receipt-message", ConversationID: conversationID, MessageType: "email"})
	requirements.NoError(err)

	now := time.Now().UTC()
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: itemID, ProviderID: "receipt-message"}
	before := inboxcontrol.State{Target: target, Inbox: new(true), ObservedAt: now}
	intent := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
	intentHash, err := inboxcontrol.IntentFingerprint(intent)
	requirements.NoError(err)
	stateHash, err := inboxcontrol.SemanticFingerprint(before)
	requirements.NoError(err)
	receipt := inboxcontrol.Receipt{ID: "receipt-canceled-lookup", PrincipalID: "agent-fixture", SourceID: source.ID, IdempotencyKey: "archive-fixture", IntentHash: intentHash, StateHash: stateHash, Intent: intent, Before: before, Status: inboxcontrol.StatusPrepared, CreatedAt: now}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trigger := cancelAtStatement{
		after:  "INSERT INTO inbox_operation_receipts",
		stop:   "SELECT receipt_json",
		cancel: cancel,
	}
	trigger.install(st.db)

	got, claimed, err := st.PrepareInboxReceipt(ctx, receipt)
	requirements.NoError(err)
	requirements.True(claimed)
	requirements.NotNil(got)
	assertions.Equal(receipt.ID, got.ID)
	assertions.True(trigger.armed, "the receipt INSERT was observed")
	assertions.False(trigger.fired, "a new claim must not require a post-insert lookup on the caller context")

	stored, err := st.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	requirements.NotNil(stored)
	assertions.Equal(inboxcontrol.StatusPrepared, stored.Status)
}
