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

	signals := tierSignals(t, a.f, a.personID, a.asOf, persontiers.DefaultConfig())

	assert.Equal(t, 1, signals.MeetingDaysLong, "only the small meeting counts")
	assert.Equal(t, 1, signals.LargeAudienceInboundEvents)
}
