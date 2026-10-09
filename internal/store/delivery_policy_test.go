package store_test

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

func deliveryPerson(t *testing.T, f *storetest.Fixture) (*store.Person, []store.DeliveryTarget) {
	t.Helper()
	participant := f.EnsureParticipant("peer@example.test", "Example Person", "example.test")
	p, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	targets := []store.DeliveryTarget{}
	for _, address := range []string{"first@example.test", "second@example.test"} {
		cp, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: address, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		require.NoError(t, err)
		targets = append(targets, store.DeliveryTarget{SourceID: f.Source.ID, SourceType: "gmail", AccountID: f.Source.Identifier, Network: "email", Endpoint: address, ContactPointID: cp.Envelope.ID})
	}
	p, err = f.Store.GetPersonContext(t.Context(), p.ID)
	require.NoError(t, err)
	return p, targets
}
func readDelivery(t *testing.T, st *store.Store, uid string, target *store.DeliveryTarget) *store.DeliveryPolicyState {
	t.Helper()
	state, err := st.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: uid, Target: target})
	require.NoError(t, err)
	return state
}
func deliveryWrite(state *store.DeliveryPolicyState, policy store.DeliveryPolicy) store.DeliveryPolicyWrite {
	return store.DeliveryPolicyWrite{Query: store.DeliveryPolicyQuery{PersonUID: state.PersonUID, Target: state.Target}, ExpectedRevision: state.PolicyRevision, ExpectedPersonRevision: state.PersonRevision, BindingDigest: state.BindingDigest, Policy: policy, ScopeAcknowledgement: "person_all_routes", Actor: "owner", Reason: "Explicit synthetic approval"}
}
func TestDeliveryPolicyExactMethodsAccountsAndInheritance(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	initial := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	assertions.Equal(store.DeliveryDraftOnly, initial.EffectivePolicy)
	assertions.Nil(initial.StoredPolicy)
	assertions.Equal("system_default", initial.InheritanceSource)
	receipt, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(initial, store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, receipt.After.EffectivePolicy)
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &targets[1]).EffectivePolicy)
	other, err := f.Store.GetOrCreateSource("gmail", "other-account@example.test")
	requirements.NoError(err)
	another := targets[0]
	another.SourceID = other.ID
	another.AccountID = other.Identifier
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &another).EffectivePolicy)
	whatsapp := targets[0]
	whatsapp.Network = "whatsapp"
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &whatsapp).EffectivePolicy)
	defaults := readDelivery(t, f.Store, p.VCardUID, nil)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(defaults, store.DeliverySendAllowed))
	requirements.NoError(err)
	inherited := readDelivery(t, f.Store, p.VCardUID, &targets[1])
	assertions.Equal(store.DeliverySendAllowed, inherited.EffectivePolicy)
	assertions.Equal("person_default", inherited.InheritanceSource)
	denied, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(inherited, store.DeliveryDraftOnly))
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, denied.After.EffectivePolicy)
	cleared, err := f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(&denied.After, store.DeliveryDraftOnly))
	requirements.NoError(err)
	assertions.Nil(cleared.After.StoredPolicy)
	assertions.Equal(store.DeliverySendAllowed, cleared.After.EffectivePolicy)
	assertions.Greater(cleared.After.PolicyRevision, denied.After.PolicyRevision)
	assertions.Equal("owner", cleared.Audit.Actor)
	assertions.NotEmpty(cleared.Audit.Reason)
	assertions.False(cleared.Audit.CreatedAt.IsZero())
}
func TestDeliveryPolicyCASAndBrokenTargetRevocation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	initial := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(initial, store.DeliverySendAllowed))
	requirements.NoError(err)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(initial, store.DeliverySendAllowed))
	requirements.ErrorIs(err, store.ErrDeliveryPolicyRevisionConflict)
	requirements.NoError(f.Store.SupersedePersonContactPointContext(t.Context(), p.ID, targets[0].ContactPointID, nil))
	broken := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	assertions.Equal(store.DeliverySendAllowed, *broken.StoredPolicy)
	assertions.Equal(store.DeliveryDraftOnly, broken.EffectivePolicy)
	assertions.NotEmpty(broken.Reason)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed.After, store.DeliverySendAllowed))
	requirements.ErrorIs(err, store.ErrDeliveryTargetChanged)
	denied, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(broken, store.DeliveryDraftOnly))
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, *denied.After.StoredPolicy)
	_, err = f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(&denied.After, store.DeliveryDraftOnly))
	requirements.NoError(err)
}
func TestDeliveryPolicyDefaultScopeAndAwayBackDoNotReviveGrant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	defaults := readDelivery(t, f.Store, p.VCardUID, nil)
	input := deliveryWrite(defaults, store.DeliverySendAllowed)
	input.ScopeAcknowledgement = ""
	_, err := f.Store.SetDeliveryPolicyContext(t.Context(), input)
	requirements.ErrorIs(err, store.ErrDeliveryPolicyInvalid)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET identifier='changed@example.test' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), f.Source.Identifier, f.Source.ID)
	requirements.NoError(err)
	state := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.Equal("binding_changed", state.Reason)
}

