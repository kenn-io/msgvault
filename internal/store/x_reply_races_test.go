package store_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Reopen the actual isolated fixture, including its cloned PostgreSQL database
// and schema. No second schema or independent draft authority is created.
func xReplyPeer(t *testing.T, st *store.Store) *store.Store {
	t.Helper()
	var path string
	if st.IsPostgreSQL() {
		var database, schema string
		require.NoError(t, st.DB().QueryRow("SELECT current_database(),current_schema()").Scan(&database, &schema))
		address, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		address.Path = "/" + database
		query := address.Query()
		query.Set("search_path", schema)
		address.RawQuery = query.Encode()
		path = address.String()
	} else {
		require.NoError(t, st.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path))
	}
	peer, err := store.OpenForTest(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, peer.Close()) })
	return peer
}

func TestXDraftFreezeIsAtomic(t *testing.T) {
	for _, operation := range []string{"edit", "delete", "admit"} {
		t.Run(operation, func(t *testing.T) {
			st, d, input := xReplyFixture(t)
			peer := xReplyPeer(t, st)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			gate, err := st.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = gate.Rollback() }()
			_, err = gate.ExecContext(ctx, st.Rebind("UPDATE chat_drafts SET updated_at=updated_at WHERE draft_id=?"), d.DraftID)
			require.NoError(t, err)
			start := make(chan struct{})
			entered := make(chan struct{}, 2)
			admitted := make(chan struct {
				result store.XReplyAdmissionResult
				err    error
			}, 1)
			go func() {
				entered <- struct{}{}
				<-start
				r, err := st.AdmitXReplyContext(ctx, input)
				admitted <- struct {
					result store.XReplyAdmissionResult
					err    error
				}{r, err}
			}()
			mutated := make(chan error, 1)
			var competing store.XReplyAdmissionResult
			go func() {
				entered <- struct{}{}
				<-start
				var err error
				switch operation {
				case "edit":
					_, err = peer.UpdateChatDraftContext(ctx, d.DraftID, d.Revision, "concurrent edit")
				case "delete":
					err = peer.DeleteChatDraftContext(ctx, d.DraftID, d.Revision)
				case "admit":
					competing, err = peer.AdmitXReplyContext(ctx, input)
				}
				mutated <- err
			}()
			<-entered
			<-entered
			close(start)
			require.NoError(t, gate.Rollback())
			var first struct {
				result store.XReplyAdmissionResult
				err    error
			}
			select {
			case first = <-admitted:
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
			}
			var secondErr error
			select {
			case secondErr = <-mutated:
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
			}
			if operation == "admit" {
				require.NoError(t, first.err)
				require.NoError(t, secondErr)
				assert.NotEqual(t, first.result.Created, competing.Created)
				assert.Equal(t, first.result.Attempt.AttemptID, competing.Attempt.AttemptID)
			} else if first.err == nil {
				require.True(t, first.result.Created)
				require.ErrorIs(t, secondErr, store.ErrXReplyFrozen)
				same, err := st.GetChatDraftContext(ctx, d.DraftID)
				require.NoError(t, err)
				assert.Equal(t, d, same)
			} else {
				require.NoError(t, secondErr)
				if operation == "edit" {
					require.ErrorIs(t, first.err, store.ErrChatDraftRevisionConflict)
				} else {
					require.ErrorIs(t, first.err, store.ErrChatDraftNotFound)
				}
				_, err := st.GetXReplyReceiptContext(ctx, d.DraftID)
				require.ErrorIs(t, err, store.ErrXReplyReceiptNotFound)
			}
			state, err := st.LoadXControlContext(ctx, d.SourceID)
			require.NoError(t, err)
			if first.err == nil || operation == "admit" {
				require.Len(t, state.Reservations, 1)
				assert.Equal(t, int64(200_000), state.SpendBuckets[0].ReservedMicros)
			} else {
				assert.Empty(t, state.Reservations)
			}
		})
	}
}

func xReplyFailInsert(t *testing.T, st *store.Store, table string) func() {
	t.Helper()
	// These fixed test-owned table names exercise the real transaction's writes.
	require.Contains(t, []string{"x_reply_attempts", "message_bodies"}, table)
	name := "reject_reply_" + table
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic reply write failure'; END $$; CREATE TRIGGER " + name + " BEFORE INSERT OR UPDATE ON " + table + " FOR EACH ROW EXECUTE FUNCTION " + name + "()")
	} else {
		_, err = st.DB().Exec("CREATE TRIGGER " + name + " BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'synthetic reply write failure'); END")
	}
	require.NoError(t, err)
	return func() {
		if st.IsPostgreSQL() {
			_, err = st.DB().Exec("DROP TRIGGER " + name + " ON " + table + "; DROP FUNCTION " + name + "()")
		} else {
			_, err = st.DB().Exec("DROP TRIGGER " + name)
		}
		require.NoError(t, err)
	}
}

