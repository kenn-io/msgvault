package query

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelationshipsRanksByReciprocityAndGatesNewsletters builds an owner
// (O), a reciprocal counterpart (A, clustered with a second identity A2 that
// only has chat activity), and an inbound-only newsletter sender (B). It
// asserts: A ranks first with cluster-combined signals under
// canonical_id = min(A, A2); B is gated out by default; ShowAll lifts the
// gate; and the result is deterministic given an injected Now.
func TestRelationshipsRanksByReciprocityAndGatesNewsletters(t *testing.T) {
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	aID := b.AddParticipant("alice@example.com", "example.com", "Alice")
	a2ID := b.AddParticipant("alice@chat.example", "chat.example", "Alice Chat")
	b.LinkCluster(aID, a2ID)

	bID := b.AddParticipant("newsletter@example.com", "example.com", "Newsletter")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	// Owner sent 3 messages to A.
	for i := range 3 {
		msgID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true, SentAt: now.AddDate(0, 0, -(3 - i))})
		b.AddFrom(msgID, ownerID, "Owner")
		b.AddTo(msgID, aID, "Alice")
	}

	// One meeting together (owner + A).
	meetingID := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "calendar_event", SentAt: now.AddDate(0, 0, -2)})
	b.AddFrom(meetingID, ownerID, "Owner")
	b.AddTo(meetingID, aID, "Alice")

	// A2 (clustered with A) has chat activity with the owner.
	chatID := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "imessage", SentAt: now.AddDate(0, 0, -1)})
	b.AddFrom(chatID, a2ID, "Alice Chat")
	b.AddTo(chatID, ownerID, "Owner")

	// Newsletter: 50 inbound messages, zero sent/meetings.
	for i := range 50 {
		msgID := b.AddMessage(MessageOpt{SourceID: srcID, SentAt: now.AddDate(0, 0, -(10 + i))})
		b.AddFrom(msgID, bID, "Newsletter")
		b.AddTo(msgID, ownerID, "Owner")
	}

	engine := b.BuildEngine()
	ctx := context.Background()

	t.Run("default gate excludes the newsletter", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		result, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10})
		require.NoError(err)
		require.Len(result.Rows, 1)
		assert.Equal(int64(1), result.TotalCount)

		row := result.Rows[0]
		assert.Equal(aID, row.CanonicalID)
		assert.Equal([]int64{aID, a2ID}, row.MemberIDs)
		assert.Equal(int64(3), row.Signals.SentCount)
		assert.Equal(int64(1), row.Signals.MeetingCount)
		assert.Equal(3, row.Signals.Modalities) // email + meeting + chat
		assert.Positive(row.Score)
		assert.WithinDuration(now.AddDate(0, 0, -1), row.LastAt, 0)
	})

	t.Run("show_all lifts the gate and includes the newsletter", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		result, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
		require.NoError(err)
		require.Len(result.Rows, 2)
		assert.Equal(int64(2), result.TotalCount)

		byCanonicalID := make(map[int64]RelationshipRow, len(result.Rows))
		for _, row := range result.Rows {
			byCanonicalID[row.CanonicalID] = row
		}
		require.Contains(byCanonicalID, aID)
		require.Contains(byCanonicalID, bID)
		newsletter := byCanonicalID[bID]
		assert.Equal(int64(0), newsletter.Signals.SentCount)
		assert.Equal(int64(0), newsletter.Signals.MeetingCount)
		assert.Positive(newsletter.Signals.ReceivedFromThem)
	})

	t.Run("deterministic given an injected Now", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		first, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
		require.NoError(err)
		second, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
		require.NoError(err)
		require.Len(second.Rows, len(first.Rows))
		for i := range first.Rows {
			assert.Equal(first.Rows[i].CanonicalID, second.Rows[i].CanonicalID)
			// DuckDB's parallel SUM can reorder floating-point additions between
			// runs, so scores may differ in the last bit; the ranking they
			// produce must not.
			assert.InEpsilon(first.Rows[i].Score, second.Rows[i].Score, 1e-9)
		}
	})
}