func TestDeliveryPolicyDefaultApprovalRejectsAccountAddedSinceReview(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	reviewed := readDelivery(t, f.Store, p.VCardUID, nil)
	account, err := f.Store.GetOrCreateSource("gmail", "fresh-account@example.test")
	requirements.NoError(err)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(reviewed, store.DeliverySendAllowed))
	requirements.ErrorIs(err, store.ErrDeliveryTargetChanged)
	current := readDelivery(t, f.Store, p.VCardUID, nil)
	assertions.Equal(store.DeliveryDraftOnly, current.EffectivePolicy)
	assertions.NotEqual(reviewed.BindingDigest, current.BindingDigest)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(current, store.DeliverySendAllowed))
	requirements.NoError(err)
	target := targets[0]
	target.SourceID, target.AccountID = account.ID, account.Identifier
	assertions.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, &target).EffectivePolicy)
}
func FuzzDeliveryPolicyRejectsControlUID(f *testing.F) {
	f.Add("person-uid")
	f.Add("")
	f.Fuzz(func(t *testing.T, uid string) {
		err := store.ValidateDeliveryPolicyQuery(store.DeliveryPolicyQuery{PersonUID: uid + "\x00"})
		require.ErrorIs(t, err, store.ErrDeliveryPolicyInvalid)
	})
}

func TestDeliveryPolicyUnknownAmbiguousAndPersonMergeSplit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	_, err := f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: "unknown-synthetic-uid"})
	requirements.ErrorIs(err, store.ErrDeliveryPersonUnknown)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)
	otherParticipant := f.EnsureParticipant("other@example.test", "Other Example Person", "example.test")
	other, _, err := f.Store.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	_, err = f.Store.AddPersonContactPointContext(t.Context(), other.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: targets[0].Endpoint, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	state := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.Equal("ambiguous_endpoint", state.Reason)
	other, err = f.Store.GetPersonContext(t.Context(), other.ID)
	requirements.NoError(err)
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: p.ID, AbsorbedID: other.ID, ExpectedSurvivorRevision: p.Revision, ExpectedAbsorbedRevision: other.Revision, IdempotencyKey: "delivery-policy-merge", Actor: "owner"})
	requirements.NoError(err)
	_, err = f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: other.VCardUID})
	requirements.ErrorIs(err, store.ErrDeliveryPersonUnknown)
	state = readDelivery(t, f.Store, p.VCardUID, &targets[0])
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: p.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{otherParticipant}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "delivery-policy-split", Actor: "owner"})
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, split.NewPerson.VCardUID, nil).EffectivePolicy)
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &targets[0]).EffectivePolicy)
}

