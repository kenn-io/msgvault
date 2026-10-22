package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonTierSignalsWindowsRolesAndMeetingDays(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, event := range []struct {
		days                    int
		outbound, meeting, list bool
	}{
		{0, true, false, false}, {0, false, false, false}, {-89, true, false, false},
		{-90, false, false, false}, {-364, false, false, false}, {-365, true, false, false},
		{1, false, false, false}, {-2, false, false, true},
		{-3, true, true, false}, {-3, false, true, false},
	} {
		from, to := counterpart, owner
		if event.outbound {
			from, to = owner, counterpart
		}
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-window-%d", i)).WithSentAt(asOf.AddDate(0, 0, event.days)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		if event.meeting {
			message.MessageType = "calendar_event"
		}
		if event.list {
			message.ListID = sql.NullString{String: "announcements.example.com", Valid: true}
		}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, []string{"Synthetic sender"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, []string{"Synthetic recipient"}))
	}
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	s := signals[personID]
	assert.Equal(t, 2, s.OutboundShort)
	assert.Equal(t, 2, s.OutboundLong)
	assert.Equal(t, 1, s.InboundShort)
	assert.Equal(t, 3, s.InboundLong)
	assert.Equal(t, 3, s.ActiveDaysShort)
	assert.Equal(t, 5, s.ActiveDaysLong)
	assert.Equal(t, 1, s.MeetingDaysLong)
	assert.Equal(t, 2, s.ChannelsLong)
	assert.Equal(t, 1, s.ExcludedListEvents)
	require.NotNil(t, s.LastDirectAt)
	assert.True(t, asOf.Equal(*s.LastDirectAt))
	cfg := persontiers.DefaultConfig()
	cfg.ExcludeListMessages = false
	signals, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, cfg, 25)
	require.NoError(t, err)
	assert.Equal(t, 2, signals[personID].InboundShort)
	assert.Equal(t, 4, signals[personID].ActiveDaysShort)
}

func TestPersonTierSignalsCountDistinctEmailAudience(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")
	var sender int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-audience-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, extra}, []string{"Owner", "Extra"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "cc", []int64{extra}, []string{"Extra"}))
		// Two alias snapshots for the same participant must not inflate audience.
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'to',?)`), id, extra, "extra-alias@example.com")
		require.NoError(t, err)
		if i == 1 {
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "bcc", []int64{third}, []string{"Third"}))
		}
	}
	// Email audience is per-message, regardless of conversation membership.
	for i := range 30 {
		p := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		require.NoError(t, f.Store.EnsureConversationParticipant(f.ConvID, p, "member"))
	}
	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, signals[personID].InboundShort)
	assert.Equal(t, 1, signals[personID].ActiveDaysShort)
	assert.Equal(t, 1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsRejectLargeChatAndUseLocalDates(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var sender int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-group", "group_chat", "Synthetic group")
	require.NoError(t, err)
	for _, p := range []int64{owner, sender} {
		require.NoError(t, f.Store.EnsureConversationParticipant(conversation, p, "member"))
	}
	for i := range 30 {
		p := f.EnsureParticipant(fmt.Sprintf("chat%d@example.com", i), "Member", "example.com")
		require.NoError(t, f.Store.EnsureConversationParticipant(conversation, p, "member"))
	}
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	asOf := time.Date(2026, 11, 2, 0, 0, 0, 0, zone)
	for i, conv := range []int64{conversation, f.ConvID, f.ConvID} {
		at := asOf.AddDate(0, 0, -89).Add(23*time.Hour + 30*time.Minute)
		if i == 2 {
			at = asOf.AddDate(0, 0, -90).Add(23*time.Hour + 30*time.Minute)
		}
		message := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID(fmt.Sprintf("tier-local-%d", i)).WithSentAt(at).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
	}
	projectTierSignals(t, f, zone.String(), 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	assert.Equal(t, 1, signals[personID].InboundShort)
	assert.Equal(t, 2, signals[personID].InboundLong)
	assert.Equal(t, 1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierReadiness(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	seedTierSignalsActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(t, err)
	assert.True(t, ready.Ready)
	assert.Empty(t, ready.Reasons)
	f.CreateMessage("pending-tier-projection")
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(t, err)
	assert.False(t, ready.Ready)
	assert.Contains(t, ready.Reasons, "pending_activity")
	assert.Contains(t, ready.Reasons, "watermark_behind")
	_, err = f.Store.ClaimActivityTimezoneTransitionContext(t.Context(), "America/New_York")
	require.NoError(t, err)
	_, err = f.Store.ClaimActivityDirectLimitTransitionContext(t.Context(), 10)
	require.NoError(t, err)
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(t, err)
	assert.Contains(t, ready.Reasons, "timezone_transition")
	assert.Contains(t, ready.Reasons, "direct_limit_transition")
}

func projectTierSignals(t *testing.T, f *storetest.Fixture, zone string, maxDirect int) {
	t.Helper()
	p, err := activity.NewProjector(f.Store, activity.Options{Timezone: zone, BatchSize: 500, MaxDirectCounterparts: maxDirect})
	require.NoError(t, err)
	_, err = p.RunOnce(t.Context())
	require.NoError(t, err)
}

// Co-authors are senders, not people the owner addressed. Inbound cc and
// owner-absent mail must remain explanation-only evidence.
func TestPersonTierSignalsRejectCoauthorsCCAndObserved(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	stranger := f.EnsureParticipant("stranger@example.com", "Stranger", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-roles-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		from, to := []int64{stranger}, []int64{owner}
		if i == 0 {
			from, to = []int64{owner, counterpart}, []int64{stranger}
		}
		if i == 2 {
			to = []int64{stranger}
		}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", from, nil))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", to, nil))
		if i > 0 {
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "cc", []int64{counterpart}, nil))
		}
	}
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	s := signals[personID]
	assert.Zero(t, s.OutboundLong)
	assert.Zero(t, s.InboundLong)
	assert.Zero(t, s.ActiveDaysLong)
	assert.Zero(t, s.ChannelsLong)
	assert.Equal(t, 2, s.CoPresenceEvents)
	assert.Equal(t, 1, s.ObservedEvents)
}

func TestPersonTierSignalsLastDirectAnyAgeAndLinkedIdentifiers(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedTierSignalsActivity(t, f, personID, []time.Time{asOf.AddDate(0, 0, -400)})
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	s := signals[personID]
	require.NotNil(t, s.LastDirectAt)
	assert.True(t, asOf.AddDate(0, 0, -400).Equal(*s.LastDirectAt))
	assert.Zero(t, s.InboundLong)
	assert.Equal(t, []string{"counterpart@example.com"}, s.Emails)
	assert.False(t, s.HasPhoneOrChat)
	assert.False(t, s.Owner)
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value) VALUES (?,'beeper',?)`), counterpart, "synthetic-chat-owner")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "synthetic-chat-owner", "manual"))
	signals, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	assert.True(t, signals[personID].HasPhoneOrChat)
	assert.True(t, signals[personID].Owner)
}

