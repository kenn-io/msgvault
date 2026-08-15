package activity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestProjectorRunOnceDrainsQueueAndBuildsContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, personID := projectorFixture(
		t, time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(f.Store, Options{
		Timezone:              "UTC",
		BatchSize:             1,
		MaxDirectCounterparts: 25,
	})
	require.NoError(err)

	result, err := projector.RunOnce(t.Context())
	require.NoError(err)
	assert.Positive(result.Processed)
	assert.Positive(result.EventsWritten)

	queued, err := f.Store.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	assert.Empty(queued)
	state, err := f.Store.ContactStateContext(
		t.Context(), personID, time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.Equal("message:"+formatProjectorID(messageID), state.LastContactRef)
	assert.False(state.Stale)
}

func TestProjectorIncludesChannelMembersWhenFromRecipientExists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	conversationID, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "activity-channel", "channel", "Activity channel")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	sender := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	senderPerson, _, err := f.Store.CreatePersonFromParticipant(sender)
	require.NoError(err)
	memberPerson, _, err := f.Store.CreatePersonFromParticipant(member)
	require.NoError(err)
	for _, participantID := range []int64{owner, sender, member} {
		require.NoError(f.Store.EnsureConversationParticipant(
			conversationID, participantID, "member"))
	}
	message := f.NewMessage().
		WithSourceMessageID("activity-channel-from-member").
		WithSentAt(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)).
		Build()
	message.ConversationID = conversationID
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{sender}, []string{"Sender"}))

	projector, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	rows, err := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, role, evidence
		FROM activity_event_persons
		WHERE message_id = ?
		ORDER BY person_id
	`), messageID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	type personLink struct {
		personID int64
		role     string
		evidence string
	}
	var got []personLink
	for rows.Next() {
		var link personLink
		require.NoError(rows.Scan(&link.personID, &link.role, &link.evidence))
		got = append(got, link)
	}
	require.NoError(rows.Err())
	assert.Equal([]personLink{
		{personID: senderPerson.ID, role: "sender", evidence: "direct"},
		{personID: memberPerson.ID, role: "member", evidence: "co_presence"},
	}, got)
}

func TestProjectorReconcilesChangedDirectCounterpartLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	first := f.EnsureParticipant("first@example.com", "First", "example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	_, _, err := f.Store.CreatePersonFromParticipant(first)
	require.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(second)
	require.NoError(err)
	messageID := f.NewMessage().
		WithSourceMessageID("activity-direct-limit-change").
		WithSentAt(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{owner}, []string{"Owner"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{first, second}, []string{"First", "Second"}))

	initial, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 1, MaxDirectCounterparts: 1,
	})
	require.NoError(err)
	_, err = initial.RunOnce(t.Context())
	require.NoError(err)
	assert.Equal(int64(2), activityEvidenceCount(
		t, f.Store, messageID, store.EvidenceCoPresence))

	changed, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 1, MaxDirectCounterparts: 2,
	})
	require.NoError(err)
	result, err := changed.RunOnce(t.Context())
	require.NoError(err)
	assert.Positive(result.EventsWritten)
	assert.Equal(int64(2), activityEvidenceCount(
		t, f.Store, messageID, store.EvidenceDirect))
	transition, err := f.Store.ActivityDirectLimitTransitionContext(t.Context())
	require.NoError(err)
	assert.False(transition.Active)
	assert.Equal(2, transition.Target)
	assert.Equal(int64(2), transition.Generation)
}

func activityEvidenceCount(
	t *testing.T,
	st *store.Store,
	messageID int64,
	evidence store.ActivityEvidence,
) int64 {
	t.Helper()
	var count int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM activity_event_persons
		WHERE message_id = ? AND evidence = ?
	`), messageID, evidence).Scan(&count))
	return count
}