func TestDeliveryPolicyParticipantTargetRejectsSharedMailbox(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "shared-mailbox@example.test")
	deliveryEmailMessage(t, f, "shared-mailbox-source-evidence", target.ParticipantID)
	otherParticipant := f.EnsureParticipant("shared-mailbox-owner@example.test", "Other Example Person", "example.test")
	other, _, err := f.Store.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	_, err = f.Store.AddPersonContactPointContext(t.Context(), other.ID, store.PersonContactPointInput{
		AddressKind:   store.ContactAddressEmail,
		OriginalValue: target.Endpoint,
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)

	state := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.Equal("ambiguous_endpoint", state.Reason)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(state, store.DeliverySendAllowed))
	requirements.ErrorIs(err, store.ErrDeliveryTargetChanged)
}

func TestDeliveryPolicyMixedCaseParticipantHonorsEquivalentRestriction(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, participantTarget := deliveryParticipantTarget(t, f, "peer@EXAMPLE.TEST")
	deliveryEmailMessage(t, f, "mixed-case-restriction-source-evidence", participantTarget.ParticipantID)

	cp, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "peer@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	contactTarget := participantTarget
	contactTarget.Endpoint = "peer@example.test"
	contactTarget.ParticipantID = 0
	contactTarget.ContactPointID = cp.Envelope.ID

	defaults := readDelivery(t, f.Store, p.VCardUID, nil)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(defaults, store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, &participantTarget).EffectivePolicy)
	contactState := readDelivery(t, f.Store, p.VCardUID, &contactTarget)
	assertions.Equal(store.DeliverySendAllowed, contactState.EffectivePolicy)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(contactState, store.DeliveryDraftOnly))
	requirements.NoError(err)

	participantState := readDelivery(t, f.Store, p.VCardUID, &participantTarget)
	assertions.Equal(store.DeliveryDraftOnly, participantState.EffectivePolicy)
	assertions.Equal("equivalent_endpoint_restricted", participantState.Reason)
	calls := 0
	err = f.Store.WithDeliveryAdmissionContext(t.Context(),
		[]store.DeliveryRecipient{admissionRecipient(participantState, "to")},
		func(context.Context) error { calls++; return nil })
	var blocked *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &blocked)
	assertions.Equal("draft_required", blocked.Code)
	assertions.Zero(calls)
}

func TestDeliveryPolicyAmbiguityUsesNormalizedEmailRoute(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "peer@EXAMPLE.TEST")
	deliveryEmailMessage(t, f, "mixed-case-ambiguity-source-evidence", target.ParticipantID)

	otherParticipant := f.EnsureParticipant("other-owner@example.test", "Other Example Person", "example.test")
	other, _, err := f.Store.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	_, err = f.Store.AddPersonContactPointContext(t.Context(), other.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "peer@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)

	state := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.Equal("ambiguous_endpoint", state.Reason)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(state, store.DeliverySendAllowed))
	requirements.ErrorIs(err, store.ErrDeliveryTargetChanged)
}

func TestDeliveryPolicySourceReanchorAndEndpointReassignment(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO archive_metadata(key,value) VALUES (?,?)`), store.BeeperReanchorMarkerKey(f.Source.ID), "true")
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &targets[0]).EffectivePolicy)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM archive_metadata WHERE key=?`), store.BeeperReanchorMarkerKey(f.Source.ID))
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &targets[0]).EffectivePolicy)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)
	another, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alternate@example.test", "Alternate Example Person", "example.test"))
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE person_contact_points SET person_id=? WHERE id=?`), another.ID, targets[0].ContactPointID)
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, &targets[0]).EffectivePolicy)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE person_contact_points SET person_id=? WHERE id=?`), p.ID, targets[0].ContactPointID)
	requirements.NoError(err)
	state := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.NotEqual(allowed.After.BindingDigest, state.BindingDigest)
}

