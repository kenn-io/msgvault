package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
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
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
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
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, []string{"Synthetic sender"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, []string{"Synthetic recipient"}))
	}
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	s := signals[personID]
	assert.Equal(2, s.OutboundShort)
	assert.Equal(2, s.OutboundLong)
	assert.Equal(1, s.InboundShort)
	assert.Equal(3, s.InboundLong)
	assert.Equal(3, s.ActiveDaysShort)
	assert.Equal(5, s.ActiveDaysLong)
	assert.Equal(1, s.MeetingDaysLong)
	assert.Equal(2, s.ChannelsLong)
	assert.Equal(1, s.ExcludedListEvents)
	cfg := persontiers.DefaultConfig()
	cfg.ExcludeListMessages = false
	signals, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, cfg, 25)
	require.NoError(err)
	assert.Equal(2, signals[personID].InboundShort)
	assert.Equal(4, signals[personID].ActiveDaysShort)
}

func TestPersonTierSignalsCountDistinctEmailAudience(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-audience-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, extra}, []string{"Owner", "Extra"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "cc", []int64{extra}, []string{"Extra"}))
		// Two alias snapshots for the same participant must not inflate audience.
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'to',?)`), id, extra, "extra-alias@example.com")
		require.NoError(err)
		if i == 1 {
			require.NoError(f.Store.ReplaceMessageRecipients(id, "bcc", []int64{third}, []string{"Third"}))
		}
	}
	// The sender counts as part of the audience, as in ClassifyActivityCandidate.
	// Email audience is per-message, regardless of conversation membership.
	for i := range 30 {
		p := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, p, "member"))
	}
	projectTierSignals(t, f, "UTC", 2)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 2)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsRejectLargeChatAndUseLocalDates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-group", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, p := range []int64{owner, sender} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, p, "member"))
	}
	for i := range 30 {
		p := f.EnsureParticipant(fmt.Sprintf("chat%d@example.com", i), "Member", "example.com")
		require.NoError(f.Store.EnsureConversationParticipant(conversation, p, "member"))
	}
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(err)
	asOf := time.Date(2026, 11, 2, 0, 0, 0, 0, zone)
	for i, conv := range []int64{conversation, f.ConvID, f.ConvID} {
		at := asOf.AddDate(0, 0, -89).Add(23*time.Hour + 30*time.Minute)
		if i == 2 {
			at = asOf.AddDate(0, 0, -90).Add(23*time.Hour + 30*time.Minute)
		}
		message := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID(fmt.Sprintf("tier-local-%d", i)).WithSentAt(at).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		// A chat message with no recipients takes its audience from the members.
		if conv != conversation {
			require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
		}
	}
	projectTierSignals(t, f, zone.String(), 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(2, signals[personID].InboundLong)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierReadiness(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.True(ready.Ready)
	assert.Empty(ready.Reasons)
	f.CreateMessage("pending-tier-projection")
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "projection_pending")
	assert.Contains(ready.Reasons, "watermark_behind")
	_, err = f.Store.ClaimActivityTimezoneTransitionContext(t.Context(), "America/New_York")
	require.NoError(err)
	_, err = f.Store.ClaimActivityDirectLimitTransitionContext(t.Context(), 10)
	require.NoError(err)
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.Contains(ready.Reasons, "projection_pending")
}

func projectTierSignals(t *testing.T, f *storetest.Fixture, zone string, maxDirect int) {
	t.Helper()
	require := require.New(t)
	p, err := activity.NewProjector(f.Store, activity.Options{Timezone: zone, BatchSize: 500, MaxDirectCounterparts: maxDirect})
	require.NoError(err)
	_, err = p.RunOnce(t.Context())
	require.NoError(err)
}

// Co-authors are senders, not people the owner addressed. Inbound cc and
// owner-absent mail must not count toward any tier.
func TestPersonTierSignalsRejectCoauthorsCCAndObserved(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	stranger := f.EnsureParticipant("stranger@example.com", "Stranger", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
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
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", from, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
		if i > 0 {
			require.NoError(f.Store.ReplaceMessageRecipients(id, "cc", []int64{counterpart}, nil))
		}
	}
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	s := signals[personID]
	assert.Zero(s.OutboundLong)
	assert.Zero(s.InboundLong)
	assert.Zero(s.ActiveDaysLong)
	assert.Zero(s.ChannelsLong)
}

func TestPersonTierSignalsBindingsFeedExclusions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf.AddDate(0, 0, -400)})
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	s := signals[personID]
	assert.Zero(s.InboundLong)
	assert.Equal([]string{"counterpart@example.com"}, s.Emails)
	assert.False(s.HasPhoneOrChat)
	assert.False(s.Owner)
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value) VALUES (?,'beeper',?)`), counterpart, "synthetic-chat-owner")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "synthetic-chat-owner", "manual"))
	signals, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.True(signals[personID].HasPhoneOrChat)
	assert.True(signals[personID].Owner)
}

