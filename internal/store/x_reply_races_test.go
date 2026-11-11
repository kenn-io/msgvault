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
	peer, err := store.OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, peer.Close()) })
	return peer
}

func TestXDraftFreezeIsAtomic(t *testing.T) {
	for _, operation := range []string{"edit", "delete", "admit"} {
		t.Run(operation, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st, d, input := xReplyFixture(t)
			peer := xReplyPeer(t, st)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			gate, err := st.DB().BeginTx(ctx, nil)
			_testRequire.NoError(err)
			defer func() { _ = gate.Rollback() }()
			_, err = gate.ExecContext(ctx, st.Rebind("UPDATE chat_drafts SET updated_at=updated_at WHERE draft_id=?"), d.DraftID)
			_testRequire.NoError(err)
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
			_testRequire.NoError(gate.Rollback())
			var first struct {
				result store.XReplyAdmissionResult
				err    error
			}
			select {
			case first = <-admitted:
			case <-ctx.Done():
				_testRequire.NoError(ctx.Err())
			}
			var secondErr error
			select {
			case secondErr = <-mutated:
			case <-ctx.Done():
				_testRequire.NoError(ctx.Err())
			}
			if operation == "admit" {
				_testRequire.NoError(first.err)
				_testRequire.NoError(secondErr)
				_testAssert.NotEqual(first.result.Created, competing.Created)
				_testAssert.Equal(first.result.Attempt.AttemptID, competing.Attempt.AttemptID)
			} else if first.err == nil {
				_testRequire.True(first.result.Created)
				_testRequire.ErrorIs(secondErr, store.ErrXReplyFrozen)
				same, err := st.GetChatDraftContext(ctx, d.DraftID)
				_testRequire.NoError(err)
				_testAssert.Equal(d, same)
			} else {
				_testRequire.NoError(secondErr)
				if operation == "edit" {
					_testRequire.ErrorIs(first.err, store.ErrChatDraftRevisionConflict)
				} else {
					_testRequire.ErrorIs(first.err, store.ErrChatDraftNotFound)
				}
				_, err := st.GetXReplyReceiptContext(ctx, d.DraftID)
				_testRequire.ErrorIs(err, store.ErrXReplyReceiptNotFound)
			}
			state, err := st.LoadXControlContext(ctx, d.SourceID)
			_testRequire.NoError(err)
			if first.err == nil || operation == "admit" {
				_testRequire.Len(state.Reservations, 1)
				_testAssert.Equal(int64(200_000), state.SpendBuckets[0].ReservedMicros)
			} else {
				_testAssert.Empty(state.Reservations)
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, d, input := xReplyFixture(t)
	before, err := st.LoadXControlContext(t.Context(), d.SourceID)
	_testRequire.NoError(err)
	undo := xReplyFailInsert(t, st, "x_reply_attempts")
	_, err = st.AdmitXReplyContext(t.Context(), input)
	_testRequire.ErrorContains(err, "synthetic reply write failure")
	after, err := st.LoadXControlContext(t.Context(), d.SourceID)
	_testRequire.NoError(err)
	_testAssert.Equal(before, after)
	_, err = st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	_testRequire.ErrorIs(err, store.ErrXReplyReceiptNotFound)
	undo()
	admitted, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	_testRequire.NoError(st.RecordXReplyAcceptedContext(t.Context(), admitted.Attempt.XAttemptKey, "9001"))
	undo = xReplyFailInsert(t, st, "message_bodies")
	_, err = st.FinishXReplyContext(t.Context(), admitted.Attempt.XAttemptKey)
	_testRequire.ErrorContains(err, "synthetic reply write failure")
	same, err := st.GetChatDraftContext(t.Context(), d.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal(d, same)
	receipt, err := st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal("remote_accepted", receipt.State)
	_testAssert.Equal("9001", receipt.RemoteID)
	var count int
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM x_post_versions WHERE source_id=? AND post_id='9001'"), d.SourceID).Scan(&count))
	_testAssert.Zero(count)
	after, err = st.LoadXControlContext(t.Context(), d.SourceID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(200_000), after.SpendBuckets[0].ReservedMicros)
	_testAssert.Zero(after.SpendBuckets[0].SpentMicros)
	undo()
	_, err = st.FinishXReplyContext(t.Context(), admitted.Attempt.XAttemptKey)
	_testRequire.NoError(err)
}

func TestXDraftConsolidationPreservesNativeReview(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	left := xPostInput(source, "200", "100", "left")
	left.ReplyToPostID = "100"
	right := xPostInput(source, "201", "102", "right")
	right.ReplyToPostID = "102"
	l, err := st.PersistXPostContext(t.Context(), left)
	_testRequire.NoError(err)
	r, err := st.PersistXPostContext(t.Context(), right)
	_testRequire.NoError(err)
	lt, err := st.ResolveXPostContext(t.Context(), source, "200")
	_testRequire.NoError(err)
	rt, err := st.ResolveXPostContext(t.Context(), source, "201")
	_testRequire.NoError(err)
	ld, err := st.CreateChatDraftContext(t.Context(), lt.ConversationID, l.MessageID, "left review", allowChatDraft)
	_testRequire.NoError(err)
	rd, err := st.CreateChatDraftContext(t.Context(), rt.ConversationID, r.MessageID, "right review", allowChatDraft)
	_testRequire.NoError(err)
	_, err = st.PersistXPostContext(t.Context(), xPostInput(source, "102", "102", "root edit", "100", "102"))
	_testRequire.NoError(err)
	updated, err := st.GetChatDraftContext(t.Context(), rd.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal(ld.ConversationID, updated.ConversationID)
	_testAssert.Equal(rd.SourceConversationID, updated.SourceConversationID)
	_testAssert.Equal(rd.ReplyToSourceMessageID, updated.ReplyToSourceMessageID)
	_testAssert.Equal(rd.Body, updated.Body)
	_testAssert.Equal(rd.Revision, updated.Revision)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	_testRequire.NoError(err)
	cost := store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: 5_000_000, CreateReservationMicros: 200_000, At: xPostTime}
	_, err = st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{SourceID: source, OwnerID: "1001", DraftID: rd.DraftID, Revision: rd.Revision, TargetPostID: rd.ReplyToSourceMessageID, Cost: cost})
	_testRequire.ErrorIs(err, store.ErrXTargetChanged)
}

func TestXDraftScopedStoreCannotReadOrTransitionAnotherSource(t *testing.T) {
	_testRequire := require.New(t)

	st, d, input := xReplyFixture(t)
	admitted, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	// Reads must enforce the same source boundary even without a writable run.
	scoped := st.ScopedToSync(other.ID, 1)
	_, err = scoped.GetXReplyReceiptContext(t.Context(), d.DraftID)
	_testRequire.ErrorContains(err, "scoped to source")
	_testRequire.ErrorContains(scoped.MarkXReplyUnknownContext(t.Context(), admitted.Attempt.XAttemptKey), "scoped to source")
	receipt, err := st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	_testRequire.NoError(err)
	assert.Equal(t, "admitted", receipt.State)
}
