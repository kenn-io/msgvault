package store_test

import (
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
	drafts, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{"DRAFT": {Name: "DRAFT", Type: "system"}})
	require.NoError(err)
	for i, event := range []struct {
		days                           int
		outbound, meeting, list, draft bool
	}{
		{0, true, false, false, false}, {0, false, false, false, false}, {-364, false, false, false, false},
		{-365, true, false, false, false}, {1, false, false, false, false}, {-2, false, false, true, false},
		{-3, true, true, false, false}, {-3, false, true, false, false}, {-4, true, false, false, true},
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
		if event.draft {
			require.NoError(f.Store.ReplaceMessageLabels(id, []int64{drafts["DRAFT"]}))
		}
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, []string{"Synthetic sender"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, []string{"Synthetic recipient"}))
	}
	projectTierSignals(t, f, "UTC", 25)
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	s := signals
	assert.Equal(1, s.OutboundShort)
	assert.Equal(1, s.OutboundLong)
	assert.Equal(1, s.InboundShort)
	assert.Equal(2, s.InboundLong)
	assert.Equal(2, s.ActiveDaysShort)
	assert.Equal(3, s.ActiveDaysLong)
	assert.Equal(1, s.MeetingDaysLong)
	assert.Equal(1, s.ChannelsLong, "a meeting day is not a message channel")
	assert.Equal(1, s.ExcludedListEvents)
}

func TestPersonTierSignalsUseLocalDates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(err)
	asOf := time.Date(2026, 11, 2, 0, 0, 0, 0, zone)
	for i, days := range []int{-89, -90} {
		at := asOf.AddDate(0, 0, days).Add(23*time.Hour + 30*time.Minute)
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-local-%d", i)).WithSentAt(at).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
	}
	projectTierSignals(t, f, zone.String(), 25)
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	assert.Equal(1, signals.InboundShort)
	assert.Equal(2, signals.InboundLong)
}

// tierSignals reads one person's counts through explain.
func tierSignals(t *testing.T, f *storetest.Fixture, personID int64, asOf time.Time, cfg persontiers.Config) persontiers.Signals {
	t.Helper()
	explanation, err := f.Store.ExplainPersonTierContext(t.Context(), personID, store.TierOptionsAsOf(store.PersonTierOptions{Config: cfg}, asOf))
	require.NoError(t, err)
	return explanation.Signals
}

func tierReadiness(t *testing.T, f *storetest.Fixture) store.PersonTierReadiness {
	t.Helper()
	preview, err := f.Store.PreviewPersonTiersContext(t.Context(), tierOptions())
	require.NoError(t, err)
	return preview.Readiness
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
// owner-absent mail must not count toward any tier, and a list message in a
// role no tier counts is not an excluded list message either.
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
		message.ListID = sql.NullString{String: "announcements.example.com", Valid: true}
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
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	s := signals
	assert.Zero(s.OutboundLong)
	assert.Zero(s.InboundLong)
	assert.Zero(s.ActiveDaysLong)
	assert.Zero(s.ChannelsLong)
	assert.Zero(s.ExcludedListEvents)
}

func TestPersonTierSignalsBindingsFeedExclusions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf.AddDate(0, 0, -400)})
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	s := signals
	assert.Zero(s.InboundLong)
	assert.Equal([]string{"counterpart@example.com"}, s.Emails)
	assert.False(s.HasPhoneOrChat)
	assert.False(s.Owner)
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value) VALUES (?,'beeper',?)`), counterpart, "synthetic-chat-owner")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "synthetic-chat-owner", "manual"))
	signals = tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	assert.True(signals.HasPhoneOrChat)
	assert.True(signals.Owner)
}

func TestPersonTierReadiness(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	f.CreateMessage("pending-tier-projection")
	ready := tierReadiness(t, f)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "watermark_behind")
	projectTierSignals(t, f, "UTC", 25)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "second-owner@example.com", "manual"))
	ready = tierReadiness(t, f)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "projection_pending")
	projectTierSignals(t, f, "UTC", 25)
	id := f.CreateMessage("tier-deleted")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	require.NoError(err)
	projectTierSignals(t, f, "UTC", 25)
	// Model a restored watermark ending at the last live message. A later deleted
	// row must not keep an otherwise reconciled archive permanently unready.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'activity_spine_watermark'`), strconv.FormatInt(id-1, 10))
	require.NoError(err)
	ready = tierReadiness(t, f)
	assert.True(ready.Ready)
	assert.Empty(ready.Reasons)
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
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	assert.True(signals.Owner)
	assert.Zero(signals.InboundLong)
	assert.ElementsMatch([]string{"owner@example.com", "counterpart@example.com"}, signals.Emails)
}

// The projection drops the owner's own addresses only within a message's
// account. A person who is your address in another account is still excluded.
func TestPersonTierSignalsOwnerAddressInAnotherAccountIsExcluded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	other, err := f.Store.GetOrCreateSource("gmail", "other-account@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(other.ID, "counterpart@example.com", "manual"))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf})

	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())

	assert.Equal(1, signals.InboundLong, "this account's projection still counts the person")
	assert.True(signals.Owner)
}