func TestProjectorHelpsCrashedTimezoneTransitionBeforeConfiguredTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, _ := projectorFixture(
		t, time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC), false)
	claimed, err := f.Store.ClaimActivityTimezoneTransitionContext(
		t.Context(), "America/New_York")
	require.NoError(err)
	require.True(claimed.Active)

	projector, err := NewProjector(f.Store, Options{
		Timezone:  "Pacific/Kiritimati",
		BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	transition, err := f.Store.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.False(transition.Active)
	assert.Equal("Pacific/Kiritimati", transition.Target)
	assert.Greater(transition.Generation, claimed.Generation)

	var timezone, localDate string
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT timezone, local_date FROM activity_events WHERE message_id = ?
	`), messageID).Scan(&timezone, &localDate)
	require.NoError(err)
	assert.Equal("Pacific/Kiritimati", timezone)
	assert.Equal("2026-08-01", localDate)
}

func TestProjectorBackstopReconstructsClearedDerivedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, _ := projectorFixture(
		t, time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`DELETE FROM activity_events WHERE message_id = ?`), messageID)
	require.NoError(err)

	result, err := projector.RunBackstop(t.Context())
	require.NoError(err)
	assert.Positive(result.EventsWritten)
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`),
		messageID).Scan(&count))
	assert.Equal(1, count)
}

func TestProjectorBackstopRestoresMissingContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _, personID := projectorFixture(
		t, time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`DELETE FROM person_contact_state WHERE person_id = ?`), personID)
	require.NoError(err)

	_, err = projector.RunBackstop(t.Context())
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, time.Now())
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
	assert.False(state.Stale)
}

func TestProjectorBackstopRepairsCorruptFreshContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _, personID := projectorFixture(
		t, time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE person_contact_state
		SET interaction_count = 99, first_contact_at = NULL,
		    first_contact_message_id = NULL
		WHERE person_id = ?
	`), personID)
	require.NoError(err)

	_, err = projector.RunBackstop(t.Context())
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, time.Now())
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
	assert.NotNil(state.FirstContactAt)
	assert.False(state.Stale)
}

func TestProjectorRunOnceProcessesSevenMessagesAtBatchSizeThree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, lastMessageID := projectorManyFixture(t, 7)
	projector, err := NewProjector(f.Store, Options{
		Timezone:  "UTC",
		BatchSize: 3,
	})
	require.NoError(err)

	result, err := projector.RunOnce(t.Context())
	require.NoError(err)
	assert.Equal(7, result.EventsProjected)
	assert.Equal(7, result.EventsWritten)
	assert.Equal(7, result.Processed)
	assert.GreaterOrEqual(result.Batches, 3)
	assert.Equal(lastMessageID, result.Watermark)

	resumed, err := projector.RunOnce(t.Context())
	require.NoError(err)
	assert.Zero(resumed.EventsProjected)
	assert.Equal(lastMessageID, resumed.Watermark)
}

func TestProjectorBoundsStaleTokenReloads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _, _ := projectorFixture(
		t, time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC), false)
	stale := &alwaysStaleProjectorStore{Store: f.Store}
	projector, err := NewProjector(stale, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)

	_, err = projector.RunOnce(t.Context())
	var staleErr *store.ErrActivityProjectionStale
	require.ErrorAs(err, &staleErr)
	assert.Contains(err.Error(), "projection stayed stale after 3 attempts")
	assert.Equal(int64(projectorStaleRetries), stale.calls.Load())
	assert.Equal(int64(projectorStaleRetries-1), stale.loads.Load())
}

func TestProjectorReloadsAndReclassifiesChangedStaleCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, _ := projectorFixture(
		t, time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC), false)
	initial, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = initial.RunOnce(t.Context())
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE messages SET metadata = ? WHERE id = ?
	`), activityTouch("queued reclassification"), messageID)
	require.NoError(err)

	reclassifying := &reclassifyingStaleProjectorStore{
		Store: f.Store, messageID: messageID,
	}
	projector, err := NewProjector(reclassifying, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	assert.Equal(int64(2), reclassifying.calls.Load())
	assert.Equal(int64(1), reclassifying.loads.Load())
	var eventCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT COUNT(*) FROM activity_events WHERE message_id = ?
	`), messageID).Scan(&eventCount))
	assert.Zero(eventCount,
		"the exact reload must reclassify the newly deleted message as a retraction")
	pending, err := f.Store.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	assert.Empty(pending)
}

func TestProjectorProcessesQueuedRepairBelowWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, watermark := projectorManyFixture(t, 3)
	projector, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 2,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	var messageID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT MIN(message_id) FROM activity_events`).Scan(&messageID))
	require.Less(messageID, watermark)
	repaired := time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE messages SET sent_at = ? WHERE id = ?
	`), repaired, messageID)
	require.NoError(err)

	result, err := projector.RunOnce(t.Context())
	require.NoError(err)
	assert.Positive(result.Processed)
	assert.Equal(watermark, result.Watermark)
	pending, err := f.Store.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	assert.Empty(pending)
	var occurredAt time.Time
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT occurred_at FROM activity_events WHERE message_id = ?
	`), messageID).Scan(&occurredAt))
	assert.True(repaired.Equal(occurredAt.UTC()))
}

