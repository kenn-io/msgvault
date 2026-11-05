package store_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportedMIMEInvitationFromExcludedSenderRemainsVisible(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	raw := []byte("From: Calendar sender <noreply@example.com>\r\n" +
		"To: Owner <owner@example.com>\r\n" +
		"Date: Sat, 17 Oct 2026 12:00:00 +0000\r\n" +
		"Message-ID: <invitation@example.com>\r\n" +
		"Subject: Planning invitation\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=invite\r\n\r\n" +
		"--invite\r\nContent-Type: text/plain\r\n\r\nCan you join?\r\n" +
		"--invite\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Calendar//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:planning@example.com\r\nDTSTAMP:20261017T120000Z\r\n" +
		"DTSTART:20261018T120000Z\r\nDTEND:20261018T130000Z\r\n" +
		"ORGANIZER:mailto:noreply@example.com\r\nATTENDEE:mailto:owner@example.com\r\n" +
		"SUMMARY:Planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n--invite--\r\n")
	require.NoError(importer.IngestRawMessage(t.Context(), f.Store, f.Source.ID, "owner@example.com", "", nil, "invitation", "invitation-hash", raw, tierAsOf, slog.Default()))
	var id, conversationID int64
	var messageType string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id,conversation_id,message_type FROM messages WHERE source_id=? AND source_message_id='invitation'`), f.Source.ID).Scan(&id, &conversationID, &messageType))
	require.Equal("email", messageType)
	projectTimelineActivity(t, f, "UTC")
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	assert.Len(page.Bursts, 1)
	assert.Zero(page.NoiseOmitted)
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conversationID, Limit: 10})
	require.NoError(err)
	assert.Len(messages, 1)
	if len(messages) == 1 {
		assert.Equal(id, messages[0].MessageID)
	}
}

func TestSavedCounterpartMatchesBurstCountWithOtherAccountOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	require.NoError(err)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(other.ID, "other@example.com", "manual"))
	otherOwner := f.EnsureParticipant("other@example.com", "Other owner", "example.com")
	id := f.add(t, f.ConvID, f.Source.ID, tierAsOf, true, "")
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, otherOwner}, nil))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", MaxDirectCounterparts: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	tier, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, tierOptions())
	require.NoError(err)
	assert.Zero(tier.Signals.OutboundLong)
	q := timelineBurstQuery()
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	q.Filters.IncludeNoise = true
	page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(err)
	require.Len(page.Bursts, 1)
	assert.Zero(page.Bursts[0].Outbound)
	found := false
	for _, cp := range page.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			found = true
			assert.Equal(store.EvidenceCoPresence, cp.Evidence)
			assert.False(cp.Counted)
		}
	}
	assert.True(found)
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: q.Filters})
	require.NoError(err)
	require.Len(messages, 1)
	for _, cp := range messages[0].Counterparts {
		if cp.ParticipantID == f.peer {
			assert.Equal(store.EvidenceCoPresence, cp.Evidence)
			assert.False(cp.Counted)
		}
	}
}

func TestInvitationFromExcludedAutomatedSenderRemainsVisible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("noreply@example.com", "Calendar sender", "example.com")
	id := f.add(t, f.ConvID, f.Source.ID, tierAsOf, false, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='calendar_event' WHERE id=?`), id)
	require.NoError(err)
	projectTimelineActivity(t, f, "UTC")
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	assert.Zero(page.NoiseOmitted)
	require.Len(page.Bursts, 1)
	assert.Equal(id, page.Bursts[0].AnchorMessageID)
	assert.Zero(page.Bursts[0].Inbound)
	for _, cp := range page.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			assert.Equal("automated_address", cp.Excluded)
			assert.False(cp.Counted)
		}
	}
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10})
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(id, messages[0].MessageID)
	contact := timelineContactQuery(f)
	contact.Now = tierAsOf
	aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
	require.NoError(err)
	assert.Zero(aggregate.Count)
}

func TestInboundAudienceIncludesOtherAccountOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	require.NoError(err)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(other.ID, "other@example.com", "manual"))
	otherOwner := f.EnsureParticipant("other@example.com", "Other owner", "example.com")
	id := f.add(t, f.ConvID, f.Source.ID, tierAsOf, false, "")
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, otherOwner}, nil))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", MaxDirectCounterparts: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	tier, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, tierOptions())
	require.NoError(err)
	assert.Zero(tier.Signals.InboundLong)
	q := timelineBurstQuery()
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	q.Filters.IncludeNoise = true
	page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(err)
	require.Len(page.Bursts, 1)
	assert.Zero(page.Bursts[0].Inbound)
	for _, cp := range page.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			assert.False(cp.Counted)
		}
	}
}
