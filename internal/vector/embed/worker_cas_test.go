//go:build sqlite_vec

package embed

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

// stamps builds a single-item EmbedGenStamp slice for a CAS stamp call.
func stamps(id int64, lastModified any) []store.EmbedGenStamp {
	return []store.EmbedGenStamp{{ID: id, LastModified: lastModified}}
}

// lmOf reads a message's last_modified as the literal stored text (CAST AS
// TEXT defeats go-sqlite3's DATETIME coercion, matching the worker).
func lmOf(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(
		`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`, id).Scan(&s))
	return s
}

// setBaselineLM pins last_modified to a fixed far-past value so a subsequent
// trigger-driven bump is guaranteed to differ (sidesteps SQLite's 1-second
// timestamp resolution). The explicit write is preserved by the trigger's
// WHEN guard (OLD != NEW), not re-bumped.
func setBaselineLM(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	_, err := db.Exec(
		`UPDATE messages SET last_modified = '2000-01-01 00:00:00' WHERE id = ?`, id)
	require.NoError(t, err, "baseline last_modified")
	return lmOf(t, db, id)
}

// TestWorker_CASRepairRace is the core regression for Codex 129d #1: a
// concurrent content edit (repair-encoding) that lands BETWEEN the worker
// reading a message's content and stamping embed_gen must NOT leave the row
// marked embedded-with-stale-content. The optimistic CAS on last_modified
// catches the change and leaves the row "needs embedding".
func TestWorker_CASRepairRace(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	// Baseline last_modified to a fixed past value (= the token T the worker
	// will capture at read time).
	tokenAtRead := setBaselineLM(t, f.MainDB, 1)

	// Inject the race: when the embedder is called (after the worker scanned +
	// fetched content and captured last_modified = T, before it stamps),
	// simulate repair-encoding rewriting the body. The body UPDATE fires the
	// trigger, bumping last_modified to T2 (!= T); repair-encoding also resets
	// embed_gen -> NULL.
	f.FakeClient.preReturn = func() {
		_, err := f.MainDB.Exec(
			`UPDATE message_bodies SET body_text = 'corrected content' WHERE message_id = 1`)
		require.NoError(t, err, "race: rewrite body")
		_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 1`)
		require.NoError(t, err, "race: reset embed_gen")
	}

	w := newTestWorker(f, 1)
	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(t, err, "RunOnce")

	// The CAS stamp targeted WHERE last_modified = T, but the row is now T2,
	// so 0 rows were stamped: embed_gen is still NULL and the row still needs
	// embedding. (Without the CAS, the unconditional stamp would have marked
	// it covered with the STALE pre-repair content — proven below.)
	_, isNull := embedGenOf(t, f.MainDB, 1)
	assert.True(t, isNull, "raced row must NOT be stamped (embed_gen still NULL)")
	assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"raced row still needs embedding")

	// A CAS miss is NOT counted as a success.
	assert.Equal(t, 0, res.Succeeded, "CAS-missed row not counted in Succeeded")

	// The watermark still advances to batchMax (the single scanned id) — the
	// drain does not stick on the missed row; the backstop is the recovery.
	assert.Equal(t, int64(1), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)),
		"watermark advances past the CAS-missed row")

	// Confirm last_modified actually moved (the race really happened).
	assert.NotEqual(t, tokenAtRead, lmOf(t, f.MainDB, 1), "last_modified bumped by race")

	// Recovery: clear the preReturn race, then a backstop pass (scans from 0,
	// ignoring the watermark) re-embeds the row with the corrected content.
	f.FakeClient.preReturn = nil
	res, err = w.RunBackstop(ctx, f.BuildingGen)
	require.NoError(t, err, "RunBackstop recovery")
	assert.Equal(t, 1, res.Succeeded, "raced row re-embedded on recovery")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"coverage complete after recovery")
}

// TestWorker_CASRepairRace_OldCodeWouldFail proves the OLD behavior was
// buggy: an UNCONDITIONAL stamp (the pre-fix Store.SetEmbedGen) applied after
// the same race marks the row covered-with-stale-content — exactly the defect
// the CAS fix removes.
func TestWorker_CASRepairRace_OldCodeWouldFail(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)
	setBaselineLM(t, f.MainDB, 1)

	// Simulate the worker having read content (token captured), then the race
	// edit landing (body rewrite bumps last_modified; embed_gen reset to NULL).
	_, err := f.MainDB.Exec(
		`UPDATE message_bodies SET body_text = 'corrected content' WHERE message_id = 1`)
	require.NoError(t, err, "race: rewrite body")
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 1`)
	require.NoError(t, err, "race: reset embed_gen")

	// OLD path: unconditional stamp ignores last_modified and WRONGLY marks
	// the row covered despite the post-read content change.
	require.NoError(t, f.Store.SetEmbedGen(ctx, []int64{1}, int64(f.BuildingGen)),
		"old unconditional stamp")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"OLD code: row wrongly marked covered (the bug)")

	// NEW path: a CAS stamp with the STALE token (captured before the race)
	// does NOT mark it covered — the desired behavior.
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 1`)
	require.NoError(t, err, "reset for CAS check")
	staleToken := "2000-01-01 00:00:00"
	missed, err := f.Store.SetEmbedGenIfUnchanged(ctx,
		stamps(1, staleToken), int64(f.BuildingGen))
	require.NoError(t, err, "CAS with stale token")
	assert.Equal(t, []int64{1}, missed, "stale-token CAS returns the missed id")
	assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"NEW code: CAS with stale token leaves row needing embedding")
}

// TestWorker_CASNormalPath verifies the happy path: when last_modified is
// unchanged between read and stamp, the CAS stamp succeeds and embed_gen is
// set — and the trigger bumping last_modified as a side effect of the stamp's
// own UPDATE does not break it (the WHERE matches the pre-trigger value).
func TestWorker_CASNormalPath(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 3)

	w := newTestWorker(f, 3)
	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 3, res.Succeeded, "all embedded")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"all stamped via CAS normal path")

	// Every row is stamped to the building gen.
	for id := int64(1); id <= 3; id++ {
		v, isNull := embedGenOf(t, f.MainDB, id)
		assert.False(t, isNull, "msg %d stamped", id)
		assert.Equal(t, int64(f.BuildingGen), v, "msg %d embed_gen", id)
	}
}

// TestWorker_CASMissAccounting is the focused accounting regression for the
// "surface CAS misses" change: in a batch where ONE row is raced (its
// last_modified moves between read and stamp) and the others are not, the
// worker must (a) NOT count the missed row in Succeeded, (b) LOG the missed id,
// (c) still ADVANCE the watermark to batchMax (no head-of-line block), and (d)
// recover the missed row on a subsequent RunBackstop.
func TestWorker_CASMissAccounting(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 2)

	// Pin both rows' last_modified to a fixed far-past token (= what the worker
	// captures at read time) so the raced bump is guaranteed to differ.
	setBaselineLM(t, f.MainDB, 1)
	setBaselineLM(t, f.MainDB, 2)

	// Inject the race for ONLY message 1: after the worker scanned + read both
	// rows' content (capturing last_modified), rewrite msg 1's body (bumps its
	// last_modified via trigger; CAS for id 1 will miss) and reset its
	// embed_gen. Message 2 is untouched and stamps normally.
	f.FakeClient.preReturn = func() {
		_, err := f.MainDB.Exec(
			`UPDATE message_bodies SET body_text = 'corrected content' WHERE message_id = 1`)
		require.NoError(t, err, "race: rewrite body of msg 1")
		_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 1`)
		require.NoError(t, err, "race: reset embed_gen of msg 1")
	}

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// Batch size 2 so both ids are read and stamped in one batch (one CAS miss,
	// one success).
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Store:     f.Store,
		Client:    f.FakeClient,
		BatchSize: 2,
		Log:       logger,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(t, err, "RunOnce")

	// (a) Only the non-raced row counts as succeeded; the CAS miss does not.
	assert.Equal(t, 1, res.Succeeded, "only the non-raced row counts as Succeeded")

	// (b) The missed id is logged.
	logs := logbuf.String()
	assert.Contains(t, logs, "embed_gen CAS misses", "CAS miss is logged")
	assert.Contains(t, logs, "count=1", "logs the miss count")

	// (c) The watermark advanced to batchMax (id 2) despite the miss — the
	// drain does not stick on the missed row.
	assert.Equal(t, int64(2), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)),
		"watermark advances to batchMax despite the CAS miss")

	// The raced row (1) is still missing; the clean row (2) is covered.
	_, isNull := embedGenOf(t, f.MainDB, 1)
	assert.True(t, isNull, "raced row 1 still needs embedding")
	v2, isNull2 := embedGenOf(t, f.MainDB, 2)
	assert.False(t, isNull2, "clean row 2 stamped")
	assert.Equal(t, int64(f.BuildingGen), v2, "row 2 embed_gen")
	assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"exactly the raced row remains")

	// (d) A backstop pass (scans from 0, ignoring the watermark) recovers the
	// CAS-missed row with its corrected content.
	f.FakeClient.preReturn = nil
	bres, err := w.RunBackstop(ctx, f.BuildingGen)
	require.NoError(t, err, "RunBackstop recovery")
	assert.Equal(t, 1, bres.Succeeded, "backstop re-embeds the CAS-missed row")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"coverage complete after backstop")
}