func TestProjectorRepairQueueFailureCannotClearOldContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, personID := projectorFixture(
		t, time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	repaired := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE messages SET sent_at = ? WHERE id = ?
	`), repaired, messageID)
	require.NoError(err)
	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))

	injected := errors.New("injected projection failure")
	failing, err := NewProjector(
		&failingActivityProjectorStore{Store: f.Store, err: injected},
		Options{Timezone: "UTC", BatchSize: 1},
	)
	require.NoError(err)
	_, err = failing.RunOnce(t.Context())
	require.ErrorIs(err, injected)

	state, err := f.Store.ContactStateContext(t.Context(), personID, time.Now())
	require.NoError(err)
	assert.True(state.Stale,
		"queued repair must fail before stale cleanup can bless the old date")
	require.NotNil(state.LastContactAt)
	assert.NotEqual(repaired, state.LastContactAt.UTC())
}

func TestProjectorPostDrainRepairFailureCannotPublishFreshOldFacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, personID := projectorFixture(
		t, time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))

	injected := errors.New("injected post-drain projection failure")
	racing := &postDrainRepairFailureStore{
		Store:     f.Store,
		messageID: messageID,
		repaired:  time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC),
		failure:   injected,
	}
	failing, err := NewProjector(racing, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = failing.RunOnce(t.Context())
	require.ErrorIs(err, injected)

	state, err := f.Store.ContactStateContext(t.Context(), personID, time.Now())
	require.NoError(err)
	assert.True(state.Stale,
		"a queued repair cannot leave old evidence marked fresh when its drain fails")
	require.NotNil(state.LastContactAt)
	assert.NotEqual(racing.repaired, state.LastContactAt.UTC())
}

func TestProjectorRestartsWhenAnotherWorkerChangesTimezoneGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, _ := projectorFixture(
		t, time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC), false)
	_, err := f.Store.ClaimActivityTimezoneTransitionContext(
		t.Context(), "America/New_York")
	require.NoError(err)

	racing := &timezoneRacingProjectorStore{Store: f.Store}
	projector, err := NewProjector(racing, Options{
		Timezone: "America/New_York", BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	transition, err := f.Store.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.False(transition.Active)
	assert.Equal("America/New_York", transition.Target)
	assert.GreaterOrEqual(transition.Generation, int64(3))
	var timezone string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT timezone FROM activity_events WHERE message_id = ?`),
		messageID).Scan(&timezone))
	assert.Equal("America/New_York", timezone)
}