func TestPersonTierSignalsEmptyPageAndCanceledContext(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	got, err := f.Store.PersonTierSignalsContext(t.Context(), nil, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	assert.Empty(t, got)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.PersonTierSignalsContext(ctx, []int64{personID}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), persontiers.DefaultConfig(), 25)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPersonTierReadinessUnreconciledIdentityAndDeletedWatermark(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	seedTierSignalsActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "second-owner@example.com", "manual"))
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(t, err)
	assert.False(t, ready.Ready)
	assert.Contains(t, ready.Reasons, "identity_revision_changed")
	projectTierSignals(t, f, "UTC", 25)
	id := f.CreateMessage("tier-deleted")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	require.NoError(t, err)
	projectTierSignals(t, f, "UTC", 25)
	// Model a restored watermark ending at the last live message. A later deleted
	// row must not keep an otherwise reconciled archive permanently unready.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'activity_spine_watermark'`), fmt.Sprint(id-1))
	require.NoError(t, err)
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(t, err)
	assert.True(t, ready.Ready)
	assert.Empty(t, ready.Reasons)
}

func TestPersonTierSignalsChatAudienceExcludesOnlySourceOwners(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	alias, err := f.Store.EnsureParticipantByIdentifier("beeper", "synthetic-owner-id", "Owner alias")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "synthetic-owner-id", "manual"))
	otherOwner := f.EnsureParticipant("other-owner@example.com", "Other owner", "example.com")
	otherSource, err := f.Store.GetOrCreateSource("gmail", "other-owner@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(otherSource.ID, "other-owner@example.com", "manual"))
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	var sender int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, fourth := range []int64{alias, otherOwner} {
		conv, err := f.Store.EnsureConversationWithType(f.Source.ID, fmt.Sprintf("tier-chat-scope-%d", i), "group_chat", "Synthetic group")
		require.NoError(t, err)
		for _, id := range []int64{owner, sender, extra, fourth} {
			require.NoError(t, f.Store.EnsureConversationParticipant(conv, id, "member"))
		}
		message := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID(fmt.Sprintf("tier-chat-scope-message-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	}
	projectTierSignals(t, f, "UTC", 2)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 2)
	require.NoError(t, err)
	assert.Equal(t, 1, signals[personID].InboundShort)
	assert.Equal(t, 1, signals[personID].ActiveDaysShort)
	assert.Equal(t, 1, signals[personID].ChannelsLong)
	assert.Equal(t, 1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsOwnerBoundThroughSecondParticipant(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedTierSignalsActivity(t, f, personID, []time.Time{asOf})
	_, err := f.Store.LinkParticipants(counterpart, owner)
	require.NoError(t, err)
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(t, err)
	assert.True(t, signals[personID].Owner)
	assert.Zero(t, signals[personID].InboundLong)
	assert.ElementsMatch(t, []string{"owner@example.com", "counterpart@example.com"}, signals[personID].Emails)
}

func newTierSignalsFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	participant := f.EnsureParticipant("counterpart@example.com", "Counterpart", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	require.True(t, created)
	return f, person.ID
}

func seedTierSignalsActivity(t *testing.T, f *storetest.Fixture, personID int64, occurred []time.Time) {
	t.Helper()
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	for i, at := range occurred {
		id := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-seed-%d-%d-%d", personID, at.Unix(), i)).WithSentAt(at).Create(t, f.Store)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{counterpart}, nil))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	}
	projectTierSignals(t, f, "UTC", 25)
}

func TestPersonTierSignalsValidatePageAndWindows(t *testing.T) {
	f, personID := newTierSignalsFixture(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	cfg := persontiers.DefaultConfig()
	for _, ids := range [][]int64{{0}, {-1}, make([]int64, 501)} {
		_, err := f.Store.PersonTierSignalsContext(t.Context(), ids, asOf, cfg, 25)
		require.ErrorIs(t, err, store.ErrInvalidActivityRequest)
	}
	_, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, time.Time{}, cfg, 25)
	require.ErrorIs(t, err, store.ErrInvalidActivityRequest)
	cfg.ShortWindowDays = 0
	_, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, cfg, 25)
	require.ErrorIs(t, err, store.ErrInvalidActivityRequest)
}