func TestDeliveryPolicyPreservesDraftsInBothModes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, _ := deliveryPerson(t, f)
	source, err := f.Store.GetOrCreateSource("slack", "synthetic-account")
	requirements.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "synthetic-conversation", "channel", "Example channel")
	requirements.NoError(err)
	existing, err := f.Store.CreateChatDraftContext(t.Context(), conversation, 0, "synthetic retained draft", allowChatDraft)
	requirements.NoError(err)
	for _, policy := range []store.DeliveryPolicy{store.DeliverySendAllowed, store.DeliveryDraftOnly} {
		_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, nil), policy))
		requirements.NoError(err)
		retained, err := f.Store.GetChatDraftContext(t.Context(), existing.DraftID)
		requirements.NoError(err)
		assertions.Equal(existing, retained)
		another, err := f.Store.CreateChatDraftContext(t.Context(), conversation, 0, "synthetic new draft", allowChatDraft)
		requirements.NoError(err)
		edited, err := f.Store.UpdateChatDraftContext(t.Context(), another.DraftID, another.Revision, "synthetic edited draft")
		requirements.NoError(err)
		assertions.Equal("synthetic edited draft", edited.Body)
	}
}

func TestDeliveryPolicyParticipantUsesNativeTextCanonicalEvidence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, _ := deliveryPerson(t, f)
	participant := p.ParticipantIDs[0]
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "synthetic-mail-evidence", "email_thread", "Example thread")
	requirements.NoError(err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: conversation, SourceMessageID: "synthetic-message-evidence", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE participants SET canonical_id=? WHERE id=?`), "legacy-mailbox@example.test", participant)
	requirements.NoError(err)
	target := store.DeliveryTarget{SourceID: f.Source.ID, SourceType: "gmail", AccountID: f.Source.Identifier, Network: "email", Endpoint: "peer@example.test", ParticipantID: participant}
	state, err := f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: p.VCardUID, Target: &target})
	requirements.NoError(err)
	assertions.NotEmpty(state.BindingDigest)
	receipt, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(state, store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, receipt.After.EffectivePolicy)
}

func deliveryParticipantTarget(t *testing.T, f *storetest.Fixture, email string) (*store.Person, store.DeliveryTarget) {
	t.Helper()
	participant := f.EnsureParticipant(email, "Synthetic Example Person", "example.test")
	p, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return p, store.DeliveryTarget{
		SourceID:      f.Source.ID,
		SourceType:    "gmail",
		AccountID:     f.Source.Identifier,
		Network:       "email",
		Endpoint:      email,
		ParticipantID: participant,
	}
}

func deliveryEmailMessage(t *testing.T, f *storetest.Fixture, id string, senderID int64) int64 {
	t.Helper()
	message := &store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: id,
		MessageType:     "email",
	}
	if senderID > 0 {
		message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(t, err)
	return messageID
}

func TestDeliveryPolicyNewParticipantSourceEvidenceDoesNotInheritPriorDefault(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "new-route@example.test")

	defaults := readDelivery(t, f.Store, p.VCardUID, nil)
	_, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(defaults, store.DeliverySendAllowed))
	requirements.NoError(err)
	beforeEvidence := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, beforeEvidence.EffectivePolicy)
	assertions.Equal("missing_source_evidence", beforeEvidence.Reason)

	messageID := deliveryEmailMessage(t, f, "new-recipient-evidence", 0)
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", []int64{target.ParticipantID}, []string{"Synthetic Example Person"}))

	state := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.Equal("binding_changed", state.Reason)
}

func TestDeliveryPolicyParticipantSourceEvidenceLossAndRestoreInvalidatesExactApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "restored-route@example.test")
	senderMessageID := deliveryEmailMessage(t, f, "sender-evidence", target.ParticipantID)
	recipientMessageID := deliveryEmailMessage(t, f, "recipient-evidence", 0)
	requirements.NoError(f.Store.ReplaceMessageRecipients(recipientMessageID, "to", []int64{target.ParticipantID}, []string{"Synthetic Example Person"}))
	approved, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, approved.After.EffectivePolicy)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), senderMessageID)
	requirements.NoError(err)
	whileRecipientEvidenceRemains := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliverySendAllowed, whileRecipientEvidenceRemains.EffectivePolicy)
	assertions.Equal("explicit_approval", whileRecipientEvidenceRemains.Reason)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), recipientMessageID)
	requirements.NoError(err)
	lost := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, lost.EffectivePolicy)
	assertions.Equal("missing_source_evidence", lost.Reason)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=NULL WHERE id=?`), senderMessageID)
	requirements.NoError(err)
	restored := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, restored.EffectivePolicy)
	assertions.Equal("binding_changed", restored.Reason)
}

