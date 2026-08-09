package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/embed"
)

type embedGenGroupFixture struct {
	t              *testing.T
	store          *store.Store
	sourceID       int64
	conversationID int64
	aliceID        int64
	bobID          int64
	messageIDs     []int64
}

func newEmbedGenGroupFixture(t *testing.T, members int) *embedGenGroupFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", fmt.Sprintf("group-cas-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "synthetic-chat", "group_chat", "Synthetic chat")
	require.NoError(t, err)
	aliceID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	bobID, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(t, err)
	require.NoError(t, st.EnsureConversationParticipant(conversationID, bobID, "member"))
	require.NoError(t, st.EnsureConversationParticipant(conversationID, aliceID, "member"))

	f := &embedGenGroupFixture{
		t: t, store: st, sourceID: source.ID, conversationID: conversationID,
		aliceID: aliceID, bobID: bobID,
	}
	for i := range members {
		id, err := st.UpsertMessage(&store.Message{
			SourceID: source.ID, SourceMessageID: fmt.Sprintf("member-%d", i),
			ConversationID: conversationID, MessageType: "beeper",
			SentAt:   sql.NullTime{Time: time.Date(2026, 8, 8, 9, i, 0, 0, time.UTC), Valid: true},
			SenderID: sql.NullInt64{Int64: aliceID, Valid: true},
		})
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageBody(id,
			sql.NullString{String: fmt.Sprintf("body %d", i), Valid: true}, sql.NullString{}))
		f.messageIDs = append(f.messageIDs, id)
	}
	return f
}

func (f *embedGenGroupFixture) snapshot() ([]store.EmbedGenStamp, store.EmbedGenMetadataVersion) {
	f.t.Helper()
	snapshot, err := embed.BeginSourceSnapshot(f.t.Context(), f.store)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, snapshot.Close()) }()
	versions := make([]store.EmbedGenStamp, 0, len(f.messageIDs))
	for _, id := range f.messageIDs {
		row, found, err := snapshot.Message(f.t.Context(), id)
		require.NoError(f.t, err)
		require.True(f.t, found)
		versions = append(versions, store.EmbedGenStamp{ID: id, LastModified: row.LastModified})
	}
	conversation, found, err := snapshot.Conversation(f.t.Context(), f.conversationID)
	require.NoError(f.t, err)
	require.True(f.t, found)
	return versions, store.EmbedGenMetadataVersion{
		ConversationID: conversation.MetadataVersion.ConversationID,
		Digest:         conversation.MetadataVersion.Digest,
	}
}

func (f *embedGenGroupFixture) exec(query string, args ...any) {
	f.t.Helper()
	_, err := f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(query), args...)
	require.NoError(f.t, err)
}

func (f *embedGenGroupFixture) embedGens() []sql.NullInt64 {
	f.t.Helper()
	out := make([]sql.NullInt64, 0, len(f.messageIDs))
	for _, id := range f.messageIDs {
		var value sql.NullInt64
		err := f.store.DB().QueryRowContext(f.t.Context(),
			f.store.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), id).Scan(&value)
		if err == sql.ErrNoRows {
			continue
		}
		require.NoError(f.t, err)
		out = append(out, value)
	}
	return out
}

func TestSetEmbedGenGroupIfUnchanged_SuccessAndReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbedGenGroupFixture(t, 2)
	versions, metadata := f.snapshot()

	stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 4)
	require.NoError(err)
	assert.True(stamped)
	assert.Equal([]sql.NullInt64{{Int64: 4, Valid: true}, {Int64: 4, Valid: true}}, f.embedGens())

	replayVersions, replayMetadata := f.snapshot()
	stamped, err = f.store.SetEmbedGenGroupIfUnchanged(t.Context(), replayVersions, replayMetadata, 4)
	require.NoError(err)
	assert.True(stamped)
	assert.Equal([]sql.NullInt64{{Int64: 4, Valid: true}, {Int64: 4, Valid: true}}, f.embedGens())
}

func TestSetEmbedGenGroupIfUnchanged_PreservesPublishedRevisionTokens(t *testing.T) {
	f := newEmbedGenGroupFixture(t, 2)
	fixed := time.Date(2000, 6, 15, 12, 30, 45, 0, time.UTC)
	for _, id := range f.messageIDs {
		f.exec(`UPDATE messages SET last_modified = ? WHERE id = ?`, fixed, id)
	}
	versions, metadata := f.snapshot()

	stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 4)
	require.NoError(t, err)
	require.True(t, stamped)
	after, _ := f.snapshot()
	assert.Equal(t, versions, after,
		"contextual coverage bookkeeping must not change the published document revision")
}

