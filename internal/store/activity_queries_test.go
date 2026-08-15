package store_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestActivityIntersectionsUseTheSameStableRefsAndIndependentPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{
		time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
	})
	_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2026-07-31",
		Body:      "Synthetic note-only day",
		Author:    "Test User",
		PersonIDs: []int64{personID},
	})
	require.NoError(err)
	firstEntry, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2026-07-30",
		Body:      "Synthetic first note",
		Author:    "Test User",
		PersonIDs: []int64{personID},
	})
	require.NoError(err)
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2026-07-30",
		Body:      "Synthetic second note",
		Author:    "Test User",
		PersonIDs: []int64{personID},
	})
	require.NoError(err)

	days, err := f.Store.PersonDaysContext(t.Context(), store.PersonDaysRequest{
		PersonID: personID,
		From:     "2026-07-29",
		To:       "2026-07-31",
		Limit:    2,
	})
	require.NoError(err)
	require.Len(days.Days, 2)
	assert.Equal(int64(3), days.TotalCount)
	assert.Equal("2026-07-31", days.Days[0].LocalDate)
	assert.Equal(int64(0), days.Days[0].EventCount)
	assert.Equal(int64(1), days.Days[0].EntryCount)
	assert.Equal("2026-07-30", days.Days[1].LocalDate)
	assert.Equal(int64(2), days.Days[1].EventCount)

	personDay, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
		PersonID:    personID,
		LocalDate:   "2026-07-30",
		Limit:       1,
		EntryLimit:  1,
		EntryOffset: 0,
	})
	require.NoError(err)
	require.Len(personDay.Activity, 1)
	require.Len(personDay.Entries, 1)
	assert.Equal(int64(2), personDay.ActivityTotalCount)
	assert.Equal(int64(2), personDay.EntryTotalCount)
	assert.Equal(firstEntry.ID, personDay.Entries[0].ID)
	assert.Equal(store.RefKindMessage, personDay.Activity[0].Kind)
	assert.Equal("UTC", personDay.Activity[0].Timezone)
	assert.Equal("sent_at", personDay.Activity[0].DateOrigin)
	assert.Equal("timestamp", personDay.Activity[0].DatePrecision)

	day, err := f.Store.DayContext(t.Context(), store.DayRequest{
		LocalDate:              "2026-07-30",
		Limit:                  10,
		EntryLimit:             1,
		ActivityLimitPerPerson: 1,
	})
	require.NoError(err)
	require.Len(day.Persons, 1)
	require.Len(day.Entries, 1)
	assert.Equal(int64(1), day.PersonTotalCount)
	assert.Equal(int64(2), day.EntryTotalCount)
	assert.Equal(int64(2), day.Persons[0].EventCount)
	assert.True(day.Persons[0].ActivityTruncated)
	require.Len(day.Persons[0].Activity, 1)
	assert.Equal(personDay.Activity[0], day.Persons[0].Activity[0])
}

func TestActivityIntersectionValidationAndEmptyPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)

	_, err := f.Store.PersonDaysContext(t.Context(), store.PersonDaysRequest{
		PersonID: personID,
		From:     "2026-07-31",
		To:       "2026-07-30",
	})
	require.ErrorIs(err, store.ErrInvalidActivityRequest)
	_, err = f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
		PersonID: personID, LocalDate: "2026-07-30", EntryOffset: -1,
	})
	require.ErrorIs(err, store.ErrInvalidActivityRequest)
	_, err = f.Store.DayContext(t.Context(), store.DayRequest{LocalDate: "2026-7-30"})
	require.ErrorIs(err, store.ErrInvalidActivityRequest)
	_, err = f.Store.PersonDaysContext(t.Context(), store.PersonDaysRequest{PersonID: 999999})
	require.ErrorIs(err, store.ErrPersonNotFound)
	assert.Equal(store.ErrPersonNotFound.Error(), err.Error())

	page, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
		PersonID: personID, LocalDate: "2026-07-30",
	})
	require.NoError(err)
	assert.NotNil(page.Activity)
	assert.NotNil(page.Entries)
	assert.Zero(page.ActivityTotalCount)
	assert.Zero(page.EntryTotalCount)
}

