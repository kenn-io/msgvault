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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assertions := assert.New(t)

			requirements := require.New(t)

			f, receipt := inboxReceiptFixture(t)
			before, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			requirements.NoError(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID))
			requirements.NoError(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized))
			audit, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
			requirements.NoError(err)
			assertions.Equal(before, audit, "source removal preserves original receipt/status for trusted audit")
			requirements.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID), inboxcontrol.ErrDenied)
			requirements.ErrorIs(f.Store.MarkInboxDispatching(t.Context(), receipt.ID), inboxcontrol.ErrConflict)
			lookup, err := f.Store.LookupInboxReceipt(t.Context(), receipt.PrincipalID, receipt.SourceID, receipt.IdempotencyKey)
			requirements.NoError(err)
			assertions.Equal(before, lookup)
		})
	}
}

func TestInboxSourceRemovalRetirementRollsBack(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			requirements := require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION inbox_delete_failure() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic source delete failure'; END; $$ LANGUAGE plpgsql`)
				requirements.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER inbox_delete_failure BEFORE DELETE ON sources FOR EACH ROW EXECUTE FUNCTION inbox_delete_failure()`)
			} else {
				_, err = f.Store.DB().Exec(`CREATE TRIGGER inbox_delete_failure BEFORE DELETE ON sources BEGIN SELECT RAISE(ABORT, 'synthetic source delete failure'); END`)
			}
			requirements.NoError(err)
			requirements.ErrorContains(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized), "synthetic source delete failure")
			requirements.NoError(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID))
			requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
			_, err = f.Store.GetSourceByID(receipt.SourceID)
			requirements.NoError(err)
		})
	}
}

func TestInboxSourceRemovalExcludesActiveControl(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			requirements := require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			execution, err := f.Store.AcquireInboxExecutionContext(t.Context(), receipt.SourceID)
			requirements.NoError(err)
			t.Cleanup(func() { _ = execution.Release() })
			requirements.ErrorIs(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized), store.ErrSyncAlreadyActive)
			requirements.NoError(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID))
			requirements.NoError(execution.Release())
			requirements.NoError(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized))
			requirements.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID), inboxcontrol.ErrDenied)
		})
	}
}

func TestInboxSourceRetirementKeepsDispatchedSettlement(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "serialized"}[serialized], func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			f, receipt := inboxReceiptFixture(t)
			_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
			requirements.NoError(err)
			requirements.NoError(f.Store.MarkInboxDispatching(t.Context(), receipt.ID))
			requirements.NoError(removeInboxTestSource(t, f.Store, receipt.SourceID, serialized))
			requirements.NoError(f.Store.FinishInboxReceipt(t.Context(), receipt.ID, inboxcontrol.StatusUnknown, nil, "synthetic-dispatch-completion"))
			audit, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
			requirements.NoError(err)
			requirements.NotNil(audit)
			assertions.Equal(inboxcontrol.StatusUnknown, audit.Status)
			assertions.Equal("synthetic-dispatch-completion", audit.FailureCode)
			requirements.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(t.Context(), receipt.ID), inboxcontrol.ErrDenied)
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
	require.NoError(t, err, "the removal fence must leave the Store connection available to its transaction")
}

// The child is a real second process using the production backend locks. Its
// stdin only bounds lock lifetime; no Store/provider commands are stubbed.
func TestInboxSourceFenceProcess(t *testing.T) {
	requirements := require.New(t)

	if os.Getenv("MSGVAULT_INBOX_FENCE_HELPER") == "1" {
		st, err := store.OpenForTest(os.Getenv("MSGVAULT_INBOX_FENCE_DB"))
		requirements.NoError(err)
		defer func() { requirements.NoError(st.Close()) }()
		id, err := strconv.ParseInt(os.Getenv("MSGVAULT_INBOX_FENCE_SOURCE"), 10, 64)
		requirements.NoError(err)
		execution, err := st.AcquireInboxExecutionContext(t.Context(), id)
		requirements.NoError(err)
		fmt.Println("inbox-fence-ready")
		_, err = bufio.NewReader(os.Stdin).ReadString('\n')
		requirements.NoError(err)
		requirements.NoError(execution.Release())
		return
	}
	f, receipt := inboxReceiptFixture(t)
	_, _, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	second, err := store.OpenForTest(store.DBPathForTest(f.Store))
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInboxSourceFenceProcess$") //nolint:gosec // Re-executes this test binary with a fixed test selector.
	child.Env = append(os.Environ(), "MSGVAULT_INBOX_FENCE_HELPER=1", "MSGVAULT_INBOX_FENCE_DB="+store.DBPathForTest(f.Store), "MSGVAULT_INBOX_FENCE_SOURCE="+strconv.FormatInt(receipt.SourceID, 10))
	stdout, err := child.StdoutPipe()
	requirements.NoError(err)
	stdin, err := child.StdinPipe()
	requirements.NoError(err)
	child.Stderr = os.Stderr
	requirements.NoError(child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	scanner := bufio.NewScanner(stdout)
	requirements.True(scanner.Scan())
	requirements.Equal("inbox-fence-ready", scanner.Text())
	requirements.ErrorIs(second.RemoveSource(receipt.SourceID), store.ErrSyncAlreadyActive)
	_, _, err = second.RemoveSourceSerialized(ctx, receipt.SourceID)
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	_, err = second.AcquireInboxExecutionContext(ctx, receipt.SourceID)
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	requirements.NoError(f.Store.ValidateInboxReceiptAuthorityContext(ctx, receipt.ID), "failed removal does not partially retire authority")
	_, err = fmt.Fprintln(stdin, "release")
	requirements.NoError(err)
	requirements.NoError(stdin.Close())
	for scanner.Scan() {
	}
	requirements.NoError(scanner.Err())
	requirements.NoError(child.Wait())
	requirements.NoError(second.RemoveSource(receipt.SourceID))
	requirements.ErrorIs(f.Store.ValidateInboxReceiptAuthorityContext(ctx, receipt.ID), inboxcontrol.ErrDenied)
}

func TestInboxSourceFenceReleasesAfterSyncBusy(t *testing.T) {
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "fence@example.test")
	requirements.NoError(err)
	syncLease, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.NoError(err)
	t.Cleanup(func() { _ = syncLease.Release() })
	_, err = st.AcquireInboxExecutionContext(t.Context(), source.ID)
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	requirements.NoError(st.RemoveSource(source.ID), "failed Inbox acquisition releases its separate fence while sync ownership remains held")
	requirements.NoError(syncLease.Release())
}
