package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func inboxIMAPMappingReceipt(t *testing.T) (imapMembershipFixture, inboxcontrol.Receipt, inboxcontrol.State) {
	t.Helper()
	f, target := newInboxIMAPSyncFixture(t)
	before := inboxcontrol.State{Target: target, Read: new(false), Inbox: new(true), Flags: []string{}, Tags: []string{}, ObservedAt: time.Now().UTC()}
	destination := inboxcontrol.Folder{ID: "Archive", Name: "Archive", UIDValidity: 23}
	projected := before
	projected.Folders = []inboxcontrol.Folder{destination}
	projected.Location, projected.Inbox = "Archive", new(false)
	intent := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &target, Destination: &destination, DryRun: true}
	intentHash, err := inboxcontrol.IntentFingerprint(intent)
	require.NoError(t, err)
	stateHash, err := inboxcontrol.SemanticFingerprint(before)
	require.NoError(t, err)
	receipt := inboxcontrol.Receipt{ID: "native-move", PrincipalID: "native-principal", SourceID: target.SourceID, IdempotencyKey: "native-move", IntentHash: intentHash, StateHash: stateHash, Intent: intent, Before: before, Projected: &projected, Status: inboxcontrol.StatusPrepared, CreatedAt: time.Now().UTC()}
	mapped := target
	mapped.Mailbox, mapped.UIDValidity, mapped.UID = "Archive", 23, 9
	return f, receipt, inboxcontrol.State{Target: mapped, ObservedAt: time.Now().UTC()}
}

func TestInboxIMAPMappingIsAppendOnlyAndSurvivesUncertainFinish(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, receipt, mapping := inboxIMAPMappingReceipt(t)
	ledger, ok := any(f.store).(inboxcontrol.ReceiptMappingLedger)
	requirements.True(ok, "native Store must persist COPYUID before readback")
	_, claimed, err := f.store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.True(claimed)
	requirements.ErrorIs(ledger.RecordInboxReceiptMapping(t.Context(), receipt.ID, mapping), inboxcontrol.ErrConflict)
	requirements.NoError(f.store.MarkInboxDispatching(t.Context(), receipt.ID))
	requirements.NoError(ledger.RecordInboxReceiptMapping(t.Context(), receipt.ID, mapping))
	requirements.ErrorIs(ledger.RecordInboxReceiptMapping(t.Context(), receipt.ID, mapping), inboxcontrol.ErrConflict)
	changed := mapping
	changed.Target.UID++
	requirements.ErrorIs(f.store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusVerified, &changed, ""), inboxcontrol.ErrInvalid)
	requirements.NoError(f.store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusUnknown, nil, "readback-unavailable"))
	got, err := f.store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	requirements.NotNil(got.After)
	assertions.Equal(mapping.Target, got.After.Target)
	assertions.Equal(inboxcontrol.StatusUnknown, got.Status)
	requirements.ErrorIs(ledger.RecordInboxReceiptMapping(t.Context(), receipt.ID, changed), inboxcontrol.ErrConflict)
	_, claimed, err = f.store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	assertions.False(claimed, "interrupted mapped MOVE never receives another dispatch claim")
}

func TestInboxIMAPMappingRejectsUnsignedIdentity(t *testing.T) {
	for _, field := range []string{"source", "account", "item", "provider", "scope", "mailbox", "epoch", "uid", "observation"} {
		t.Run(field, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, receipt, mapping := inboxIMAPMappingReceipt(t)
			ledger, ok := any(f.store).(inboxcontrol.ReceiptMappingLedger)
			requirements.True(ok)
			_, _, err := f.store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			requirements.NoError(f.store.MarkInboxDispatching(t.Context(), receipt.ID))
			switch field {
			case "source":
				mapping.Target.SourceID++
			case "account":
				mapping.Target.AccountID = "other@example.test"
			case "item":
				mapping.Target.ItemID++
			case "provider":
				mapping.Target.ProviderID = "other-message"
			case "scope":
				mapping.Target.Scope = inboxcontrol.ScopeChat
			case "mailbox":
				mapping.Target.Mailbox = "Other"
			case "epoch":
				mapping.Target.UIDValidity++
			case "uid":
				mapping.Target.UID = 0
			case "observation":
				mapping.Read = new(true)
			}
			requirements.Error(ledger.RecordInboxReceiptMapping(t.Context(), receipt.ID, mapping))
			got, err := f.store.GetInboxReceipt(t.Context(), receipt.ID)
			requirements.NoError(err)
			assertions.Nil(got.After)
			assertions.Equal(inboxcontrol.StatusDispatching, got.Status)
		})
	}
}