func TestActivityPaginationAcceptanceMatrix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	base := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{
		base, base.Add(time.Hour), base.Add(2 * time.Hour),
	})
	entryIDs := make([]int64, 0, 3)
	for index := range 3 {
		entry, err := f.Store.CreateDailyNoteEntryContext(
			t.Context(), store.DailyNoteEntryInput{
				LocalDate: "2026-07-30",
				Body:      fmt.Sprintf("Synthetic note %d", index),
				Author:    "Test User",
				PersonIDs: []int64{personID},
			})
		require.NoError(err)
		entryIDs = append(entryIDs, entry.ID)
	}

	defaultPage, err := f.Store.PersonDayContext(
		t.Context(), store.PersonDayRequest{
			PersonID: personID, LocalDate: "2026-07-30",
		})
	require.NoError(err)
	require.Len(defaultPage.Activity, 3)
	require.Len(defaultPage.Entries, 3)
	assert.Greater(defaultPage.Activity[0].OccurredAt, defaultPage.Activity[1].OccurredAt)
	gotEntryIDs := []int64{
		defaultPage.Entries[0].ID,
		defaultPage.Entries[1].ID,
		defaultPage.Entries[2].ID,
	}
	assert.Equal(entryIDs, gotEntryIDs)

	capped, err := f.Store.PersonDayContext(
		t.Context(), store.PersonDayRequest{
			PersonID: personID, LocalDate: "2026-07-30",
			Limit: store.ActivityMaxLimit + 1, EntryLimit: store.ActivityMaxLimit + 1,
		})
	require.NoError(err)
	assert.Len(capped.Activity, 3)
	assert.Len(capped.Entries, 3)

	daysDefault, err := f.Store.PersonDaysContext(
		t.Context(), store.PersonDaysRequest{PersonID: personID})
	require.NoError(err)
	assert.Len(daysDefault.Days, 1)
	daysCapped, err := f.Store.PersonDaysContext(
		t.Context(), store.PersonDaysRequest{
			PersonID: personID, Limit: store.ActivityMaxLimit + 1,
		})
	require.NoError(err)
	assert.Equal(daysDefault, daysCapped)

	dayDefault, err := f.Store.DayContext(t.Context(), store.DayRequest{
		LocalDate: "2026-07-30",
	})
	require.NoError(err)
	require.Len(dayDefault.Persons, 1)
	require.Len(dayDefault.Entries, 3)
	dayCapped, err := f.Store.DayContext(t.Context(), store.DayRequest{
		LocalDate:              "2026-07-30",
		Limit:                  store.ActivityMaxLimit + 1,
		EntryLimit:             store.ActivityMaxLimit + 1,
		ActivityLimitPerPerson: store.ActivityMaxLimit + 1,
	})
	require.NoError(err)
	assert.Equal(dayDefault, dayCapped)

	secondPage, err := f.Store.PersonDayContext(
		t.Context(), store.PersonDayRequest{
			PersonID: personID, LocalDate: "2026-07-30",
			Limit: 1, Offset: 1, EntryLimit: 1, EntryOffset: 1,
		})
	require.NoError(err)
	require.Len(secondPage.Activity, 1)
	require.Len(secondPage.Entries, 1)
	assert.Equal(defaultPage.Activity[1], secondPage.Activity[0])
	assert.Equal(entryIDs[1], secondPage.Entries[0].ID)
	assert.Equal(int64(3), secondPage.ActivityTotalCount)
	assert.Equal(int64(3), secondPage.EntryTotalCount)

	for name, call := range map[string]func() error{
		"person days": func() error {
			_, err := f.Store.PersonDaysContext(t.Context(), store.PersonDaysRequest{
				PersonID: personID, Offset: -1,
			})
			return err
		},
		"person day activity": func() error {
			_, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
				PersonID: personID, LocalDate: "2026-07-30", Offset: -1,
			})
			return err
		},
		"person day entries": func() error {
			_, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
				PersonID: personID, LocalDate: "2026-07-30", EntryOffset: -1,
			})
			return err
		},
		"day people": func() error {
			_, err := f.Store.DayContext(t.Context(), store.DayRequest{
				LocalDate: "2026-07-30", Offset: -1,
			})
			return err
		},
		"day entries": func() error {
			_, err := f.Store.DayContext(t.Context(), store.DayRequest{
				LocalDate: "2026-07-30", EntryOffset: -1,
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(call(), store.ErrInvalidActivityRequest)
		})
	}
}