func TestSetEmbedGenGroupIfUnchanged_PostgresAvoidsPersistenceLockInversion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbedGenGroupFixture(t, 1)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only advisory-lock ordering regression")
	}
	versions, metadata := f.snapshot()

	persistence, err := f.store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = persistence.Rollback()
		}
	})
	_, err = persistence.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock_shared(
		hashtextextended('msgvault.embedding_change_clock', 0))`)
	require.NoError(err)
	var conversationID int64
	require.NoError(persistence.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT id FROM conversations WHERE id = ? FOR UPDATE`), f.conversationID).
		Scan(&conversationID))
	assert.Equal(f.conversationID, conversationID)

	type casResult struct {
		stamped bool
		err     error
	}
	casCtx, cancelCAS := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCAS()
	resultCh := make(chan casResult, 1)
	go func() {
		stamped, casErr := f.store.SetEmbedGenGroupIfUnchanged(casCtx, versions, metadata, 4)
		resultCh <- casResult{stamped: stamped, err: casErr}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND (
			      query LIKE '%FROM conversations WHERE id = %FOR UPDATE%'
			      OR (
			          query LIKE '%pg_advisory_xact_lock%'
			          AND query LIKE '%msgvault.embedding_change_clock%'
			      )
			  )`).Scan(&blocked))
		if blocked > 0 {
			break
		}
		require.False(time.Now().After(deadline), "group CAS did not reach a blocked lock request")
		time.Sleep(10 * time.Millisecond)
	}

	var messageID int64
	require.NoError(persistence.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT id FROM messages WHERE id = ? FOR UPDATE NOWAIT`), f.messageIDs[0]).Scan(&messageID),
		"the group CAS must wait on the advisory lock before it locks message rows")
	assert.Equal(f.messageIDs[0], messageID)
	require.NoError(persistence.Commit())
	committed = true

	select {
	case result := <-resultCh:
		require.NoError(result.err)
		assert.True(result.stamped)
	case <-casCtx.Done():
		require.NoError(casCtx.Err(), "group CAS did not finish after persistence committed")
	}
}

func TestSetEmbedGenGroupIfUnchanged_OneMissStampsNone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbedGenGroupFixture(t, 2)
	versions, metadata := f.snapshot()
	changed := make(chan error, 1)
	go func() {
		if err := f.store.UpsertMessageBody(f.messageIDs[1],
			sql.NullString{String: "changed after snapshot", Valid: true}, sql.NullString{}); err != nil {
			changed <- err
			return
		}
		_, err := f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(
			`UPDATE messages SET last_modified = ? WHERE id = ?`),
			time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC), f.messageIDs[1])
		changed <- err
	}()
	require.NoError(<-changed)

	stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 4)
	require.NoError(err)
	assert.False(stamped)
	assert.Equal([]sql.NullInt64{{}, {}}, f.embedGens())
}

func TestSetEmbedGenGroupIfUnchanged_MissingDeletedAndDuplicateMembers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*embedGenGroupFixture, []store.EmbedGenStamp)
	}{
		{"missing member", func(f *embedGenGroupFixture, _ []store.EmbedGenStamp) {
			f.exec(`DELETE FROM messages WHERE id = ?`, f.messageIDs[1])
		}},
		{"soft-deleted member", func(f *embedGenGroupFixture, _ []store.EmbedGenStamp) {
			f.exec(`UPDATE messages SET deleted_at = ? WHERE id = ?`,
				time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC), f.messageIDs[1])
		}},
		{"duplicate member token", func(_ *embedGenGroupFixture, versions []store.EmbedGenStamp) {
			versions[1] = versions[0]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEmbedGenGroupFixture(t, 2)
			versions, metadata := f.snapshot()
			tt.mutate(f, versions)
			stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 4)
			require.NoError(t, err)
			assert.False(t, stamped)
			for _, value := range f.embedGens() {
				assert.False(t, value.Valid)
			}
		})
	}
}

func TestSetEmbedGenGroupIfUnchanged_MetadataChangesStampNone(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*embedGenGroupFixture) error
	}{
		{"conversation title", func(f *embedGenGroupFixture) error {
			_, err := f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(
				`UPDATE conversations SET title = ? WHERE id = ?`), "Renamed chat", f.conversationID)
			return err
		}},
		{"conversation membership role", func(f *embedGenGroupFixture) error {
			_, err := f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(
				`UPDATE conversation_participants SET role = ? WHERE conversation_id = ? AND participant_id = ?`),
				"admin", f.conversationID, f.aliceID)
			return err
		}},
		{"conversation membership added", func(f *embedGenGroupFixture) error {
			participantID, err := f.store.EnsureParticipant(
				"carol@example.test", "Carol", "example.test")
			if err != nil {
				return err
			}
			return f.store.EnsureConversationParticipant(
				f.conversationID, participantID, "member")
		}},
		{"conversation membership removed", func(f *embedGenGroupFixture) error {
			_, err := f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(
				`DELETE FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`),
				f.conversationID, f.bobID)
			return err
		}},
		{"captured message moved to another conversation", func(f *embedGenGroupFixture) error {
			conversationID, err := f.store.EnsureConversationWithType(
				f.sourceID, "moved-chat", "group_chat", "Moved chat")
			if err != nil {
				return err
			}
			_, err = f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(
				`UPDATE messages SET conversation_id = ? WHERE id = ?`),
				conversationID, f.messageIDs[len(f.messageIDs)-1])
			return err
		}},
		{"participant display and revision", func(f *embedGenGroupFixture) error {
			_, err := f.store.DB().ExecContext(f.t.Context(), f.store.Rebind(
				`UPDATE participants SET display_name = ?, updated_at = ? WHERE id = ?`),
				"Alice Updated", time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC), f.aliceID)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbedGenGroupFixture(t, 2)
			versions, metadata := f.snapshot()
			changed := make(chan error, 1)
			go func() {
				changed <- tt.mutate(f)
			}()
			require.NoError(<-changed)

			stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 4)
			require.NoError(err)
			assert.False(stamped)
			assert.Equal([]sql.NullInt64{{}, {}}, f.embedGens())
		})
	}
}

