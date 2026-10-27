package beeper

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperColdPolicyDoesNotPromoteRichIdentity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "account")
	requirements.NoError(err)
	existing, err := s.EnsureParticipantByPhone("+12025550123", "Independent Contact", "sms")
	requirements.NoError(err)
	imp := NewImporter(s, nil)
	chat := &Chat{ID: "cold", AccountID: "account", Network: "Signal", Type: "group"}
	chat.Participants.Items = []Participant{{ID: "synthetic-user", PhoneNumber: "+12025550123", FullName: "Cold User"}}
	_, err = s.EnsureConversation(source.ID, chat.ID, "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, chat.ID, conversationpolicy.Cold))
	sum := &ImportSummary{}
	room, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, chat, ImportOptions{AccountID: "account"}, sum)
	requirements.NoError(err)
	pid, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "synthetic-user"))
	requirements.NoError(err)
	assertions.NotEqual(existing, pid)
	assertions.Empty(imp.res.rich)
	raw, err := s.ListParticipantObservationsContext(t.Context(), pid, true)
	requirements.NoError(err)
	requirements.Len(raw, 1)
	eligible, err := s.FindObservationsByProviderUserIDContext(t.Context(), *raw[0].ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Empty(eligible)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, chat.ID, conversationpolicy.Full))
	requirements.NoError(imp.replayConversationRoster(t.Context(), room, source.ID, chat, true, routeEvidenceFor(chat, ImportOptions{AccountID: "account"}), ImportOptions{AccountID: "account"}, sum))
	pid, _, err = s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "synthetic-user"))
	requirements.NoError(err)
	assertions.Equal(existing, pid)
	eligible, err = s.FindObservationsByProviderUserIDContext(t.Context(), *raw[0].ProviderUserID, 0)
	requirements.NoError(err)
	assertions.NotEmpty(eligible)
}

func TestBeeperColdPolicyKeepsEstablishedIdentityAndSelfActivity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "account")
	requirements.NoError(err)
	imp := NewImporter(s, nil)
	imp.res.accountID = "account"
	user := User{ID: "synthetic-self", PhoneNumber: "+12025550124", IsSelf: true}
	existing, err := imp.res.resolveUser(&user)
	requirements.NoError(err)
	chat := &Chat{ID: "cold", AccountID: "account", Network: "Signal", Type: "group"}
	chat.Participants.Items = []Participant{{User: user}}
	_, err = s.EnsureConversation(source.ID, chat.ID, "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, chat.ID, conversationpolicy.Cold))
	room, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, chat, ImportOptions{AccountID: "account"}, &ImportSummary{})
	requirements.NoError(err)
	pid, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", user.ID))
	requirements.NoError(err)
	assertions.Equal(existing, pid)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "own", MessageType: "beeper", SenderID: sql.NullInt64{Int64: pid, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
}

func TestBeeperColdPolicyFreshSelfMentionPromotesAlreadyFetchedRoster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "self-account")
	requirements.NoError(err)
	imp := NewImporter(s, nil)
	chat := &Chat{ID: "large", AccountID: "self-account", Network: "Signal", Type: "group"}
	chat.Participants.Total = 21
	chat.Participants.Items = []Participant{{ID: "new-self", PhoneNumber: "+12025550125", IsSelf: true}}
	opts := ImportOptions{AccountID: "self-account", routeAccount: &Account{AccountID: "self-account", Status: "connected", Bridge: &AccountBridge{Type: "signal"}}}
	sum := &ImportSummary{}
	room, complete, membership, err := imp.ensureConversation(t.Context(), 0, source.ID, chat, opts, sum)
	requirements.NoError(err)
	full, err := s.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	requirements.False(full)
	self, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback(opts.AccountID, "new-self"))
	requirements.NoError(err)
	assertions.Empty(imp.res.rich)
	now := time.Now().UTC().Truncate(time.Second)
	message, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "mention-self", MessageType: "beeper", SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(s.ReplaceMessageRecipients(message, "mention", []int64{self}, []string{""}))
	activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
	refreshed, err := s.RefreshConversationPolicy(t.Context(), []int64{room}, now)
	requirements.NoError(err)
	assertions.Contains(refreshed.ChangedConversationIDs, room)
	requirements.NoError(imp.replayConversationRoster(t.Context(), room, source.ID, membership.roster, complete, membership.routeEvidence, opts, sum))
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
	first, err := s.EnsureConversation(source.ID, "first", "First")
	requirements.NoError(err)
	second, err := s.EnsureConversation(source.ID, "second", "Second")
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
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM participant_contact_observation_conversations WHERE observation_id=?`), initial[0].Observation.Envelope.ID).Scan(&count))
	assertions.Equal(2, count)
	repeated, err = recorder.capture(t.Context(), pid, user, cc)
	requirements.NoError(err)
	assertions.Empty(repeated)
}

func TestBeeperColdPolicyConfirmsSelfBeforeRichAdmission(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "history-account")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "history-room", "History")
	requirements.NoError(err)
	resolver := newParticipantResolver(s, "history-account")
	self, err := resolver.resolveID("history-self", "")
	requirements.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "historical-own", MessageType: "beeper", SenderID: sql.NullInt64{Int64: self, Valid: true}, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	chat := &Chat{ID: "history-room", AccountID: "history-account", Network: "Signal", Type: "group"}
	chat.Participants.Total = 21
	chat.Participants.Items = []Participant{{ID: "history-self", IsSelf: true, PhoneNumber: "+12025550126"}}
	opts := ImportOptions{AccountID: "history-account", routeAccount: &Account{AccountID: "history-account", Status: "connected", Bridge: &AccountBridge{Type: "signal"}}}
	imp := NewImporter(s, nil)
	conversationID, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, chat, opts, &ImportSummary{})
	requirements.NoError(err)
	assertions.Equal(room, conversationID)
	full, err := s.ConversationFullEligibleContext(t.Context(), room)
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
