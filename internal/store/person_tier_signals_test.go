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
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
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
	require.NotNil(s.LastDirectAt)
	assert.True(asOf.Equal(*s.LastDirectAt))
	cfg := persontiers.DefaultConfig()
	cfg.ExcludeListMessages = false
	signals, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, cfg, 25)
	require.NoError(err)
	assert.Equal(2, signals[personID].InboundShort)
	assert.Equal(4, signals[personID].ActiveDaysShort)
}

func TestPersonTierSignalsCountDistinctEmailAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
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
	// Email audience is per-message, regardless of conversation membership.
	for i := range 30 {
		p := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, p, "member"))
	}
	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsEmailAudienceUsesEnvelopeOwnerIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSourceMessageID("tier-envelope-owner-audience").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, extra}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address = ? WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`), "owner@example.com", id, owner)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address = ? WHERE id = ?`), "changed-owner@example.com", owner)
	require.NoError(err)

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsEmailAudienceExcludesMergedOwnerEnvelopeAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSourceMessageID("tier-merged-owner-envelope-audience").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, ownerAlias, extra}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = ?
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		"owner@example.com", id, owner)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = ?
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		"owner-alias@example.com", id, ownerAlias)
	require.NoError(err)
	require.NoError(f.Store.MergeParticipants(owner, ownerAlias))

	var ownerSnapshotCount, ownerSnapshotParticipantCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT COUNT(*), COUNT(DISTINCT participant_id) FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'to'
		 AND email_address IN ('owner@example.com', 'owner-alias@example.com')`), id).
		Scan(&ownerSnapshotCount, &ownerSnapshotParticipantCount))
	assert.Equal(2, ownerSnapshotCount)
	assert.Equal(1, ownerSnapshotParticipantCount,
		"participant merge preserves both owner envelope addresses under one participant")

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsEmailAudienceExcludesBoundOwnerAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	ownerPerson, created, err := f.Store.CreatePersonFromParticipant(owner)
	require.NoError(err)
	require.True(created)
	_, err = f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)
	boundOwner, err := f.Store.PersonForParticipants([]int64{ownerAlias})
	require.NoError(err)
	require.NotNil(boundOwner)
	assert.Equal(ownerPerson.ID, boundOwner.ID)
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSourceMessageID("tier-bound-owner-email-audience").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, ownerAlias, extra}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = ?
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		"owner-alias@example.com", id, ownerAlias)
	require.NoError(err)

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsEmailAudienceExcludesUnboundLinkedOwnerAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.invalid", "Owner alias", "example.invalid")
	_, err := f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)
	boundOwner, err := f.Store.PersonForParticipants([]int64{ownerAlias})
	require.NoError(err)
	require.Nil(boundOwner)
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSourceMessageID("tier-linked-owner-envelope").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, ownerAlias, extra}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = ?
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		"owner-alias@example.invalid", id, ownerAlias)
	require.NoError(err)

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsRejectLargeChatAndUseLocalDates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
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
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
	}
	projectTierSignals(t, f, zone.String(), 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(2, signals[personID].InboundLong)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierReadiness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	seedTierSignalsActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.True(ready.Ready)
	assert.Empty(ready.Reasons)
	f.CreateMessage("pending-tier-projection")
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "pending_activity")
	assert.Contains(ready.Reasons, "watermark_behind")
	_, err = f.Store.ClaimActivityTimezoneTransitionContext(t.Context(), "America/New_York")
	require.NoError(err)
	_, err = f.Store.ClaimActivityDirectLimitTransitionContext(t.Context(), 10)
	require.NoError(err)
	ready, err = f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.Contains(ready.Reasons, "timezone_transition")
	assert.Contains(ready.Reasons, "direct_limit_transition")
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
// owner-absent mail must remain explanation-only evidence.
func TestPersonTierSignalsRejectCoauthorsCCAndObserved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
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
	assert.Equal(2, s.CoPresenceEvents)
	assert.Equal(1, s.ObservedEvents)
}

func TestPersonTierSignalsLastDirectAnyAgeAndLinkedIdentifiers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedTierSignalsActivity(t, f, personID, []time.Time{asOf.AddDate(0, 0, -400)})
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	s := signals[personID]
	require.NotNil(s.LastDirectAt)
	assert.True(asOf.AddDate(0, 0, -400).Equal(*s.LastDirectAt))
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
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	got, err := f.Store.PersonTierSignalsContext(t.Context(), nil, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.Empty(got)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.PersonTierSignalsContext(ctx, []int64{personID}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), persontiers.DefaultConfig(), 25)
	require.ErrorIs(err, context.Canceled)
}

func TestPersonTierReadinessUnreconciledIdentityAndDeletedWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	seedTierSignalsActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "second-owner@example.com", "manual"))
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "identity_revision_changed")
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
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
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
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	}
	projectTierSignals(t, f, "UTC", 2)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 2)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].ChannelsLong)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsCountLinkedAliasesSeparatelyAsChatCounterparts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	alias := f.EnsureParticipant("counterpart-alias@example.com", "Counterpart alias", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	_, err := f.Store.LinkParticipants(counterpart, alias)
	require.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-audience", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, counterpart, alias} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := storetest.NewMessage(f.Source.ID, conversation).
		WithSourceMessageID("tier-linked-audience-inbound").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: counterpart, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{counterpart}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Zero(signals[personID].InboundShort)
	assert.Zero(signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsCountUnboundLinkedAliasesSeparatelyInChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	aliasA := f.EnsureParticipant("unbound-a@example.com", "Unbound A", "example.com")
	aliasB := f.EnsureParticipant("unbound-b@example.com", "Unbound B", "example.com")
	_, err := f.Store.LinkParticipants(aliasA, aliasB)
	require.NoError(err)
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-unbound-link-audience", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, sender, aliasA, aliasB} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := storetest.NewMessage(f.Source.ID, conversation).
		WithSourceMessageID("tier-unbound-link-audience-inbound").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))

	projectTierSignals(t, f, "UTC", 2)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 2)
	require.NoError(err)
	assert.Zero(signals[personID].InboundShort)
	assert.Zero(signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsCountMergedAliasesSeparatelyInEmailAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	contact := f.EnsureParticipant("contact@example.com", "Contact", "example.com")
	contactAlias := f.EnsureParticipant("contact-alias@example.com", "Contact alias", "example.com")
	mergeTierTestParticipants(t, f.Store, contact, contactAlias, "tier-signal-merged-email-contact")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSourceMessageID("tier-merged-email-audience-inbound").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, contact, contactAlias}, nil))

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Zero(signals[personID].InboundShort)
	assert.Zero(signals[personID].ActiveDaysShort)
	assert.Equal(1, signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsExcludeLinkedOwnerAliasesFromChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	_, err := f.Store.RemoveAccountIdentity(f.Source.ID, "owner@example.com")
	require.NoError(err)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias, err := f.Store.EnsureParticipantByIdentifier("beeper", "synthetic-owner-id", "Owner alias")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "synthetic-owner-id", "manual"))
	_, err = f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-owner-audience", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, ownerAlias, counterpart} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := storetest.NewMessage(f.Source.ID, conversation).
		WithSourceMessageID("tier-linked-owner-audience-inbound").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: counterpart, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{counterpart}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{ownerAlias}, nil))

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsExcludeMergedOwnerAliasesFromChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	mergeTierTestParticipants(t, f.Store, owner, ownerAlias, "tier-signal-merged-owner")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-merged-owner-audience", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, ownerAlias, counterpart} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := storetest.NewMessage(f.Source.ID, conversation).
		WithSourceMessageID("tier-merged-owner-audience-inbound").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: counterpart, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{counterpart}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{ownerAlias}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = ?
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		"owner@example.com", id, ownerAlias)
	require.NoError(err)

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsExcludeMergedSourceOwnerFromChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerSurvivor := f.EnsureParticipant("merged-owner@example.com", "Merged owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-merged-source-owner-chat", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, counterpart} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := storetest.NewMessage(f.Source.ID, conversation).
		WithSourceMessageID("tier-merged-source-owner-inbound").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: counterpart, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{counterpart}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = ?
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		"owner@example.com", id, owner)
	require.NoError(err)
	require.NoError(f.Store.MergeParticipants(owner, ownerSurvivor))
	var archivedOwnerAddress string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT email_address FROM message_recipients
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`), id, ownerSurvivor).
		Scan(&archivedOwnerAddress))
	require.Equal("owner@example.com", archivedOwnerAddress)

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsExcludeBoundOwnerAliasWithNullEnvelope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	mergeTierTestParticipants(t, f.Store, owner, ownerAlias, "tier-signal-null-envelope-owner")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSourceMessageID("tier-bound-owner-null-envelope").WithSentAt(asOf).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, ownerAlias, extra}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE message_recipients SET email_address = NULL
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`), id, ownerAlias)
	require.NoError(err)

	projectTierSignals(t, f, "UTC", 1)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 1)
	require.NoError(err)
	assert.Equal(1, signals[personID].InboundShort)
	assert.Equal(1, signals[personID].ActiveDaysShort)
	assert.Zero(signals[personID].LargeAudienceInboundEvents)
}

func TestPersonTierSignalsOwnerBoundThroughSecondParticipant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedTierSignalsActivity(t, f, personID, []time.Time{asOf})
	_, err := f.Store.LinkParticipants(counterpart, owner)
	require.NoError(err)
	projectTierSignals(t, f, "UTC", 25)
	signals, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, persontiers.DefaultConfig(), 25)
	require.NoError(err)
	assert.True(signals[personID].Owner)
	assert.Zero(signals[personID].InboundLong)
	assert.ElementsMatch([]string{"owner@example.com", "counterpart@example.com"}, signals[personID].Emails)
}

func newTierSignalsFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	participant := f.EnsureParticipant("counterpart@example.com", "Counterpart", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	require.True(created)
	return f, person.ID
}

func mergeTierTestParticipants(
	t *testing.T,
	st *store.Store,
	survivorParticipantID, absorbedParticipantID int64,
	idempotencyKey string,
) {
	t.Helper()
	require := require.New(t)
	survivor, created, err := st.CreatePersonFromParticipant(survivorParticipantID)
	require.NoError(err)
	require.True(created)
	absorbed, created, err := st.CreatePersonFromParticipant(absorbedParticipantID)
	require.NoError(err)
	require.True(created)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           idempotencyKey,
		Actor:                    "test",
	})
	require.NoError(err)
	got, err := st.PersonForParticipants([]int64{survivorParticipantID, absorbedParticipantID})
	require.NoError(err)
	require.NotNil(got)
	require.Equal(merged.Person.ID, got.ID)
}

func seedTierSignalsActivity(t *testing.T, f *storetest.Fixture, personID int64, occurred []time.Time) {
	t.Helper()
	require := require.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	for i, at := range occurred {
		id := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-seed-%d-%d-%d", personID, at.Unix(), i)).WithSentAt(at).Create(t, f.Store)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{counterpart}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	}
	projectTierSignals(t, f, "UTC", 25)
}

func TestPersonTierSignalsValidatePageAndWindows(t *testing.T) {
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	cfg := persontiers.DefaultConfig()
	oversized := make([]int64, 501)
	for i := range oversized {
		oversized[i] = personID
	}
	for _, ids := range [][]int64{{0}, {-1}, oversized} {
		_, err := f.Store.PersonTierSignalsContext(t.Context(), ids, asOf, cfg, 25)
		require.ErrorIs(err, store.ErrInvalidActivityRequest)
	}
	_, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, time.Time{}, cfg, 25)
	require.ErrorIs(err, store.ErrInvalidActivityRequest)
	cfg.ShortWindowDays = 0
	_, err = f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, asOf, cfg, 25)
	require.ErrorIs(err, store.ErrInvalidActivityRequest)
}
