package rederive

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
)

func TestEmailResumeSkipsCommittedPrefix(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, src, _ := archiveEmailSource(t, "outlook", "stale")
			if count == 2 {
				archiveEmail(t, st, src, "later", "stale")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sum, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
			require.ErrorIs(err, context.Canceled)
			require.Equal(int64(1), sum.MessagesScanned)
			sum, ran, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			require.True(ran)
			assert.Equal(int64(count-1), sum.MessagesScanned, "a committed prefix must not be rescanned")
		})
	}
}

func failEmailCheckpointWrite(t *testing.T, st *store.Store, table, event, condition string) func() {
	t.Helper()
	require := require.New(t)
	name := "fail_email_checkpoint_" + table
	query := fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s WHEN %s BEGIN SELECT RAISE(ABORT, 'checkpoint fixture failure'); END`, name, event, table, condition)
	drop := "DROP TRIGGER " + name
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN RAISE EXCEPTION 'checkpoint fixture failure'; END IF; RETURN NEW; END $$`, name, condition))
		require.NoError(err)
		query = fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, event, table, name)
		drop += " ON " + table
	}
	_, err := st.DB().Exec(query)
	require.NoError(err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(err)
	}
}

func TestEmailResumePreservesOverflowFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	var failed []int64
	for batch := range 2 {
		for i := range 40 {
			failed = append(failed, archiveEmail(t, st, src, fmt.Sprintf("failed-%d-%d", batch, i), "stale"))
		}
		archiveEmail(t, st, src, fmt.Sprintf("pause-%d", batch), "stale")
	}
	last := archiveEmail(t, st, src, "last", "stale")
	ids := make([]string, len(failed))
	for i, id := range failed {
		ids[i] = strconv.FormatInt(id, 10)
	}
	drop := failEmailCheckpointWrite(t, st, "messages", "UPDATE OF snippet", "NEW.id IN ("+strings.Join(ids, ",")+")")
	for range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		sum, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
		cancel()
		require.ErrorIs(err, context.Canceled)
		require.Equal(int64(41), sum.MessagesScanned)
		require.Equal(int64(40), sum.Errors)
	}
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned)
	assert.Zero(sum.Errors)
	base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
	applied, err := st.IsMigrationApplied(base)
	require.NoError(err)
	assert.False(applied, "old failed rows keep an otherwise successful resumed tail pending")
	var markers int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM applied_migrations WHERE name LIKE ?`), base+"#%").Scan(&markers))
	assert.LessOrEqual(markers, emailRetrySlots+3)
	drop()
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Errors)
	for _, id := range append(failed, last) {
		body, err := st.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal("Heizkörper", body)
	}
	applied, err = st.IsMigrationApplied(base)
	require.NoError(err)
	assert.True(applied)
}

func TestEmailCheckpointFailureReplaysSafely(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, _ := archiveEmailSource(t, "outlook", "stale")
	last := archiveEmail(t, st, src, "later", "stale")
	drop := failEmailCheckpointWrite(t, st, "applied_migrations", "INSERT", "NEW.name LIKE '%#c%'")
	ctx, cancel := context.WithCancel(t.Context())
	_, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
	cancel()
	require.ErrorIs(err, context.Canceled)
	require.ErrorContains(err, "checkpoint fixture failure")
	drop()
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesScanned, "an unpersisted cursor must replay the safe prefix")
	body, err := st.GetMessageBodyText(last)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
}

func TestEmailMalformedProgressRestarts(t *testing.T) {
	for _, suffix := range []string{"#cgarbage", "#c000000000000ffff-0000000000000001", "#rbroken"} {
		t.Run(suffix, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, src, _ := archiveEmailSource(t, "outlook", "stale")
			base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
			require.NoError(st.MarkMigrationApplied(fmt.Sprintf("%s#c%016x-%016x", base, src.ID, 1)))
			require.NoError(st.MarkMigrationApplied(base + suffix))
			_, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
			require.Error(err)
			sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			assert.Equal(int64(1), sum.MessagesScanned)
		})
	}
}

func TestEmailRetryIDsRoundTrip(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		assert := assert.New(ht)
		id := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		got, ok := parseEmailRetryID(fmt.Sprintf("%016x", id))
		require.True(ok)
		assert.Equal(id, got)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestEmailRetryIDsRejectNoncanonicalText(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		assert := assert.New(ht)
		text := hegel.Draw(ht, hegel.Text())
		id, ok := parseEmailRetryID(text)
		raw, err := hex.DecodeString(text)
		var want int64
		if err == nil && len(raw) == 8 && strings.ToLower(text) == text {
			want = int64(binary.BigEndian.Uint64(raw))
		}
		require.Equal(want > 0, ok)
		if want > 0 {
			assert.Equal(want, id)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestEmailRetryRejectsMultipleOverflowRanges(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		count := 2 + hegel.Draw(ht, hegel.Integers(uint(0), uint(math.MaxUint)))%4
		st, src, _ := archiveEmailSource(ht.T, "outlook", "stale")
		defer func() { require.NoError(st.Close()) }()
		base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
		for i := uint(1); i < count; i++ {
			archiveEmail(ht.T, st, src, strconv.FormatUint(uint64(i), 10), "stale")
		}
		for i := int64(1); i <= int64(count); i++ {
			require.NoError(st.MarkMigrationApplied(emailRetryRangeKey(base, i, i)))
		}
		require.NoError(st.MarkMigrationApplied(base + emailRetryPhase))
		_, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
		require.Error(err, "ambiguous retry coverage must not publish completion")
		sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
		require.NoError(err)
		require.Equal(int64(count), sum.MessagesScanned)
	}, hegel.WithTestCases(16), hegel.WithDatabase(""))
}

func TestEmailProcessLossReplaysOnlyUncheckpointedBatch(t *testing.T) {
	for _, cut := range []int{127, 128, 129} {
		t.Run(strconv.Itoa(cut), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			st := testutil.NewSQLiteTestStoreAt(t, path)
			src, err := st.GetOrCreateSource("outlook", "archive@example.net")
			require.NoError(err)
			const count = 130
			for i := range count {
				archiveEmail(t, st, src, strconv.Itoa(i), "stale")
			}
			ctx, cancel := context.WithCancel(t.Context())
			base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
			w := &emailWalk{s: st, sourceID: src.ID, base: base, sum: &Summary{unchanged: true}}
			w.progress = func(string) {
				if w.sum.MessagesScanned == int64(cut) {
					cancel()
				}
			}
			require.NoError(w.restore(ctx))
			w.checkpointing = true
			// Cut the real scan between transactions without repairEmail's
			// graceful flush, as a lost process would do.
			err = w.scan(ctx, 0, math.MaxInt64)
			cancel()
			require.ErrorIs(err, context.Canceled)
			require.NoError(st.Close())
			reopened, err := store.OpenForTest(path)
			require.NoError(err)
			defer func() { require.NoError(reopened.Close()) }()
			sum, _, err := RunIfStale(t.Context(), reopened, src.SourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			checkpointed := (cut - 1) / emailCheckpointRows * emailCheckpointRows
			assert.Equal(int64(count-checkpointed), sum.MessagesScanned)
			assert.Positive(derivedDataRevision(t, reopened), "replayed no-op rows must still publish committed repairs")
		})
	}
}

func TestEmailRemovedSourceCannotInheritCheckpoint(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(strconv.FormatBool(serialized), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, src, _ := archiveEmailSource(t, "outlook", "stale")
			archiveEmail(t, st, src, "later", "stale")
			ctx, cancel := context.WithCancel(t.Context())
			_, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
			cancel()
			require.ErrorIs(err, context.Canceled)
			if serialized {
				_, _, err = st.RemoveSourceSerialized(t.Context(), src.ID)
			} else {
				err = st.RemoveSource(src.ID)
			}
			require.NoError(err)
			recreated, err := st.GetOrCreateSource(src.SourceType, src.Identifier)
			require.NoError(err)
			if !st.IsPostgreSQL() {
				assert.Equal(src.ID, recreated.ID, "the SQLite fixture reuses the removed ID")
			}
			id := archiveEmail(t, st, recreated, "new", "stale")
			sum, ran, err := RunIfStale(t.Context(), st, recreated.SourceType, recreated.Identifier, recreated.ID, nil)
			require.NoError(err)
			require.True(ran)
			assert.Equal(int64(1), sum.MessagesScanned)
			body, err := st.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal("Heizkörper", body)
		})
	}
}

func TestEmailMergeRestartsCheckpointForEarlierMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, history, earlier := archiveEmailSource(t, "outlook", "stale")
	live, err := st.GetOrCreateSource("outlook", "live@example.net")
	require.NoError(err)
	archiveEmail(t, st, live, "live", "stale")
	ctx, cancel := context.WithCancel(t.Context())
	_, _, err = RunIfStale(ctx, st, live.SourceType, live.Identifier, live.ID, func(string) { cancel() })
	cancel()
	require.ErrorIs(err, context.Canceled)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
	require.NoError(err)
	sum, _, err := RunIfStale(t.Context(), st, live.SourceType, live.Identifier, live.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesScanned, "merged rows before the cursor require a new full walk")
	body, err := st.GetMessageBodyText(earlier)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
}

func TestEmailCompletedWalkRetriesPublicationWithoutRescan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, _ := archiveEmailSource(t, "outlook", "stale")
	base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
	drop := failEmailCheckpointWrite(t, st, "applied_migrations", "INSERT", "NEW.name = '"+base+"'")
	_, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.ErrorContains(err, "checkpoint fixture failure")
	drop()
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Zero(sum.MessagesScanned)
	assert.Positive(derivedDataRevision(t, st))
	applied, err := st.IsMigrationApplied(base)
	require.NoError(err)
	assert.True(applied)
}

func TestEmailFailedMarkerWriteDoesNotAdvanceCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, first := archiveEmailSource(t, "outlook", "stale")
	archiveEmail(t, st, src, "last", "stale")
	dropRow := failEmailCheckpointWrite(t, st, "messages", "UPDATE OF snippet", fmt.Sprintf("NEW.id = %d", first))
	dropMarker := failEmailCheckpointWrite(t, st, "applied_migrations", "INSERT", "NEW.name LIKE '%#f%'")
	_, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.ErrorContains(err, "checkpoint fixture failure")
	dropMarker()
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesScanned, "a failure without a durable marker must not be skipped")
	assert.Equal(int64(1), sum.Errors)
	dropRow()
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned)
	assert.Zero(sum.Errors)
}

func TestEmailOverflowCommitBeforeCursorReplaysSafely(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	var ids []int64
	for i := range 42 {
		ids = append(ids, archiveEmail(t, st, src, strconv.Itoa(i), "stale"))
	}
	base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
	for _, id := range ids[:32] {
		require.NoError(st.MarkMigrationApplied(fmt.Sprintf("%s#f%016x", base, id)))
	}
	oldRange := emailRetryRangeKey(base, ids[32], ids[39])
	oldCursor := fmt.Sprintf("%s#c%016x-%016x", base, src.ID, ids[31])
	require.NoError(st.MarkMigrationApplied(oldRange))
	require.NoError(st.MarkMigrationApplied(oldCursor))
	drop := failEmailCheckpointWrite(t, st, "applied_migrations", "INSERT", "NEW.name LIKE '%#c%'")
	w := &emailWalk{s: st, sourceID: src.ID, base: base, rangeKey: oldRange, cursor: oldCursor,
		lo: ids[32], hi: ids[40], savedID: ids[31], afterID: ids[40]}
	require.ErrorContains(w.checkpoint(t.Context()), "checkpoint fixture failure")
	applied, err := st.IsMigrationApplied(emailRetryRangeKey(base, ids[32], ids[40]))
	require.NoError(err)
	require.True(applied, "overflow coverage committed before the rejected cursor")
	drop()
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(10), sum.MessagesScanned)
	applied, err = st.IsMigrationApplied(base)
	require.NoError(err)
	require.False(applied)
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	for _, id := range ids {
		body, err := st.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal("Heizkörper", body)
	}
}

func TestEmailOverflowMarkerFailureKeepsOlderCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	var failed []int64
	for batch := range 2 {
		for i := range 40 {
			failed = append(failed, archiveEmail(t, st, src, fmt.Sprintf("failed-%d-%d", batch, i), "stale"))
		}
		archiveEmail(t, st, src, fmt.Sprintf("pause-%d", batch), "stale")
	}
	ids := make([]string, len(failed))
	for i, id := range failed {
		ids[i] = strconv.FormatInt(id, 10)
	}
	dropRow := failEmailCheckpointWrite(t, st, "messages", "UPDATE OF snippet", "NEW.id IN ("+strings.Join(ids, ",")+")")
	runCanceled := func() error {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		_, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
		return err
	}
	require.ErrorIs(runCanceled(), context.Canceled)
	base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
	oldCursor, err := st.NextMigrationKeyContext(t.Context(), base+emailCursor, "")
	require.NoError(err)
	oldRange, err := st.NextMigrationKeyContext(t.Context(), base+emailRetryRange, "")
	require.NoError(err)
	require.NotEmpty(oldRange)
	dropRange := failEmailCheckpointWrite(t, st, "applied_migrations", "INSERT", "NEW.name LIKE '%#r%'")
	err = runCanceled()
	require.ErrorIs(err, context.Canceled)
	require.ErrorContains(err, "checkpoint fixture failure")
	cursor, err := st.NextMigrationKeyContext(t.Context(), base+emailCursor, "")
	require.NoError(err)
	assert.Equal(oldCursor, cursor)
	rangeKey, err := st.NextMigrationKeyContext(t.Context(), base+emailRetryRange, "")
	require.NoError(err)
	assert.Equal(oldRange, rangeKey)
	applied, err := st.IsMigrationApplied(base)
	require.NoError(err)
	assert.False(applied)
	dropRange()
	dropRow()
	for range 2 {
		_, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
		require.NoError(err)
	}
	for _, id := range failed {
		body, err := st.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal("Heizkörper", body)
	}
}

func TestEmailRetryPhaseFailureKeepsRetainedFailuresPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, failed := archiveEmailSource(t, "outlook", "stale")
	archiveEmail(t, st, src, "last", "stale")
	dropRow := failEmailCheckpointWrite(t, st, "messages", "UPDATE OF snippet", fmt.Sprintf("NEW.id = %d", failed))
	ctx, cancel := context.WithCancel(t.Context())
	_, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
	cancel()
	require.ErrorIs(err, context.Canceled)
	dropRow()
	dropPhase := failEmailCheckpointWrite(t, st, "applied_migrations", "INSERT", "NEW.name LIKE '%#p'")
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.ErrorContains(err, "checkpoint fixture failure")
	assert.Zero(sum.MessagesScanned)
	assert.Zero(sum.Errors)
	base := LedgerKey(src.SourceType, src.Identifier, emailVersion)
	applied, err := st.IsMigrationApplied(base)
	require.NoError(err)
	assert.False(applied)
	dropPhase()
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned)
	body, err := st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
}

func TestEmailInterruptedWalkMakesProgress(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require :=
			// Bound the materialized archive and number of interruption attempts.
			require.New(ht)
		assert := assert.New(ht)

		count := 1 + hegel.Draw(ht, hegel.Integers(uint(0), uint(math.MaxUint)))%32
		budget := 1 + hegel.Draw(ht, hegel.Integers(uint(0), uint(math.MaxUint)))%count
		st, src, first := archiveEmailSource(ht.T, "outlook", "stale")
		defer func() { require.NoError(st.Close()) }()
		ids := []int64{first}
		for i := uint(1); i < count; i++ {
			ids = append(ids, archiveEmail(ht.T, st, src, strconv.FormatUint(uint64(i), 10), "stale"))
		}
		var scanned int64
		for attempt := uint(0); attempt <= count; attempt++ {
			ctx, cancel := context.WithCancel(t.Context())
			var rows uint
			sum, _, err := RunIfStale(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) {
				rows++
				if rows == budget {
					cancel()
				}
			})
			cancel()
			if sum != nil {
				scanned += sum.MessagesScanned
			}
			if err == nil {
				require.Equal(int64(count), scanned, "gracefully interrupted walks visit each row once")
				applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, emailVersion))
				require.NoError(err)
				require.True(applied)
				for _, id := range ids {
					body, err := st.GetMessageBodyText(id)
					require.NoError(err)
					assert.Equal("Heizkörper", body)
				}
				return
			}
			require.ErrorIs(err, context.Canceled)
		}
		require.Fail("full walk made no durable progress")
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}
