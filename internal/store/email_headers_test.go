package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEmailReplyParentEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parentID string
		state    string
		want     bool
	}{
		{"bare", "parent@example.test", "", true},
		{"legacy brackets", "<parent@example.test>", "", true},
		{"missing", "different@example.test", "", false},
		{"case sensitive", "Parent@example.test", "", false},
		{"duplicate", "parent@example.test", "duplicate", false},
		{"self", "parent@example.test", "self", false},
		{"cross source", "parent@example.test", "cross source", false},
		{"hidden", "parent@example.test", "hidden", false},
		{"source deleted", "parent@example.test", "source deleted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			st := f.Store
			parent := f.CreateMessage("parent")
			child := f.CreateMessage("child")
			_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), tc.parentID, parent)
			requirements.NoError(err)
			switch tc.state {
			case "duplicate":
				duplicate := f.CreateMessage("duplicate")
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "<parent@example.test>", duplicate)
			case "self":
				child = parent
			case "cross source":
				other, sourceErr := st.GetOrCreateSource("apple-mail", "other@example.test")
				requirements.NoError(sourceErr)
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id = ? WHERE id = ?`), other.ID, parent)
			case "hidden":
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), parent)
			case "source deleted":
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), parent)
			}
			requirements.NoError(err)
			requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, child, "", "<parent@example.test>"))
			requirements.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil))
			var reply sql.NullInt64
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), child).Scan(&reply))
			want := sql.NullInt64{}
			if tc.want {
				want = sql.NullInt64{Int64: parent, Valid: true}
			}
			assertions.Equal(want, reply)
		})
	}
}

func TestEmailHeaderRepairPreservesMetadataAndRevision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	st := f.Store
	child := f.CreateMessage("child")
	requirements.NoError(st.SetMessageMetadata(child, sql.NullString{String: `{"unrelated":{"answer":42}}`, Valid: true}))
	before, err := st.DerivedDataRevision()
	requirements.NoError(err)
	requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, child, "<child@example.test>", "<parent@example.test>"))
	metadata, err := st.GetMessageMetadata(child)
	requirements.NoError(err)
	assertions.JSONEq(`{"unrelated":{"answer":42},"email_in_reply_to":"parent@example.test"}`, metadata.String)
	after, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(before+1, after)
	requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, child, "different@example.test", "parent@example.test"))
	again, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(after, again)
	var rfcID string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), child).Scan(&rfcID))
	assertions.Equal("child@example.test", rfcID)
}

func TestEmailRepliesSurviveDeduplication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	parent := f.CreateMessage("parent")
	child := f.CreateMessage("child")
	require.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, parent, "parent@example.test", ""))
	require.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, child, "child@example.test", "parent@example.test"))
	require.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil))

	other, err := st.GetOrCreateSource("mbox", "other@example.test")
	require.NoError(err)
	conversation, err := st.EnsureConversation(other.ID, "thread", "Thread")
	require.NoError(err)
	survivor, err := st.UpsertMessage(&store.Message{
		SourceID: other.ID, SourceMessageID: "parent-copy", ConversationID: conversation,
		MessageType:     store.MessageTypeEmail,
		RFC822MessageID: sql.NullString{String: "parent@example.test", Valid: true},
	})
	require.NoError(err)
	_, err = st.MergeDuplicates(survivor, []int64{parent}, "reply-dedup")
	require.NoError(err)
	var reply int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), child).Scan(&reply))
	assert.Equal(survivor, reply)
	deleted, _, err := st.DeleteAllDedupedContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), deleted)
}

func TestEmailReplyResolutionResumesCommittedPages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	st := f.Store
	var children []int64
	for i := range 205 {
		child := f.CreateMessage(fmt.Sprintf("child-%d", i))
		children = append(children, child)
		requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, child, "", "parent@example.test"))
	}
	var cursor int64
	err := st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, func(after int64) error {
		cursor = after
		return context.Canceled
	})
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(children[199], cursor)
	parent := f.CreateMessage("late-parent")
	requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, parent, "parent@example.test", ""))
	requirements.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, cursor, nil))
	var linked int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL`).Scan(&linked))
	assertions.Equal(5, linked, "resume must not revisit the first committed page")
	requirements.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil))
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL`).Scan(&linked))
	assertions.Equal(205, linked, "a later full pass resolves older missing parents")
}

func TestEmailHeaderRepairRejectsWrongSourceAndCancellation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("message")
	err := f.Store.RecordEmailHeadersContext(t.Context(), f.Source.ID+1, id, "message@example.test", "")
	requirements.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requirements.ErrorIs(f.Store.RecordEmailHeadersContext(ctx, f.Source.ID, id, "message@example.test", ""), context.Canceled)
	var got sql.NullString
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), id).Scan(&got))
	assertions.False(got.Valid)
}

func TestEmailHeaderRepairRollsBackWhenRevisionWriteFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	st := f.Store
	id := f.CreateMessage("repair")
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"other":true}`, Valid: true}))
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`ALTER TABLE archive_metadata ADD CONSTRAINT reject_email_revision CHECK (key <> 'derived_data_revision')`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_email_revision BEFORE INSERT ON archive_metadata
   WHEN NEW.key = 'derived_data_revision' BEGIN SELECT RAISE(ABORT,'synthetic revision failure'); END`)
	}
	requirements.NoError(err)
	requirements.Error(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, id, "repair@example.test", "parent@example.test"))
	var rfcID sql.NullString
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), id).Scan(&rfcID))
	assertions.False(rfcID.Valid)
	metadata, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.JSONEq(`{"other":true}`, metadata.String)
}

func TestEmailHeadersFenceSupersededSync(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	st := f.Store
	id := f.CreateMessage("child")
	parent := f.CreateMessage("parent")
	requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, id, "", "parent@example.test"))
	requirements.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, parent, "parent@example.test", ""))
	oldRun, err := st.StartSync(f.Source.ID, "import-emlx")
	requirements.NoError(err)
	stale := st.ScopedToSync(f.Source.ID, oldRun)
	requirements.NoError(st.FailSync(oldRun, "interrupted"))
	_, err = st.StartSync(f.Source.ID, "import-emlx")
	requirements.NoError(err)
	requirements.ErrorIs(stale.RecordEmailHeadersContext(t.Context(), f.Source.ID, id, "child@example.test", ""), store.ErrSyncRunSuperseded)
	requirements.ErrorIs(stale.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil), store.ErrSyncRunSuperseded)
	requirements.ErrorIs(stale.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, id, "child@example.test", "parent@example.test", "parent@example.test"), store.ErrSyncRunSuperseded)
	var reply sql.NullInt64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), id).Scan(&reply))
	assertions.False(reply.Valid)
}

func TestPstEmailHeaderRepair(t *testing.T) {
	for _, tc := range []struct {
		name, storedID, metadata, incomingID, parent, key string
		wantMetadata                                      string
	}{
		{"fill", "", `{"unrelated":42}`, "<child@example.test>", "<parent@example.test>", "root@example.test", `{"unrelated":42,"email_in_reply_to":"parent@example.test","pst_thread_key":"root@example.test"}`},
		{"conflicting own ID rejects parent", "original@example.test", `{}`, "other@example.test", "parent@example.test", "parent@example.test", `{}`},
		{"conflicting parent rejects key", "child@example.test", `{"email_in_reply_to":"original-parent@example.test"}`, "child@example.test", "other-parent@example.test", "other-parent@example.test", `{"email_in_reply_to":"original-parent@example.test"}`},
		{"references with existing ID", "child@example.test", `{}`, "child@example.test", "", "root@example.test", `{"pst_thread_key":"root@example.test"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			id := f.CreateMessage("child")
			_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ?, metadata = ? WHERE id = ?`), tc.storedID, tc.metadata, id)
			require.NoError(err)
			require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, id, tc.incomingID, tc.parent, tc.key))
			metadata, err := st.GetMessageMetadata(id)
			require.NoError(err)
			assert.JSONEq(tc.wantMetadata, metadata.String)
			before, err := st.DerivedDataRevision()
			require.NoError(err)
			require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, id, tc.incomingID, tc.parent, tc.key))
			after, err := st.DerivedDataRevision()
			require.NoError(err)
			assert.Equal(before, after)
			var gotID string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), id).Scan(&gotID))
			want := tc.storedID
			if want == "" {
				want = "child@example.test"
			}
			assert.Equal(want, gotID)
		})
	}
}

// A header read before taking the writer reservation can lose independently
// supplied facts (or fail with SQLITE_BUSY_SNAPSHOT). These repairs contribute
// compatible facts under the existing PST precedence rules in either order.
func TestEmailHeaderRepairConcurrentFacts(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "unscoped"
		if scoped {
			name = "scoped"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := storetest.New(t)
			st := fixture.Store
			st.DB().SetMaxOpenConns(2)
			st.DB().SetMaxIdleConns(2)
			if scoped {
				runID := fixture.StartSync()
				st = st.ScopedToSync(fixture.Source.ID, runID)
			}
			for iteration := range 8 {
				id := fixture.CreateMessage(fmt.Sprintf("concurrent-headers-%d", iteration))
				requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"unrelated":42}`, Valid: true}))
				before, err := st.DerivedDataRevision()
				requirements.NoError(err)
				start := make(chan struct{})
				done := make(chan error, 2)
				// Synchronize BEFORE acquisition; a barrier after two exclusive
				// writer acquisitions would prevent either writer completing.
				go func() {
					<-start
					done <- st.RecordEmailHeadersContext(t.Context(), fixture.Source.ID, id, "child@example.test", "")
				}()
				go func() {
					<-start
					done <- st.RecordPstEmailHeadersContext(t.Context(), fixture.Source.ID, id,
						"", "parent@example.test", "root@example.test")
				}()
				close(start)
				requirements.NoError(<-done)
				requirements.NoError(<-done)
				got := readEmailHeaderSnapshot(t, st, id)
				assertions.Equal(sql.NullString{String: "child@example.test", Valid: true}, got.rfcID)
				assertions.JSONEq(`{"unrelated":42,"email_in_reply_to":"parent@example.test","pst_thread_key":"root@example.test"}`,
					got.metadata.String)
				assertions.Equal(before+1, got.revision, "only the missing own-ID repair advances revision")
			}
		})
	}
}

