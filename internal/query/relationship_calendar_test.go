package query

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relationshipTestTimezoneUTCPlus14 = "Pacific/Kiritimati"

func TestRelationshipCalendarReturnsLocalDaysAndCachedTemperatureSummary(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	engine, canonicalID, _, _ := buildTimelineFixture(t)

	response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
		CanonicalID: canonicalID,
		Year:        2026,
		Timezone:    "America/Chicago",
	})
	require.NoError(err)
	assert.Equal(canonicalID, response.CanonicalID)
	assert.Equal(2026, response.Year)
	assert.Equal("America/Chicago", response.Timezone)
	assert.Equal("UTC", response.ScoringTimezone)
	assert.Equal(identityindex.RelationshipScoreVersion, response.ScoreVersion)
	assert.NotEmpty(response.CacheRevision)
	assert.Equal(100, response.Current.Temperature)
	assert.Equal(int64(1), response.Current.Rank)
	assert.Equal(int64(1), response.Current.Population)
	assert.Equal(100, response.PeakTemperature)
	assert.Equal(2026, response.PeakYear)
	require.NotEmpty(response.Annual)
	people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{
		Page: PageSpec{Limit: 25},
	})
	require.NoError(err)
	var person *PersonSummary
	for index := range people.Rows {
		if people.Rows[index].ID == canonicalID {
			person = &people.Rows[index]
			break
		}
	}
	require.NotNil(person, "the indexed people directory includes the cluster")
	assert.Equal(100, person.CurrentRelationshipTemperature)
	assert.Equal(100, person.PeakRelationshipTemperature)
	assert.Equal(2026, person.PeakRelationshipYear)
	legacyFallback, err := engine.GetPerson(t.Context(), canonicalID, Context{}, nil)
	require.NoError(err)
	require.NotNil(legacyFallback)
	assert.Equal(person.CurrentRelationshipTemperature,
		legacyFallback.CurrentRelationshipTemperature,
		"cluster-drift fallback retains the compact graph temperature")

	byDate := make(map[string]RelationshipCalendarDay, len(response.Days))
	for _, day := range response.Days {
		byDate[day.Date] = day
	}
	assert.Equal(RelationshipCalendarDay{
		Date: "2026-07-11", Meetings: 1, Total: 1,
		ModalityMask: identityindex.ModalityMeeting,
		Level:        identityindex.HeatFirstQuartile,
	}, byDate["2026-07-11"])
	assert.Equal(int64(1), byDate["2026-07-12"].Received)
	assert.Equal(int64(1), byDate["2026-07-12"].Email)
	assert.Equal(identityindex.HeatFirstQuartile, byDate["2026-07-12"].Level)
	assert.Equal(int64(3), byDate["2026-07-13"].Received)
	assert.Equal(int64(3), byDate["2026-07-13"].Chat)
	assert.Equal(int64(3), byDate["2026-07-13"].Total)
	assert.Equal(identityindex.HeatThirdQuartile, byDate["2026-07-13"].Level)
	assert.Equal(identityindex.HeatNone, byDate["2026-01-01"].Level)
	assert.Equal("2026-07-13", response.Days[len(response.Days)-1].Date,
		"future local dates after the exact published snapshot remain absent")
}

func TestRelationshipCalendarValidatesIdentityYearAndTimezone(t *testing.T) {
	t.Parallel()
	engine := NewTestDataBuilder(t).BuildEngine()
	tests := []RelationshipCalendarRequest{
		{CanonicalID: 0, Year: 2026, Timezone: "UTC"},
		{CanonicalID: 1, Year: 1969, Timezone: "UTC"},
		{CanonicalID: 1, Year: 2026, Timezone: "Mars/Olympus_Mons"},
	}
	for _, request := range tests {
		_, err := engine.RelationshipCalendar(context.Background(), request)
		assert.ErrorIs(t, err, ErrInvalidExploreRequest, request)
	}
}

