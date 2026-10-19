package store_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func policyObservationFixture(t *testing.T) (*store.Store, int64, int64, int64, int64) {
	t.Helper()
	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	source, err := s.GetOrCreateSource("beeper", "observation-account")
	require.NoError(t, err)
	full, err := s.EnsureConversation(source.ID, "full", "Full")
	require.NoError(t, err)
	cold, err := s.EnsureConversation(source.ID, "cold", "Cold")
	require.NoError(t, err)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source.ID, "full", conversationpolicy.Full))
	pid, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-user", "Synthetic User")
	require.NoError(t, err)
	return s, source.ID, full, cold, pid
}

func policyObservationInput(source int64, room *int64) store.ParticipantContactObservationInput {
	return store.ParticipantContactObservationInput{SourceID: &source, ConversationID: room, AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.org", ProviderUserID: new("provider:synthetic-user"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
}

func TestNativePhoneAndServiceIdentityRemainEligibleAfterColdChatRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, source, full, _, _ := policyObservationFixture(t)
	ctx := t.Context()
	const phone = "+15550123456"

	nativeParticipant, err := s.EnsureParticipantByPhone(phone, "Synthetic Native Contact", "whatsapp")
	requirements.NoError(err)
	nativeSource, err := s.GetOrCreateSource("whatsapp", "synthetic-native-archive")
	requirements.NoError(err)
	_, err = s.UpsertMessageContext(ctx, &store.Message{
		SourceID: nativeSource.ID, SourceMessageID: "native-phone-support", MessageType: "whatsapp",
		SenderID: sql.NullInt64{Int64: nativeParticipant, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, full, nativeParticipant, "member"))

	coldOnlyParticipant, err := s.EnsureParticipantByPhone("+15550123457", "Synthetic Cold Only Contact", "whatsapp")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, full, coldOnlyParticipant, "member"))

	phoneWhere := store.FullParticipantValueWhere("p.id", "'phone'", "p.phone_number")
	phoneCount := func(participantID int64) int64 {
		var count int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participants p WHERE p.id=? AND `+phoneWhere), participantID).Scan(&count)
		requirements.NoError(err)
		return count
	}
	identifierWhere := store.FullParticipantIdentifierWhereSQLite("pi")
	if s.IsPostgreSQL() {
		identifierWhere = store.FullParticipantIdentifierWherePostgres("pi")
	}
	identifierCount := func(participantID int64, value string) int64 {
		var count int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participant_identifiers pi
			WHERE pi.participant_id=? AND pi.identifier_type='whatsapp' AND pi.identifier_value=? AND `+identifierWhere), participantID, value).Scan(&count)
		requirements.NoError(err)
		return count
	}

	assertions.Equal(int64(1), phoneCount(nativeParticipant), "native history supports this exact phone")
	assertions.Equal(int64(1), identifierCount(nativeParticipant, phone), "native history supports this exact service identifier")
	assertions.Zero(phoneCount(coldOnlyParticipant), "room membership alone does not support a phone")
	assertions.Zero(identifierCount(coldOnlyParticipant, "+15550123457"), "room membership alone does not support a provider identifier")

	requirements.NoError(s.SetConversationPolicyOverride(ctx, source, "full", conversationpolicy.Cold))
	assertions.Equal(int64(1), phoneCount(nativeParticipant), "independent native history survives chat demotion")
	assertions.Equal(int64(1), identifierCount(nativeParticipant, phone), "independent native identity survives chat demotion")
	assertions.Zero(phoneCount(coldOnlyParticipant), "cold-only phone data stays hidden")
	assertions.Zero(identifierCount(coldOnlyParticipant, "+15550123457"), "cold-only provider identifier stays hidden")
}

