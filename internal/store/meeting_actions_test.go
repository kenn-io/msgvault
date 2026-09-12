package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

func TestMeetingActionsCoverageFiltersAndPaging(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)

	first, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Limit: 2})
	requirements.NoError(err)
	assertions.Equal(int64(4), first.TotalCount)
	assertions.Equal(meetingcontent.ActionCoverage{
		MeetingCount: 6,
		Available:    2,
		Partial:      1,
		Unsupported:  1,
		Unavailable:  2,
	}, first.Coverage)
	requirements.Len(first.Rows, 2)
	assertions.Equal(fixture.meetingIDs[2], first.Rows[0].Meeting.MessageID)
	assertions.Equal(0, first.Rows[0].Action.Ordinal)
	assertions.Equal(fixture.meetingIDs[0], first.Rows[1].Meeting.MessageID)
	assertions.Equal(0, first.Rows[1].Action.Ordinal)
	assertions.NotEmpty(first.NextCursor)

	second, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Limit: 2, Cursor: first.NextCursor,
	})
	requirements.NoError(err)
	assertions.Equal(first.TotalCount, second.TotalCount)
	assertions.Equal(first.Coverage, second.Coverage)
	requirements.Len(second.Rows, 2)
	assertions.Equal(fixture.meetingIDs[0], second.Rows[0].Meeting.MessageID)
	assertions.Equal(1, second.Rows[0].Action.Ordinal)
	assertions.Equal(fixture.undatedID, second.Rows[1].Meeting.MessageID)
	assertions.Nil(second.Rows[1].Meeting.OccurredAt)
	assertions.Empty(second.NextCursor)
	assertions.Equal("direct", second.Scope.Kind)
	assertions.NotEmpty(second.ArchiveUID)
}

func TestMeetingActionsFiltersAreExactAndCoveragePrecedesThem(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)

	assignee, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		AssigneeEmail: "alias@EXAMPLE.TEST",
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), assignee.TotalCount)
	assertions.Equal(int64(6), assignee.Coverage.MeetingCount)
	for _, row := range assignee.Rows {
		assertions.Equal("alias@example.test", strings.ToLower(row.Action.AssigneeEmail))
	}

	completed, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Status: meetingcontent.StatusCompleted,
	})
	requirements.NoError(err)
	requirements.Len(completed.Rows, 1)
	assertions.Equal("Review_under_score", completed.Rows[0].Action.Title)

	percent, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Query: "100%"})
	requirements.NoError(err)
	requirements.Len(percent.Rows, 1)
	assertions.Equal("Send 100% plan", percent.Rows[0].Action.Title)
	wildcards, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Query: "%_ literal"})
	requirements.NoError(err)
	requirements.Len(wildcards.Rows, 1)
	assertions.Equal("Wildcard %_ literal", wildcards.Rows[0].Action.Title)

	nameOnly, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		AssigneeEmail: "Name only",
	})
	requirements.NoError(err)
	assertions.Empty(nameOnly.Rows)
	assertions.Zero(nameOnly.TotalCount)
}

func TestMeetingActionsIntersectsMeetingAndActionFilters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	after := time.Date(2026, time.January, 6, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Scope: MeetingQueryScope{
			SourceIDs:      []int64{fixture.sourceOne},
			ParticipantIDs: []int64{fixture.attendee},
			Domains:        []string{"EXAMPLE.TEST"},
			After:          &after,
			Before:         &before,
			Deletion:       "active",
		},
		AssigneeEmail: "ALIAS@example.test",
		Status:        meetingcontent.StatusPending,
		Query:         "%_ literal",
	})
	requirements.NoError(err)
	requirements.Len(page.Rows, 1)
	assertions.Equal(fixture.meetingIDs[2], page.Rows[0].Meeting.MessageID)
	assertions.Equal(int64(2), page.Coverage.MeetingCount)
}