func TestRelationshipCalendarValidationUsesRequestedTimezoneYear(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, time.December, 31, 10, 30, 0, 0, time.UTC)
	_, _, err := validateRelationshipCalendarRequestAt(RelationshipCalendarRequest{
		CanonicalID: 1, Year: 2026, Timezone: relationshipTestTimezoneUTCPlus14,
	}, now)
	require.NoError(t, err, "UTC+14 is already in the next local year")

	_, _, err = validateRelationshipCalendarRequestAt(RelationshipCalendarRequest{
		CanonicalID: 1, Year: 2027, Timezone: relationshipTestTimezoneUTCPlus14,
	}, now)
	assert.ErrorIs(t, err, ErrInvalidRelationshipYear)
}

func TestRelationshipCalendarAllowsCurrentYearBeyondStaleCacheWatermark(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	currentYear := time.Now().UTC().Year()
	effectiveAt := time.Date(currentYear-1, time.December, 31, 12, 0, 0, 0, time.UTC)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "imessage")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	personID := b.AddParticipant("person@example.test", "example.test", "Person")
	b.AddOwnerParticipant(sourceID, ownerID)
	messageID := b.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: 902,
		MessageType: "imessage", ConversationType: "direct_chat", SentAt: effectiveAt,
	})
	b.AddFrom(messageID, personID, "Person")
	b.AddTo(messageID, ownerID, "Owner")
	b.AddConversationParticipant(902, personID)
	b.AddConversationParticipant(902, ownerID)
	engine := b.BuildEngine()

	response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
		CanonicalID: personID, Year: currentYear, Timezone: "UTC",
	})
	require.NoError(err)
	assert.Equal(effectiveAt.Format(time.DateOnly), response.EffectiveDate)
	assert.Empty(response.Days, "a stale cache has no current-year days through its watermark")
	assert.NotZero(response.Current.Temperature, "the compact relationship summary remains available")
}

func TestRelationshipCalendarReadsBothUTCPartitionsAtLocalYearBoundary(t *testing.T) {
	t.Parallel()
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "imessage")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	personID := b.AddParticipant("person@example.test", "example.test", "Person")
	b.AddOwnerParticipant(sourceID, ownerID)
	for index, when := range []time.Time{
		time.Date(2025, time.December, 31, 23, 30, 0, 0, time.UTC),
		time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC),
	} {
		messageID := b.AddMessage(MessageOpt{
			SourceID: sourceID, ConversationID: int64(700 + index),
			MessageType: "imessage", ConversationType: "direct_chat", SentAt: when,
		})
		b.AddFrom(messageID, personID, "Person")
		b.AddTo(messageID, ownerID, "Owner")
		b.AddConversationParticipant(int64(700+index), personID)
		b.AddConversationParticipant(int64(700+index), ownerID)
	}
	engine := b.BuildEngine()

	response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
		CanonicalID: personID, Year: 2026, Timezone: "Asia/Tokyo",
	})
	require.NoError(t, err)
	byDate := make(map[string]RelationshipCalendarDay, len(response.Days))
	for _, day := range response.Days {
		byDate[day.Date] = day
	}
	assert.Equal(t, int64(1), byDate["2026-01-01"].Total,
		"the local-year query includes the preceding UTC-year shard")
	assert.Equal(t, int64(1), byDate["2026-01-02"].Total)
}

func TestRelationshipCalendarUsesSnapshotInstantForLatestLocalDay(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	effectiveAt := time.Date(2025, time.December, 31, 23, 30, 0, 0, time.UTC)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "imessage")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	personID := b.AddParticipant("person@example.test", "example.test", "Person")
	b.AddOwnerParticipant(sourceID, ownerID)
	messageID := b.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: 901,
		MessageType: "imessage", ConversationType: "direct_chat", SentAt: effectiveAt,
	})
	b.AddFrom(messageID, personID, "Person")
	b.AddTo(messageID, ownerID, "Owner")
	b.AddConversationParticipant(901, personID)
	b.AddConversationParticipant(901, ownerID)
	engine := b.BuildEngine()
	state, err := ReadCacheSyncState(engine.analyticsDir)
	require.NoError(err)
	state.LastSyncAt = effectiveAt
	state.PublishedAt = effectiveAt.Add(time.Minute)
	stateJSON, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(os.WriteFile(CacheStatePath(engine.analyticsDir), stateJSON, 0o600))

	response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
		CanonicalID: personID, Year: 2026, Timezone: relationshipTestTimezoneUTCPlus14,
	})
	require.NoError(err)
	require.Len(response.Days, 1)
	assert.Equal(RelationshipCalendarDay{
		Date: "2026-01-01", Received: 1, Chat: 1, Total: 1,
		ModalityMask: identityindex.ModalityChat,
		Level:        identityindex.HeatFirstQuartile,
	}, response.Days[0])
	assert.Equal("2025-12-31", response.EffectiveDate,
		"score years stay pegged to UTC while the calendar uses local days")
}