// TestRelationshipsMemoizesRankedListPerRevision verifies the engine-level
// memoization contract: within one committed cache revision, repeated calls
// (including later same-UTC-day Now values and offset pagination) reuse one
// ranking query; ShowAll keys separately; and any revision change — here an
// identity-revision bump rewritten into the commit marker — forces a
// recompute, so a stale list can never be served after identities change.
func TestRelationshipsMemoizesRankedListPerRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)
	bobID := b.AddParticipant("bob@example.com", "example.com", "Bob")
	carolID := b.AddParticipant("carol@example.com", "example.com", "Carol")

	now := time.Date(2026, 1, 10, 3, 0, 0, 0, time.UTC)
	for _, recipient := range []struct {
		id   int64
		name string
	}{{bobID, "Bob"}, {carolID, "Carol"}} {
		msgID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true, SentAt: now.AddDate(0, 0, -1)})
		b.AddFrom(msgID, ownerID, "Owner")
		b.AddTo(msgID, recipient.id, recipient.name)
	}

	engine := b.BuildEngine()
	ctx := context.Background()

	first, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10})
	require.NoError(err)
	require.Len(first.Rows, 2)
	require.Equal(uint64(1), engine.relationshipsQueryRuns.Load())

	second, err := engine.Relationships(ctx, RelationshipsRequest{Now: now.Add(6 * time.Hour), Limit: 10})
	require.NoError(err)
	assert.Equal(uint64(1), engine.relationshipsQueryRuns.Load(),
		"a same-revision, same-UTC-day repeat must not re-run the ranking query")
	require.Len(second.Rows, 2)
	assert.Equal(first.Rows[0].CanonicalID, second.Rows[0].CanonicalID)

	page, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 1, Offset: 1})
	require.NoError(err)
	assert.Equal(uint64(1), engine.relationshipsQueryRuns.Load(),
		"offset pages must slice the cached list, not re-query")
	require.Len(page.Rows, 1)
	assert.Equal(first.Rows[1].CanonicalID, page.Rows[0].CanonicalID)
	assert.Equal(first.TotalCount, page.TotalCount)

	_, err = engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	assert.Equal(uint64(2), engine.relationshipsQueryRuns.Load(), "show_all must key separately")

	state, err := ReadCacheSyncState(engine.analyticsDir)
	require.NoError(err)
	state.IdentityRevision++
	stateData, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(os.WriteFile(CacheStatePath(engine.analyticsDir), stateData, 0o600))

	third, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10})
	require.NoError(err)
	assert.Equal(uint64(3), engine.relationshipsQueryRuns.Load(),
		"a revision bump must miss the memo and recompute")
	assert.Equal(state.IdentityRevision, third.IdentityRevision)
}

// TestRelationshipsExcludesClusteredOwners verifies that when the owner's own
// participant identity is itself linked into a cluster with another
// participant, the whole cluster is excluded from ranking (you never rank
// yourself, even under an alias).
func TestRelationshipsExcludesClusteredOwners(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	ownerAliasID := b.AddParticipant("owner@alias.example", "alias.example", "Owner Alias")
	b.AddOwnerParticipant(srcID, ownerID)
	b.LinkCluster(ownerID, ownerAliasID)

	otherID := b.AddParticipant("bob@example.com", "example.com", "Bob")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	msgID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true, SentAt: now.AddDate(0, 0, -1)})
	b.AddFrom(msgID, ownerID, "Owner")
	b.AddTo(msgID, otherID, "Bob")

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(result.Rows, 1)
	assert.Equal(otherID, result.Rows[0].CanonicalID)
}