func policyObservationSupport(t *testing.T, s *store.Store, id int64) []int64 {
	t.Helper()
	rows, err := s.DB().Query(s.Rebind(`SELECT conversation_id FROM participant_contact_observation_conversations WHERE observation_id=? ORDER BY conversation_id`), id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestConversationPolicyObservationMixedSupport(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	input := policyObservationInput(source, &cold)
	first, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Empty(found)
	peer, err := s.EnsureParticipantByIdentifier("test", "independent-peer", "")
	requirements.NoError(err)
	independent := policyObservationInput(source, nil)
	independent.SourceID = nil
	independent.ProviderUserID = new("another-provider")
	_, err = s.RecordContactObservationContext(t.Context(), peer, independent)
	requirements.NoError(err)
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assertions.Empty(candidates)
	input.ConversationID = &full
	second, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	assertions.Equal(first.Observation.Envelope.ID, second.Observation.Envelope.ID)
	found, err = s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Len(found, 1)
	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assertions.Len(candidates, 1)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assertions.Empty(candidates)
	raw, err := s.ListParticipantObservationsContext(t.Context(), pid, true)
	requirements.NoError(err)
	assertions.Len(raw, 1)
}

func TestConversationPolicyObservationDedupKeepsEveryRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	input := policyObservationInput(source, &cold)
	first, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	before, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	input.ConversationID = &full
	second, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	assertions.False(second.Created)
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, first.Observation.Envelope.ID))
	after, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
	_, err = s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	repeated, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(after, repeated)
	other, err := s.GetOrCreateSource("beeper", "other-account")
	requirements.NoError(err)
	wrong, err := s.EnsureConversation(other.ID, "wrong", "Wrong")
	requirements.NoError(err)
	input.ConversationID = &wrong
	_, err = s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.Error(err)
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, first.Observation.Envelope.ID))
}

func TestConversationPolicyLegacyObservationCannotBorrowFullSourceRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, _, _, pid := policyObservationFixture(t)
	input := policyObservationInput(source, nil)
	_, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Empty(found)
	input.SourceID = nil
	_, err = s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	found, err = s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Len(found, 1)
}

func TestConversationPolicyDemotionPreservesManualIdentityDecision(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "accepted"}[accept], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, _, pid := policyObservationFixture(t)
			peer, err := s.EnsureParticipantByIdentifier("test", "manual-peer", "")
			requirements.NoError(err)
			input := policyObservationInput(source, &full)
			_, err = s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchStableProviderID, NormalizedValue: input.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source})
			requirements.NoError(err)
			if accept {
				_, _, err = s.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "user", nil)
			} else {
				_, err = s.DecideIdentityMatchCandidateContext(t.Context(), candidate.ID, store.IdentityMatchStateRejected, "user", nil)
			}
			requirements.NoError(err)
			person, _, err := s.CreatePersonFromParticipant(pid)
			requirements.NoError(err)
			contactBefore, err := s.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "curated@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			requirements.NoError(err)
			before, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
			after, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			assertions.Equal(before, after)
			contacts, err := s.ListPersonContactPointsContext(t.Context(), person.ID, true)
			requirements.NoError(err)
			requirements.Len(contacts, 1)
			assertions.Equal(*contactBefore, contacts[0])
		})
	}
}

func TestConversationPolicyObservationMergeSubsetRoundTrip(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	// Subset is a SQLite export API; exercise its real file-backed path.
	path := filepath.Join(t.TempDir(), "source.db")
	s, err := store.Open(path)
	requirements.NoError(err)
	defer func() { require.NoError(t, s.Close()) }()
	requirements.NoError(s.InitSchema())
	source, err := s.GetOrCreateSource("beeper", "merge-account")
	requirements.NoError(err)
	full, err := s.EnsureConversation(source.ID, "full", "Full")
	requirements.NoError(err)
	cold, err := s.EnsureConversation(source.ID, "cold", "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "full", conversationpolicy.Full))
	left, err := s.EnsureParticipantByIdentifier("test", "merge-left", "")
	requirements.NoError(err)
	right, err := s.EnsureParticipantByIdentifier("test", "merge-right", "")
	requirements.NoError(err)
	survivor, err := s.RecordContactObservationContext(t.Context(), left, policyObservationInput(source.ID, &cold))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), right, policyObservationInput(source.ID, &full))
	requirements.NoError(err)
	requirements.NoError(s.MergeParticipants(right, left))
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, survivor.Observation.Envelope.ID))
	for _, room := range []int64{full, cold} {
		_, err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: map[int64]string{full: "full-message", cold: "cold-message"}[room], MessageType: "beeper", SenderID: sql.NullInt64{Int64: left, Valid: true}})
		requirements.NoError(err)
	}
	dst := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubsetWithOptions(path, dst, 10, store.CopySubsetOptions{IncludeProfiles: true})
	requirements.NoError(err)
	copied, err := store.Open(filepath.Join(dst, "msgvault.db"))
	requirements.NoError(err)
	defer func() { require.NoError(t, copied.Close()) }()
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, copied, survivor.Observation.Envelope.ID))
	requirements.NoError(copied.RemoveSource(source.ID))
	assertions.Empty(policyObservationSupport(t, copied, survivor.Observation.Envelope.ID))
}