func TestMeetingActionsCursorRejectsChangedInputsAndArchive(t *testing.T) {
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Limit: 1})
	requirements.NoError(err)
	requirements.NotEmpty(page.NextCursor)

	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Limit: 1, Cursor: page.NextCursor, Status: meetingcontent.StatusPending,
	})
	requirements.ErrorIs(err, ErrMeetingInvalidCursor)
	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Limit: 1, Cursor: "not-base64",
	})
	requirements.ErrorIs(err, ErrMeetingInvalidCursor)
	tooLarge := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 2049)))
	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Cursor: tooLarge})
	requirements.ErrorIs(err, ErrMeetingInvalidCursor)

	originalUID := page.ArchiveUID
	_, err = fixture.store.db.Exec(`UPDATE archive_metadata SET value = ? WHERE key = ?`, strings.Repeat("a", 64), archiveUIDKey)
	requirements.NoError(err)
	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Limit: 1, Cursor: page.NextCursor,
	})
	requirements.ErrorIs(err, ErrMeetingInvalidCursor)
	_, restoreErr := fixture.store.db.Exec(`UPDATE archive_metadata SET value = ? WHERE key = ?`, originalUID, archiveUIDKey)
	requirements.NoError(restoreErr)
}

func TestMeetingActionsNextPageReadsCurrentEvidence(t *testing.T) {
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Limit: 2})
	requirements.NoError(err)
	requirements.NotEmpty(page.NextCursor)
	requirements.NoError(fixture.store.UpsertMessageRawWithFormat(fixture.meetingIDs[0], []byte(
		`{"summary_text":"Changed source snapshot","started_at":"2026-01-05T09:00:00Z","ended_at":"2026-01-05T09:30:00Z","action_items":[{"title":"Send 100% plan","status":"open"},{"title":"Changed between pages","status":"done"}]}`), "meeting_json"))

	next, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Limit: 2, Cursor: page.NextCursor,
	})
	requirements.NoError(err)
	requirements.NotEmpty(next.Rows)
	assert.Equal(t, "Changed between pages", next.Rows[0].Action.Title)
}

func TestMeetingActionsRejectsCursorWithMissingPositionFields(t *testing.T) {
	fixture := newMeetingQueryFixture(t)
	uid, err := fixture.store.ArchiveUID()
	require.NoError(t, err)
	malformed := base64.RawURLEncoding.EncodeToString([]byte(`{"version":1,"archive_uid":"` + uid + `","filter_hash":"` + strings.Repeat("0", 64) + `"}`))
	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Cursor: malformed})
	require.ErrorIs(t, err, ErrMeetingInvalidCursor)
}

func TestMeetingActionsCursorCanonicalizesScopePopulation(t *testing.T) {
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	firstIDs := []int64{fixture.meetingIDs[0], fixture.meetingIDs[2], fixture.meetingIDs[0]}
	first, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &firstIDs}, Limit: 1,
	})
	requirements.NoError(err)
	requirements.NotEmpty(first.NextCursor)
	secondIDs := []int64{fixture.meetingIDs[2], fixture.meetingIDs[0]}
	second, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &secondIDs}, Limit: 1, Cursor: first.NextCursor,
	})
	requirements.NoError(err)
	requirements.Len(second.Rows, 1)

	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &secondIDs, Authority: "changed"},
		Limit: 1, Cursor: first.NextCursor,
	})
	requirements.ErrorIs(err, ErrMeetingInvalidCursor)
	empty := []int64{}
	_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &empty}, Limit: 1, Cursor: first.NextCursor,
	})
	requirements.ErrorIs(err, ErrMeetingInvalidCursor)
}

func TestMeetingActionsCursorDistinguishesUnrestrictedFromPresentEmptyScopes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	var nilIDs []int64
	emptyIDs := []int64{}

	unrestrictedHash, err := meetingActionsFilterHash(MeetingActionsQuery{})
	requirements.NoError(err)
	nilIDsHash, err := meetingActionsFilterHash(MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &nilIDs},
	})
	requirements.NoError(err)
	emptyIDsHash, err := meetingActionsFilterHash(MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &emptyIDs},
	})
	requirements.NoError(err)
	assertions.NotEqual(unrestrictedHash, nilIDsHash)
	assertions.Equal(nilIDsHash, emptyIDsHash)

	page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Limit: 1})
	requirements.NoError(err)
	requirements.NotEmpty(page.NextCursor)
	for _, ids := range []*[]int64{&nilIDs, &emptyIDs} {
		_, err = fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
			Scope: MeetingQueryScope{MessageIDs: ids}, Limit: 1, Cursor: page.NextCursor,
		})
		requirements.ErrorIs(err, ErrMeetingInvalidCursor)
	}
}