// TestWorker_CASSelfBumpDoesNotBlockStamp pins the self-bump invariant: the
// stamp UPDATE itself fires the AFTER-UPDATE trigger and bumps last_modified,
// but because the WHERE compares the PRE-trigger value the stamp still
// matches its row. Verified directly against the store CAS method.
func TestWorker_CASSelfBumpDoesNotBlockStamp(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)
	token := setBaselineLM(t, f.MainDB, 1)

	missed, err := f.Store.SetEmbedGenIfUnchanged(ctx,
		stamps(1, token), int64(f.BuildingGen))
	require.NoError(t, err, "CAS stamp")
	assert.Empty(t, missed, "self-bump stamp succeeds (no CAS miss)")

	v, isNull := embedGenOf(t, f.MainDB, 1)
	require.False(t, isNull, "row stamped despite self-bump")
	assert.Equal(t, int64(f.BuildingGen), v, "embed_gen set")
	// The stamp's own UPDATE bumped last_modified off the baseline.
	assert.NotEqual(t, token, lmOf(t, f.MainDB, 1), "self-bump moved last_modified")
}

// TestWorker_Downshift_EmptySkipCASMissNotSkippedPastWatermark is the
// fail-on-regression for the empty/skip-mark contiguity bug (Codex 129h
// follow-up). Within a singleton drain, an EMPTY singleton (id 1) CAS-MISSES
// its skip-mark (a concurrent edit moved last_modified between the worker's
// content read and the stamp), and a later sibling (id 2) returns a genuine 4xx
// while NOTHING embeds — so the drain takes the all-drop error/return path and
// the caller advances the watermark to the drain's safeAdvanceID
// (contiguousStampedID).
//
// PRE-FIX: the empty/skip branch advanced contiguousStampedID to the empty
// singleton's id gated ONLY on !brokeContiguity — it ignored whether the
// skip-mark actually stamped. A CAS-missed (unstamped) empty singleton therefore
// extended the contiguous-stamped prefix, so the error-path safeAdvanceID
// skipped PAST it: the watermark advanced to id 1, and a subsequent NORMAL
// RunOnce (id > watermark) no longer re-found the unstamped row — only the
// backstop's full scan from 0 could recover it (backstop-only recovery).
//
// POST-FIX: the branch mirrors the embed branch — it advances the prefix only
// when the skip-mark ACTUALLY stamped, else latches brokeContiguity. The
// CAS-missed empty singleton breaks the prefix, so safeAdvanceID stays below it;
// the watermark does not skip past it and a normal RunOnce re-finds it.
func TestWorker_Downshift_EmptySkipCASMissNotSkippedPastWatermark(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 2)

	// Make msg 1 EMPTY (no subject, blank body) so embedBatch reports it in
	// `empty` and the singleton drain takes the len(eb.chunks)==0 skip branch.
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = NULL WHERE id = 1`)
	require.NoError(t, err, "null subject of msg 1")
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = '' WHERE message_id = 1`)
	require.NoError(t, err, "blank body of msg 1")

	// Force the downshift, then a genuine 4xx for msg 2 with NOTHING embedded:
	//   - the whole-batch embedBatch call (msg 2's chunk; msg 1 is empty) 4xxs;
	//   - singleton msg 1 is empty → no Embed call → skip branch (CAS misses);
	//   - singleton msg 2 4xxs → deferred; embeddedOK stays 0 → all-drop return.
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		return nil, fmt.Errorf("embed: HTTP 400: blocked content: %w", ErrPermanent4xx)
	}
	missOnce := true

	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              2,
		MaxConsecutiveFailures: 1, // abort after the single all-drop failure (no busy re-scan)
		beforeSkipStamp: func(ctx context.Context, ids []int64) {
			if !missOnce {
				return
			}
			for _, id := range ids {
				if id != 1 {
					continue
				}
				missOnce = false
				_, err := f.MainDB.ExecContext(ctx,
					`UPDATE messages SET last_modified = '2099-01-01 00:00:00' WHERE id = ?`, id)
				require.NoError(t, err, "force skip CAS miss")
				return
			}
		},
	})

	// The drain is an all-drop (embeddedOK==0): RunOnce returns the wrapped
	// ErrPermanent4xx without advancing past the unstamped rows.
	_, err = w.RunOnce(ctx, f.BuildingGen)
	require.Error(t, err, "all-drop drain surfaces an error")

	// THE INVARIANT: the watermark must NOT skip past the CAS-missed empty
	// singleton (id 1). Pre-fix it advanced to 1 (stranding the row); post-fix
	// it stays below 1 so a normal scan (id > watermark) re-finds it.
	assert.Less(t, readWatermark(t, f.VectorsDB, int64(f.BuildingGen)), int64(1),
		"watermark must not skip past the CAS-missed empty singleton")

	// The empty singleton was NOT stamped (its skip-mark CAS-missed) and so
	// still needs embedding — recoverable.
	_, isNull1 := embedGenOf(t, f.MainDB, 1)
	assert.True(t, isNull1, "CAS-missed empty singleton (msg 1) left unstamped")

	// Concrete proof of re-discovery by a NORMAL (non-backstop) scan: with the
	// 4xx cleared and the race no longer firing, a plain RunOnce re-finds msg 1
	// (id > watermark) and skip-marks it. This is the behavior the bug broke —
	// pre-fix the watermark sat at 1 and a normal RunOnce scanned id > 1 only,
	// so msg 1 was reachable solely via the backstop.
	f.FakeClient.OnEmbed = nil
	_, err = w.RunOnce(ctx, f.BuildingGen)
	require.NoError(t, err, "follow-up normal RunOnce")
	_, isNull1After := embedGenOf(t, f.MainDB, 1)
	assert.False(t, isNull1After, "normal RunOnce re-found and skip-marked msg 1 (not backstop-only)")
}