func TestConversationPolicyObservationSourceMerge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	// Existing source maintenance reserves sync-lease connections separately.
	s.DB().SetMaxOpenConns(4)
	other, err := s.GetOrCreateSource("beeper", "merge-destination")
	requirements.NoError(err)
	destination, err := s.EnsureConversation(other.ID, "destination", "Destination")
	requirements.NoError(err)
	kept, err := s.RecordContactObservationContext(t.Context(), pid, policyObservationInput(other.ID, &destination))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), pid, policyObservationInput(source, &full))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), pid, policyObservationInput(source, &cold))
	requirements.NoError(err)
	_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source, IntoSourceID: other.ID})
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{full, cold, destination}, policyObservationSupport(t, s, kept.Observation.Envelope.ID))
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), "provider:synthetic-user", 0)
	requirements.NoError(err)
	assertions.Len(found, 1)
}

func TestConversationPolicyObservationScoringRequiresBothFullSides(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-%t", legacy), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, cold, pid := policyObservationFixture(t)
			other, err := s.GetOrCreateSource("gmail", "independent@example.org")
			requirements.NoError(err)
			peer, err := s.EnsureParticipantByIdentifier("test", "score-peer", "")
			requirements.NoError(err)
			first := policyObservationInput(source, &cold)
			second := policyObservationInput(other.ID, nil)
			_, err = s.RecordContactObservationContext(t.Context(), pid, first)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, second)
			requirements.NoError(err)
			count, err := s.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
			requirements.NoError(err)
			assertions.Zero(count)
			first.ConversationID = &full
			_, err = s.RecordContactObservationContext(t.Context(), pid, first)
			requirements.NoError(err)
			count, err = s.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
			requirements.NoError(err)
			assertions.Equal(1, count)
			candidatesBefore, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
			requirements.NoError(err)
			requirements.Len(candidatesBefore, 1)
			// A source attachment without independent producer evidence corroborates
			// this pair; it cannot turn one surviving endpoint into an independent claim.
			contacts, err := s.GetOrCreateSource("muesli", "untyped-corroboration")
			requirements.NoError(err)
			requirements.NoError(s.AttachIdentityMatchCandidateSourceContext(t.Context(), candidatesBefore[0].ID, contacts.ID))
			if legacy {
				_, err = s.DB().Exec(s.Rebind(`UPDATE identity_match_candidates SET source_ref=NULL WHERE id=?`), candidatesBefore[0].ID)
				requirements.NoError(err)
				_, err = s.DB().Exec(s.Rebind(`UPDATE identity_match_evidence SET evidence_ref=NULL WHERE candidate_id=?`), candidatesBefore[0].ID)
				requirements.NoError(err)
			}

			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
			candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
			requirements.NoError(err)
			assertions.Empty(candidates)
			independent, err := s.ListParticipantObservationsContext(t.Context(), peer, true)
			requirements.NoError(err)
			assertions.Len(independent, 1)
		})
	}
}