func TestMeetingActionsExplicitEmptyScopeMatchesNone(t *testing.T) {
	assertions := assert.New(t)
	fixture := newMeetingQueryFixture(t)
	empty := []int64{}
	page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{
		Scope: MeetingQueryScope{MessageIDs: &empty},
	})
	require.NoError(t, err)
	assertions.Empty(page.Rows)
	assertions.Zero(page.TotalCount)
	assertions.Equal(meetingcontent.ActionCoverage{}, page.Coverage)
}

func TestMeetingActionsCursorPreservesFullSignedMessageIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	ids := []int64{math.MinInt64, -1, 0}
	for _, id := range ids {
		insertMeetingQueryRowWithID(t, fixture, id)
	}
	_, err := fixture.store.db.Exec(`
		INSERT INTO meeting_action_items
			(message_id, ordinal, title, status, origin, locator)
		VALUES (?, 1, 'Minimum second action', 'pending', 'structured', 'action_items[1]')`, math.MinInt64)
	requirements.NoError(err)

	query := MeetingActionsQuery{Scope: MeetingQueryScope{MessageIDs: &ids}, Limit: 1}
	want := []struct {
		messageID int64
		ordinal   int
	}{
		{0, 0}, {-1, 0}, {math.MinInt64, 0}, {math.MinInt64, 1},
	}
	for index, expected := range want {
		page, pageErr := fixture.store.ListMeetingActionsContext(t.Context(), query)
		requirements.NoError(pageErr)
		requirements.Len(page.Rows, 1)
		assertions.Equal(expected.messageID, page.Rows[0].Meeting.MessageID)
		assertions.Equal(expected.ordinal, page.Rows[0].Action.Ordinal)
		if index < len(want)-1 {
			requirements.NotEmpty(page.NextCursor)
			query.Cursor = page.NextCursor
		} else {
			assertions.Empty(page.NextCursor)
		}
	}

	result, err := fixture.store.GetMeetingContextContext(t.Context(), ids,
		meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20})
	requirements.NoError(err)
	var packet meetingcontent.Packet
	requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
	assertions.Equal(ids, packet.RequestedMessageIDs)
}

func insertMeetingQueryRowWithID(t *testing.T, fixture *meetingQueryFixture, id int64) {
	t.Helper()
	insert := `INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type, subject)
		VALUES (?, (SELECT MIN(id) FROM conversations), ?, ?, 'meeting_transcript', ?)`
	if fixture.store.IsPostgreSQL() {
		insert = `INSERT INTO messages
			(id, conversation_id, source_id, source_message_id, message_type, subject)
			OVERRIDING SYSTEM VALUE
			VALUES (?, (SELECT MIN(id) FROM conversations), ?, ?, 'meeting_transcript', ?)`
	}
	_, err := fixture.store.db.Exec(insert, id, fixture.sourceOne, fmt.Sprintf("signed-%d", id), fmt.Sprintf("Signed %d", id))
	require.NoError(t, err)
	_, err = fixture.store.db.Exec(`
		INSERT INTO meeting_details
			(message_id, projection_version, content_hash, content_json, action_coverage, transcript_state)
		VALUES (?, 1, 'synthetic', '{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"unavailable"},"actions":[],"action_coverage":"available","duration_seconds":null}', 'available', 'unavailable')`, id)
	require.NoError(t, err)
	_, err = fixture.store.db.Exec(`
		INSERT INTO meeting_action_items
			(message_id, ordinal, title, status, origin, locator)
		VALUES (?, 0, ?, 'pending', 'structured', 'action_items[0]')`, id, fmt.Sprintf("Signed %d action", id))
	require.NoError(t, err)
}