func TestDayPaginationAndReferenceMetadataAreDeterministic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, firstPersonID := newActivityQueryFixture(t)
	secondParticipant := f.EnsureParticipant(
		"second@example.com", "Second", "example.com")
	secondPerson, _, err := f.Store.CreatePersonFromParticipant(secondParticipant)
	require.NoError(err)
	at := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, firstPersonID, []time.Time{at})
	seedProjectedActivity(t, f, secondPerson.ID, []time.Time{at})
	meetingID := seedProjectedMeetingActivity(t, f, firstPersonID, at.Add(-time.Hour))

	all, err := f.Store.DayContext(t.Context(), store.DayRequest{
		LocalDate: "2026-07-30", Limit: 10, ActivityLimitPerPerson: 10,
	})
	require.NoError(err)
	require.Len(all.Persons, 2)
	assert.Equal(firstPersonID, all.Persons[0].PersonID)
	assert.Equal(secondPerson.ID, all.Persons[1].PersonID)

	secondPage, err := f.Store.DayContext(t.Context(), store.DayRequest{
		LocalDate: "2026-07-30", Limit: 1, Offset: 1,
	})
	require.NoError(err)
	require.Len(secondPage.Persons, 1)
	assert.Equal(secondPerson.ID, secondPage.Persons[0].PersonID)
	assert.Equal(int64(2), secondPage.PersonTotalCount)

	personView, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
		PersonID: firstPersonID, LocalDate: "2026-07-30", Limit: 10,
	})
	require.NoError(err)
	var meeting store.ActivityRef
	for _, ref := range personView.Activity {
		if ref.MessageID == meetingID {
			meeting = ref
		}
	}
	assert.Equal(store.RefKindMeeting, meeting.Kind)
	assert.Equal("meeting:"+strconv.FormatInt(meetingID, 10), meeting.Ref)
	assert.Equal("sent_at", meeting.DateOrigin)
	assert.Equal("day", meeting.DatePrecision)
	assert.Equal("UTC", meeting.Timezone)
	assert.Zero(meeting.UTCOffsetMinutes)
	assert.Equal(store.RoleOrganizer, meeting.Role)
	assert.Equal(store.EvidenceDirect, meeting.Evidence)
	assert.Equal("2026-07-30", meeting.LocalDate)

	var dayMeeting store.ActivityRef
	for _, person := range all.Persons {
		if person.PersonID != firstPersonID {
			continue
		}
		for _, ref := range person.Activity {
			if ref.MessageID == meetingID {
				dayMeeting = ref
			}
		}
	}
	assert.Equal(meeting, dayMeeting)
}

func TestActivityPagesKeepTotalsBeyondTheTailAndExcludeNoteOnlyDayPersons(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{
		time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
	})
	_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2026-07-31", Body: "Synthetic note", Author: "Test User",
		PersonIDs: []int64{personID},
	})
	require.NoError(err)

	personDay, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
		PersonID: personID, LocalDate: "2026-07-30",
		Limit: 1, Offset: 50, EntryLimit: 1, EntryOffset: 50,
	})
	require.NoError(err)
	assert.Empty(personDay.Activity)
	assert.Empty(personDay.Entries)
	assert.Equal(int64(2), personDay.ActivityTotalCount)
	assert.Zero(personDay.EntryTotalCount)
	personEntriesBeyond, err := f.Store.PersonDayContext(
		t.Context(), store.PersonDayRequest{
			PersonID: personID, LocalDate: "2026-07-31",
			EntryLimit: 1, EntryOffset: 50,
		})
	require.NoError(err)
	assert.Empty(personEntriesBeyond.Entries)
	assert.Equal(int64(1), personEntriesBeyond.EntryTotalCount)

	days, err := f.Store.PersonDaysContext(t.Context(), store.PersonDaysRequest{
		PersonID: personID, Limit: 1, Offset: 50,
	})
	require.NoError(err)
	assert.Empty(days.Days)
	assert.Equal(int64(2), days.TotalCount)

	day, err := f.Store.DayContext(t.Context(), store.DayRequest{
		LocalDate: "2026-07-31", Limit: 1, Offset: 50,
		EntryLimit: 1, EntryOffset: 50,
	})
	require.NoError(err)
	assert.Empty(day.Persons)
	assert.Zero(day.PersonTotalCount)
	assert.Empty(day.Entries)
	assert.Equal(int64(1), day.EntryTotalCount)
}

func TestContactStateDerivesCadenceAndDirectChannelAtReadTime(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	last := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{last})
	days := int64(2)
	_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID:       personID,
		DefinitionSlug: store.AttributeSlugContactFrequency,
		Value: store.AttributeValue{
			Type:    store.AttributeValueInteger,
			Integer: &days,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	state, err := f.Store.ContactStateContext(t.Context(), personID, last.AddDate(0, 0, 2))
	require.NoError(err)
	require.NotNil(state.CadenceDueAt)
	assert.Equal(last.AddDate(0, 0, 2), *state.CadenceDueAt)
	assert.Equal(store.CadenceOK, state.CadenceStatus)
	assert.Equal(store.ChannelEmail, state.InferredChannel)

	state, err = f.Store.ContactStateContext(t.Context(), personID, last.AddDate(0, 0, 2).Add(time.Nanosecond))
	require.NoError(err)
	assert.Equal(store.CadenceOverdue, state.CadenceStatus)

	encoded, err := json.Marshal(state)
	require.NoError(err)
	assert.NotContains(string(encoded), "primary_channel")
}

func TestContactStateMarksExistingProjectionStaleWhileQueueIsPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("contact-state-pending-existing").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, messageID, personID, occurredAt),
	})
	require.NoError(err)

	fresh, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.False(fresh.Stale)

	_, err = activityExec(f.Store,
		`UPDATE messages SET subject = ? WHERE id = ?`, "queued change", messageID)
	require.NoError(err)
	pending, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(pending.Stale,
		"a stored contact projection is not fresh while its message is queued")
}