func TestConversationPolicyObservationStaleMatcherCannotRecreateCandidate(t *testing.T) {
	requirements := require.New(t)

	s, source, full, _, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipantByIdentifier("test", "stale-peer", "")
	requirements.NoError(err)
	input := policyObservationInput(source, &full)
	observation, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	candidateInput := store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchStableProviderID, NormalizedValue: input.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source, ObservationID: observation.Observation.Envelope.ID}
	_, _, err = s.UpsertIdentityMatchCandidateContext(t.Context(), candidateInput)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	_, _, err = s.UpsertIdentityMatchCandidateContext(t.Context(), candidateInput)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assert.Empty(t, candidates)
}

func TestConversationPolicyObservationStaleSystemAcceptance(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "accept", true: "resume"}[resume], func(t *testing.T) {
			requirements := require.New(t)

			s, source, _, cold, pid := policyObservationFixture(t)
			s.DB().SetMaxOpenConns(1)
			peer, err := s.EnsureParticipantByIdentifier("test", "late-peer", "")
			requirements.NoError(err)
			input := policyObservationInput(source, &cold)
			_, err = s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, input)
			requirements.NoError(err)
			// A candidate read before demotion, or an old pending system decision,
			// cannot use cold support when its actual publication transaction begins.
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchStableProviderID, NormalizedValue: input.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source})
			requirements.NoError(err)
			if resume {
				_, err = s.DB().Exec(s.Rebind(`UPDATE identity_match_candidates SET state='accepted',decided_by='system',application_pending=TRUE WHERE id=?`), candidate.ID)
				requirements.NoError(err)
				_, _, _, err = s.ResumeAcceptedIdentityMatchCandidateContext(t.Context(), candidate.ID)
			} else {
				_, _, err = s.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
			}
			requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			members, err := s.ClusterMembers(pid)
			requirements.NoError(err)
			assert.NotContains(t, members, peer)
		})
	}
}

func TestConversationPolicyLegacyDerivedObservationRequiresRoomSupport(t *testing.T) {
	for _, provenance := range []store.Provenance{store.ProvenanceExtraction, store.ProvenanceEnrichment, store.ProvenanceSystem} {
		t.Run(string(provenance), func(t *testing.T) {
			s, source, _, _, pid := policyObservationFixture(t)
			input := policyObservationInput(source, nil)
			input.Envelope.Source = provenance
			_, err := s.RecordContactObservationContext(t.Context(), pid, input)
			require.NoError(t, err)
			found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
			require.NoError(t, err)
			assert.Empty(t, found)
		})
	}
}

// seedFullObservationTestSupport gives legacy matching fixtures explicit full
// conversation provenance. Production never infers these support edges.
func seedFullObservationTestSupport(t *testing.T, s *store.Store) {
	t.Helper()
	rows, err := s.DB().Query(`SELECT id,source_id FROM participant_contact_observations WHERE source_id IS NOT NULL`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	type support struct{ observation, source int64 }
	var pending []support
	for rows.Next() {
		var item support
		require.NoError(t, rows.Scan(&item.observation, &item.source))
		pending = append(pending, item)
	}
	require.NoError(t, rows.Err())
	rooms := map[int64]int64{}
	for _, item := range pending {
		room, ok := rooms[item.source]
		if !ok {
			room = fullObservationTestRoom(t, s, item.source)
			rooms[item.source] = room
		}
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO participant_contact_observation_conversations(observation_id,conversation_id) VALUES(?,?) ON CONFLICT(observation_id,conversation_id) DO NOTHING`), item.observation, room)
		require.NoError(t, err)
	}
}

func fullObservationTestRoom(t *testing.T, s *store.Store, source int64) int64 {
	t.Helper()
	room, err := s.EnsureConversation(source, "identity-test-room", "Identity fixture")
	require.NoError(t, err)
	var kind string
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT source_type FROM sources WHERE id=?`), source).Scan(&kind))
	if conversationpolicy.Supported(kind) {
		require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source, "identity-test-room", conversationpolicy.Full))
	}
	return room
}

