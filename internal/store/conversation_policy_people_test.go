package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

func TestConversationPolicySweepEvidenceAndBriefInvalidation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newPersonSweepJournalFixture(t, true, false)
	requirements.NoError(f.store.SetConversationPolicyOverride(t.Context(), f.sourceID, "person-sweep-chat", conversationpolicy.Full))
	id := f.insertMessage(t, "policy-sweep", "chat", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Synthetic full room evidence")
	before := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	requirements.NotEmpty(before.Seeds)
	requirements.NoError(f.store.SetConversationPolicyOverride(t.Context(), f.sourceID, "person-sweep-chat", conversationpolicy.Cold))
	after := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	assertions.Empty(after.Seeds)
	candidates, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(), peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 100})
	requirements.NoError(err)
	assertions.Empty(candidates)
}

func TestConversationPolicyIdentitySuggestionsAndScoring(t *testing.T) {
	for _, provider := range []string{"beeper", "slack", "slackdump", "discord", "teams"} {
		t.Run(provider, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newPersonSweepJournalFixture(t, true, false)
			_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE sources SET source_type=? WHERE id=?`), provider, f.sourceID)
			requirements.NoError(err)
			message := f.insertMessage(t, "policy-identity", "chat", f.aliceID, time.Now().UTC())
			// This Full-name expectation requires the exact envelope value,
			// not merely the participant's presence in a Full room.
			_, err = f.store.DB().Exec(f.store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(?,?,'from','Alice')`), message, f.aliceID)
			requirements.NoError(err)
			requirements.NoError(f.store.SetConversationPolicyOverride(t.Context(), f.sourceID, "person-sweep-chat", conversationpolicy.Cold))
			count, err := f.store.CountIdentityDiscoveryMessagesContext(t.Context(), f.sourceID)
			requirements.NoError(err)
			assertions.Zero(count)
			_, err = f.store.RecordContactObservationContext(t.Context(), f.aliceID, store.ParticipantContactObservationInput{SourceID: &f.sourceID, ConversationID: &f.conversationID, AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
			requirements.NoError(err)
			packet, err := f.store.PersonMatchPairSummariesContext(t.Context(), f.aliceID, f.bobID)
			requirements.NoError(err)
			assertions.Empty(packet.Left.Email)
			assertions.Empty(packet.Left.DisplayName)
			page, err := f.store.ScanIdentityDiscoveryPageContext(t.Context(), f.sourceID, 0, 100)
			requirements.NoError(err)
			assertions.Empty(page.Observations)
			requirements.NoError(f.store.SetConversationPolicyOverride(t.Context(), f.sourceID, "person-sweep-chat", conversationpolicy.Full))
			count, err = f.store.CountIdentityDiscoveryMessagesContext(t.Context(), f.sourceID)
			requirements.NoError(err)
			assertions.Equal(int64(1), count)
			packet, err = f.store.PersonMatchPairSummariesContext(t.Context(), f.aliceID, f.bobID)
			requirements.NoError(err)
			assertions.Equal("alice@example.test", packet.Left.Email)
			assertions.Equal("Alice", packet.Left.DisplayName)
		})
	}
}

func TestConversationParticipantResolutionSupportsEmailOnlyInFullRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, _, _ := policyObservationFixture(t)
	_, err := s.DB().Exec(s.Rebind(`UPDATE sources SET source_type='slack' WHERE id=?`), source)
	requirements.NoError(err)
	peer, err := s.EnsureParticipant("peer@example.test", "Synthetic Peer", "example.test")
	requirements.NoError(err)
	resolution, err := s.ResolveConversationParticipantContext(t.Context(), full, store.ConversationParticipantIdentity{
		IdentifierType: "slack", IdentifierValue: "WORKSPACE:USER", DisplayName: "Synthetic Provider User",
		Email: "provider-user@example.test",
	})
	requirements.NoError(err)
	person, _, err := s.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), resolution.ParticipantID, nil)
	requirements.NoError(err)
	lease := personenrichment.WorkLease{PersonID: person.ID}
	input, err := s.LoadRequestInput(t.Context(), lease)
	requirements.NoError(err)
	requirements.Len(input.Emails, 1)
	assertions.Equal("provider-user@example.test", input.Emails[0].Value)
	packet, err := s.PersonMatchPairSummariesContext(t.Context(), resolution.ParticipantID, peer)
	requirements.NoError(err)
	assertions.Equal("provider-user@example.test", packet.Left.Email)

	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
	input, err = s.LoadRequestInput(t.Context(), lease)
	requirements.NoError(err)
	assertions.Empty(input.Emails, "support from another Full room is not borrowed")
	packet, err = s.PersonMatchPairSummariesContext(t.Context(), resolution.ParticipantID, peer)
	requirements.NoError(err)
	assertions.Empty(packet.Left.Email)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Cold))
	input, err = s.LoadRequestInput(t.Context(), lease)
	requirements.NoError(err)
	requirements.Len(input.Emails, 1)
	assertions.Equal("provider-user@example.test", input.Emails[0].Value)
}