// TestProjectorClassifiesImportedMeetingTranscriptAsMeeting projects an
// import-shaped meeting transcript (message_type meeting_transcript in a
// 'meeting' conversation — the Granola/Circleback/meeting-import shape)
// through the real incremental projector. The shared meeting predicate must
// make classification AND projection-token validation agree that this is a
// meeting reference on the meeting channel with organizer/attendee roles;
// with divergent predicates the authoritative write rejects the batch as
// stale, and with a message-shaped classification the transcript surfaces as
// message:* on channel other with sender/addressed roles.
func TestProjectorClassifiesImportedMeetingTranscriptAsMeeting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := activityProjectionFixture(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	var attendeeParticipantID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`),
		personID).Scan(&attendeeParticipantID))
	organizerID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")

	conversationID, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "meeting-thread", "meeting", "Weekly sync")
	require.NoError(err)
	occurredAt := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	message := storetest.NewMessage(f.Source.ID, conversationID).
		WithSourceMessageID("imported-meeting-transcript").
		WithSentAt(occurredAt).
		Build()
	message.MessageType = "meeting_transcript"
	message.SenderID = sql.NullInt64{Int64: organizerID, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{organizerID}, []string{"Owner"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{attendeeParticipantID}, []string{"Activity Contact"}))

	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	var refKind, channel, direction string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT ref_kind, channel, direction FROM activity_events WHERE message_id = ?`),
		messageID).Scan(&refKind, &channel, &direction))
	assert.Equal(string(store.RefKindMeeting), refKind)
	assert.Equal(string(store.ChannelMeeting), channel)
	assert.Equal(string(store.DirectionOutbound), direction)

	var role string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT role FROM activity_event_persons
		WHERE message_id = ? AND person_id = ?`),
		messageID, personID).Scan(&role))
	assert.Equal(string(store.RoleAttendee), role,
		"the addressed person is a meeting attendee, not an email recipient")
}

// TestRepairedParticipantEmailReprojectsActivity guards the ownership surface
// against maintenance rewrites: activity ownership reads the participant's
// current email, but projection freshness keys on the identity revisions.
// RepairParticipantEmailAddresses (the encoding-repair path) must advance
// them so the INCREMENTAL projector reclassifies affected messages — without
// it a repaired owner address would leave direction, evidence, and contact
// state permanently stale.
func TestRepairedParticipantEmailReprojectsActivity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := activityProjectionFixture(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	var contactParticipantID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id = ?`),
		personID).Scan(&contactParticipantID))

	senderID := f.EnsureParticipant("owner@broken.example.com", "Broken Owner", "example.com")
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().
		WithSourceMessageID("repaired-owner-email").
		WithSentAt(occurredAt).
		Build()
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{senderID}, []string{"Broken Owner"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{contactParticipantID}, []string{"Activity Contact"}))

	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	readDirection := func() string {
		var direction string
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
			`SELECT direction FROM activity_events WHERE message_id = ?`),
			messageID).Scan(&direction))
		return direction
	}
	require.Equal(string(store.DirectionObserved), readDirection(),
		"a mangled owner email matches no identity, so nothing owns the message")

	// The encoding repair rewrites the address and, in the same transaction,
	// settles attribution and advances the revisions the projector keys on.
	require.NoError(f.Store.RepairParticipantEmailAddresses(
		[]store.ParticipantEmailRepair{
			{ParticipantID: senderID, EmailAddress: "owner@example.com"},
		}))

	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	assert.Equal(string(store.DirectionOutbound), readDirection(),
		"the incremental projector must reclassify under the repaired owner email")

	isFromMe, err := f.Store.GetMessageIsFromMe(messageID)
	require.NoError(err)
	assert.True(isFromMe, "persisted attribution must agree with the repaired ownership")

	var directLinks int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*) FROM activity_event_persons
		WHERE message_id = ? AND person_id = ? AND evidence = 'direct'`),
		messageID, personID).Scan(&directLinks))
	assert.Equal(1, directLinks,
		"the addressed person becomes direct contact evidence on the outbound message")
}

// TestRecomputeContactStateLocksAcrossPersonChunks drives contact recompute
// with more person IDs than one lock chunk (500), so the evidence and
// contact-state lock queries run in several chunked statements; an unchunked
// placeholder list would exceed SQLite's bound-variable limit at maximum
// projection batch sizes.
func TestRecomputeContactStateLocksAcrossPersonChunks(t *testing.T) {
	require := require.New(t)
	f, personID := activityProjectionFixture(t)

	const extraPersons = 1200
	values := make([]string, 0, extraPersons)
	args := make([]any, 0, extraPersons)
	for i := range extraPersons {
		values = append(values, "(?)")
		args = append(args, fmt.Sprintf("chunk-lock-person-%d", i))
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO persons (vcard_uid) VALUES `+strings.Join(values, ",")), args...)
	require.NoError(err, "seed persons across several lock chunks")

	personIDs := []int64{personID}
	rows, err := f.Store.DB().Query(
		`SELECT id FROM persons WHERE vcard_uid LIKE 'chunk-lock-person-%'`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		personIDs = append(personIDs, id)
	}
	require.NoError(rows.Err())
	require.Len(personIDs, extraPersons+1)

	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	require.NoError(
		f.Store.RecomputeContactStateContext(t.Context(), personIDs, revisions),
		"recompute must lock and process more persons than one chunk holds")
}