func TestXDraftTransactionsRollBackReservationAndFinalization(t *testing.T) {
	st, d, input := xReplyFixture(t)
	before, err := st.LoadXControlContext(t.Context(), d.SourceID)
	require.NoError(t, err)
	undo := xReplyFailInsert(t, st, "x_reply_attempts")
	_, err = st.AdmitXReplyContext(t.Context(), input)
	require.ErrorContains(t, err, "synthetic reply write failure")
	after, err := st.LoadXControlContext(t.Context(), d.SourceID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	require.ErrorIs(t, err, store.ErrXReplyReceiptNotFound)
	undo()
	admitted, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	require.NoError(t, st.RecordXReplyAcceptedContext(t.Context(), admitted.Attempt.XAttemptKey, "9001"))
	undo = xReplyFailInsert(t, st, "message_bodies")
	_, err = st.FinishXReplyContext(t.Context(), admitted.Attempt.XAttemptKey)
	require.ErrorContains(t, err, "synthetic reply write failure")
	same, err := st.GetChatDraftContext(t.Context(), d.DraftID)
	require.NoError(t, err)
	assert.Equal(t, d, same)
	receipt, err := st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "remote_accepted", receipt.State)
	assert.Equal(t, "9001", receipt.RemoteID)
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM x_post_versions WHERE source_id=? AND post_id='9001'"), d.SourceID).Scan(&count))
	assert.Zero(t, count)
	after, err = st.LoadXControlContext(t.Context(), d.SourceID)
	require.NoError(t, err)
	assert.Equal(t, int64(200_000), after.SpendBuckets[0].ReservedMicros)
	assert.Zero(t, after.SpendBuckets[0].SpentMicros)
	undo()
	_, err = st.FinishXReplyContext(t.Context(), admitted.Attempt.XAttemptKey)
	require.NoError(t, err)
}

func TestXDraftConsolidationPreservesNativeReview(t *testing.T) {
	st, source := xPostFixture(t)
	left := xPostInput(source, "200", "100", "left")
	left.ReplyToPostID = "100"
	right := xPostInput(source, "201", "102", "right")
	right.ReplyToPostID = "102"
	l, err := st.PersistXPostContext(t.Context(), left)
	require.NoError(t, err)
	r, err := st.PersistXPostContext(t.Context(), right)
	require.NoError(t, err)
	lt, err := st.ResolveXPostContext(t.Context(), source, "200")
	require.NoError(t, err)
	rt, err := st.ResolveXPostContext(t.Context(), source, "201")
	require.NoError(t, err)
	ld, err := st.CreateChatDraftContext(t.Context(), lt.ConversationID, l.MessageID, "left review", allowChatDraft)
	require.NoError(t, err)
	rd, err := st.CreateChatDraftContext(t.Context(), rt.ConversationID, r.MessageID, "right review", allowChatDraft)
	require.NoError(t, err)
	_, err = st.PersistXPostContext(t.Context(), xPostInput(source, "102", "102", "root edit", "100", "102"))
	require.NoError(t, err)
	updated, err := st.GetChatDraftContext(t.Context(), rd.DraftID)
	require.NoError(t, err)
	assert.Equal(t, ld.ConversationID, updated.ConversationID)
	assert.Equal(t, rd.SourceConversationID, updated.SourceConversationID)
	assert.Equal(t, rd.ReplyToSourceMessageID, updated.ReplyToSourceMessageID)
	assert.Equal(t, rd.Body, updated.Body)
	assert.Equal(t, rd.Revision, updated.Revision)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	require.NoError(t, err)
	cost := store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: 5_000_000, CreateReservationMicros: 200_000, At: xPostTime}
	_, err = st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{SourceID: source, OwnerID: "1001", DraftID: rd.DraftID, Revision: rd.Revision, TargetPostID: rd.ReplyToSourceMessageID, Cost: cost})
	require.ErrorIs(t, err, store.ErrXTargetChanged)
}

func TestXDraftScopedStoreCannotReadOrTransitionAnotherSource(t *testing.T) {
	st, d, input := xReplyFixture(t)
	admitted, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	// Reads must enforce the same source boundary even without a writable run.
	scoped := st.ScopedToSync(other.ID, 1)
	_, err = scoped.GetXReplyReceiptContext(t.Context(), d.DraftID)
	require.ErrorContains(t, err, "scoped to source")
	require.ErrorContains(t, scoped.MarkXReplyUnknownContext(t.Context(), admitted.Attempt.XAttemptKey), "scoped to source")
	receipt, err := st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "admitted", receipt.State)
}