func TestConversationPolicyObservationCrossScopeRequiresEligiblePeerScope(t *testing.T) {
	requirements := require.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipantByIdentifier("test", "scope-peer", "")
	requirements.NoError(err)
	input := store.ParticipantContactObservationInput{SourceID: &source, ConversationID: &full, AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"), ScopeKind: new("workspace"), ScopeValue: new("full-scope"), OriginalValue: "scope-user", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
	origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	input.ConversationID = &cold
	input.ScopeValue = new("cold-scope")
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	_, _, err = s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{ObservationID: origin.Observation.Envelope.ID, LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchServiceScopeUsername, ServiceSlug: input.ServiceSlug, CrossScope: true, NormalizedValue: new("scope-user"), State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source})
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
}

func TestConversationPolicyObservationLateEvidenceCannotSurviveDemotion(t *testing.T) {
	requirements := require.New(t)

	s, source, full, _, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipantByIdentifier("test", "late-evidence-peer", "")
	requirements.NoError(err)
	input := policyObservationInput(source, &full)
	origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
	requirements.NoError(err)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	gathered, err := s.ListFullParticipantObservationsContext(t.Context(), pid)
	requirements.NoError(err)
	requirements.Len(gathered, 1)
	evidence := store.IdentityMatchEvidenceInput{EvidenceKind: "email", Detail: new(gathered[0].NormalizedValue), Source: store.ProvenanceArchiveObservation, SourceID: &source, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin.Observation.Envelope.ID}}
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	kept, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	assert.Empty(t, kept.Evidence)
	// Even a fresh revision cannot substitute for the origin's live support.
	evidence.Admission.PolicyRevision, err = s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "manual", Detail: new("user-reviewed"), Source: store.ProvenanceUser})
	requirements.NoError(err)
}

func TestConversationPolicyNameEvidenceRequiresExactFullNames(t *testing.T) {
	for _, tc := range []struct {
		name                string
		leftName, rightName string
		rightRaw            string
		want                bool
	}{
		{name: "blank", rightRaw: "Synthetic User"},
		{name: "different", leftName: "Different User", rightName: "Different User", rightRaw: "Synthetic User"},
		{name: "one-endpoint", leftName: "Synthetic User", rightName: "Different User", rightRaw: "Synthetic User"},
		{name: "exact", leftName: "Synthetic User", rightName: "Synthetic User", rightRaw: "Synthetic User", want: true},
		{name: "case-folded-pair", leftName: "Synthetic User", rightName: "SYNTHETIC USER", rightRaw: "SYNTHETIC USER", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)

			s, source, full, cold, pid := policyObservationFixture(t)
			peer, err := s.EnsureParticipantByIdentifier("beeper", "name-peer", tc.rightRaw)
			requirements.NoError(err)
			policyNameEnvelope(t, s, source, cold, "cold-name", []int64{pid, peer}, []string{"Synthetic User", "Synthetic User"})
			policyNameEnvelope(t, s, source, full, "full-name", []int64{pid, peer}, []string{tc.leftName, tc.rightName})
			input := policyObservationInput(source, &full)
			origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, input)
			requirements.NoError(err)
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
			requirements.NoError(err)
			revision, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			evidence := store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Synthetic User"), Source: store.ProvenanceArchiveObservation, SourceID: &source, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin.Observation.Envelope.ID}}
			_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
			if tc.want {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			}
			evidence.EvidenceKind, evidence.Detail = "email", new("shared@example.org")
			_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
			requirements.NoError(err, "independent Full address evidence must survive")
			evidence.EvidenceKind, evidence.Detail, evidence.Source, evidence.Admission = "display_name", new("Synthetic User"), store.ProvenanceUser, nil
			_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
			requirements.NoError(err, "manual name evidence remains independent")
		})
	}
}