// TestSourceNativeOwnershipScopedToSender pins the counterpart owner rule for
// multi-author messages: source-native ownership belongs to the participant
// messages.sender_id identifies, not to every co-author From row — otherwise
// co-authors are excluded from contact evidence as if they were the owner.
func TestSourceNativeOwnershipScopedToSender(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	author := f.EnsureParticipant("author@example.com", "Author", "example.com")
	coauthor := f.EnsureParticipant("coauthor@example.com", "Coauthor", "example.com")
	coauthorPerson, created, err := f.Store.CreatePersonFromParticipant(coauthor)
	require.NoError(err)
	require.True(created)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "coauthor@example.com", "manual"))
	message := f.NewMessage().
		WithSourceMessageID("source-native-coauthors").
		WithIsFromMe(true).
		WithSentAt(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)).
		Build()
	message.SenderID = sql.NullInt64{Int64: author, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{author, coauthor}, []string{"Author", "Coauthor"}))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE message_recipients
		SET email_address = CASE participant_id
			WHEN ? THEN 'author@example.com'
			WHEN ? THEN 'coauthor@example.com'
		END
		WHERE message_id = ? AND recipient_type = 'from'`),
		author, coauthor, messageID)
	require.NoError(err)

	candidates, err := f.Store.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	require.True(candidates[0].Eligible)
	assert.True(candidates[0].SourceIsFromMe)
	owners := make(map[int64]bool)
	for _, counterpart := range candidates[0].Counterparts {
		if counterpart.RecipientType == "from" {
			owners[counterpart.ParticipantID] = counterpart.IsOwner
		}
	}
	assert.True(owners[author], "the resolved sender carries source-native ownership")
	assert.False(owners[coauthor], "a co-author From row must stay a counterpart")

	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	var projectedRole string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT role
		FROM activity_event_persons
		WHERE message_id = ? AND person_id = ?`),
		messageID, coauthorPerson.ID).Scan(&projectedRole))
	assert.Equal(string(store.RoleSender), projectedRole,
		"the external co-author must remain in projected contact activity")

	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "author@example.com", "manual"))
	envelopeCoauthor := f.EnsureParticipant(
		"envelope-coauthor@example.com", "Envelope Coauthor", "example.com")
	envelopeMessage := f.NewMessage().
		WithSourceMessageID("envelope-native-coauthors").
		WithIsFromMe(true).
		Build()
	envelopeMessage.SenderID = sql.NullInt64{}
	envelopeMessageID, err := f.Store.UpsertMessage(envelopeMessage)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		envelopeMessageID, "from", []int64{author, envelopeCoauthor},
		[]string{"Author", "Envelope Coauthor"}))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE message_recipients
		SET email_address = CASE participant_id
			WHEN ? THEN 'author@example.com'
			WHEN ? THEN 'envelope-coauthor@example.com'
		END
		WHERE message_id = ? AND recipient_type = 'from'`),
		author, envelopeCoauthor, envelopeMessageID)
	require.NoError(err)

	candidates, err = f.Store.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{envelopeMessageID})
	require.NoError(err)
	require.Len(candidates, 1)
	owners = make(map[int64]bool)
	for _, counterpart := range candidates[0].Counterparts {
		if counterpart.RecipientType == "from" {
			owners[counterpart.ParticipantID] = counterpart.IsOwner
		}
	}
	assert.True(owners[author], "the matching envelope participant carries ownership")
	assert.False(owners[envelopeCoauthor],
		"an unmatched envelope co-author must stay a counterpart")
}

// TestAttachActivityCounterpartsChunksLargeBatches loads more candidates than
// one counterpart chunk (512 IDs) so the attach path exercises multiple
// chunked queries; the query repeats its ID list four times, and an unchunked
// maximum batch would exceed SQLite's bound-variable limit.
func TestAttachActivityCounterpartsChunksLargeBatches(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	recipient := f.EnsureParticipant(
		"chunk-recipient@example.com", "Chunk Recipient", "example.com")
	const total = 520
	ids := make([]int64, 0, total)
	for i := range total {
		messageID := f.NewMessage().
			WithSourceMessageID(fmt.Sprintf("chunk-batch-%d", i)).
			Create(t, f.Store)
		require.NoError(f.Store.ReplaceMessageRecipients(
			messageID, "to", []int64{recipient}, []string{"Chunk Recipient"}))
		ids = append(ids, messageID)
	}

	candidates, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), ids)
	require.NoError(err)
	require.Len(candidates, total)
	for _, candidate := range candidates {
		require.NotEmpty(candidate.Counterparts,
			"message %d must have counterparts attached across chunk boundaries",
			candidate.MessageID)
	}
}

// TestActivityCandidatesCarrySourceNativeOwnership pins the candidate columns:
// classification needs the source-native flag because a source-native outgoing
// message can lack a resolved author counterpart entirely.
func TestActivityCandidatesCarrySourceNativeOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	owned := f.NewMessage().
		WithSourceMessageID("candidate-source-native").
		WithIsFromMe(true).
		Create(t, f.Store)
	counterpart := f.NewMessage().
		WithSourceMessageID("candidate-counterpart").
		Create(t, f.Store)

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	byID := make(map[int64]store.ActivityCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.MessageID] = candidate
	}
	assert.True(byID[owned].SourceIsFromMe,
		"source-native ownership must reach classification")
	assert.False(byID[counterpart].SourceIsFromMe)
}

// TestContactStateStaysStaleDuringActivePolicyTransitions guards the freshness
// barrier against partial policy replays: a timezone or direct-counterpart
// transition reprojects the archive in batches, draining the queue between
// batches, so an empty queue alone must not present partially reclassified
// counts as fresh.
func TestContactStateStaysStaleDuringActivePolicyTransitions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("contact-state-policy-transition").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, messageID, personID, occurredAt),
	})
	require.NoError(err)

	fresh, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	require.False(fresh.Stale, "drained queue and reconciled epoch start fresh")

	directLimit, err := f.Store.ClaimActivityDirectLimitTransitionContext(t.Context(), 5)
	require.NoError(err)
	require.True(directLimit.Active)
	midReplay, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(midReplay.Stale,
		"an active direct-counterpart transition must hold the freshness barrier")
	require.NoError(f.Store.CompleteActivityDirectLimitTransitionContext(
		t.Context(), directLimit))

	timezone, err := f.Store.ClaimActivityTimezoneTransitionContext(t.Context(), "UTC")
	require.NoError(err)
	require.True(timezone.Active)
	timezoneReplay, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(timezoneReplay.Stale,
		"an active timezone transition must hold the freshness barrier")
	require.NoError(f.Store.CompleteActivityTimezoneTransitionContext(
		t.Context(), timezone))

	settled, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.False(settled.Stale, "completed transitions release the barrier")
}

func TestContactStateMarksMissingProjectionStaleForPendingSenderBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	var senderID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT participant_id FROM person_participants WHERE person_id = ?
	`), personID).Scan(&senderID))
	message := f.NewMessage().
		WithSourceMessageID("contact-state-pending-missing").
		WithSentAt(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)).
		Build()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: message,
	})
	require.NoError(err)

	pending, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.True(pending.Stale,
		"a missing contact row must stay stale while a new sender binding is queued")

	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, messageID)
	require.NoError(err)
	fresh, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.False(fresh.Stale,
		"without evidence or pending work an empty contact row is fresh")
}