func TestPersonTierSignalsEmptyPageAndCanceledContext(t *testing.T) {
	require := require.New(t)
	f, personID := newActivityQueryFixture(t)
	got, err := f.Store.PersonTierSignalsContext(t.Context(), nil, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.Empty(t, got)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.PersonTierSignalsContext(ctx, []int64{personID}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), persontiers.DefaultConfig(), 25)
	require.ErrorIs(err, context.Canceled)
}

func TestPersonTierReadinessUnreconciledIdentityAndDeletedWatermark(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "second-owner@example.com", "manual"))
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "projection_pending")
	projectTierSignals(t, f, "UTC", 25)
	id := f.CreateMessage("tier-deleted")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	require.NoError(err)
	projectTierSignals(t, f, "UTC", 25)
	// Model a restored watermark ending at the last live message. A later deleted
	// row must not keep an otherwise reconciled archive permanently unready.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'activity_spine_watermark'`), strconv.FormatInt(id-1, 10))
	require.NoError(err)
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.True(ready.Ready)
	assert.Empty(ready.Reasons)
}

func TestPersonTierSignalsChatAudienceExcludesOnlySourceOwners(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	alias, err := f.Store.EnsureParticipantByIdentifier("beeper", "synthetic-owner-id", "Owner alias")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "synthetic-owner-id", "manual"))
	otherOwner := f.EnsureParticipant("other-owner@example.com", "Other owner", "example.com")
	otherSource, err := f.Store.GetOrCreateSource("gmail", "other-owner@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(otherSource.ID, "other-owner@example.com", "manual"))
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, fourth := range []int64{alias, otherOwner} {
		conv, err := f.Store.EnsureConversationWithType(f.Source.ID, fmt.Sprintf("tier-chat-scope-%d", i), "group_chat", "Synthetic group")
		require.NoError(err)
		for _, id := range []int64{owner, sender, extra, fourth} {
			require.NoError(f.Store.EnsureConversationParticipant(conv, id, "member"))
		}
		message := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID(fmt.Sprintf("tier-chat-scope-message-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	}
	projectTierSignals(t, f, "UTC", 2)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 2)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].ChannelsLong)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsOwnerBoundThroughSecondParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf})
	_, err := f.Store.LinkParticipants(counterpart, owner)
	require.NoError(err)
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.True(signals[personID].Owner)
	assert.Zero(signals[personID].InboundLong)
	assert.ElementsMatch([]string{"owner@example.com", "counterpart@example.com"}, signals[personID].Emails)
}

func TestPersonTierSignalsValidatePage(t *testing.T) {
	require := require.New(t)
	f, personID := newActivityQueryFixture(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	cfg := persontiers.DefaultConfig()
	for _, ids := range [][]int64{{0}, {-1}, make([]int64, 501)} {
		_, err := f.Store.PersonTierSignalsContext(t.Context(), ids, asOf, cfg, 25)
		require.ErrorIs(err, store.ErrInvalidActivityRequest)
	}
	_, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, time.Time{}, cfg, 25)
	require.ErrorIs(err, store.ErrInvalidActivityRequest)
}

// Aliases of one resolved person must count once in an audience, as they do in
// ClassifyActivityCandidate. Counting participant IDs would make a chat or
// email with a few people look like a broadcast and drop real contact.
func TestPersonTierSignalsCountResolvedPeopleOncePerAudience(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	// One other person reached through six participant records.
	other := f.EnsureParticipant("other0@example.com", "Other", "example.com")
	_, _, err := f.Store.CreatePersonFromParticipant(other)
	require.NoError(err)
	aliases := []int64{other}
	for i := 1; i < 6; i++ {
		alias := f.EnsureParticipant(fmt.Sprintf("other%d@example.com", i), "Other", "example.com")
		_, err = f.Store.LinkParticipants(other, alias)
		require.NoError(err)
		aliases = append(aliases, alias)
	}
	chat, err := f.Store.EnsureConversationWithType(f.Source.ID, "alias-group", "group_chat", "Aliases")
	require.NoError(err)
	for _, p := range append([]int64{owner, sender}, aliases...) {
		require.NoError(f.Store.EnsureConversationParticipant(chat, p, "member"))
	}
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, conv := range []int64{chat, f.ConvID} {
		message := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID(fmt.Sprintf("tier-alias-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", append([]int64{owner}, aliases...), nil))
	}
	// Two people besides the owner: the sender and the aliased person.
	projectTierSignals(t, f, "UTC", 2)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 2)
	require.NoError(err)
	assert.Equal(2, signals[personID].InboundShort, "chat and email both stay under the limit")
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
	// The limit still applies: two people are a broadcast at a limit of one.
	signals, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(2, signals[personID].LargeAudienceInboundEvents, "the limit still applies")
}