func TestProjectorTimezoneChangeKeepsDayPrecisionCalendarStable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	occurredAt := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	f, messageID, _ := projectorFixture(t, occurredAt, true)
	utc, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = utc.RunOnce(t.Context())
	require.NoError(err)
	kiritimati, err := NewProjector(f.Store, Options{
		Timezone: "Pacific/Kiritimati", BatchSize: 1,
	})
	require.NoError(err)
	_, err = kiritimati.RunOnce(t.Context())
	require.NoError(err)

	var timezone, localDate, precision string
	var offset int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT timezone, local_date, date_precision, utc_offset_minutes
		FROM activity_events WHERE message_id = ?
	`), messageID).Scan(&timezone, &localDate, &precision, &offset))
	assert.Equal("UTC", timezone)
	assert.Equal("2026-07-31", localDate)
	assert.Equal("day", precision)
	assert.Zero(offset)
}

func TestProjectorMigratesLegacyCompletedTimezoneWithoutGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	occurredAt := time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC)
	f, messageID, _ := projectorFixture(t, occurredAt, false)
	utc, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = utc.RunOnce(t.Context())
	require.NoError(err)

	_, err = f.Store.DB().ExecContext(t.Context(), `
		UPDATE archive_metadata
		SET value = 'America/New_York'
		WHERE key = 'activity_spine_timezone'
	`)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `
		DELETE FROM archive_metadata
		WHERE key = 'activity_spine_timezone_generation'
	`)
	require.NoError(err)

	newYork, err := NewProjector(f.Store, Options{
		Timezone: "America/New_York", BatchSize: 1,
	})
	require.NoError(err)
	_, err = newYork.RunOnce(t.Context())
	require.NoError(err)

	var timezone, localDate string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT timezone, local_date
		FROM activity_events
		WHERE message_id = ?
	`), messageID).Scan(&timezone, &localDate))
	assert.Equal("America/New_York", timezone)
	assert.Equal("2026-07-31", localDate)
	transition, err := f.Store.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.False(transition.Active)
	assert.Equal("America/New_York", transition.Target)
	assert.Positive(transition.Generation)
}

func TestConcurrentDifferentTimezoneWorkersRejectStaleGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	primary, secondary := newConcurrentActivityStores(t)
	source, err := primary.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err)
	conversationID, err := primary.EnsureConversation(
		source.ID, "timezone-race", "Timezone Race")
	require.NoError(err)
	f := &storetest.Fixture{
		T: t, Store: primary, Source: source, ConvID: conversationID,
	}
	_, messageID, _ := seedProjectorFixture(
		t, f, time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC), false)
	utc, err := NewProjector(primary, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = utc.RunOnce(t.Context())
	require.NoError(err)
	candidates, err := primary.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	staleUTC, err := utc.projections(
		candidates, time.UTC, utc.maxDirectCounterparts)
	require.NoError(err)
	_, err = secondary.ClaimActivityTimezoneTransitionContext(
		t.Context(), "America/New_York")
	require.NoError(err)
	newYork, err := NewProjector(secondary, Options{
		Timezone: "America/New_York", BatchSize: 1,
	})
	require.NoError(err)

	start := make(chan struct{})
	staleResult := make(chan error, 1)
	transitionResult := make(chan error, 1)
	go func() {
		<-start
		_, projectErr := primary.ProjectActivityBatchContext(
			t.Context(), staleUTC)
		staleResult <- projectErr
	}()
	go func() {
		<-start
		_, projectErr := newYork.RunOnce(t.Context())
		transitionResult <- projectErr
	}()
	close(start)

	require.NoError(<-transitionResult)
	var staleErr *store.ErrActivityProjectionStale
	require.ErrorAs(<-staleResult, &staleErr)
	transition, err := primary.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.False(transition.Active)
	assert.Equal("America/New_York", transition.Target)
	var timezone, localDate string
	require.NoError(primary.DB().QueryRowContext(t.Context(), primary.Rebind(`
		SELECT timezone, local_date
		FROM activity_events
		WHERE message_id = ?
	`), messageID).Scan(&timezone, &localDate))
	assert.Equal("America/New_York", timezone)
	assert.Equal("2026-07-31", localDate)
}