func TestContactStateHandlesMissingProjectionAndMalformedCadence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	last := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{last})
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`DELETE FROM person_contact_state WHERE person_id = ?`), personID)
	require.NoError(err)

	state, err := f.Store.ContactStateContext(t.Context(), personID, last)
	require.NoError(err)
	assert.True(state.Stale)
	assert.Zero(state.InteractionCount)
	assert.Equal(store.ChannelEmail, state.InferredChannel)
	assert.Equal(store.CadenceUnknown, state.CadenceStatus)

	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 2, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunBackstop(t.Context())
	require.NoError(err)

	tooManyDays := int64(math.MaxInt64)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
		Value: store.AttributeValue{
			Type: store.AttributeValueInteger, Integer: &tooManyDays,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	state, err = f.Store.ContactStateContext(t.Context(), personID, last)
	require.NoError(err)
	assert.Nil(state.CadenceDueAt)
	assert.Equal(store.CadenceUnknown, state.CadenceStatus)

	emptyParticipant := f.EnsureParticipant(
		"empty@example.com", "Empty", "example.com")
	emptyPerson, _, err := f.Store.CreatePersonFromParticipant(emptyParticipant)
	require.NoError(err)
	empty, err := f.Store.ContactStateContext(t.Context(), emptyPerson.ID, last)
	require.NoError(err)
	assert.True(empty.Stale,
		"a missing contact row is stale until the activity epoch is reconciled")
	assert.Empty(empty.InferredChannel)
	assert.Zero(empty.InteractionCount)
}

