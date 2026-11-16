package store

import (
	"context"
	"database/sql/driver"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type catalogRevisionCtxKey struct{}

func TestCommitBumpsCatalogRevisionWithTheCallersContext(t *testing.T) {
	base := newRFC822IDBackfillBackendStore(t)
	var bumpValue any
	gate := &meetingProjectionGate{before: func(ctx context.Context, query string, args []driver.NamedValue) error {
		if strings.Contains(query, "INSERT INTO archive_metadata") && slices.ContainsFunc(args, func(arg driver.NamedValue) bool { return arg.Value == accountCatalogRevisionKey }) {
			bumpValue = ctx.Value(catalogRevisionCtxKey{})
		}
		return nil
	}}
	st := gatedMeetingProjectionStore(t, base, gate)
	source, err := st.GetOrCreateSource("mbox", "me@example.net")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Thread")
	require.NoError(t, err)
	bumpValue = nil
	ctx := context.WithValue(t.Context(), catalogRevisionCtxKey{}, "caller")
	_, err = st.UpsertMessageContext(ctx, &Message{
		SourceID: source.ID, ConversationID: conv, SourceMessageID: "m-1", MessageType: "email", SizeEstimate: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, "caller", bumpValue, "the bump runs under the caller's context")
}

func TestAccountCatalogRevisionAdvancesWhenAttributionIsMarkedPending(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Thread")
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), `INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,account_address,account_path) VALUES(?,?,?,'email','work@example.org','inbound')`, source.ID, "one", conv)
	require.NoError(t, err)
	read := func() int64 {
		var revision int64
		err := st.db.QueryRowContext(t.Context(), `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = ?), 0)`, accountCatalogRevisionKey).Scan(&revision)
		require.NoError(t, err)
		return revision
	}
	before := read()
	err = st.withAttributionTxContext(t.Context(), attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		_, err := st.markAccountAttributionPendingForAddressesTx(t.Context(), tx, source.ID, []string{"work@example.org"})
		return err
	})
	require.NoError(t, err)
	assert.Greater(t, read(), before)
}

func TestAccountCatalogRevisionAdvancesAtSourceMergeCommit(t *testing.T) {
	base := newRFC822IDBackfillBackendStore(t)
	from, err := base.GetOrCreateSource("gmail", "history@example.test")
	require.NoError(t, err)
	into, err := base.GetOrCreateSource("gmail", "live@example.test")
	require.NoError(t, err)
	conv, err := base.EnsureConversation(t.Context(), from.ID, "thread", "Thread")
	require.NoError(t, err)
	_, err = base.db.ExecContext(t.Context(), `INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,account_path) VALUES(?,?,?,'email','inbound')`, from.ID, "one", conv)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var marked atomic.Bool
	gate := &meetingProjectionGate{
		before: func(_ context.Context, query string, _ []driver.NamedValue) error {
			if strings.Contains(query, "UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id =") {
				marked.Store(true)
			}
			return nil
		},
		committed: func(bool) {
			if marked.Swap(false) {
				cancel()
			}
		},
	}
	st := gatedMeetingProjectionStore(t, base, gate)
	_, err = st.MergeSourcesContext(ctx, MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	var revision int64
	err = base.db.QueryRowContext(t.Context(), `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = ?), 0)`, accountCatalogRevisionKey).Scan(&revision)
	require.NoError(t, err)
	assert.Positive(t, revision, "the merge marks the catalog before post-commit matching")
}