func TestEmailHeaderRepairMalformedParentRollsBack(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "unscoped"
		if scoped {
			name = "scoped"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := storetest.New(t)
			id := fixture.CreateMessage("malformed-parent")
			st := fixture.Store
			// Valid JSON with an invalid parent fact is supported by both DBs.
			requirements.NoError(st.SetMessageMetadata(id, sql.NullString{
				String: `{"email_in_reply_to":42,"unrelated":true}`, Valid: true,
			}))
			if scoped {
				st = st.ScopedToSync(fixture.Source.ID, fixture.StartSync())
			}
			before := readEmailHeaderSnapshot(t, st, id)
			err := st.RecordPstEmailHeadersContext(t.Context(), fixture.Source.ID, id,
				"child@example.test", "parent@example.test", "root@example.test")
			requirements.ErrorContains(err, "decode email parent ID")
			assertions.Equal(before, readEmailHeaderSnapshot(t, st, id),
				"invalid archived facts must abort without a partial header/revision repair")
		})
	}
}

func TestEmailHeaderRepairScopedStoredFactsWin(t *testing.T) {
	for _, tc := range []struct {
		name, storedID, metadata, incomingID, parent, key string
	}{
		{"own ID", "original@example.test", `{"unrelated":42}`, "different@example.test", "parent@example.test", "root@example.test"},
		{"parent", "child@example.test", `{"email_in_reply_to":"original-parent@example.test"}`, "child@example.test", "different-parent@example.test", "different-root@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := storetest.New(t)
			id := fixture.CreateMessage("stored-facts")
			st := fixture.Store
			_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ?, metadata = ? WHERE id = ?`),
				tc.storedID, tc.metadata, id)
			requirements.NoError(err)
			st = st.ScopedToSync(fixture.Source.ID, fixture.StartSync())
			before := readEmailHeaderSnapshot(t, st, id)
			requirements.NoError(st.RecordPstEmailHeadersContext(t.Context(), fixture.Source.ID, id,
				tc.incomingID, tc.parent, tc.key))
			assertions.Equal(before, readEmailHeaderSnapshot(t, st, id),
				"existing PST conflict precedence must preserve stored facts")
		})
	}
}

func TestEmailHeaderRepairScopedRollsBackWhenRevisionWriteFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	id := fixture.CreateMessage("scoped-rollback")
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"unrelated":42}`, Valid: true}))
	st = st.ScopedToSync(fixture.Source.ID, fixture.StartSync())
	before := readEmailHeaderSnapshot(t, st, id)
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`ALTER TABLE archive_metadata ADD CONSTRAINT reject_scoped_email_revision
			CHECK (key <> 'derived_data_revision')`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_scoped_email_revision BEFORE INSERT ON archive_metadata
			WHEN NEW.key = 'derived_data_revision' BEGIN SELECT RAISE(ABORT,'synthetic revision failure'); END`)
	}
	requirements.NoError(err)
	requirements.Error(st.RecordEmailHeadersContext(t.Context(), fixture.Source.ID, id,
		"child@example.test", "parent@example.test"))
	assertions.Equal(before, readEmailHeaderSnapshot(t, st, id),
		"fenced repairs must roll back header facts and revision together")
}

func TestEmailHeaderRepairScopedRejectsInvalidGenerationAndSource(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "running"
		if completed {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := storetest.New(t)
			id := fixture.CreateMessage("scoped-boundaries")
			base := fixture.Store
			runID := fixture.StartSync()
			st := base.ScopedToSync(fixture.Source.ID, runID)
			before := readEmailHeaderSnapshot(t, base, id)
			// Source checks still apply even when no header facts are supplied.
			requirements.Error(st.RecordEmailHeadersContext(t.Context(), fixture.Source.ID+1, id, "", ""))
			requirements.Error(st.RecordPstEmailHeadersContext(t.Context(), fixture.Source.ID+1, id,
				"child@example.test", "parent@example.test", "root@example.test"))
			requirements.Error(st.ResolveEmailReplyParentsContext(t.Context(), fixture.Source.ID+1, 0, nil))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			requirements.ErrorIs(st.RecordEmailHeadersContext(ctx, fixture.Source.ID, id,
				"child@example.test", ""), context.Canceled)
			if completed {
				requirements.NoError(base.CompleteSync(runID, ""))
				requirements.ErrorIs(st.RecordEmailHeadersContext(t.Context(), fixture.Source.ID, id,
					"child@example.test", ""), store.ErrSyncRunSuperseded)
				requirements.ErrorIs(st.RecordPstEmailHeadersContext(t.Context(), fixture.Source.ID, id,
					"child@example.test", "parent@example.test", "root@example.test"), store.ErrSyncRunSuperseded)
			}
			assertions.Equal(before, readEmailHeaderSnapshot(t, base, id))
		})
	}
}