func TestProjectorRestartAfterPartialTimezonePassDoesNotDoubleCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _ := projectorManyFixture(t, 3)
	injected := errors.New("injected crash")
	crashing := &failNthActivityProjectorStore{
		Store: f.Store, failAt: 2, err: injected,
	}
	projector, err := NewProjector(crashing, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.ErrorIs(err, injected)

	transition, err := f.Store.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.True(transition.Active,
		"a partial pass must leave completion metadata unchanged")

	resumed, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = resumed.RunOnce(t.Context())
	require.NoError(err)

	var interactionCount int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `
		SELECT interaction_count FROM person_contact_state
	`).Scan(&interactionCount))
	assert.Equal(int64(3), interactionCount)
	transition, err = f.Store.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.False(transition.Active)
	assert.Equal("UTC", transition.Target)
}

func TestProjectorMutationDuringCommitLeavesAndDrainsHigherGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, messageID, _ := projectorFixture(
		t, time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC), false)
	projector, err := NewProjector(
		&mutatingAfterProjectionStore{Store: f.Store, messageID: messageID},
		Options{Timezone: "UTC", BatchSize: 1},
	)
	require.NoError(err)
	result, err := projector.RunOnce(t.Context())
	require.NoError(err)
	assert.GreaterOrEqual(result.Processed, 2)
	pending, err := f.Store.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	assert.Empty(pending)
}

func TestProjectorIdentityChangeMidPassRestartsAtNewEpoch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _ := projectorManyFixture(t, 3)
	initial, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 1})
	require.NoError(err)
	_, err = initial.RunOnce(t.Context())
	require.NoError(err)

	participant := f.EnsureParticipant(
		"epoch-bump@example.com", "Epoch Bump", "example.com")
	_, created, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	require.True(created)
	racing := &identityRacingProjectorStore{Store: f.Store}
	projector, err := NewProjector(racing, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	current, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	reconciled, ok, err := f.Store.ActivityReconciledRevisionsContext(t.Context())
	require.NoError(err)
	assert.True(ok)
	assert.Equal(current, reconciled)
	var mismatched int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT COUNT(*) FROM activity_events
		WHERE projected_identity_revision <> ?
		   OR projected_account_identity_revision <> ?
	`), current.IdentityRevision, current.AccountIdentityRevision).Scan(&mismatched))
	assert.Zero(mismatched)
}

func TestProjectorAccountRevisionChangeDuringStaleRecomputeConverges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _ := projectorManyFixture(t, 3)
	initial, err := NewProjector(f.Store, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = initial.RunOnce(t.Context())
	require.NoError(err)
	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))

	racing := &recomputeRevisionRacingStore{
		Store: f.Store, sourceID: f.Source.ID,
	}
	projector, err := NewProjector(racing, Options{
		Timezone: "UTC", BatchSize: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	current, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	reconciled, ok, err := f.Store.ActivityReconciledRevisionsContext(t.Context())
	require.NoError(err)
	assert.True(ok)
	assert.Equal(current, reconciled)
	stale, err := f.Store.StaleContactStatePersonsContext(t.Context(), 10)
	require.NoError(err)
	assert.Empty(stale)
	assert.LessOrEqual(racing.calls.Load(), int64(3),
		"revision restart must not spin on the same stale contact batch")
}

func TestProjectorBackstopMatchesReverseExactProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _ := projectorManyFixture(t, 5)
	projector, err := NewProjector(f.Store, Options{Timezone: "UTC", BatchSize: 2})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	candidates, err := f.Store.ScanAllActivityCandidatesContext(t.Context(), 0, 10)
	require.NoError(err)
	ids := make([]int64, len(candidates))
	for index := range candidates {
		ids[index] = candidates[len(candidates)-1-index].MessageID
	}
	require.NoError(clearProjectorDerivedState(t.Context(), f.Store))
	for _, messageID := range ids {
		_, err = projector.ProjectMessages(t.Context(), []int64{messageID})
		require.NoError(err)
	}
	var personID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT person_id FROM person_contact_state`).Scan(&personID))
	incremental, err := f.Store.ContactStateContext(
		t.Context(), personID, time.Now())
	require.NoError(err)
	incrementalRefs := projectedActivityRefs(t, f.Store)

	require.NoError(clearProjectorDerivedState(t.Context(), f.Store))
	_, err = projector.RunBackstop(t.Context())
	require.NoError(err)
	backfilled, err := f.Store.ContactStateContext(
		t.Context(), personID, time.Now())
	require.NoError(err)
	incremental.ComputedAt = backfilled.ComputedAt
	assert.Equal(incremental, backfilled)
	assert.Equal(incrementalRefs, projectedActivityRefs(t, f.Store))
}