// Opposing IDs expose rounded timestamp order; page size one also exercises
// continuation inside exact ties and across the submillisecond boundary.
func TestMeetingActionsExactInstantsAndBounds(t *testing.T) {
	fixture := newMeetingQueryFixture(t)
	older := time.Date(2026, time.January, 1, 0, 0, 0, 100000, time.UTC)
	newer := older.Add(100 * time.Microsecond)
	zeroBound := time.Time{}
	ids := []int64{-10, -20, -30, -40}
	for _, id := range ids {
		insertMeetingQueryRowWithID(t, fixture, id)
	}
	for _, item := range []struct {
		id int64
		at time.Time
	}{
		{-10, older}, {-20, newer}, {-30, newer.In(time.FixedZone("offset", 2*3600))},
	} {
		_, err := fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, item.at, item.id)
		require.NoError(t, err)
	}
	_, err := fixture.store.db.Exec(`INSERT INTO meeting_action_items (message_id, ordinal, title, status, origin, locator)
 VALUES (-20, 1, 'Second tied action', 'pending', 'structured', 'action_items[1]')`)
	require.NoError(t, err)
	t.Run("ordered continuation", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		query := MeetingActionsQuery{Scope: MeetingQueryScope{MessageIDs: &ids}, Limit: 1}
		want := []struct {
			id      int64
			ordinal int
		}{{-20, 0}, {-20, 1}, {-30, 0}, {-10, 0}, {-40, 0}}
		for index, expected := range want {
			page, err := fixture.store.ListMeetingActionsContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(page.Rows, 1)
			assertions.Equal(expected.id, page.Rows[0].Meeting.MessageID)
			assertions.Equal(expected.ordinal, page.Rows[0].Action.Ordinal)
			assertions.Equal(int64(5), page.TotalCount)
			assertions.Equal(int64(4), page.Coverage.MeetingCount)
			if index < len(want)-1 {
				requirements.NotEmpty(page.NextCursor)
			} else {
				assertions.Empty(page.NextCursor)
			}
			query.Cursor = page.NextCursor
		}
	})
	for _, tc := range []struct {
		name              string
		after, before     *time.Time
		meetings, actions int64
		first, last       *time.Time
	}{
		{"inclusive after", &newer, nil, 2, 3, &newer, &newer},
		{"zero after bound", &zeroBound, nil, 3, 4, &older, &newer},
		{"exclusive before", nil, &newer, 1, 1, &older, &older},
		{"exact interval", &older, &newer, 1, 1, &older, &older},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			scope := MeetingQueryScope{MessageIDs: &ids, After: tc.after, Before: tc.before}
			page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Scope: scope})
			requirements.NoError(err)
			assertions.Equal(tc.actions, page.TotalCount)
			assertions.Equal(tc.meetings, page.Coverage.MeetingCount)
			metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), scope)
			requirements.NoError(err)
			assertions.Equal(tc.meetings, metrics.Totals.MeetingCount)
			assertions.Equal(tc.first, metrics.FirstMeetingAt)
			assertions.Equal(tc.last, metrics.LastMeetingAt)
		})
	}
}

func TestMeetingActionsWideInstants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	ids := []int64{-10, -20}
	earlier := time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2300, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index, instant := range []time.Time{earlier, later} {
		insertMeetingQueryRowWithID(t, fixture, ids[index])
		_, err := fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, instant, ids[index])
		requirements.NoError(err)
	}

	scope := MeetingQueryScope{MessageIDs: &ids}
	query := MeetingActionsQuery{Scope: scope, Limit: 1}
	for _, id := range []int64{-20, -10} {
		page, err := fixture.store.ListMeetingActionsContext(t.Context(), query)
		requirements.NoError(err)
		requirements.Len(page.Rows, 1)
		assertions.Equal(id, page.Rows[0].Meeting.MessageID)
		query.Cursor = page.NextCursor
	}
	assertions.Empty(query.Cursor)
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), scope)
	requirements.NoError(err)
	assertions.Equal(&earlier, metrics.FirstMeetingAt)
	assertions.Equal(&later, metrics.LastMeetingAt)

	scope.After = new(time.Date(1700, time.January, 1, 0, 0, 0, 0, time.UTC))
	scope.Before = new(time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC))
	page, err := fixture.store.ListMeetingActionsContext(t.Context(), MeetingActionsQuery{Scope: scope})
	requirements.NoError(err)
	requirements.Len(page.Rows, 1)
	assertions.Equal(int64(-20), page.Rows[0].Meeting.MessageID)
	metrics, err = fixture.store.GetMeetingMetricsContext(t.Context(), scope)
	requirements.NoError(err)
	assertions.Equal(int64(1), metrics.Totals.MeetingCount)
	assertions.Equal(&later, metrics.FirstMeetingAt)
	assertions.Equal(&later, metrics.LastMeetingAt)
}