func TestSetEmbedGenGroupIfUnchanged_DialectNativeTimestampToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbedGenGroupFixture(t, 1)
	versions, metadata := f.snapshot()
	require.Len(versions, 1)
	if f.store.IsPostgreSQL() {
		assert.IsType(time.Time{}, versions[0].LastModified)
	} else {
		assert.IsType("", versions[0].LastModified)
	}

	stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 9)
	require.NoError(err)
	assert.True(stamped)
}

func TestSetEmbedGenGroupIfUnchanged_StampErrorRollsBackEveryMember(t *testing.T) {
	f := newEmbedGenGroupFixture(t, 2)
	if f.store.IsPostgreSQL() {
		t.Skip("SQLite trigger injection proves the shared manual transaction rollback path")
	}
	versions, metadata := f.snapshot()
	f.exec(fmt.Sprintf(`CREATE TRIGGER synthetic_group_stamp_failure
		BEFORE UPDATE OF embed_gen ON messages FOR EACH ROW
		WHEN NEW.id = %d
		BEGIN
			SELECT RAISE(ABORT, 'synthetic group stamp failure');
		END`, f.messageIDs[1]))

	stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 4)
	require.ErrorContains(t, err, "synthetic group stamp failure")
	assert.False(t, stamped)
	assert.Equal(t, []sql.NullInt64{{}, {}}, f.embedGens(),
		"the first member stamp must roll back when a later member errors")
}

func TestEmbedGenMetadataVersion_MatchesAssemblerCanonicalDigest(t *testing.T) {
	f := newEmbedGenGroupFixture(t, 1)
	if f.store.IsPostgreSQL() {
		t.Skip("the literal vector pins SQLite's canonical timestamp text; PostgreSQL parity is covered by the live snapshot test")
	}
	f.exec(`UPDATE participants SET updated_at = ? WHERE id IN (?, ?)`,
		"2030-01-02 03:04:05", f.aliceID, f.bobID)
	versions, metadata := f.snapshot()

	assert.Equal(t, "008696efed0969c656a9fef5057430881961388f760d2286213b606605837a3d", metadata.Digest)
	stamped, err := f.store.SetEmbedGenGroupIfUnchanged(t.Context(), versions, metadata, 11)
	require.NoError(t, err)
	assert.True(t, stamped, "store recomputation must accept Task 5's exact canonical digest")
}

func TestContextualConvergenceCounts_PartitionsContextualAndOrdinaryMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", fmt.Sprintf("coverage-%d", time.Now().UnixNano()))
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "coverage", "group_chat", "Coverage")
	require.NoError(err)
	ids := make(map[string]int64)
	for i, messageType := range []string{"beeper", "meeting_transcript", "email", "sms"} {
		id, err := st.UpsertMessage(&store.Message{
			SourceID: source.ID, SourceMessageID: fmt.Sprintf("coverage-%d", i),
			ConversationID: conversationID, MessageType: messageType,
		})
		require.NoError(err)
		ids[messageType] = id
	}
	require.NoError(st.SetEmbedGen(t.Context(), []int64{ids["beeper"], ids["email"]}, 7))

	got, err := st.ContextualConvergenceCounts(t.Context(), 7)
	require.NoError(err)
	assert.Equal(store.EmbeddingConvergenceCounts{
		Live: 4, Stamped: 2, Missing: 2,
		ContextualLive: 2, ContextualStamped: 1, ContextualMissing: 1,
		OrdinaryLive: 2, OrdinaryStamped: 1, OrdinaryMissing: 1,
	}, got)
	assert.Equal(got.Live, got.ContextualLive+got.OrdinaryLive)
	assert.Equal(got.Stamped, got.ContextualStamped+got.OrdinaryStamped)
	assert.Equal(got.Missing, got.ContextualMissing+got.OrdinaryMissing)

	live, stamped, blank, missing, err := st.CoverageCounts(t.Context(), 7)
	require.NoError(err)
	assert.Equal([]int64{4, 2, 0, 2}, []int64{live, stamped, blank, missing},
		"ordinary CoverageCounts behavior must remain unchanged")

	zero, err := st.ContextualConvergenceCounts(t.Context(), 0)
	require.NoError(err)
	assert.Equal(int64(0), zero.Stamped)
	assert.Equal(zero.Live, zero.Missing)
	assert.Equal(zero.ContextualLive, zero.ContextualMissing)
	assert.Equal(zero.OrdinaryLive, zero.OrdinaryMissing)
}