func TestPostgreSQLConcurrentSourceEmailTombstonesAndRestoreInvalidateApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL participant/source evidence serialization")
	}
	p, target := deliveryParticipantTarget(t, f, "concurrent-evidence@example.test")
	senderMessageID := deliveryEmailMessage(t, f, "concurrent-evidence-sender", target.ParticipantID)
	recipientMessageID := deliveryEmailMessage(t, f, "concurrent-evidence-recipient", 0)
	requirements.NoError(f.Store.ReplaceMessageRecipients(recipientMessageID, "to",
		[]int64{target.ParticipantID}, []string{"Synthetic Example Person"}))
	approved, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, approved.After.EffectivePolicy)
	var initialVersion int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT version FROM delivery_source_email_evidence
		WHERE participant_id=? AND source_id=? AND evidence_present=TRUE`),
		target.ParticipantID, f.Source.ID).Scan(&initialVersion))

	first, err := f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	firstOpen := true
	var second *sql.Tx
	secondOpen := false
	secondStarted := false
	secondDone := make(chan error, 1)
	t.Cleanup(func() {
		if firstOpen {
			_ = first.Rollback()
		}
		if secondStarted {
			select {
			case <-secondDone:
			case <-time.After(5 * time.Second):
			}
		}
		if secondOpen {
			_ = second.Rollback()
		}
	})
	var firstPID int
	requirements.NoError(first.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&firstPID))
	_, err = first.ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), senderMessageID)
	requirements.NoError(err)

	second, err = f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	secondOpen = true
	var secondPID int
	requirements.NoError(second.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&secondPID))
	secondStarted = true
	go func() {
		_, updateErr := second.ExecContext(t.Context(), f.Store.Rebind(`
			UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), recipientMessageID)
		secondDone <- updateErr
	}()
	requirements.Eventually(func() bool {
		var waiting bool
		err := f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity activity
				WHERE activity.pid=? AND activity.wait_event_type='Lock'
				  AND ?=ANY(pg_blocking_pids(activity.pid))
				  AND EXISTS (
					SELECT 1 FROM pg_locks waiting_lock
					JOIN pg_locks blocking_lock
					  ON blocking_lock.locktype=waiting_lock.locktype
					 AND blocking_lock.classid=waiting_lock.classid
					 AND blocking_lock.objid=waiting_lock.objid
					 AND blocking_lock.objsubid=waiting_lock.objsubid
					 AND blocking_lock.pid=?
					WHERE waiting_lock.pid=activity.pid
					  AND waiting_lock.locktype='advisory'
					  AND NOT waiting_lock.granted
					  AND blocking_lock.granted
				  )
			)`), secondPID, firstPID, firstPID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond,
		"the second tombstone must wait for the participant/source evidence lock")

	requirements.NoError(first.Commit())
	firstOpen = false
	requirements.NoError(<-secondDone)
	secondStarted = false
	requirements.NoError(second.Commit())
	secondOpen = false

	var present bool
	var lostVersion int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT evidence_present,version FROM delivery_source_email_evidence
		WHERE participant_id=? AND source_id=?`),
		target.ParticipantID, f.Source.ID).Scan(&present, &lostVersion))
	assertions.False(present)
	assertions.Equal(initialVersion+1, lostVersion)
	lost := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, lost.EffectivePolicy)
	assertions.Equal("missing_source_evidence", lost.Reason)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE messages SET deleted_from_source_at=NULL WHERE id=?`), senderMessageID)
	requirements.NoError(err)
	var restoredPresent bool
	var restoredVersion int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT evidence_present,version FROM delivery_source_email_evidence
		WHERE participant_id=? AND source_id=?`),
		target.ParticipantID, f.Source.ID).Scan(&restoredPresent, &restoredVersion))
	assertions.True(restoredPresent)
	assertions.Equal(lostVersion+1, restoredVersion)
	restored := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, restored.EffectivePolicy)
	assertions.Equal("binding_changed", restored.Reason)
	assertions.NotEqual(approved.After.BindingDigest, restored.BindingDigest)

	calls := 0
	err = f.Store.WithDeliveryAdmissionContext(t.Context(),
		[]store.DeliveryRecipient{admissionRecipient(&approved.After, "to")},
		func(context.Context) error { calls++; return nil })
	var refusal *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &refusal)
	assertions.Zero(calls)
}