func TestRelationshipCalendarRejectsMissingAndOwnerPeople(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "gmail")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	personID := b.AddParticipant("person@example.test", "example.test", "Person")
	draftRecipient := b.AddParticipant("draft@example.test", "example.test", "Draft recipient")
	b.AddOwnerParticipant(sourceID, ownerID)
	messageID := b.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "email",
		SentAt: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC),
	})
	b.AddFrom(messageID, ownerID, "Owner")
	b.AddTo(messageID, personID, "Person")
	draft := b.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "email", IsFromMe: true,
		SentAt: time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC),
	})
	b.AddFrom(draft, ownerID, "Owner")
	b.AddTo(draft, draftRecipient, "Draft recipient")
	b.unsentMessages = fmt.Sprintf("(SELECT %d::BIGINT AS message_id)", draft)
	engine := b.BuildEngine()

	for _, canonicalID := range []int64{ownerID, 999_999, draftRecipient} {
		_, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
			CanonicalID: canonicalID, Year: 2026, Timezone: "UTC",
		})
		assert.ErrorIs(err, ErrRelationshipPersonNotFound, canonicalID)
	}
}

func TestRelationshipCalendarHandlesExtremeOffsetsDSTFractionalOffsetsAndLeapDays(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "imessage")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	personID := b.AddParticipant("person@example.test", "example.test", "Person")
	b.AddOwnerParticipant(sourceID, ownerID)
	addInbound := func(when time.Time) {
		messageID := b.AddMessage(MessageOpt{
			SourceID: sourceID, ConversationType: "direct_chat",
			MessageType: "imessage", SentAt: when,
		})
		b.AddFrom(messageID, personID, "Person")
		b.AddTo(messageID, ownerID, "Owner")
	}
	for _, when := range []time.Time{
		time.Date(2024, time.February, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2025, time.December, 31, 10, 30, 0, 0, time.UTC),
		time.Date(2025, time.December, 31, 18, 20, 0, 0, time.UTC),
		time.Date(2026, time.January, 1, 8, 30, 0, 0, time.UTC),
		time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC),
		time.Date(2026, time.March, 8, 8, 30, 0, 0, time.UTC),
	} {
		addInbound(when)
	}
	engine := b.BuildEngine()

	queryDay := func(year int, timezone, date string) RelationshipCalendarDay {
		response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
			CanonicalID: personID, Year: year, Timezone: timezone,
		})
		require.NoError(t, err)
		for _, day := range response.Days {
			if day.Date == date {
				return day
			}
		}
		require.FailNow(t, "calendar date missing", "%s in %s", date, timezone)
		return RelationshipCalendarDay{}
	}

	assert.Equal(int64(3), queryDay(2026, relationshipTestTimezoneUTCPlus14, "2026-01-01").Total,
		"UTC+14 reads the preceding UTC-year partition")
	assert.Equal(int64(3), queryDay(2025, "America/Adak", "2025-12-31").Total,
		"UTC-10 reads the following UTC-year partition")
	assert.Equal(int64(2), queryDay(2026, "Asia/Kathmandu", "2026-01-01").Total,
		"fractional UTC offsets preserve local dates")
	assert.Equal(int64(2), queryDay(2026, "America/Chicago", "2026-03-08").Total,
		"both sides of the DST jump stay on the same local day")

	leap, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
		CanonicalID: personID, Year: 2024, Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Len(leap.Days, 366)
	assert.Equal(int64(1), queryDay(2024, "UTC", "2024-02-29").Total)
}

