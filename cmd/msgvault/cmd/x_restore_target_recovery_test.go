package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXRestoreCannotOverwriteCurrentXArchiveWithPreXSnapshot(t *testing.T) {
	for _, unrelated := range []bool{false, true} {
		t.Run(map[bool]string{false: "configured_home", true: "unrelated_target"}[unrelated], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newXRestoreFixture(t)
			require.NoError(f.st.RemoveSource(f.source.ID))
			old := f.snapshot(t)
			var err error
			f.source, err = f.st.GetOrCreateSource("x", "1001")
			require.NoError(err)
			generation := int64(1)
			_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{OwnerID: "1001", InitializeInstance: true, Generation: &generation})
			require.NoError(err)
			_, err = f.st.ReserveXRequestContext(t.Context(), f.source.ID, store.XRequestCost{
				Kind: store.XRequestTimeline, OwnerID: "1001", Generation: generation,
				GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 1_000_000,
				RequestedResults: 5, PostPriceMicros: 5_000, UserPriceMicros: 10_000,
				At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(err)
			cleaner := NewXContentCleanup(f.st, f.cfg, nil)
			_, err = cleaner.RemovePost(t.Context(), f.source.ID, "102", "removed")
			require.NoError(err)
			require.NoError(f.st.Close(), "simulate a stopped daemon before attempting restore")
			target := f.cfg.Data.DataDir
			if unrelated {
				target = t.TempDir()
				require.NoError(os.CopyFS(target, os.DirFS(f.cfg.Data.DataDir)))
			}
			dbPath := filepath.Join(target, "msgvault.db")
			before, err := os.ReadFile(dbPath)
			require.NoError(err)
			err = f.restoreInto(t, old, "", "", target, true)
			require.ErrorContains(err, "existing X archive requires a fresh isolated restore target")
			after, err := os.ReadFile(dbPath)
			require.NoError(err)
			assert.Equal(before, after, "spending, receipts, and suppression authority must survive refusal")
			owner, err := tryAcquireDaemonOwnerLock(f.cfg.Data.DataDir)
			require.NoError(err, "a rejected restore releases daemon ownership")
			require.NoError(owner.Close())
			writer, err := tryAcquireWriteOwnerLock(f.cfg.Data.DataDir)
			require.NoError(err, "a rejected restore releases direct-writer ownership")
			require.NoError(writer.Close())
			assert.Zero(f.providerCalls.Load())
		})
	}
}

func TestXRestoreTargetCheckAllowsExistingNonXArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newXRestoreFixture(t)
	require.NoError(f.st.RemoveSource(f.source.ID))
	source, err := f.st.GetOrCreateSource("text", "example-archive")
	require.NoError(err)
	snapshot := f.snapshot(t)
	require.NoError(f.st.UpdateSourceDisplayName(source.ID, "A later display name"))
	require.NoError(f.st.Close())
	require.NoError(f.restoreInto(t, snapshot, "", "", f.cfg.Data.DataDir, true))
	readOnly, err := store.OpenReadOnly(f.cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { assert.NoError(readOnly.Close()) }()
	got, err := readOnly.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(got.DisplayName.String, "a non-X archive can still publish its prior snapshot")
	assert.Zero(f.providerCalls.Load())
}