func TestConversationPolicyBootstrapRetractsLegacyUnprovenance(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, _, pid := policyObservationFixture(t)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
	settings := conversationpolicy.DefaultSettings()
	settings.MaxParticipants["beeper"] = 0
	_, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now().UTC())
	requirements.NoError(err)
	// Simulate policy configured before people bootstrap was introduced.
	_, err = s.DB().Exec(`DELETE FROM archive_metadata WHERE key='conversation_policy_people_bootstrapped'`)
	requirements.NoError(err)
	legacy := policyObservationInput(source, nil)
	observation, err := s.RecordContactObservationContext(t.Context(), pid, legacy)
	requirements.NoError(err)
	person, _, err := s.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	before, err := s.LoadPersonSemanticDocumentContext(t.Context(), person.ID)
	requirements.NoError(err)
	refreshed, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now().UTC())
	requirements.NoError(err)
	assertions.Empty(refreshed.ChangedConversationIDs)
	after, err := s.LoadPersonSemanticDocumentContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(before.Text, after.Text)
	assertions.NotEqual(before.Revision, after.Revision)
	eligible, err := s.ObservationFullEligibleContext(t.Context(), observation.Observation.Envelope.ID)
	requirements.NoError(err)
	assertions.False(eligible)
	legacy.ConversationID = &full
	_, err = s.RecordContactObservationContext(t.Context(), pid, legacy)
	requirements.NoError(err)
	restored, err := s.LoadPersonSemanticDocumentContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(after.Text, restored.Text)
	assertions.NotEqual(after.Revision, restored.Revision)
	_, err = s.RecordContactObservationContext(t.Context(), pid, legacy)
	requirements.NoError(err)
	duplicate, err := s.LoadPersonSemanticDocumentContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(restored.Revision, duplicate.Revision)
	steady, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now().UTC())
	requirements.NoError(err)
	assertions.Empty(steady.ChangedConversationIDs)
	assertions.Equal(refreshed.Revision, steady.Revision, "unchanged settings use the steady-state path")
	stable, err := s.LoadPersonSemanticDocumentContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(duplicate.Revision, stable.Revision)
}

func TestConversationPolicyIdentityPacketKeepsValueSpecificSupport(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, weak := policyObservationFixture(t)
	rich, err := s.EnsureParticipant("shared@example.org", "Synthetic Directory Person", "example.org")
	requirements.NoError(err)
	peer, err := s.EnsureParticipant("peer@example.org", "Synthetic Peer", "example.org")
	requirements.NoError(err)
	// The real weak-to-rich merge preserves archival identifiers, but does not
	// give a newly introduced chat identifier the contact email's support.
	requirements.NoError(s.SetParticipantIdentifierContext(t.Context(), weak, "beeper", "beeper:19:observation-account:11:opaque:user"))
	requirements.NoError(s.MergeParticipantsContext(t.Context(), weak, rich))
	input := policyObservationInput(source, &cold)
	input.Envelope.SourceRef = new("beeper:observation-account:opaque:user")
	input.ProviderUserID = new("beeper:19:observation-account:11:opaque:user")
	_, err = s.RecordContactObservationContext(t.Context(), rich, input)
	requirements.NoError(err)
	independent := input
	independent.SourceID = nil
	independent.ConversationID = nil
	independent.ProviderUserID = nil
	independent.Envelope.Source = "user"
	_, err = s.RecordContactObservationContext(t.Context(), rich, independent)
	requirements.NoError(err)
	packet, err := s.PersonMatchPairSummariesContext(t.Context(), rich, peer)
	requirements.NoError(err)
	assertions.Equal("shared@example.org", packet.Left.Email)
	for _, identifier := range packet.Left.Identifiers {
		assertions.NotEqual("beeper:19:observation-account:11:opaque:user", identifier.Value)
	}
	input.ConversationID = &full
	_, err = s.RecordContactObservationContext(t.Context(), rich, input)
	requirements.NoError(err)
	packet, err = s.PersonMatchPairSummariesContext(t.Context(), rich, peer)
	requirements.NoError(err)
	found := false
	for _, identifier := range packet.Left.Identifiers {
		if identifier.Value == "beeper:19:observation-account:11:opaque:user" {
			found = true
		}
	}
	assertions.True(found)
}

func TestConversationPolicySweepDocumentEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f, person := newPersonSweepDocumentFixture(t)
	request := peoplesweep.DocumentContextRequest{PersonID: person, Query: "synthetic", Limit: 10}
	seeds, err := f.Store.SearchPersonSweepDocuments(t.Context(), request)
	requirements.NoError(err)
	requirements.Len(seeds, 1)
	var room string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT c.source_conversation_id FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE m.id=?`), seeds[0].Ref.MessageID).Scan(&room))
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, room, conversationpolicy.Cold))
	after, err := f.Store.SearchPersonSweepDocuments(t.Context(), request)
	requirements.NoError(err)
	assertions.Empty(after)
	alignment, err := (store.PersonSweepEvidenceAligner{Store: f.Store}).Align(t.Context(), sweepEvidenceInput(t, seeds[0]))
	requirements.NoError(err)
	assertions.False(alignment.Accepted)
}

func TestConversationPolicyIdentityPacketRequiresExactRecipientValue(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "old-full-value", "chat", f.bobID, time.Now().UTC())
	_, err := f.store.DB().Exec(f.store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'to',?)`), id, f.aliceID, "previous@example.test")
	require.NoError(t, err)
	packet, err := f.store.PersonMatchPairSummariesContext(t.Context(), f.aliceID, f.bobID)
	require.NoError(t, err)
	assert.Empty(t, packet.Left.Email, "Full evidence for another email must not support the current raw value")
}

func TestConversationPolicyIdentityPacketRequiresCanonicalNamespace(t *testing.T) {
	for _, mismatch := range []string{"provider", "service", "scope-kind", "scope-value", "source-account", "opaque-utf8"} {
		t.Run(mismatch, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, cold, weak := policyObservationFixture(t)
			account, user, anchor := "observation-account", "opaque:user", "beeper:19:observation-account:11:opaque:user"
			if mismatch == "opaque-utf8" {
				account, user, anchor = "accé:one", "usér:一", "beeper:9:accé:one:9:usér:一"
				src, err := s.GetOrCreateSource("beeper", account)
				requirements.NoError(err)
				source = src.ID
				full, err = s.EnsureConversation(source, "full", "Full")
				requirements.NoError(err)
				cold, err = s.EnsureConversation(source, "cold", "Cold")
				requirements.NoError(err)
				requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Full))
			}
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Cold))
			rich, err := s.EnsureParticipant("namespace@example.test", "Namespace Owner", "example.test")
			requirements.NoError(err)
			peer, err := s.EnsureParticipant("peer@example.test", "Peer", "example.test")
			requirements.NoError(err)
			requirements.NoError(s.SetParticipantIdentifierContext(t.Context(), weak, "beeper", anchor))
			service, err := s.ResolveCommunicationServiceContext(t.Context(), "slack")
			requirements.NoError(err)
			requirements.NoError(s.ClassifyParticipantIdentifierServiceContext(t.Context(), "beeper", anchor, &service.ID, new("account"), &account))
			requirements.NoError(s.MergeParticipantsContext(t.Context(), weak, rich))
			own := store.ParticipantContactObservationInput{SourceID: &source, ConversationID: &cold, AddressKind: store.ContactAddressEmail, OriginalValue: "namespace@example.test", ServiceSlug: new("slack"), ScopeKind: new("account"), ScopeValue: &account, ProviderUserID: new("provider:5:slack:7:account:4:test:6:native"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: new("beeper:" + account + ":" + user)}}
			_, err = s.RecordContactObservationContext(t.Context(), rich, own)
			requirements.NoError(err)
			other := own
			other.ConversationID = &full
			other.ProviderUserID = &anchor // Same opaque bytes are not sufficient support.
			other.OriginalValue = "different@example.test"
			switch mismatch {
			case "provider", "source-account", "opaque-utf8":
				provider := "beeper"
				if mismatch == "provider" {
					provider = "slack"
				}
				src, err := s.GetOrCreateSource(provider, "other-account")
				requirements.NoError(err)
				room, err := s.EnsureConversation(src.ID, "other-full", "Other Full")
				requirements.NoError(err)
				requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "other-full", conversationpolicy.Full))
				other.SourceID, other.ConversationID = &src.ID, &room
				other.Envelope.SourceRef = new(provider + ":other-account:" + user)
			case "service":
				other.ServiceSlug = new("irc")
			case "scope-kind":
				other.ScopeKind = new("workspace")
			case "scope-value":
				other.ScopeValue = new("other-account")
			}
			_, err = s.RecordContactObservationContext(t.Context(), rich, other)
			requirements.NoError(err)
			packet, err := s.PersonMatchPairSummariesContext(t.Context(), rich, peer)
			requirements.NoError(err)
			for _, id := range packet.Left.Identifiers {
				assertions.NotEqual(anchor, id.Value)
			}
			own.ConversationID = &full
			_, err = s.RecordContactObservationContext(t.Context(), rich, own)
			requirements.NoError(err)
			packet, err = s.PersonMatchPairSummariesContext(t.Context(), rich, peer)
			requirements.NoError(err)
			values := make([]string, 0, len(packet.Left.Identifiers))
			for _, id := range packet.Left.Identifiers {
				values = append(values, id.Value)
			}
			assertions.Contains(values, anchor, "its own Full edge supports the canonical anchor even when the native provider key differs")
		})
	}
}
