package beeper

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// servedPolicyRoom serves one Beeper group so ensureConversation can verify
// its roster. Synthetic members pad the room to size.
func servedPolicyRoom(t *testing.T, accountID, chatID string, size int, members ...map[string]any) (*Importer, *store.Store, int64, *Chat) {
	t.Helper()
	for i := len(members); i < size; i++ {
		members = append(members, map[string]any{"id": fmt.Sprintf("@member%d:beeper.local", i), "fullName": fmt.Sprintf("Member %d", i)})
	}
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{ID: chatID, AccountID: accountID, Network: "Signal", Title: "Room", Type: "group", Participants: members})
	imp, st, closeServer := newTestImporter(t, f)
	t.Cleanup(closeServer)
	sourceID := newBeeperTestSource(t, st, accountID)
	chat, err := imp.client.GetChat(t.Context(), chatID)
	require.NoError(t, err)
	return imp, st, sourceID, chat
}

func TestBeeperColdRoomResolvesRichIdentityOnceFull(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	imp, s, sourceID, chat := servedPolicyRoom(t, "account", "cold", 1,
		map[string]any{"id": "synthetic-user", "phoneNumber": "+12025550123", "fullName": "Cold User"})
	existing, err := s.EnsureParticipantByPhone(t.Context(), "+12025550123", "Independent Contact", "sms")
	requirements.NoError(err)
	_, err = s.EnsureConversation(t.Context(), sourceID, chat.ID, "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), sourceID, chat.ID, conversationpolicy.Cold))
	opts := ImportOptions{AccountID: "account", rosterState: &rosterWork{}}
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	requirements.True(complete)
	pid, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "synthetic-user"))
	requirements.NoError(err)
	assertions.NotEqual(existing, pid)
	assertions.Empty(imp.res.rich)
	raw, err := s.ListParticipantObservationsContext(t.Context(), pid, true)
	requirements.NoError(err)
	var providerUserID string
	for _, observation := range raw {
		if observation.ProviderUserID != nil {
			providerUserID = *observation.ProviderUserID
		}
	}
	requirements.NotEmpty(providerUserID)
	eligible, err := s.FindObservationsByProviderUserIDContext(t.Context(), providerUserID, 0)
	requirements.NoError(err)
	assertions.Empty(eligible)

	// A promotion made outside the importer still reaches the finished roster.
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), sourceID, chat.ID, conversationpolicy.Full))
	_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	requirements.True(complete)
	pid, _, err = s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "synthetic-user"))
	requirements.NoError(err)
	assertions.Equal(existing, pid)
	eligible, err = s.FindObservationsByProviderUserIDContext(t.Context(), providerUserID, 0)
	requirements.NoError(err)
	assertions.NotEmpty(eligible)
}

func TestBeeperColdPolicyKeepsEstablishedIdentityAndSelfActivity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	imp, s, sourceID, chat := servedPolicyRoom(t, "account", "cold", 1,
		map[string]any{"id": "synthetic-self", "phoneNumber": "+12025550124", "isSelf": true})
	imp.res.accountID = "account"
	existing, err := imp.res.resolveUser(t.Context(), &chat.Participants.Items[0].User)
	requirements.NoError(err)
	_, err = s.EnsureConversation(t.Context(), sourceID, chat.ID, "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), sourceID, chat.ID, conversationpolicy.Cold))
	room, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, ImportOptions{AccountID: "account"}, &ImportSummary{})
	requirements.NoError(err)
	pid, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "synthetic-self"))
	requirements.NoError(err)
	assertions.Equal(existing, pid)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: room, SourceMessageID: "own", MessageType: "beeper", SenderID: sql.NullInt64{Int64: pid, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
}

func TestBeeperColdPolicyFreshSelfMentionResolvesRosterOnNextVisit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	imp, s, sourceID, chat := servedPolicyRoom(t, "self-account", "large", 21,
		map[string]any{"id": "new-self", "phoneNumber": "+12025550125", "isSelf": true})
	_, err := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	opts := ImportOptions{AccountID: "self-account", rosterState: &rosterWork{}, routeAccount: &Account{AccountID: "self-account", Status: "connected", Bridge: &AccountBridge{Type: "signal"}}}
	room, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	full, err := s.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	requirements.False(full)
	self, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback(opts.AccountID, "new-self"))
	requirements.NoError(err)
	assertions.Empty(imp.res.rich)
	now := time.Now().UTC().Truncate(time.Second)
	message, err := s.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: room, SourceMessageID: "mention-self", MessageType: "beeper", SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(s.ReplaceMessageRecipients(message, "mention", []int64{self}, []string{""}))
	activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
	refreshed, err := s.RefreshConversationPolicy(t.Context(), []int64{room}, now)
	requirements.NoError(err)
	assertions.Contains(refreshed.ChangedConversationIDs, room)
	revisited, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	assertions.Equal(room, revisited)
	assertions.NotEmpty(imp.res.rich)
	activity, err = s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
}

func TestBeeperColdPolicyCapturesEveryRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "multi-room")
	requirements.NoError(err)
	first, err := s.EnsureConversation(t.Context(), source.ID, "first", "First")
	requirements.NoError(err)
	second, err := s.EnsureConversation(t.Context(), source.ID, "second", "Second")
	requirements.NoError(err)
	pid, err := s.EnsureParticipantByIdentifier("beeper", "multi-room-user", "")
	requirements.NoError(err)
	recorder := newObservationRecorder(s)
	user := &User{ID: "multi-room-user", Email: "rooms@example.org"}
	cc := captureContext{SourceID: source.ID, ConversationID: first, AccountID: "multi-room", Network: "Signal"}
	initial, err := recorder.capture(t.Context(), pid, user, cc)
	requirements.NoError(err)
	requirements.Len(initial, 1)
	cc.ConversationID = second
	repeated, err := recorder.capture(t.Context(), pid, user, cc)
	requirements.NoError(err)
	requirements.Len(repeated, 1)
	assertions.False(repeated[0].Created)
	assertions.Equal(initial[0].Observation.Envelope.ID, repeated[0].Observation.Envelope.ID)
	var count int
	requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT COUNT(*) FROM participant_contact_observation_conversations WHERE observation_id=?`), initial[0].Observation.Envelope.ID).Scan(&count))
	assertions.Equal(2, count)
	repeated, err = recorder.capture(t.Context(), pid, user, cc)
	requirements.NoError(err)
	assertions.Empty(repeated)
}

func TestBeeperColdPolicyConfirmsSelfBeforeRichAdmission(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	imp, s, sourceID, chat := servedPolicyRoom(t, "history-account", "history-room", 21,
		map[string]any{"id": "history-self", "isSelf": true, "phoneNumber": "+12025550126"})
	// Classification must be on: otherwise every room is full and the test
	// cannot tell whether self proof arrived before rich admission.
	_, err := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	room, err := s.EnsureConversation(t.Context(), sourceID, chat.ID, "History")
	requirements.NoError(err)
	resolver := newParticipantResolver(s, "history-account")
	self, err := resolver.resolveID(t.Context(), "history-self", "")
	requirements.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: room, SourceMessageID: "historical-own", MessageType: "beeper", SenderID: sql.NullInt64{Int64: self, Valid: true}, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	full, err := s.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	requirements.False(full, "the own message does not count until the sender is proven to be the user")
	opts := ImportOptions{AccountID: "history-account", routeAccount: &Account{AccountID: "history-account", Status: "connected", Bridge: &AccountBridge{Type: "signal"}}}
	conversationID, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	assertions.Equal(room, conversationID)
	full, err = s.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	assertions.True(full)
	assertions.NotEmpty(imp.res.rich)
}

func TestBeeperColdPolicyLateEvidenceDoesNotFailCapture(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	source, err := s.GetOrCreateSource("beeper", "late-account")
	requirements.NoError(err)
	room := fullBeeperObservationRoom(t, s, source.ID)
	left, err := s.EnsureParticipantByIdentifier("test", "late-left", "")
	requirements.NoError(err)
	right, err := s.EnsureParticipantByIdentifier("test", "late-right", "")
	requirements.NoError(err)
	user := &User{ID: "late-shared-user", PhoneNumber: "+12025550127"}
	imp := NewImporter(s, nil)
	peer, err := imp.obs.capture(t.Context(), right, user, captureContext{SourceID: source.ID, ConversationID: room, AccountID: "late-account", Network: "Signal"})
	requirements.NoError(err)
	requirements.Len(peer, 1)
	obs := peer[0].Observation
	candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: left, RightKind: store.IdentityMatchParticipant, RightID: right, Basis: store.IdentityMatchStableProviderID, ServiceSlug: obs.ServiceSlug, NormalizedValue: obs.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
	requirements.NoError(err)
	imp.matcher.beforeEvidenceWrite = func() {
		imp.matcher.beforeEvidenceWrite = nil
		require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source.ID, "identity-test-room", conversationpolicy.Cold))
	}
	sum := &ImportSummary{}
	imp.captureObservations(t.Context(), left, user, &Chat{Network: "Signal"}, source.ID, room, "late-account", "", sum)
	assertions.Nil(imp.matcher.beforeEvidenceWrite, "exercise demotion immediately before evidence publication")
	assertions.Zero(sum.Errors)
	assertions.EqualValues(1, sum.ObservationsRecorded)
	kept, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	assertions.Empty(kept.Evidence)
	assertions.False(linked(t, s, left, right))
}

// A Full room whose roster grows past the participant limit must be demoted
// before its new members are resolved, so their contact details cannot merge
// identities.
func TestBeeperGrownRoomIsDemotedBeforeRichResolution(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newFakeBeeper(t)
	room := &fakeChat{ID: "growing", AccountID: "account", Network: "Signal", Title: "Room", Type: "group",
		Participants: []map[string]any{{"id": "@member0:beeper.local"}, {"id": "@member1:beeper.local"}}}
	f.addChat(room)
	imp, s, closeServer := newTestImporter(t, f)
	t.Cleanup(closeServer)
	sourceID := newBeeperTestSource(t, s, "account")
	_, err := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	existing, err := s.EnsureParticipantByPhone(t.Context(), "+12025550131", "Independent Contact", "sms")
	requirements.NoError(err)
	opts := ImportOptions{AccountID: "account", rosterState: &rosterWork{}}
	chat, err := imp.client.GetChat(t.Context(), room.ID)
	requirements.NoError(err)
	conversationID, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	// The chat sync clears the repair marker and refreshes policy after a visit.
	requirements.NoError(s.SetConversationRosterRepairPendingContext(t.Context(), conversationID, false))
	_, err = s.RefreshConversationPolicy(t.Context(), []int64{conversationID}, time.Now())
	requirements.NoError(err)
	full, err := s.ConversationFullEligibleContext(t.Context(), conversationID)
	requirements.NoError(err)
	requirements.True(full, "a two-member room is within the limit")

	f.mu.Lock()
	room.Participants = append(room.Participants, map[string]any{"id": "@phone-member:beeper.local", "phoneNumber": "+12025550131"})
	for i := 2; len(room.Participants) < 22; i++ {
		room.Participants = append(room.Participants, map[string]any{"id": fmt.Sprintf("@member%d:beeper.local", i)})
	}
	f.mu.Unlock()
	chat, err = imp.client.GetChat(t.Context(), room.ID)
	requirements.NoError(err)
	revisited, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	assertions.Equal(conversationID, revisited)
	full, err = s.ConversationFullEligibleContext(t.Context(), conversationID)
	requirements.NoError(err)
	assertions.False(full, "22 members exceed the limit")
	member, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "@phone-member:beeper.local"))
	requirements.NoError(err)
	assertions.NotZero(member)
	assertions.NotEqual(existing, member, "the demoted room's phone must not merge the member")
}

// A message-first visit skips the roster but still demotes a Full room whose
// roster outgrew its limit before its messages resolve anyone.
func TestBeeperGrownRoomIsDemotedBeforeMessageFirstVisit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newFakeBeeper(t)
	room := &fakeChat{ID: "growing-messages", AccountID: "account", Network: "Signal", Title: "Room", Type: "group",
		Participants: []map[string]any{{"id": "@member0:beeper.local"}, {"id": "@member1:beeper.local"}}}
	f.addChat(room)
	imp, s, closeServer := newTestImporter(t, f)
	t.Cleanup(closeServer)
	sourceID := newBeeperTestSource(t, s, "account")
	_, err := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	existing, err := s.EnsureParticipantByPhone(t.Context(), "+12025550132", "Independent Contact", "sms")
	requirements.NoError(err)
	work := &rosterWork{}
	chat, err := imp.client.GetChat(t.Context(), room.ID)
	requirements.NoError(err)
	conversationID, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, ImportOptions{AccountID: "account", rosterState: work}, &ImportSummary{})
	requirements.NoError(err)
	requirements.NoError(s.SetConversationRosterRepairPendingContext(t.Context(), conversationID, false))
	_, err = s.RefreshConversationPolicy(t.Context(), []int64{conversationID}, time.Now())
	requirements.NoError(err)
	full, err := s.ConversationFullEligibleContext(t.Context(), conversationID)
	requirements.NoError(err)
	requirements.True(full, "a two-member room is within the limit")

	f.mu.Lock()
	room.Participants = append(room.Participants, map[string]any{"id": "@phone-member:beeper.local", "phoneNumber": "+12025550132"})
	for i := 2; len(room.Participants) < 22; i++ {
		room.Participants = append(room.Participants, map[string]any{"id": fmt.Sprintf("@member%d:beeper.local", i)})
	}
	f.mu.Unlock()
	chat, err = imp.client.GetChat(t.Context(), room.ID)
	requirements.NoError(err)
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, ImportOptions{AccountID: "account", rosterState: work, skipRoster: true}, &ImportSummary{})
	requirements.NoError(err)
	requirements.False(complete, "the message-first visit defers the changed roster")
	message := &Message{ID: "mention-after-growth", SenderID: "@member0:beeper.local", Text: "Synthetic text", Mentions: []string{"@phone-member:beeper.local"}}
	requirements.NoError(imp.persistMessage(t.Context(), &chatScope{convID: conversationID, sourceID: sourceID, cs: &ChatState{}}, message, &ImportSummary{}))
	member, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "@phone-member:beeper.local"))
	requirements.NoError(err)
	assertions.NotZero(member)
	assertions.NotEqual(existing, member, "a grown room's listing must not merge the mentioned member")
}
