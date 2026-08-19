package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// decodeJSONObject reads a metadata payload as a plain JSON object.
func decodeJSONObject(t *testing.T, payload string) map[string]any {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &object))
	return object
}

// membershipFixture is one archived message whose conversation carries both an
// observed participant count and a provider membership record.
type membershipFixture struct {
	sourceID       int64
	conversationID int64
	messageID      int64
}

// newMembershipFixture archives a message in a channel conversation under
// sourceType, reporting observed participants and the given metadata JSON
// (empty leaves the column NULL).
func newMembershipFixture(
	t *testing.T, st *store.Store, sourceType string, observed int, metadata string,
) membershipFixture {
	t.Helper()
	require := require.New(t)

	source, err := st.GetOrCreateSource(sourceType, sourceType+"-account")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, sourceType+"-conversation", "channel", "Membership")
	require.NoError(err)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`),
		observed, conversationID)
	require.NoError(err)
	if metadata != "" {
		require.NoError(st.SetConversationMetadata(conversationID,
			sql.NullString{String: metadata, Valid: true}))
	}
	return membershipFixture{
		sourceID:       source.ID,
		conversationID: conversationID,
		messageID: insertStoreTestMessage(
			t, st, source.ID, conversationID, sourceType+"-message"),
	}
}

// TestAttachmentConversationAppliesProviderMembershipRecord pins the two
// membership semantics media policy evaluates. Discord stages a catalog count
// that may only raise the observed one. Teams and Slack archive the exact
// roster they read, which must win in both directions: participant rows only
// ever accumulate, so a conversation whose membership shrank below the
// threshold would otherwise stay excluded forever.
func TestAttachmentConversationAppliesProviderMembershipRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sourceType string
		observed   int
		metadata   string
		want       int
	}{
		{
			name:       "teams roster shrinks below the observed rows",
			sourceType: "teams", observed: 9, metadata: `{"member_count":3}`,
			want: 3,
		},
		{
			name:       "slack roster shrinks below the observed rows",
			sourceType: "slack", observed: 9, metadata: `{"member_count":3}`,
			want: 3,
		},
		{
			name:       "teams roster above the observed rows",
			sourceType: "teams", observed: 2, metadata: `{"member_count":7}`,
			want: 7,
		},
		{
			name:       "discord catalog count only ever raises",
			sourceType: "discord", observed: 9, metadata: `{"member_count":3}`,
			want: 9,
		},
		{
			name:       "no record keeps the observed count",
			sourceType: "teams", observed: 4,
			want: 4,
		},
		{
			name:       "unreadable record keeps the observed count",
			sourceType: "slack", observed: 4, metadata: `{"member_count":"many"}`,
			want: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			fixture := newMembershipFixture(t, st, tc.sourceType, tc.observed, tc.metadata)

			conversation, err := st.AttachmentConversation(fixture.messageID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, conversation.ParticipantCount)
		})
	}
}

// TestRetryableAttachmentMessagesFailClosedOnUnknownMembership covers the
// durable half of a failed roster read. The provider skips the media in memory,
// but a backfill re-reads the archive: without the unknown marker the missing
// roster reads as a small conversation and the backfill downloads exactly what
// the threshold excluded.
func TestRetryableAttachmentMessagesFailClosedOnUnknownMembership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fixture := newMembershipFixture(t, st, "slack", 2, `{"member_count_unknown":true}`)
	require.NoError(st.ReplaceMessageSlackAttachments(fixture.messageID, []store.AttachmentRef{{
		SourceAttachmentID: "slack:F_PENDING",
		StoragePath:        "https://files.slack.com/files-pri/T01-F_PENDING/a.png",
		Size:               5, State: attachmentpolicy.StatePending,
	}}))

	policy := attachmentpolicy.Policy{MaxParticipants: 4, MaxBytes: 1 << 20}
	items, err := st.ListSlackRetryableAttachmentMessages(fixture.sourceID, policy)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(5, items[0].ParticipantCount,
		"an unreadable roster must not evaluate as a conversation under the limit")
	assert.False(policy.Allows(attachmentpolicy.Conversation{
		Type: items[0].ConversationType, ParticipantCount: items[0].ParticipantCount,
	}, 5))

	// A skip recorded against the unknown roster stays excluded rather than
	// being re-admitted by the archived zero.
	require.NoError(st.ReplaceMessageSlackAttachments(fixture.messageID, []store.AttachmentRef{{
		SourceAttachmentID: "slack:F_PENDING",
		StoragePath:        "https://files.slack.com/files-pri/T01-F_PENDING/a.png",
		Size:               5, State: attachmentpolicy.StateSkipped,
		SkipReason: attachmentpolicy.SkipParticipantThreshold,
	}}))
	items, err = st.ListSlackRetryableAttachmentMessages(fixture.sourceID, policy)
	require.NoError(err)
	assert.Empty(items)

	// Without a configured limit there is no threshold to fail closed on.
	items, err = st.ListSlackRetryableAttachmentMessages(fixture.sourceID,
		attachmentpolicy.Policy{MaxBytes: 1 << 20})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(2, items[0].ParticipantCount)

	// A count an earlier run read does not resolve a roster whose latest read
	// failed: the stale count may permit what the current membership excludes.
	require.NoError(st.ReplaceMessageSlackAttachments(fixture.messageID, []store.AttachmentRef{{
		SourceAttachmentID: "slack:F_PENDING",
		StoragePath:        "https://files.slack.com/files-pri/T01-F_PENDING/a.png",
		Size:               5, State: attachmentpolicy.StatePending,
	}}))
	require.NoError(st.SetConversationMemberCount(fixture.conversationID, 2))
	require.NoError(st.MarkConversationMemberCountUnknown(fixture.conversationID))
	items, err = st.ListSlackRetryableAttachmentMessages(fixture.sourceID, policy)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(5, items[0].ParticipantCount,
		"a stale count beside the unknown marker must not evaluate as a conversation under the limit")
}

// TestRetryableAttachmentMessagesFailClosedWithoutArchivedRoster covers a
// conversation archived before rosters were recorded: its participant rows
// may undercount a membership that exceeds the limit, so retry selection and
// the Beeper policy pass must not admit on them. A roster the provider reads
// later resolves it either way.
func TestRetryableAttachmentMessagesFailClosedWithoutArchivedRoster(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	slack := newMembershipFixture(t, st, "slack", 2, "")
	require.NoError(st.ReplaceMessageSlackAttachments(slack.messageID, []store.AttachmentRef{{
		SourceAttachmentID: "slack:F_LEGACY",
		StoragePath:        "https://files.slack.com/files-pri/T01-F_LEGACY/a.png",
		Size:               5, State: attachmentpolicy.StatePending,
	}}))
	policy := attachmentpolicy.Policy{MaxParticipants: 4, MaxBytes: 1 << 20}

	items, err := st.ListSlackRetryableAttachmentMessages(slack.sourceID, policy)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(5, items[0].ParticipantCount,
		"accumulated participant rows must not evaluate as a conversation under the limit")

	// Without a configured limit the observed count is all there is to report.
	items, err = st.ListSlackRetryableAttachmentMessages(slack.sourceID, attachmentpolicy.Policy{MaxBytes: 1 << 20})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(2, items[0].ParticipantCount)

	// A roster the provider reads resolves the conversation.
	require.NoError(st.SetConversationMemberCount(slack.conversationID, 2))
	items, err = st.ListSlackRetryableAttachmentMessages(slack.sourceID, policy)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(2, items[0].ParticipantCount)

	beeper := newMembershipFixture(t, st, "beeper", 2, "")
	require.NoError(st.ReplaceMessageBeeperAttachments(beeper.messageID, []store.AttachmentRef{{
		SourceAttachmentID: "beeper:mxc://beeper.local/legacy",
		StoragePath:        "mxc://beeper.local/legacy",
		Size:               5, State: attachmentpolicy.StatePending,
	}}))
	result, err := st.ApplyBeeperRetryableAttachmentPolicy(context.Background(), beeper.sourceID, policy)
	require.NoError(err)
	assert.EqualValues(1, result.NewlySkipped, "the policy pass must not admit on an unarchived roster either")
	items, err = st.ListBeeperRetryableAttachmentMessages(beeper.sourceID, policy)
	require.NoError(err)
	assert.Empty(items)
	require.NoError(st.SetConversationMemberCount(beeper.conversationID, 2))
	items, err = st.ListBeeperRetryableAttachmentMessages(beeper.sourceID, policy)
	require.NoError(err)
	require.Len(items, 1, "the resolved roster releases the skip")
	assert.Equal(2, items[0].ParticipantCount)
}

// TestBeeperRetryPolicyWeighsArchivedTotal pins the Beeper half of the
// contract: the sync weighs the participant total Beeper reports, and the
// archived participant rows can undercount it when a listing was truncated. The
// retry policy pass and retry selection must weigh the archived total — and
// fail closed on an unresolved roster — rather than the smaller stored rows.
func TestBeeperRetryPolicyWeighsArchivedTotal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fixture := newMembershipFixture(t, st, "beeper", 2, `{"member_count":8}`)
	pending := func() {
		require.NoError(st.ReplaceMessageBeeperAttachments(fixture.messageID, []store.AttachmentRef{{
			SourceAttachmentID: "beeper:mxc://beeper.local/photo1",
			StoragePath:        "mxc://beeper.local/photo1",
			Size:               5, State: attachmentpolicy.StatePending,
		}}))
	}
	pending()
	policy := attachmentpolicy.Policy{MaxParticipants: 5, MaxBytes: 1 << 20}

	result, err := st.ApplyBeeperRetryableAttachmentPolicy(context.Background(), fixture.sourceID, policy)
	require.NoError(err)
	assert.EqualValues(1, result.NewlySkipped, "the archived total excludes what the stored rows would admit")
	assert.True(result.HasExcluded)
	items, err := st.ListBeeperRetryableAttachmentMessages(fixture.sourceID, policy)
	require.NoError(err)
	assert.Empty(items)

	// The roster shrinks below the limit: the archived total is authoritative
	// in both directions.
	require.NoError(st.SetConversationMemberCount(fixture.conversationID, 3))
	items, err = st.ListBeeperRetryableAttachmentMessages(fixture.sourceID, policy)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(3, items[0].ParticipantCount)

	// An unresolved roster fails closed even though the last total permits.
	pending()
	require.NoError(st.MarkConversationMemberCountUnknown(fixture.conversationID))
	result, err = st.ApplyBeeperRetryableAttachmentPolicy(context.Background(), fixture.sourceID, policy)
	require.NoError(err)
	assert.EqualValues(1, result.NewlySkipped)
	items, err = st.ListBeeperRetryableAttachmentMessages(fixture.sourceID, policy)
	require.NoError(err)
	assert.Empty(items)
}

// TestAttachmentPolicyCandidatesReportUnresolvedRosters keeps purge on the
// safe side of a roster nobody has archived: excluding media deletes blobs, so
// a candidate must say when no authoritative roster backs its count — whether
// a read failed or none was ever recorded — rather than let the accumulated
// participant rows stand in for one.
func TestAttachmentPolicyCandidatesReportUnresolvedRosters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fixture := newMembershipFixture(t, st, "slack", 20, "")
	contentHash := strings.Repeat("ab", 32)
	require.NoError(st.ReplaceMessageSlackAttachments(fixture.messageID, []store.AttachmentRef{{
		SourceAttachmentID: "slack:F_STORED",
		StoragePath:        contentHash[:2] + "/" + contentHash,
		ContentHash:        contentHash, Size: 5, State: attachmentpolicy.StateStored,
	}}))
	candidate := func() store.AttachmentPolicyCandidate {
		candidates, err := st.ListAttachmentPolicyCandidates(context.Background())
		require.NoError(err)
		require.Len(candidates, 1)
		return candidates[0]
	}

	// A conversation archived before rosters were recorded carries only the
	// accumulated participant rows.
	assert.Equal(20, candidate().ParticipantCount)
	assert.True(candidate().RosterUnresolved,
		"accumulated participant rows are not a roster purge may delete on")

	require.NoError(st.MarkConversationMemberCountUnknown(fixture.conversationID))
	assert.Equal(20, candidate().ParticipantCount)
	assert.True(candidate().RosterUnresolved,
		"an unreadable roster must be visible to purge so it can retain the media")

	// A roster the provider has read replaces the marker and is authoritative.
	require.NoError(st.SetConversationMemberCount(fixture.conversationID, 3))
	assert.Equal(3, candidate().ParticipantCount)
	assert.False(candidate().RosterUnresolved)

	// A later failed read leaves the count for reference but the roster
	// unresolved, so purge retains rather than trusting the stale count.
	require.NoError(st.MarkConversationMemberCountUnknown(fixture.conversationID))
	assert.Equal(3, candidate().ParticipantCount)
	assert.True(candidate().RosterUnresolved)
}

// TestMembershipRecordWritesPreserveKnownCounts pins the write side: an exact
// roster replaces whatever was recorded before, an unreadable roster keeps the
// count the provider already read but marks the roster unresolved until a read
// succeeds again, and neither disturbs the other provider metadata sharing the
// column.
func TestMembershipRecordWritesPreserveKnownCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fixture := newMembershipFixture(t, st, "teams", 0, `{"topic":"releases"}`)

	memberCount := func() int {
		conversation, err := st.AttachmentConversation(fixture.messageID)
		require.NoError(err)
		return conversation.ParticipantCount
	}
	metadataKeys := func() map[string]any {
		metadata, err := st.GetConversationMetadata(fixture.conversationID)
		require.NoError(err)
		require.True(metadata.Valid)
		return decodeJSONObject(t, metadata.String)
	}

	require.NoError(st.MarkConversationMemberCountUnknown(fixture.conversationID))
	assert.Contains(metadataKeys(), "member_count_unknown")
	assert.Equal("releases", metadataKeys()["topic"], "unrelated metadata survives")

	require.NoError(st.SetConversationMemberCount(fixture.conversationID, 6))
	assert.Equal(6, memberCount())
	assert.NotContains(metadataKeys(), "member_count_unknown",
		"a roster that was read clears the unknown marker")
	membership, err := st.AttachmentConversationMembership(fixture.messageID)
	require.NoError(err)
	assert.True(membership.RosterArchived)

	// A later outage keeps the count for reference but leaves the roster
	// unresolved: the sync that could not read it failed closed, and a
	// backfill or purge that trusted the stale count could act against the
	// membership the conversation has now.
	require.NoError(st.MarkConversationMemberCountUnknown(fixture.conversationID))
	assert.Equal(6, memberCount())
	assert.Equal(true, metadataKeys()["member_count_unknown"])
	assert.Equal("releases", metadataKeys()["topic"])
	membership, err = st.AttachmentConversationMembership(fixture.messageID)
	require.NoError(err)
	assert.False(membership.RosterArchived,
		"a roster whose latest read failed must be re-resolved before the threshold is trusted")

	require.NoError(st.SetConversationMemberCount(fixture.conversationID, 4))
	assert.Equal(4, memberCount())
	assert.NotContains(metadataKeys(), "member_count_unknown")
}