// TestWorker_Downshift_CASMissNotAllDrop is the fail-on-regression for the
// downshift all-drop misclassification (Codex 129h). Within a singleton drain,
// one message genuinely returns a permanent 4xx while ANOTHER embeds + upserts
// successfully but CAS-MISSES its stamp (a concurrent content edit bumped
// last_modified between the worker's read and its stamp).
//
// PRE-FIX: downshiftDrain counted only CAS-STAMPED singletons toward
// `embedded`, and classified endpoint health on `embedded > 0`. The
// embedded-but-CAS-missed singleton contributed 0 to `embedded`, so with a
// genuine-4xx sibling the drain saw embedded==0 and misclassified a HEALTHY
// endpoint as an endpoint-wide all-drop: it left the genuine 4xx UNSTAMPED and
// returned the wrapped ErrPermanent4xx. RunOnce then did NOT reset
// consecutiveFailures (it keyed on embedded>0) nor advance the cursor, so the
// next scan re-found the same batch, re-downshifted, and tripped the
// consecutive-failure cap — a SPURIOUS abort of an otherwise-fine endpoint.
//
// POST-FIX: the drain tracks embeddedOK (successful embed+upsert regardless of
// the CAS outcome) and classifies endpoint health on it, so the genuine 4xx is
// treated as a message-specific drop (stamped), the failure cap is reset, and
// RunOnce completes without aborting. The CAS-missed row stays recoverable
// (embed_gen still NULL, picked up by the backstop).
func TestWorker_Downshift_CASMissNotAllDrop(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 2)

	// Pin msg 1's last_modified to a fixed far-past token so the mid-embed
	// body rewrite is guaranteed to bump it to a different value (the CAS
	// miss). msg 2 is the genuine 4xx; its last_modified does not matter
	// (the 4xx path stamps it unconditionally).
	setBaselineLM(t, f.MainDB, 1)

	// Downshift orchestration:
	//   - the whole-batch call (len(inputs) > 1) 4xxs, forcing the downshift;
	//   - singleton msg 1 (text contains "body 1") embeds OK, but inside the
	//     embed call we rewrite its body — bumping last_modified via the
	//     trigger so the worker's subsequent CAS stamp MISSES;
	//   - singleton msg 2 (text contains "body 2") returns a genuine 4xx.
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("embed: HTTP 400: batch too long: %w", ErrPermanent4xx)
		}
		if strings.Contains(inputs[0], "body 2") {
			return nil, fmt.Errorf("embed: HTTP 400: blocked content: %w", ErrPermanent4xx)
		}
		// msg 1: race a content edit in BETWEEN the worker's read (which
		// captured last_modified) and its stamp, so the CAS stamp misses.
		_, err := f.MainDB.Exec(
			`UPDATE message_bodies SET body_text = 'corrected content' WHERE message_id = 1`)
		require.NoError(t, err, "race: rewrite body of msg 1")
		v := make([]float32, f.FakeClient.dim)
		v[0] = 1
		return [][]float32{v}, nil
	}

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// MaxConsecutiveFailures=2 so the spurious abort would trip quickly under
	// the pre-fix logic (the all-drop misclassification re-occurs every scan).
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              2,
		MaxConsecutiveFailures: 2,
		Log:                    logger,
	})

	// (a)+(b): RunOnce must NOT abort — the genuine 4xx is a message-specific
	// drop, not an endpoint-wide all-drop, and the failure cap is reset because
	// the endpoint embedded something (embeddedOK > 0).
	_, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(t, err, "RunOnce must not abort (healthy endpoint, not an all-drop)")

	// The genuine-4xx row (msg 2) was stamp-dropped, NOT left unstamped.
	v2, isNull2 := embedGenOf(t, f.MainDB, 2)
	assert.False(t, isNull2, "genuine 4xx row (msg 2) stamp-dropped (message-specific)")
	assert.Equal(t, int64(f.BuildingGen), v2, "msg 2 embed_gen = target")

	// (c): the CAS-missed row (msg 1) is NOT stamped — it remains recoverable
	// (embed_gen still NULL), to be picked up by the backstop. The drain must
	// not have stranded it as "covered".
	_, isNull1 := embedGenOf(t, f.MainDB, 1)
	assert.True(t, isNull1, "CAS-missed row (msg 1) left unstamped (recoverable)")

	// Exactly the CAS-missed row remains needing embedding.
	assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"only the CAS-missed row remains")

	// The CAS miss was logged (proves the race really happened and was handled
	// as a miss, not silently stamped).
	assert.Contains(t, logbuf.String(), "embed_gen CAS misses", "CAS miss logged")

	// Recovery: the backstop (full scan from 0, ignoring the watermark)
	// re-embeds the CAS-missed row with its corrected content.
	f.FakeClient.OnEmbed = nil
	bres, err := w.RunBackstop(ctx, f.BuildingGen)
	require.NoError(t, err, "RunBackstop recovery")
	assert.Equal(t, 1, bres.Succeeded, "backstop re-embeds the CAS-missed row")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"coverage complete after backstop")
}