// TestRelationshipsDoesNotDoubleCountClusteredRecipientsInOneEntry verifies
// that a single entry whose recipient list contains two raw participant IDs
// resolving to the same canonical cluster (e.g. cc'ing someone's work and
// personal addresses that the archive owner has linked) is counted once for
// that cluster, not once per raw ID. The clustered scenario is compared
// against a control scenario with a single, unclustered recipient sent at
// the same instant: the two must produce identical decayed sums, whatever
// the DuckDB session's date-diff decay for that instant happens to be.
func TestRelationshipsDoesNotDoubleCountClusteredRecipientsInOneEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	sentAt := now.AddDate(0, 0, -1)

	control := NewTestDataBuilder(t)
	controlSrcID := control.AddSource("owner@example.com")
	controlOwnerID := control.AddParticipant("owner@example.com", "example.com", "Owner")
	control.AddOwnerParticipant(controlSrcID, controlOwnerID)
	controlAID := control.AddParticipant("alice@work.example", "work.example", "Alice Work")
	controlMsgID := control.AddMessage(MessageOpt{SourceID: controlSrcID, IsFromMe: true, SentAt: sentAt})
	control.AddFrom(controlMsgID, controlOwnerID, "Owner")
	control.AddTo(controlMsgID, controlAID, "Alice Work")

	controlEngine := control.BuildEngine()
	controlResult, err := controlEngine.Relationships(context.Background(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(controlResult.Rows, 1)
	require.Equal(int64(1), controlResult.Rows[0].Signals.SentCount)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	aID := b.AddParticipant("alice@work.example", "work.example", "Alice Work")
	a2ID := b.AddParticipant("alice@home.example", "home.example", "Alice Home")
	b.LinkCluster(aID, a2ID)

	msgID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true, SentAt: sentAt})
	b.AddFrom(msgID, ownerID, "Owner")
	b.AddTo(msgID, aID, "Alice Work")
	b.AddCc(msgID, a2ID, "Alice Home")

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(result.Rows, 1)

	row := result.Rows[0]
	assert.Equal(aID, row.CanonicalID)
	assert.Equal(int64(1), row.Signals.SentCount, "one message must count once, not once per linked recipient")
	assert.InEpsilon(controlResult.Rows[0].Signals.SentToThem, row.Signals.SentToThem, 1e-9,
		"a clustered duplicate recipient must not inflate the decayed sum beyond one message's decay")
}

// TestRelationshipsWithOwnerResolvesAlias verifies that a meeting attended
// under an owner alias linked only via participant_clusters (not itself an
// owner_participants row) still counts as "together" for the other
// attendee, consistent with how owner-cluster exclusion already resolves
// aliases through canon.
func TestRelationshipsWithOwnerResolvesAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	ownerAliasID := b.AddParticipant("owner@alias.example", "alias.example", "Owner Alias")
	b.AddOwnerParticipant(srcID, ownerID)
	b.LinkCluster(ownerID, ownerAliasID)

	otherID := b.AddParticipant("x@example.com", "example.com", "X")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	meetingID := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "calendar_event", SentAt: now.AddDate(0, 0, -1)})
	b.AddFrom(meetingID, ownerAliasID, "Owner Alias")
	b.AddTo(meetingID, otherID, "X")

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(result.Rows, 1)
	assert.Equal(otherID, result.Rows[0].CanonicalID)
	assert.Equal(int64(1), result.Rows[0].Signals.MeetingCount)
}

// TestRelationshipsOwnerAbsentMeetingContributesNoModality verifies that a
// meeting/event entry the archive owner did not attend contributes no
// signal at all: no modality (rather than being miscounted as a phantom
// "email" contact), no meeting count, and — since it is excluded from
// "interactions" entirely — no contribution to LastAt/LastInteractionAt.
//
// X has both a real signal (an email from the owner, earlier) and an
// owner-absent meeting with Y (later): X's row must reflect only the email
// in LastAt, not the later meeting. Y has nothing but the owner-absent
// meeting, so Y must not appear in the results at all — a meeting the owner
// never attended is not evidence of any relationship with the owner.
func TestRelationshipsOwnerAbsentMeetingContributesNoModality(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	xID := b.AddParticipant("x@example.com", "example.com", "X")
	yID := b.AddParticipant("y@example.com", "example.com", "Y")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	emailID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true, SentAt: now.AddDate(0, 0, -5)})
	b.AddFrom(emailID, ownerID, "Owner")
	b.AddTo(emailID, xID, "X")

	meetingID := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "calendar_event", SentAt: now.AddDate(0, 0, -1)})
	b.AddFrom(meetingID, xID, "X")
	b.AddTo(meetingID, yID, "Y")

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(result.Rows, 1, "Y has no signal at all and must not appear")

	row := result.Rows[0]
	assert.Equal(xID, row.CanonicalID)
	assert.Equal(1, row.Signals.Modalities,
		"only the email counts; the owner-absent meeting must not contribute a second, phantom modality")
	assert.Equal(int64(0), row.Signals.MeetingCount)
	assert.WithinDuration(now.AddDate(0, 0, -5), row.LastAt, 0,
		"LastAt must reflect the email, not the later owner-absent meeting")
}