func TestContactStateInfersOnlyDirectEvidenceWithDeterministicChannelTie(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{
		time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
	})
	var ids []int64
	func() {
		rows, queryErr := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(`
			SELECT ae.message_id
			FROM activity_events ae
			JOIN activity_event_persons aep ON aep.message_id = ae.message_id
			WHERE aep.person_id = ?
			ORDER BY ae.occurred_at
		`), personID)
		require.NoError(queryErr)
		defer func() { require.NoError(rows.Close()) }()

		for rows.Next() {
			var id int64
			require.NoError(rows.Scan(&id))
			ids = append(ids, id)
		}
		require.NoError(rows.Err())
	}()
	require.Len(ids, 3)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE activity_events SET channel = 'chat' WHERE message_id = ?`), ids[1])
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE activity_events SET channel = 'meeting' WHERE message_id = ?`), ids[2])
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE activity_event_persons SET evidence = 'co_presence' WHERE message_id = ?
	`), ids[2])
	require.NoError(err)

	state, err := f.Store.ContactStateContext(
		t.Context(), personID, time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.Equal(store.ChannelEmail, state.InferredChannel)
}

func TestContactCadenceAcceptanceMatrixAndNoteSeparation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	last := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{last})
	now := last.AddDate(0, 0, 3)

	absent, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(store.CadenceUnknown, absent.CadenceStatus)
	assert.Nil(absent.CadenceDueAt)

	channel := "chat"
	_, err = f.Store.SetPersonAttributeValueContext(
		t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
	require.NoError(err)

	twoDays := int64(2)
	first, err := f.Store.SetPersonAttributeValueContext(
		t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
			Value: store.AttributeValue{
				Type: store.AttributeValueInteger, Integer: &twoDays,
			},
			Source: store.ProvenanceUser,
		})
	require.NoError(err)
	overdue, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(store.CadenceOverdue, overdue.CadenceStatus)
	require.NotNil(overdue.CadenceDueAt)
	assert.Equal(last.AddDate(0, 0, 2), *overdue.CadenceDueAt)

	fourDays := int64(4)
	second, err := f.Store.SetPersonAttributeValueContext(
		t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
			Value: store.AttributeValue{
				Type: store.AttributeValueInteger, Integer: &fourDays,
			},
			Source:          store.ProvenanceUser,
			ExpectedValueID: &first.Value.ID,
		})
	require.NoError(err)
	changed, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(store.CadenceOK, changed.CadenceStatus)
	require.NotNil(changed.CadenceDueAt)
	assert.Equal(last.AddDate(0, 0, 4), *changed.CadenceDueAt)
	assert.Equal(overdue.InteractionCount, changed.InteractionCount)

	beforeNote := changed
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "Authored note", Author: "Test User",
		PersonIDs: []int64{personID},
	})
	require.NoError(err)
	afterNote, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(beforeNote.InteractionCount, afterNote.InteractionCount)
	personDay, err := f.Store.PersonDayContext(t.Context(), store.PersonDayRequest{
		PersonID: personID, LocalDate: "2026-07-30",
	})
	require.NoError(err)
	assert.Len(personDay.Activity, 1)
	assert.Len(personDay.Entries, 1)

	primaryValues, err := f.Store.ListPersonAttributeValuesContext(
		t.Context(), personID, store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugPrimaryChannel,
		})
	require.NoError(err)
	require.Len(primaryValues, 1)
	assert.Equal("chat", *primaryValues[0].Value.Text)
	encoded, err := json.Marshal(afterNote)
	require.NoError(err)
	assert.NotContains(string(encoded), "primary_channel")

	_, err = f.Store.SupersedePersonAttributeValueContext(
		t.Context(), store.PersonAttributeSupersedeInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
			ExpectedValueID: &second.Value.ID,
		})
	require.NoError(err)
	historicalOnly, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(store.CadenceUnknown, historicalOnly.CadenceStatus)

	var definitionID int64
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT id FROM attribute_definitions
		WHERE object_type = 'person' AND slug = ?
	`), store.AttributeSlugContactFrequency).Scan(&definitionID)
	require.NoError(err)
	insertCorruptAttribute := func(ordinal int64, column string, value any) int64 {
		t.Helper()
		var id int64
		err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(fmt.Sprintf(`
			INSERT INTO person_attribute_values (
				person_id, definition_id, ordinal, %s, source
			) VALUES (?, ?, ?, ?, 'user')
			RETURNING id
		`, column)), personID, definitionID, ordinal, value).Scan(&id)
		require.NoError(err)
		return id
	}
	deleteValue := func(id int64) {
		t.Helper()
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`DELETE FROM person_attribute_values WHERE id = ?`), id)
		require.NoError(err)
	}

	malformedID := insertCorruptAttribute(0, "value_text", "not-an-integer")
	malformed, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(store.CadenceUnknown, malformed.CadenceStatus)
	deleteValue(malformedID)

	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		malformedIntegerID := insertCorruptAttribute(
			0, "value_integer", "not-an-integer")
		malformedInteger, err := f.Store.ContactStateContext(
			t.Context(), personID, now)
		require.NoError(err)
		assert.Equal(store.CadenceUnknown, malformedInteger.CadenceStatus)
		assert.Nil(malformedInteger.CadenceDueAt)
		deleteValue(malformedIntegerID)
	}

	for _, invalidDays := range []int64{0, -1} {
		id := insertCorruptAttribute(0, "value_integer", invalidDays)
		invalid, err := f.Store.ContactStateContext(t.Context(), personID, now)
		require.NoError(err)
		assert.Equal(store.CadenceUnknown, invalid.CadenceStatus)
		assert.Nil(invalid.CadenceDueAt)
		deleteValue(id)
	}

	firstCurrent := insertCorruptAttribute(0, "value_integer", int64(2))
	secondCurrent := insertCorruptAttribute(1, "value_integer", int64(3))
	multiple, err := f.Store.ContactStateContext(t.Context(), personID, now)
	require.NoError(err)
	assert.Equal(store.CadenceUnknown, multiple.CadenceStatus)
	deleteValue(firstCurrent)
	deleteValue(secondCurrent)

	noActivityParticipant := f.EnsureParticipant(
		"no-activity@example.com", "No Activity", "example.com")
	noActivityPerson, _, err := f.Store.CreatePersonFromParticipant(noActivityParticipant)
	require.NoError(err)
	_, err = f.Store.SetPersonAttributeValueContext(
		t.Context(), store.PersonAttributeValueInput{
			PersonID:       noActivityPerson.ID,
			DefinitionSlug: store.AttributeSlugContactFrequency,
			Value: store.AttributeValue{
				Type: store.AttributeValueInteger, Integer: &twoDays,
			},
			Source: store.ProvenanceUser,
		})
	require.NoError(err)
	noLast, err := f.Store.ContactStateContext(t.Context(), noActivityPerson.ID, now)
	require.NoError(err)
	assert.Equal(store.CadenceUnknown, noLast.CadenceStatus)
	assert.Nil(noLast.CadenceDueAt)
}

