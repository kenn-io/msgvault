package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// audienceFixture is a saved person (the sender) and the owner, with mail
// written through the real projector so the tier counts are read from rows
// the projector produced.
type audienceFixture struct {
	f        *storetest.Fixture
	personID int64
	sender   int64
	owner    int64
	asOf     time.Time
}

func newAudienceFixture(t *testing.T) audienceFixture {
	t.Helper()
	f, personID := newActivityQueryFixture(t)
	a := audienceFixture{
		f: f, personID: personID,
		owner: f.EnsureParticipant("owner@example.com", "Owner", "example.com"),
		asOf:  time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(),
		f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&a.sender))
	return a
}

// message stores one message from the sender at the given day offset. With no
// recipients it is addressed to the conversation members.
func (a audienceFixture) message(t *testing.T, key string, days int, conv int64, messageType string, to ...int64) int64 {
	t.Helper()
	b := storetest.NewMessage(a.f.Source.ID, conv).WithSourceMessageID(key).WithSentAt(a.asOf.AddDate(0, 0, days))
	message := b.Build()
	message.SenderID = sql.NullInt64{Int64: a.sender, Valid: true}
	if messageType != "" {
		message.MessageType = messageType
	}
	id, err := a.f.Store.UpsertMessage(message)
	require.NoError(t, err)
	if len(to) > 0 {
		require.NoError(t, a.f.Store.ReplaceMessageRecipients(id, "to", to, nil))
	}
	return id
}

func (a audienceFixture) signals(t *testing.T) persontiers.Signals {
	t.Helper()
	return tierSignals(t, a.f, a.personID, a.asOf, persontiers.DefaultConfig())
}

// 26 participant records that belong to 25 people stay at the limit of 25, in a
// chat read through its members.
func TestPersonTierSignalsAliasedChatStaysWithinTheLimit(t *testing.T) {
	require := require.New(t)
	a := newAudienceFixture(t)
	chat, err := a.f.Store.EnsureConversationWithType(a.f.Source.ID, "alias-chat", "group_chat", "Aliases")
	require.NoError(err)
	members := []int64{a.owner, a.sender}
	var first int64
	for i := range 24 {
		p := a.f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		_, _, err = a.f.Store.CreatePersonFromParticipant(p)
		require.NoError(err)
		members = append(members, p)
		if i == 0 {
			first = p
		}
	}
	alias := a.f.EnsureParticipant("member0-alias@example.com", "Member", "example.com")
	_, err = a.f.Store.LinkParticipants(first, alias)
	require.NoError(err)
	members = append(members, alias)
	for _, p := range members {
		require.NoError(a.f.Store.EnsureConversationParticipant(chat, p, "member"))
	}
	a.message(t, "alias-chat-1", 0, chat, "")
	projectTierSignals(t, a.f, "UTC", 25)

	signals := a.signals(t)

	assert.Equal(t, 1, signals.InboundShort, "25 people, not 26 records")
	assert.Zero(t, signals.LargeAudienceInboundEvents)
}

// An archived envelope that carries the owner's address makes that recipient
// the owner, so the recipient is not part of the audience.
func TestPersonTierSignalsEnvelopeOwnerAddressIsNotAudience(t *testing.T) {
	require := require.New(t)
	a := newAudienceFixture(t)
	other := a.f.EnsureParticipant("other@example.com", "Other", "example.com")
	id := a.message(t, "envelope-owner", 0, a.f.ConvID, "", a.owner)
	require.NoError(a.f.Store.ReplaceMessageRecipients(id, "from", []int64{a.sender}, nil))
	_, err := a.f.Store.DB().ExecContext(t.Context(), a.f.Store.Rebind(
		`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'to',?)`),
		id, other, "owner@example.com")
	require.NoError(err)
	projectTierSignals(t, a.f, "UTC", 1)

	signals := a.signals(t)

	assert.Equal(t, 1, signals.InboundShort, "only the sender is in the audience")
	assert.Zero(t, signals.LargeAudienceInboundEvents)
}

// A message whose sender has no from row still counts the sender in the
// audience, as the projector does.
func TestPersonTierSignalsSenderWithoutFromRowCountsInAudience(t *testing.T) {
	require := require.New(t)
	a := newAudienceFixture(t)
	x := a.f.EnsureParticipant("x@example.com", "X", "example.com")
	y := a.f.EnsureParticipant("y@example.com", "Y", "example.com")
	a.message(t, "no-from", 0, a.f.ConvID, "", a.owner, x, y)
	projectTierSignals(t, a.f, "UTC", 2)

	signals := a.signals(t)

	require.Zero(signals.InboundShort)
	assert.Equal(t, 1, signals.LargeAudienceInboundEvents, "sender, X and Y are three people")
}

// An inbound meeting for more people than the limit is an all-hands, not a
// meeting day.
func TestPersonTierSignalsAllHandsMeetingGivesNoMeetingDay(t *testing.T) {
	a := newAudienceFixture(t)
	guests := []int64{a.owner}
	for i := range 4 {
		guests = append(guests, a.f.EnsureParticipant(fmt.Sprintf("guest%d@example.com", i), "Guest", "example.com"))
	}
	a.message(t, "all-hands", -1, a.f.ConvID, "calendar_event", guests...)
	a.message(t, "small-meeting", -2, a.f.ConvID, "calendar_event", a.owner)
	projectTierSignals(t, a.f, "UTC", 2)

	signals := a.signals(t)

	assert.Equal(t, 1, signals.MeetingDaysLong, "only the small meeting counts")
	assert.Equal(t, 1, signals.LargeAudienceInboundEvents)
}

// The excluded-list counter counts what the rules would count, not every
// message that carries a list ID. A co-author of the owner's own message is a
// sender, which no tier counts.
func TestPersonTierSignalsListCounterIgnoresRowsThatNeverCount(t *testing.T) {
	require := require.New(t)
	a := newAudienceFixture(t)
	stranger := a.f.EnsureParticipant("stranger@example.com", "Stranger", "example.com")
	counted := a.message(t, "list-counted", 0, a.f.ConvID, "", a.owner)
	coauthored := a.message(t, "list-coauthor", -1, a.f.ConvID, "", stranger)
	require.NoError(a.f.Store.ReplaceMessageRecipients(coauthored, "from", []int64{a.owner, a.sender}, nil))
	for _, id := range []int64{counted, coauthored} {
		_, err := a.f.Store.DB().ExecContext(t.Context(),
			a.f.Store.Rebind(`UPDATE messages SET list_id = 'list.example.com' WHERE id = ?`), id)
		require.NoError(err)
	}
	projectTierSignals(t, a.f, "UTC", 25)

	signals := a.signals(t)

	assert.Zero(t, signals.InboundShort)
	assert.Equal(t, 1, signals.ExcludedListEvents, "the co-authored message is not a counted role")
}