type failingActivityProjectorStore struct {
	*store.Store

	err error
}

type alwaysStaleProjectorStore struct {
	*store.Store

	calls atomic.Int64
	loads atomic.Int64
}

func (s *alwaysStaleProjectorStore) ProjectActivityBatchContext(
	context.Context,
	[]store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	s.calls.Add(1)
	return store.ActivityProjectionResult{}, &store.ErrActivityProjectionStale{
		Reason: "injected stale token",
	}
}

func (s *alwaysStaleProjectorStore) LoadActivityCandidatesByIDContext(
	ctx context.Context,
	messageIDs []int64,
) ([]store.ActivityCandidate, error) {
	s.loads.Add(1)
	return s.Store.LoadActivityCandidatesByIDContext(ctx, messageIDs)
}

// activityTouch renders a metadata payload that requeues a message without
// changing its derived event; see the store package helper of the same name.
func activityTouch(marker string) string {
	return `{"touch":` + strconv.Quote(marker) + `}`
}

type reclassifyingStaleProjectorStore struct {
	*store.Store

	messageID int64
	calls     atomic.Int64
	loads     atomic.Int64
	once      sync.Once
	err       error
}

func (s *reclassifyingStaleProjectorStore) ProjectActivityBatchContext(
	ctx context.Context,
	items []store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	s.calls.Add(1)
	first := false
	s.once.Do(func() {
		first = true
		_, s.err = s.Store.DB().ExecContext(ctx, s.Rebind(`
			UPDATE messages
			SET deleted_from_source_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`), s.messageID)
	})
	if s.err != nil {
		return store.ActivityProjectionResult{}, s.err
	}
	if first {
		return store.ActivityProjectionResult{},
			&store.ErrActivityProjectionStale{
				MessageID: s.messageID,
				Reason:    "candidate changed before commit",
			}
	}
	return s.Store.ProjectActivityBatchContext(ctx, items)
}

func (s *reclassifyingStaleProjectorStore) LoadActivityCandidatesByIDContext(
	ctx context.Context,
	messageIDs []int64,
) ([]store.ActivityCandidate, error) {
	s.loads.Add(1)
	return s.Store.LoadActivityCandidatesByIDContext(ctx, messageIDs)
}

type postDrainRepairFailureStore struct {
	*store.Store

	messageID int64
	repaired  time.Time
	once      sync.Once
	hookErr   error
	failure   error
}

func (r *postDrainRepairFailureStore) StaleContactStatePersonsContext(
	ctx context.Context,
	limit int,
) ([]int64, error) {
	personIDs, err := r.Store.StaleContactStatePersonsContext(ctx, limit)
	if err != nil {
		return nil, err
	}
	r.once.Do(func() {
		_, r.hookErr = r.Store.DB().ExecContext(ctx, r.Rebind(`
			UPDATE messages SET sent_at = ? WHERE id = ?
		`), r.repaired, r.messageID)
		if r.hookErr == nil {
			r.hookErr = r.MarkAllContactStateDirtyContext(ctx)
		}
	})
	return personIDs, r.hookErr
}

func (r *postDrainRepairFailureStore) ProjectActivityBatchContext(
	context.Context,
	[]store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	return store.ActivityProjectionResult{}, r.failure
}

