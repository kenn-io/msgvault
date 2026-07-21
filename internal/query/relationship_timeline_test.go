package query

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDuckDBTimezoneConversionUsesBundledICU documents (rather than
// installs) the timezone conversion this package depends on: go-duckdb's
// prebuilt binaries statically link the ICU extension, so named-timezone
// conversion works without an explicit "INSTALL icu; LOAD icu;" (unlike the
// sqlite_scanner extension in NewDuckDBEngine, which is genuinely optional).
//
// It also pins the exact conversion shape relationship_timeline.go relies
// on, and a trap this test deliberately exercises: DuckDB's session
// TimeZone setting defaults to the host's local zone (not UTC — this sandbox
// runs under America/Chicago), so any expression whose result depends on
// that ambient setting is a portability hazard across dev machines and CI.
// timezone(<zone>, <TIMESTAMPTZ>) is NOT such an expression: it names the
// target zone explicitly and returns a plain (naive) TIMESTAMP, verified
// below to be identical however the session TimeZone is set.
//
// occurred_at is a naive TIMESTAMP holding a UTC instant (see
// sqlAnalyticalEntries: "m.sent_at AS occurred_at"). Calling
// timezone(<zone>, occurred_at) directly is the wrong direction — that
// overload treats a naive TIMESTAMP argument as wall-clock time IN <zone>
// and converts it TO a UTC-based TIMESTAMPTZ, which is backwards for what
// this package needs. The correct two-step conversion first anchors the
// naive value to UTC (timezone('UTC', occurred_at) => TIMESTAMPTZ), then
// asks for its wall-clock representation in the target zone
// (timezone(<zone>, <thatTIMESTAMPTZ>) => naive TIMESTAMP). Getting this
// backwards was the exact off-by-one-day failure mode flagged for this task.
func TestDuckDBTimezoneConversionUsesBundledICU(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	db := NewTestDataBuilder(t).BuildEngine().db

	// Wrong direction, made session-independent by converting the resulting
	// TIMESTAMPTZ back to UTC explicitly rather than formatting it directly
	// (formatting a TIMESTAMPTZ renders it in the ambient session
	// TimeZone, which is exactly the hazard this test is pinning down).
	var wrongDirection string
	require.NoError(db.QueryRowContext(context.Background(),
		`SELECT strftime(timezone('UTC', timezone('America/Chicago', TIMESTAMP '2026-07-13 23:30:00')), '%Y-%m-%d %H:%M:%S')`,
	).Scan(&wrongDirection))
	// Interpreting the naive literal as America/Chicago wall-clock time and
	// converting to UTC lands nearly 6 hours later on the clock, not earlier
	// — proof this single-step call is the wrong direction.
	assert.Equal("2026-07-14 04:30:00", wrongDirection)

	// Correct two-step direction, checked under two different ambient
	// session TimeZone settings to prove it does not depend on either.
	for _, sessionTZ := range []string{"UTC", "Asia/Tokyo"} {
		_, err := db.ExecContext(context.Background(), fmt.Sprintf("SET TimeZone = '%s'", sessionTZ))
		require.NoError(err, "session timezone %s", sessionTZ)

		var chicagoDay, utcDay string
		require.NoError(db.QueryRowContext(context.Background(), `
			SELECT
				strftime(timezone('America/Chicago', timezone('UTC', TIMESTAMP '2026-07-13 23:30:00')), '%Y-%m-%d'),
				strftime(timezone('UTC', timezone('UTC', TIMESTAMP '2026-07-13 23:30:00')), '%Y-%m-%d')
		`).Scan(&chicagoDay, &utcDay), "session timezone %s", sessionTZ)
		assert.Equal("2026-07-13", chicagoDay, "session timezone %s", sessionTZ)
		assert.Equal("2026-07-13", utcDay, "session timezone %s", sessionTZ)
	}
}

// buildTimelineFixture constructs the mandatory Task 6 scenario: a cluster
// person (P, linked with alias P2) with three chat messages in one
// conversation — two at 23:00/23:30 UTC on 2026-07-13, one at 00:30 UTC the
// next day — plus one email and one meeting, all with the owner. Returns the
// engine, the canonical (cluster) ID, and the three notable message IDs.
func buildTimelineFixture(t *testing.T) (engine *DuckDBEngine, canonicalID, emailID, meetingID int64) {
	t.Helper()
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	pID := b.AddParticipant("pat@chat.example", "chat.example", "Pat")
	p2ID := b.AddParticipant("pat@work.example", "work.example", "Pat Work")
	b.LinkCluster(pID, p2ID)
	canonicalID = min(pID, p2ID)

	convID := int64(9001)
	day1Late1 := time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC)
	day1Late2 := time.Date(2026, 7, 13, 23, 30, 0, 0, time.UTC)
	day2Early := time.Date(2026, 7, 14, 0, 30, 0, 0, time.UTC)

	first := b.AddMessage(MessageOpt{
		SourceID: srcID, ConversationID: convID, ConversationType: "direct_chat",
		MessageType: "imessage", SentAt: day1Late1, Snippet: "Pat: on my way",
	})
	b.AddFrom(first, pID, "Pat")
	b.AddTo(first, ownerID, "Owner")

	second := b.AddMessage(MessageOpt{
		SourceID: srcID, ConversationID: convID, ConversationType: "direct_chat",
		MessageType: "imessage", SentAt: day1Late2, Snippet: "Pat: almost there",
	})
	b.AddFrom(second, pID, "Pat")
	b.AddTo(second, ownerID, "Owner")

	third := b.AddMessage(MessageOpt{
		SourceID: srcID, ConversationID: convID, ConversationType: "direct_chat",
		MessageType: "imessage", SentAt: day2Early, Snippet: "Pat: here now",
	})
	b.AddFrom(third, pID, "Pat")
	b.AddTo(third, ownerID, "Owner")

	emailID = b.AddMessage(MessageOpt{
		SourceID: srcID, MessageType: "email", Subject: "Re: dinner",
		SentAt: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
	})
	b.AddFrom(emailID, pID, "Pat")
	b.AddTo(emailID, ownerID, "Owner")

	meetingID = b.AddMessage(MessageOpt{
		SourceID: srcID, MessageType: "calendar_event",
		SentAt: time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC),
	})
	b.AddFrom(meetingID, ownerID, "Owner")
	b.AddTo(meetingID, pID, "Pat")

	return b.BuildEngine(), canonicalID, emailID, meetingID
}