func TestConversationPolicyObservationDemotionRetractsGeneratedExplanations(t *testing.T) {
	for _, support := range []string{"none", "blank-name", "different-name", "exact-name"} {
		t.Run("remaining-full-"+support, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, alternate, pid := policyObservationFixture(t)
			peer, err := s.EnsureParticipantByIdentifier("test", "explanation-peer", "Synthetic User")
			requirements.NoError(err)
			policyNameEnvelope(t, s, source, full, "original-name", []int64{pid, peer}, []string{"Synthetic User", "Synthetic User"})
			input := policyObservationInput(source, &full)
			origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, input)
			requirements.NoError(err)
			if support != "none" {
				requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
				name := map[string]string{"blank-name": "", "different-name": "Different User", "exact-name": "Synthetic User"}[support]
				policyNameEnvelope(t, s, source, alternate, "alternate-name", []int64{pid, peer}, []string{name, name})
				input.ConversationID = &alternate
				_, err = s.RecordContactObservationContext(t.Context(), pid, input)
				requirements.NoError(err)
				_, err = s.RecordContactObservationContext(t.Context(), peer, input)
				requirements.NoError(err)
			}
			requirements.NoError(s.EnsureConversationParticipant(full, pid, "member"))
			requirements.NoError(s.EnsureConversationParticipant(full, peer, "member"))
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
			requirements.NoError(err)
			revision, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			for _, item := range []struct{ kind, detail, ref string }{{"email", "shared@example.org", ""}, {"display_name", "Synthetic User", ""}, {"conversation_membership", "Shared conversation", fmt.Sprintf("conversation:%d", full)}} {
				evidence := store.IdentityMatchEvidenceInput{EvidenceKind: item.kind, Detail: &item.detail, Source: store.ProvenanceArchiveObservation, SourceID: &source, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin.Observation.Envelope.ID}}
				if item.ref != "" {
					evidence.EvidenceRef = &item.ref
				}
				_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
				requirements.NoError(err)
			}
			manual, err := s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Synthetic User"), Source: store.ProvenanceUser})
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
			kept, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			automatic := map[string]bool{}
			manualKept := false
			for _, evidence := range kept.Evidence {
				if evidence.ID == manual.ID {
					manualKept = true
				} else {
					automatic[evidence.EvidenceKind] = true
				}
			}
			wantEvidence := 1 // The independent manual name evidence.
			if support != "none" {
				wantEvidence++ // The independently supported Full email evidence.
			}
			if support == "exact-name" {
				wantEvidence++
			}
			assertions.Len(kept.Evidence, wantEvidence)
			assertions.True(manualKept)
			assertions.Equal(support != "none", automatic["email"])
			assertions.Equal(support == "exact-name", automatic["display_name"])
			assertions.False(automatic["conversation_membership"])
			assertions.Equal(store.IdentityMatchStateCandidate, kept.State, "manual candidate decision survives")
		})
	}
}

func TestConversationPolicyPromotionGeneratesColdScopedAddressConflict(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, _, cold, left := policyObservationFixture(t)
	right, err := s.EnsureParticipantByIdentifier("test", "promoted-conflict-peer", "")
	requirements.NoError(err)
	input := store.ParticipantContactObservationInput{SourceID: &source, ConversationID: &cold,
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
		ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"), OriginalValue: "shared-user",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
	for _, id := range []int64{left, right} {
		_, err = s.RecordContactObservationContext(t.Context(), id, input)
		requirements.NoError(err)
	}
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	requirements.Empty(candidates)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
	for _, id := range []int64{left, right} {
		_, err = s.RecordContactObservationContext(t.Context(), id, input)
		requirements.NoError(err)
	}
	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.Equal(store.IdentityMatchStateConflict, candidates[0].State)
	assertions.Equal(store.IdentityMatchServiceScopeUsername, candidates[0].Basis)
	before, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	identityBefore, err := s.IdentityRevisionContext(t.Context())
	requirements.NoError(err)
	for _, id := range []int64{left, right} {
		repeated, err := s.RecordContactObservationContext(t.Context(), id, input)
		requirements.NoError(err)
		assertions.False(repeated.Created)
	}
	repeated, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	assertions.Equal(candidates, repeated)
	after, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
	identityAfter, err := s.IdentityRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(identityBefore, identityAfter)
}