func TestContactCadenceRejectsDueDateOutsideJSONRange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	last := time.Date(9999, 12, 31, 10, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{last})
	oneDay := int64(1)
	_, err := f.Store.SetPersonAttributeValueContext(
		t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
			Value: store.AttributeValue{
				Type: store.AttributeValueInteger, Integer: &oneDay,
			},
			Source: store.ProvenanceUser,
		})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, last)
	require.NoError(err)
	assert.Equal(store.CadenceUnknown, state.CadenceStatus)
	assert.Nil(state.CadenceDueAt)
	_, err = json.Marshal(state)
	require.NoError(err)
}

func newActivityQueryFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(
		f.Source.ID, "owner@example.com", "manual"))
	counterpart := f.EnsureParticipant(
		"counterpart@example.com", "Counterpart", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(counterpart)
	require.NoError(t, err)
	require.True(t, created)
	return f, person.ID
}

func seedProjectedActivity(
	t *testing.T,
	f *storetest.Fixture,
	personID int64,
	occurred []time.Time,
) {
	t.Helper()
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT participant_id FROM person_participants WHERE person_id = ?
	`), personID).Scan(&counterpart))
	for index, at := range occurred {
		messageID := f.NewMessage().
			WithSourceMessageID(fmt.Sprintf(
				"activity-query-%d-%d-%d", personID, at.Unix(), index)).
			WithSentAt(at).
			Create(t, f.Store)
		require.NoError(t, f.Store.ReplaceMessageRecipients(
			messageID, "from", []int64{counterpart}, []string{"Counterpart"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(
			messageID, "to", []int64{owner}, []string{"Owner"}))
	}
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone:              "UTC",
		BatchSize:             2,
		MaxDirectCounterparts: 25,
	})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
}

func seedProjectedMeetingActivity(
	t *testing.T,
	f *storetest.Fixture,
	personID int64,
	occurredAt time.Time,
) int64 {
	t.Helper()
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT participant_id FROM person_participants WHERE person_id = ?
	`), personID).Scan(&counterpart))
	message := f.NewMessage().
		WithSourceMessageID(fmt.Sprintf(
			"activity-meeting-%d-%d", personID, occurredAt.Unix())).
		WithSentAt(occurredAt).
		Build()
	message.MessageType = "calendar_event"
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{counterpart}, []string{"Counterpart"}))
	require.NoError(t, f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{owner}, []string{"Owner"}))
	require.NoError(t, f.Store.SetMessageMetadata(
		messageID, sql.NullString{String: `{"all_day":true}`, Valid: true}))
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone:              "UTC",
		BatchSize:             2,
		MaxDirectCounterparts: 25,
	})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
	return messageID
}