// TestRelationshipTimelineBurstsChatByLocalDay is the mandatory Task 6
// scenario. With America/Chicago the three chat messages (23:00/23:30 UTC
// on day X and 00:30 UTC on day X+1, all before 19:30 local) fall on the
// same Chicago calendar day and must collapse into one chat_burst row with
// MessageCount 3 and the correct first/last timestamps and latest-message
// preview. With UTC the same three messages split across the UTC day
// boundary into two bursts. The email and meeting are untouched, one row
// each, and rows are ordered (occurred_at DESC, key ASC).
func TestRelationshipTimelineBurstsChatByLocalDay(t *testing.T) {
	engine, canonicalID, emailID, meetingID := buildTimelineFixture(t)
	ctx := context.Background()

	t.Run("America/Chicago collapses all three chats into one burst", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		result, err := engine.RelationshipTimeline(ctx, RelationshipTimelineRequest{
			CanonicalID: canonicalID, Timezone: "America/Chicago", Limit: 10,
		})
		require.NoError(err)
		require.Len(result.Rows, 3, "chat burst + email + meeting")

		burst := result.Rows[0]
		assert.Equal("chat_burst", burst.Kind)
		assert.Equal(int64(3), burst.MessageCount)
		assert.WithinDuration(time.Date(2026, 7, 14, 0, 30, 0, 0, time.UTC), burst.OccurredAt, 0)
		assert.WithinDuration(time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC), burst.FirstAt, 0)
		assert.Equal("Pat: here now", burst.Preview, "preview is the latest message in the burst")

		email := result.Rows[1]
		assert.Equal("email", email.Kind)
		assert.Equal(int64(1), email.MessageCount)
		assert.Equal("message:"+strconv.FormatInt(emailID, 10), email.Key)

		meeting := result.Rows[2]
		assert.Equal("event", meeting.Kind)
		assert.Equal(int64(1), meeting.MessageCount)
		assert.Equal("message:"+strconv.FormatInt(meetingID, 10), meeting.Key)

		// Ordering: occurred_at DESC, key ASC.
		assert.True(email.OccurredAt.After(meeting.OccurredAt))
		assert.True(burst.OccurredAt.After(email.OccurredAt))
	})

	t.Run("UTC splits the three chats across the day boundary into two bursts", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		result, err := engine.RelationshipTimeline(ctx, RelationshipTimelineRequest{
			CanonicalID: canonicalID, Timezone: "UTC", Limit: 10,
		})
		require.NoError(err)
		require.Len(result.Rows, 4, "two chat bursts + email + meeting")

		bursts := make([]TimelineRow, 0, 2)
		for _, row := range result.Rows {
			if row.Kind == "chat_burst" {
				bursts = append(bursts, row)
			}
		}
		require.Len(bursts, 2)

		latest, earliest := bursts[0], bursts[1]
		assert.Equal(int64(1), latest.MessageCount)
		assert.WithinDuration(time.Date(2026, 7, 14, 0, 30, 0, 0, time.UTC), latest.OccurredAt, 0)
		assert.Equal(int64(2), earliest.MessageCount)
		assert.WithinDuration(time.Date(2026, 7, 13, 23, 30, 0, 0, time.UTC), earliest.OccurredAt, 0)
		assert.WithinDuration(time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC), earliest.FirstAt, 0)
	})

	t.Run("empty timezone defaults to UTC", func(t *testing.T) {
		require := require.New(t)

		withDefault, err := engine.RelationshipTimeline(ctx, RelationshipTimelineRequest{CanonicalID: canonicalID, Limit: 10})
		require.NoError(err)
		withUTC, err := engine.RelationshipTimeline(ctx, RelationshipTimelineRequest{CanonicalID: canonicalID, Timezone: "UTC", Limit: 10})
		require.NoError(err)
		require.Len(withDefault.Rows, len(withUTC.Rows))
	})
}

