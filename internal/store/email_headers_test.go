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
	var reply sql.NullInt64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), id).Scan(&reply))
	assertions.False(reply.Valid)
}