func TestDeliveryPolicyIdenticalRecipientRefreshPreservesApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "reprocessed-route@example.test")
	messageID := deliveryEmailMessage(t, f, "reprocessed-recipient-evidence", 0)
	recipients := []int64{target.ParticipantID}
	displayNames := []string{"Synthetic Example Person"}
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", recipients, displayNames))

	approved, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, approved.After.EffectivePolicy)

	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", recipients, displayNames))
	refreshed := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliverySendAllowed, refreshed.EffectivePolicy)
	assertions.Equal("explicit_approval", refreshed.Reason)

	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", nil, nil))
	lost := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, lost.EffectivePolicy)
	assertions.Equal("missing_source_evidence", lost.Reason)

	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", recipients, displayNames))
	restored := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, restored.EffectivePolicy)
	assertions.Equal("binding_changed", restored.Reason)
}

func TestDeliveryPolicyRecipientTypeChangeKeepsExactApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "recipient-type-change@example.test")
	messageID := deliveryEmailMessage(t, f, "recipient-type-change", 0)
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to",
		[]int64{target.ParticipantID}, []string{"Synthetic Example Person"}))
	approved, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
	requirements.NoError(err)

	_, err = f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			ID: messageID, ConversationID: f.ConvID, SourceID: f.Source.ID,
			SourceMessageID: "recipient-type-change", MessageType: "email",
		},
		Recipients: []store.RecipientSet{
			{Type: "to"},
			{Type: "cc", ParticipantIDs: []int64{target.ParticipantID}, DisplayNames: []string{"Synthetic Example Person"}},
		},
	})
	requirements.NoError(err)

	after := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliverySendAllowed, after.EffectivePolicy)
	assertions.Equal("explicit_approval", after.Reason)
	assertions.Equal(approved.After.BindingDigest, after.BindingDigest)
}

func TestDeliveryPolicyIdentifierDisplayRefreshPreservesApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "identifier-refresh@example.test")
	deliveryEmailMessage(t, f, "identifier-refresh-evidence", target.ParticipantID)
	requirements.NoError(f.Store.SetParticipantIdentifier(target.ParticipantID, "beeper", "synthetic-beeper-id"))

	approved, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, approved.After.EffectivePolicy)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE participant_identifiers
		SET display_value = 'Synthetic Beeper Contact'
		WHERE participant_id = ? AND identifier_type = 'beeper'
	`), target.ParticipantID)
	requirements.NoError(err)

	refreshed := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliverySendAllowed, refreshed.EffectivePolicy)
	assertions.Equal(approved.After.BindingDigest, refreshed.BindingDigest)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE participant_identifiers
		SET identifier_value = 'replacement-beeper-id'
		WHERE participant_id = ? AND identifier_type = 'beeper'
	`), target.ParticipantID)
	requirements.NoError(err)
	changedBinding := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliveryDraftOnly, changedBinding.EffectivePolicy)
	assertions.Equal("binding_changed", changedBinding.Reason)
}

func TestDeliveryPolicyOrdinaryMailPreservesParticipantSourceApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, target := deliveryParticipantTarget(t, f, "steady-route@example.test")
	deliveryEmailMessage(t, f, "first-sender-evidence", target.ParticipantID)
	approved, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
	requirements.NoError(err)
	assertions.Equal(store.DeliverySendAllowed, approved.After.EffectivePolicy)

	deliveryEmailMessage(t, f, "ordinary-second-message", target.ParticipantID)
	state := readDelivery(t, f.Store, p.VCardUID, &target)
	assertions.Equal(store.DeliverySendAllowed, state.EffectivePolicy)
	assertions.Equal("explicit_approval", state.Reason)
}
