package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// TestBuildCache_DerivesIsFromMeAndIdentityDatasets verifies that:
//   - messages Parquet gains a derived is_from_me column: true when the
//     sender's participant email case-insensitively matches a confirmed
//     account_identities address for the message's source, even when the
//     stored is_from_me is false.
//   - owner_participants maps a source to every participant that resolves
//     to one of its confirmed identities.
//   - participant_clusters maps linked participants to their smaller-ID
//     canonical member after a rebuild.
func TestBuildCache_DerivesIsFromMeAndIdentityDatasets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "owner@example.com", "manual"))

	convID, err := st.EnsureConversation(src.ID, "thread-1", "Hi")
	require.NoError(err)

	// The owner's participant email differs only in case from the confirmed
	// identity address, exercising the case-insensitive match.
	ownerParticipantID, err := st.EnsureParticipant("Owner@Example.com", "Owner", "example.com")
	require.NoError(err)
	otherParticipantID, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	require.NoError(err)

	ownerMsgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: ownerParticipantID, Valid: true},
		Subject:         sql.NullString{String: "From owner", Valid: true},
		// IsFromMe left false: the export must derive true from the
		// confirmed identity match, not the stored column alone.
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(ownerMsgID, "from", []int64{ownerParticipantID}, []string{""}))

	controlMsgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m2",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 13, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: otherParticipantID, Valid: true},
		Subject:         sql.NullString{String: "From other", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(controlMsgID, "from", []int64{otherParticipantID}, []string{""}))

	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")
	require.False(result.Skipped, "buildCache unexpectedly skipped")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()

	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var ownerIsFromMe, controlIsFromMe bool
	require.NoError(duckdb.QueryRow(
		`SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`,
		msgPattern, ownerMsgID).Scan(&ownerIsFromMe))
	require.NoError(duckdb.QueryRow(
		`SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`,
		msgPattern, controlMsgID).Scan(&controlIsFromMe))
	assert.True(ownerIsFromMe, "owner-sent message should derive is_from_me = true")
	assert.False(controlIsFromMe, "control message should not derive is_from_me")

	ownerParticipantsPattern := filepath.Join(analyticsDir, "owner_participants", "*.parquet")
	var ownerRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE source_id = ? AND participant_id = ?`,
		ownerParticipantsPattern, src.ID, ownerParticipantID,
	).Scan(&ownerRows))
	assert.Equal(1, ownerRows, "owner_participants should map the source to the owner participant")

	var otherRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`,
		ownerParticipantsPattern, otherParticipantID,
	).Scan(&otherRows))
	assert.Equal(0, otherRows, "non-owner participant should not appear in owner_participants")

	clustersPattern := filepath.Join(analyticsDir, "participant_clusters", "*.parquet")
	var initialClusterRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?)`, clustersPattern,
	).Scan(&initialClusterRows))
	assert.Equal(0, initialClusterRows, "no participants are linked yet")

	// Link the owner and control participants (Task 1 API), then rebuild:
	// participant_clusters should map both to the smaller ID.
	st2, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	_, err = st2.LinkParticipants(ownerParticipantID, otherParticipantID)
	require.NoError(err, "LinkParticipants")
	require.NoError(st2.Close())

	result, err = buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "buildCache full rebuild")
	require.False(result.Skipped)

	canonical := min(ownerParticipantID, otherParticipantID)
	rows, err := duckdb.Query(
		`SELECT participant_id, canonical_id FROM read_parquet(?) ORDER BY participant_id`, clustersPattern)
	require.NoError(err)
	defer func() { _ = rows.Close() }()

	type clusterRow struct{ participantID, canonicalID int64 }
	var got []clusterRow
	for rows.Next() {
		var r clusterRow
		require.NoError(rows.Scan(&r.participantID, &r.canonicalID))
		got = append(got, r)
	}
	require.NoError(rows.Err())
	require.Len(got, 2, "both linked participants should appear in participant_clusters")
	for _, r := range got {
		assert.Equal(canonical, r.canonicalID, "participant %d should map to the canonical (smallest) ID", r.participantID)
	}
}

// TestBuildCache_OwnerParticipantsMatchesNonEmailIdentifiersVerbatim covers
// owners known only by a non-email identifier (e.g. an iMessage/SMS "me"
// confirmed as a phone number): owner_participants and the is_from_me
// derivation must match participant_identifiers of ANY type, not just
// 'email'. Per the identity CLI contract, email comparisons fold case but
// every other identifier type (phone, chat handle, ...) matches verbatim, so
// a case-differing non-email identifier must NOT match.
func TestBuildCache_OwnerParticipantsMatchesNonEmailIdentifiersVerbatim(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("imessage", "+15550100001")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "+15550100001", "manual"), "confirm phone identity")
	require.NoError(st.AddAccountIdentity(src.ID, "@user:matrix.org", "manual"), "confirm chat-handle identity")

	convID, err := st.EnsureConversation(src.ID, "thread-1", "Hi")
	require.NoError(err)

	// Owner known only by a phone participant_identifiers row, no email.
	// EnsureParticipantByPhone writes the participant_identifiers row itself.
	phoneOwnerID, err := st.EnsureParticipantByPhone("+15550100001", "Phone Owner", "phone")
	require.NoError(err)

	// Case-differing non-email identifier: must NOT match verbatim.
	caseVariantID, err := st.EnsureParticipantByIdentifier("matrix", "@User:matrix.org", "Case Variant")
	require.NoError(err)

	phoneMsgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "sms",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: phoneOwnerID, Valid: true},
		// IsFromMe left false: the export must derive true from the
		// confirmed phone identity match.
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(phoneMsgID, "from", []int64{phoneOwnerID}, []string{""}))

	caseVariantMsgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m2",
		MessageType:     "sms",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 13, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: caseVariantID, Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(caseVariantMsgID, "from", []int64{caseVariantID}, []string{""}))

	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")
	require.False(result.Skipped, "buildCache unexpectedly skipped")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()

	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var phoneIsFromMe, caseVariantIsFromMe bool
	require.NoError(duckdb.QueryRow(
		`SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`,
		msgPattern, phoneMsgID).Scan(&phoneIsFromMe))
	require.NoError(duckdb.QueryRow(
		`SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`,
		msgPattern, caseVariantMsgID).Scan(&caseVariantIsFromMe))
	assert.True(phoneIsFromMe, "phone-identified owner message should derive is_from_me = true")
	assert.False(caseVariantIsFromMe, "a case-differing non-email identifier must not match verbatim")

	ownerParticipantsPattern := filepath.Join(analyticsDir, "owner_participants", "*.parquet")
	var phoneOwnerRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE source_id = ? AND participant_id = ?`,
		ownerParticipantsPattern, src.ID, phoneOwnerID,
	).Scan(&phoneOwnerRows))
	assert.Equal(1, phoneOwnerRows, "owner_participants should map the source to the phone-identified owner")

	var caseVariantRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`,
		ownerParticipantsPattern, caseVariantID,
	).Scan(&caseVariantRows))
	assert.Equal(0, caseVariantRows, "case-differing non-email identifier must not appear in owner_participants")
}

// TestBuildCache_AccountIdentityDriftForcesFullRebuildAndRederivesIsFromMe
// covers Finding 1 of the relationships-backend review end-to-end: after a
// build has already baked is_from_me=false for a pre-existing message,
// confirming an account identity for that message's sender must force a
// full rebuild (never the lightweight identity-only refresh, even though
// the same mutation also bumps identity_revision), and the rebuilt message
// shard must re-derive is_from_me=true.
func TestBuildCache_AccountIdentityDriftForcesFullRebuildAndRederivesIsFromMe(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversation(src.ID, "thread-1", "Hi")
	require.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(err)

	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: owner, Valid: true},
		// IsFromMe left false, and no account identity is confirmed yet, so
		// the initial build must not derive it true.
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(msgID, "from", []int64{owner}, []string{""}))
	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "initial buildCache")
	require.False(result.Skipped, "initial build must run")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()
	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var isFromMe bool
	require.NoError(duckdb.QueryRow(
		`SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`,
		msgPattern, msgID).Scan(&isFromMe))
	assert.False(isFromMe, "is_from_me must be false before the identity is confirmed")

	st2, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	require.NoError(st2.AddAccountIdentity(src.ID, "owner@example.com", "manual"), "AddAccountIdentity")
	require.NoError(st2.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(staleness.NeedsBuild, "account identity confirmation should request a build")
	assert.True(staleness.HasAccountIdentityDrift, "account identity drift signal should be set")
	assert.True(staleness.FullRebuild, "account identity drift must force a full rebuild")
	assert.False(identityDriftOnly(staleness),
		"account identity drift must not take the identity-only refresh path")

	result, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err, "buildCacheAuto after account identity confirmation")
	require.False(result.Skipped, "account identity drift must still trigger a build")
	assert.False(result.IdentityOnly,
		"account identity drift must rewrite message shards, not take the identity-only path")

	require.NoError(duckdb.QueryRow(
		`SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`,
		msgPattern, msgID).Scan(&isFromMe))
	assert.True(isFromMe,
		"is_from_me must be re-derived true after the identity is confirmed and a full rebuild runs")
}

// TestBuildCache_IdentityDriftOnlyRefreshesWithoutMessageRebuild verifies
// that when identity drift (a participant link) is the only staleness
// signal, the auto-build path calls cacheops.RefreshIdentityDatasets
// instead of rewriting message shards: the message Parquet file is left
// byte-for-byte untouched (same mtime) while participant_clusters and the
// stamped identity revision are refreshed.
func TestBuildCache_IdentityDriftOnlyRefreshesWithoutMessageRebuild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversation(src.ID, "thread-1", "Hi")
	require.NoError(err)
	a, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	b, err := st.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")
	require.NoError(err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: a, Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(msgID, "from", []int64{a}, []string{""}))
	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "initial buildCache")
	require.False(result.Skipped, "initial build must run")

	messageShards, err := filepath.Glob(filepath.Join(analyticsDir, "messages", "*", "*.parquet"))
	require.NoError(err)
	require.NotEmpty(messageShards, "expected message shards after initial build")
	before, err := os.Stat(messageShards[0])
	require.NoError(err)

	st2, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	wantRevision, err := st2.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")
	require.NoError(st2.Close())

	result, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err, "buildCacheAuto after identity drift")
	require.False(result.Skipped, "identity drift alone must still trigger a build")
	assert.True(result.IdentityOnly, "identity drift alone should take the lightweight refresh path")

	after, err := os.Stat(messageShards[0])
	require.NoError(err)
	assert.Equal(before.ModTime(), after.ModTime(), "message shard must be untouched by an identity-only refresh")
	assert.Equal(before.Size(), after.Size(), "message shard must be untouched by an identity-only refresh")

	state, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err, "ReadCacheSyncState")
	assert.Equal(wantRevision, state.IdentityRevision, "_last_sync.json identity_revision")

	clustersPattern := filepath.Join(analyticsDir, "participant_clusters", "*.parquet")
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()
	var canonicalA, canonicalB int64
	require.NoError(duckdb.QueryRow(
		`SELECT canonical_id FROM read_parquet(?) WHERE participant_id = ?`, clustersPattern, a,
	).Scan(&canonicalA))
	require.NoError(duckdb.QueryRow(
		`SELECT canonical_id FROM read_parquet(?) WHERE participant_id = ?`, clustersPattern, b,
	).Scan(&canonicalB))
	want := min(a, b)
	assert.Equal(want, canonicalA, "participant a canonical id")
	assert.Equal(want, canonicalB, "participant b canonical id")
}
