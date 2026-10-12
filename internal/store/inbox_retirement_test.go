package store_test

import (
	"bufio"
	"context"
	"fmt"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

func removeInboxTestSource(t *testing.T, st *store.Store, id int64, serialized bool) error {
	t.Helper()
	if serialized {
		_, _, err := st.RemoveSourceSerialized(t.Context(), id)
		return err
	}
	return st.RemoveSource(id)
}

func TestInboxSourceRemovalRetiresPreparedAuthority(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f, receipt := inboxReceiptFixture(t)
			before, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			require.NoError(err)
			require.NoError(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID))
			require.NoError(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized))
			audit, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
			require.NoError(err)
			assert.Equal(before, audit, "source removal preserves original receipt/status for trusted audit")
			require.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID), inboxcontrol.ErrDenied)
			require.ErrorIs(f.Store.MarkInboxDispatching(t.Context(), receipt.ID), inboxcontrol.ErrConflict)
			lookup, err := f.Store.LookupInboxReceipt(t.Context(), receipt.PrincipalID, receipt.SourceID, receipt.IdempotencyKey)
			require.NoError(err)
			assert.Equal(before, lookup)
		})
	}
}

func TestInboxSourceRemovalRetirementRollsBack(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			require := Require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			require.NoError(err)
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION inbox_delete_failure() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic source delete failure'; END; $$ LANGUAGE plpgsql`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER inbox_delete_failure BEFORE DELETE ON sources FOR EACH ROW EXECUTE FUNCTION inbox_delete_failure()`)
			} else {
				_, err = f.Store.DB().Exec(`CREATE TRIGGER inbox_delete_failure BEFORE DELETE ON sources BEGIN SELECT RAISE(ABORT, 'synthetic source delete failure'); END`)
			}
			require.NoError(err)
			require.ErrorContains(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized), "synthetic source delete failure")
			require.NoError(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID))
			require.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
			_, err = f.Store.GetSourceByID(receipt.SourceID)
			require.NoError(err)
		})
	}
}

func TestInboxSourceRemovalExcludesActiveControl(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			require := Require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			require.NoError(err)
			execution, err := f.Store.AcquireInboxExecutionContext(t.Context(), receipt.SourceID)
			require.NoError(err)
			t.Cleanup(func() { _ = execution.Release() })
			require.ErrorIs(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized), store.ErrSyncAlreadyActive)
			require.NoError(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID))
			require.NoError(execution.Release())
			require.NoError(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized))
			require.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID), inboxcontrol.ErrDenied)
		})
	}
}

func TestInboxSourceRetirementKeepsDispatchedSettlement(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			require.NoError(err)
			require.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
			require.NoError(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized))
			require.NoError(f.Store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusUnknown, nil, "synthetic-dispatch-completion"))
			audit, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
			require.NoError(err)
			require.NotNil(audit)
			assert.Equal(inboxcontrol.StatusUnknown, audit.Status)
			assert.Equal("synthetic-dispatch-completion", audit.FailureCode)
			require.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID), inboxcontrol.ErrDenied)
		})
	}
}

func TestInboxSourceRemovalSingleConnectionPool(t *testing.T) {
	f, receipt := inboxReceiptFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory connection ownership")
	}
	f.Store.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := f.Store.RemoveSourceSerialized(ctx, receipt.SourceID)
	Require.NoError(t, err, "the removal fence must leave the Store connection available to its transaction")
}

// The child is a real second process using the production backend locks. Its
// stdin only bounds lock lifetime; no Store/provider commands are stubbed.
func TestInboxSourceFenceProcess(t *testing.T) {
	require := Require.New(t)

	if os.Getenv("MSGVAULT_INBOX_FENCE_HELPER") == "1" {
		st, err := store.OpenForTest(os.Getenv("MSGVAULT_INBOX_FENCE_DB"))
		require.NoError(err)
		defer st.Close()
		id, err := strconv.ParseInt(os.Getenv("MSGVAULT_INBOX_FENCE_SOURCE"), 10, 64)
		require.NoError(err)
		execution, err := st.AcquireInboxExecutionContext(t.Context(), id)
		require.NoError(err)
		fmt.Println("inbox-fence-ready")
		_, err = bufio.NewReader(os.Stdin).ReadString('\n')
		require.NoError(err)
		require.NoError(execution.Release())
		return
	}
	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	require.NoError(err)
	second, err := store.OpenForTest(store.DBPathForTest(f.Store))
	require.NoError(err)
	t.Cleanup(func() { _ = second.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInboxSourceFenceProcess$")
	child.Env = append(os.Environ(), "MSGVAULT_INBOX_FENCE_HELPER=1", "MSGVAULT_INBOX_FENCE_DB="+store.DBPathForTest(f.Store), "MSGVAULT_INBOX_FENCE_SOURCE="+strconv.FormatInt(receipt.SourceID, 10))
	stdout, err := child.StdoutPipe()
	require.NoError(err)
	stdin, err := child.StdinPipe()
	require.NoError(err)
	child.Stderr = os.Stderr
	require.NoError(child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	scanner := bufio.NewScanner(stdout)
	require.True(scanner.Scan())
	require.Equal("inbox-fence-ready", scanner.Text())
	require.ErrorIs(second.RemoveSource(receipt.SourceID), store.ErrSyncAlreadyActive)
	_, _, err = second.RemoveSourceSerialized(ctx, receipt.SourceID)
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	_, err = second.AcquireInboxExecutionContext(ctx, receipt.SourceID)
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	require.NoError(f.Store.ValidateInboxReceiptAuthorityContext(ctx, receipt.ID), "failed removal does not partially retire authority")
	_, err = fmt.Fprintln(stdin, "release")
	require.NoError(err)
	require.NoError(stdin.Close())
	for scanner.Scan() {
	}
	require.NoError(scanner.Err())
	require.NoError(child.Wait())
	require.NoError(second.RemoveSource(receipt.SourceID))
	require.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(ctx, receipt.ID), inboxcontrol.ErrDenied)
}

func TestInboxSourceFenceReleasesAfterSyncBusy(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "fence@example.test")
	require.NoError(err)
	syncLease, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() { _ = syncLease.Release() })
	_, err = st.AcquireInboxExecutionContext(t.Context(), source.ID)
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	require.NoError(st.RemoveSource(source.ID), "failed Inbox acquisition releases its separate fence while sync ownership remains held")
	require.NoError(syncLease.Release())
}