func (f *failingActivityProjectorStore) ProjectActivityBatchContext(
	context.Context,
	[]store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	return store.ActivityProjectionResult{}, f.err
}

type timezoneRacingProjectorStore struct {
	*store.Store

	once sync.Once
	err  error
}

type failNthActivityProjectorStore struct {
	*store.Store

	calls  atomic.Int64
	failAt int64
	err    error
}

func (f *failNthActivityProjectorStore) ProjectActivityBatchContext(
	ctx context.Context,
	items []store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	if f.calls.Add(1) == f.failAt {
		return store.ActivityProjectionResult{}, f.err
	}
	return f.Store.ProjectActivityBatchContext(ctx, items)
}

type mutatingAfterProjectionStore struct {
	*store.Store

	messageID int64
	once      sync.Once
	err       error
}

func (m *mutatingAfterProjectionStore) ProjectActivityBatchContext(
	ctx context.Context,
	items []store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	result, err := m.Store.ProjectActivityBatchContext(ctx, items)
	if err != nil {
		return result, err
	}
	m.once.Do(func() {
		_, m.err = m.Store.DB().ExecContext(ctx, m.Rebind(`
			UPDATE messages SET metadata = '{"touch":"changed while projecting"}'
			WHERE id = ?
		`), m.messageID)
	})
	return result, m.err
}

type identityRacingProjectorStore struct {
	*store.Store

	once sync.Once
	err  error
}

type recomputeRevisionRacingStore struct {
	*store.Store

	sourceID int64
	once     sync.Once
	calls    atomic.Int64
	err      error
}

func (r *recomputeRevisionRacingStore) StaleContactStatePersonsContext(
	ctx context.Context,
	limit int,
) ([]int64, error) {
	r.calls.Add(1)
	personIDs, err := r.Store.StaleContactStatePersonsContext(ctx, limit)
	if err != nil || len(personIDs) == 0 {
		return personIDs, err
	}
	r.once.Do(func() {
		r.err = r.AddAccountIdentity(
			r.sourceID, "recompute-epoch-owner@example.com", "manual")
	})
	return personIDs, r.err
}

func (r *identityRacingProjectorStore) ProjectActivityBatchContext(
	ctx context.Context,
	items []store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	r.once.Do(func() {
		r.err = r.AddAccountIdentity(
			items[0].Token.SourceID, "epoch-race-owner@example.com", "manual")
	})
	if r.err != nil {
		return store.ActivityProjectionResult{}, r.err
	}
	return r.Store.ProjectActivityBatchContext(ctx, items)
}