func TestRelationshipCalendarDeduplicatesLinkedAliasEdgesPerMessage(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "gmail")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	primaryID := b.AddParticipant("person@example.test", "example.test", "Person")
	aliasID := b.AddParticipant("person@work.example", "work.example", "Person Work")
	b.LinkCluster(primaryID, aliasID)
	b.AddOwnerParticipant(sourceID, ownerID)
	messageID := b.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "email",
		SentAt: time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC),
	})
	b.AddFrom(messageID, primaryID, "Person")
	b.AddTo(messageID, ownerID, "Owner")
	b.AddCc(messageID, aliasID, "Person Work")
	engine := b.BuildEngine()

	response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{
		CanonicalID: min(primaryID, aliasID), Year: 2026, Timezone: "UTC",
	})
	require.NoError(err)
	for _, day := range response.Days {
		if day.Date == "2026-05-01" {
			assert.Equal(int64(1), day.Total)
			assert.Equal(int64(1), day.Received)
			return
		}
	}
	require.Fail("calendar day missing")
}

func TestRelationshipCalendarAndTimelineReadOnlySubjectAndOwnerRows(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.test", "imessage")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	b.AddOwnerParticipant(source, owner)
	members := []int64{owner}
	for i := range 150 {
		member := b.AddParticipant(fmt.Sprintf("person-%d@example.test", i), "example.test", "Person")
		members = append(members, member)
	}
	for _, member := range members {
		b.AddConversationParticipant(900, member)
	}
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	const messages = 200
	for i := range messages {
		sender := members[i%len(members)]
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 900, ConversationType: "group_chat",
			MessageType: "imessage", SentAt: start.Add(time.Duration(i) * time.Minute), IsFromMe: sender == owner, SenderID: &sender})
		b.AddFrom(id, sender, "Sender")
		if sender == owner {
			b.AddTo(id, members[1], "Person")
		}
	}
	engine := b.BuildEngine()
	t.Run("Calendar", func(t *testing.T) {
		profile, err := profileQuery(t, engine, func() error {
			response, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: members[1], Year: 2025})
			if err == nil {
				require.NotEmpty(t, response.Days)
				assert.Equal(t, int64(4), response.Days[0].Total)
				assert.Equal(t, int64(2), response.Days[0].Sent)
				assert.Equal(t, int64(2), response.Days[0].Received)
				assert.Equal(t, int64(4), response.Days[0].Chat)
			}
			return err
		})
		require.NoError(t, err)
		require.Positive(t, profile.maxCardinality())
		assert.Less(t, profile.maxCardinality(), 10*messages)
	})
	t.Run("Timeline", func(t *testing.T) {
		profile, err := profileQuery(t, engine, func() error {
			response, err := engine.RelationshipTimeline(t.Context(), RelationshipTimelineRequest{CanonicalID: members[1]})
			if err == nil {
				assert.Equal(t, int64(1), response.TotalCount)
				require.Len(t, response.Rows, 1)
				assert.Equal(t, int64(200), response.Rows[0].MessageCount)
			}
			return err
		})
		require.NoError(t, err)
		require.Positive(t, profile.maxCardinality())
		assert.Less(t, profile.maxCardinality(), 10*messages)
	})
}

func TestRelationshipCalendarKeepsSourceNativeOwnerWithoutOwnerParticipants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.test", "imessage")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	subject := b.AddParticipant("person@example.test", "example.test", "Person")
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 900, ConversationType: "direct_chat",
			MessageType: "imessage", SentAt: start.AddDate(0, 0, i), IsFromMe: true, SenderID: &owner})
		b.AddFrom(id, owner, "Owner")
		b.AddTo(id, subject, "Person")
	}
	b.AddConversationParticipant(900, owner)
	b.AddConversationParticipant(900, subject)
	engine := b.BuildEngine()
	calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: subject, Year: 2025})
	require.NoError(err)
	require.Len(calendar.Days, 2)
	for _, day := range calendar.Days {
		assert.Equal(int64(1), day.Sent)
		assert.Equal(int64(1), day.Total)
	}
	timeline, err := engine.RelationshipTimeline(t.Context(), RelationshipTimelineRequest{CanonicalID: subject})
	require.NoError(err)
	assert.Equal(int64(2), timeline.TotalCount)
	require.Len(timeline.Rows, 2)
	for i, row := range timeline.Rows {
		assert.Equal("chat_burst", row.Kind)
		assert.Equal(int64(1), row.MessageCount)
		assert.Equal(start.AddDate(0, 0, 1-i), row.OccurredAt)
	}
}