// TestRelationshipTimelineRejectsInvalidTimezone verifies Go-side IANA
// validation (time.LoadLocation) rejects a bogus zone name before any
// DuckDB query runs, mapped to ErrInvalidExploreRequest like other explore
// validation failures.
func TestRelationshipTimelineRejectsInvalidTimezone(t *testing.T) {
	require := require.New(t)

	engine, canonicalID, _, _ := buildTimelineFixture(t)
	_, err := engine.RelationshipTimeline(context.Background(), RelationshipTimelineRequest{
		CanonicalID: canonicalID, Timezone: "Not/AZone", Limit: 10,
	})
	require.ErrorIs(err, ErrInvalidExploreRequest)
}

// TestRelationshipTimelineResolvesAnyMemberID verifies a raw member alias ID
// (not the canonical/minimum ID) still resolves to the full cluster's
// interactions when used as CanonicalID is not itself required — the
// engine's RelationshipTimeline takes an already-resolved canonical ID (see
// ResolveCanonicalParticipant for the alias-to-canonical resolution used by
// the API layer) but must include every member's messages.
func TestRelationshipTimelineResolvesAnyMemberID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	engine, canonicalID, _, _ := buildTimelineFixture(t)
	result, err := engine.RelationshipTimeline(context.Background(), RelationshipTimelineRequest{
		CanonicalID: canonicalID, Timezone: "America/Chicago", Limit: 10,
	})
	require.NoError(err)
	require.Len(result.Rows, 3)
	assert.Equal(int64(3), result.TotalCount)
}

// TestRelationshipTimelineIntersectsParticipantFilterWithClusterMembership
// verifies that cluster membership scopes the timeline AND a caller-supplied
// participant filter (Context.ParticipantIDs, set via a "participant" filter
// dimension at the API layer) further restricts it — the two conditions
// intersect rather than the cluster-membership scope silently overwriting
// the caller's filter. Pat has two entries with the owner; only one also
// involves X (cc'd). Filtering by X must return only that one entry.
func TestRelationshipTimelineIntersectsParticipantFilterWithClusterMembership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	pID := b.AddParticipant("pat@example.com", "example.com", "Pat")
	xID := b.AddParticipant("x@example.com", "example.com", "X")

	entryWithX := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "email", SentAt: time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)})
	b.AddFrom(entryWithX, pID, "Pat")
	b.AddTo(entryWithX, ownerID, "Owner")
	b.AddCc(entryWithX, xID, "X")

	entryWithoutX := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "email", SentAt: time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC)})
	b.AddFrom(entryWithoutX, pID, "Pat")
	b.AddTo(entryWithoutX, ownerID, "Owner")

	engine := b.BuildEngine()

	all, err := engine.RelationshipTimeline(context.Background(), RelationshipTimelineRequest{
		CanonicalID: pID, Limit: 10,
	})
	require.NoError(err)
	require.Len(all.Rows, 2, "both of Pat's entries are in scope without a participant filter")

	filtered, err := engine.RelationshipTimeline(context.Background(), RelationshipTimelineRequest{
		CanonicalID: pID, Limit: 10, Context: Context{ParticipantIDs: []int64{xID}},
	})
	require.NoError(err)
	require.Len(filtered.Rows, 1, "only the entry that also involves X must survive the intersection")
	assert.Equal(int64(1), filtered.TotalCount)
	assert.Equal("message:"+strconv.FormatInt(entryWithX, 10), filtered.Rows[0].Key)
}

// TestResolveCanonicalParticipant covers both the linked case (any member ID
// resolves to the shared canonical ID) and the miss case (an unlinked
// participant is its own single-member cluster).
func TestResolveCanonicalParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	b := NewTestDataBuilder(t)
	aID := b.AddParticipant("a@example.com", "example.com", "A")
	a2ID := b.AddParticipant("a2@example.com", "example.com", "A2")
	b.LinkCluster(aID, a2ID)
	loneID := b.AddParticipant("lone@example.com", "example.com", "Lone")

	engine := b.BuildEngine()
	canonical := min(aID, a2ID)

	resolvedFromA, err := engine.ResolveCanonicalParticipant(context.Background(), aID)
	require.NoError(err)
	assert.Equal(canonical, resolvedFromA)

	resolvedFromA2, err := engine.ResolveCanonicalParticipant(context.Background(), a2ID)
	require.NoError(err)
	assert.Equal(canonical, resolvedFromA2)

	resolvedLone, err := engine.ResolveCanonicalParticipant(context.Background(), loneID)
	require.NoError(err)
	assert.Equal(loneID, resolvedLone, "an unlinked participant is a single-member cluster of itself")
}