func clearProjectorDerivedState(ctx context.Context, st *store.Store) error {
	for _, statement := range []string{
		`DELETE FROM person_contact_state`,
		`DELETE FROM activity_event_persons`,
		`DELETE FROM activity_events`,
	} {
		if _, err := st.DB().ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func projectedActivityRefs(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), `
		SELECT ref_kind, message_id
		FROM activity_events
		ORDER BY ref_kind, message_id
	`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var refs []string
	for rows.Next() {
		var kind string
		var messageID int64
		require.NoError(t, rows.Scan(&kind, &messageID))
		refs = append(refs, kind+":"+formatProjectorID(messageID))
	}
	require.NoError(t, rows.Err())
	return refs
}

func (r *timezoneRacingProjectorStore) ProjectActivityBatchContext(
	ctx context.Context,
	items []store.ActivityProjection,
) (store.ActivityProjectionResult, error) {
	r.once.Do(func() {
		helper, err := NewProjector(r.Store, Options{
			Timezone: "America/New_York", BatchSize: 1,
		})
		if err != nil {
			r.err = err
			return
		}
		if _, err := helper.RunOnce(ctx); err != nil {
			r.err = err
			return
		}
		_, r.err = r.ClaimActivityTimezoneTransitionContext(
			ctx, "Pacific/Kiritimati")
	})
	if r.err != nil {
		return store.ActivityProjectionResult{}, r.err
	}
	return r.Store.ProjectActivityBatchContext(ctx, items)
}

func projectorFixture(
	t *testing.T,
	occurredAt time.Time,
	dateOnly bool,
) (*storetest.Fixture, int64, int64) {
	t.Helper()
	f := storetest.New(t)
	return seedProjectorFixture(t, f, occurredAt, dateOnly)
}

func seedProjectorFixture(
	t *testing.T,
	f *storetest.Fixture,
	occurredAt time.Time,
	dateOnly bool,
) (*storetest.Fixture, int64, int64) {
	t.Helper()
	require.NoError(t, f.Store.AddAccountIdentity(
		f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	counterpart := f.EnsureParticipant(
		"counterpart@example.com", "Counterpart", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(counterpart)
	require.NoError(t, err)
	require.True(t, created)
	message := f.NewMessage().
		WithSourceMessageID("projector-fixture").
		WithSentAt(occurredAt).
		Build()
	if dateOnly {
		message.MessageType = "calendar_event"
	}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{counterpart}, []string{"Counterpart"}))
	require.NoError(t, f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{owner}, []string{"Owner"}))
	if dateOnly {
		require.NoError(t, f.Store.SetMessageMetadata(messageID,
			sql.NullString{String: `{"all_day":true}`, Valid: true}))
	}
	return f, messageID, person.ID
}

var concurrentActivityStoreSequence atomic.Uint64

func newConcurrentActivityStores(t *testing.T) (*store.Store, *store.Store) {
	t.Helper()
	databaseURL := os.Getenv("MSGVAULT_TEST_DB")
	var first, second *store.Store
	if strings.HasPrefix(databaseURL, "postgres://") ||
		strings.HasPrefix(databaseURL, "postgresql://") {
		schema := fmt.Sprintf(
			"msgvault_activity_concurrency_%d_%d",
			time.Now().UnixNano(),
			concurrentActivityStoreSequence.Add(1),
		)
		setup, err := sql.Open("pgx", databaseURL)
		require.NoError(t, err)
		_, err = setup.ExecContext(t.Context(), "CREATE SCHEMA "+schema)
		require.NoError(t, err)
		require.NoError(t, setup.Close())
		separator := "?"
		if strings.Contains(databaseURL, "?") {
			separator = "&"
		}
		testURL := databaseURL + separator + "search_path=" + schema
		first, err = store.Open(testURL)
		require.NoError(t, err)
		second, err = store.Open(testURL)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = first.Close()
			_ = second.Close()
			cleanup, cleanupErr := sql.Open("pgx", databaseURL)
			if cleanupErr == nil {
				_, _ = cleanup.Exec("DROP SCHEMA " + schema + " CASCADE")
				_ = cleanup.Close()
			}
		})
	} else {
		databasePath := filepath.Join(t.TempDir(), "activity-concurrency.db")
		var err error
		first, err = store.OpenForTest(databasePath)
		require.NoError(t, err)
		second, err = store.OpenForTest(databasePath)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = first.Close()
			_ = second.Close()
		})
	}
	require.NoError(t, first.InitSchema())
	return first, second
}

func projectorManyFixture(
	t *testing.T,
	count int,
) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(
		f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	counterpart := f.EnsureParticipant(
		"counterpart@example.com", "Counterpart", "example.com")
	_, _, err := f.Store.CreatePersonFromParticipant(counterpart)
	require.NoError(t, err)
	var last int64
	for index := range count {
		last = f.NewMessage().
			WithSourceMessageID(fmt.Sprintf("projector-many-%d", index)).
			WithSentAt(time.Date(
				2026, 7, 31, 0, index, 0, 0, time.UTC)).
			Create(t, f.Store)
		require.NoError(t, f.Store.ReplaceMessageRecipients(
			last, "from", []int64{counterpart}, []string{"Counterpart"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(
			last, "to", []int64{owner}, []string{"Owner"}))
	}
	return f, last
}

func formatProjectorID(value int64) string {
	return strconv.FormatInt(value, 10)
}
