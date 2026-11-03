package metawebhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSpool(t *testing.T) (*Spool, *time.Time) {
	t.Helper()
	require := require.New(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s, err := OpenSpool(t.TempDir(), 1<<20, func() time.Time { return now })
	require.NoError(err)
	return s, &now
}
func accept(t *testing.T, s *Spool, body string) string {
	t.Helper()
	require := require.New(t)
	id, dup, err := s.Accept("9", []byte(body))
	require.NoError(err)
	require.False(dup)
	return id
}
func TestSpoolAcceptDurableLayout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	body := []byte(`{"object":"page","entry":[]}`)
	id, dup, err := s.Accept("9", body)
	require.NoError(err)
	assert.False(dup)
	hash := sha256.Sum256(append([]byte("9\x00"), body...))
	assert.Equal(hex.EncodeToString(hash[:]), id)
	for _, dir := range []string{"tmp", "pending", "held", "failed", "done"} {
		info, err := os.Stat(filepath.Join(s.dir, "9", dir))
		require.NoError(err)
		if runtime.GOOS != "windows" {
			assert.Equal(os.FileMode(0700), info.Mode().Perm())
		}
	}
	info, err := os.Stat(filepath.Join(s.dir, "9", "pending", id+".json"))
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
	}
	got, err := s.Body("9", id)
	require.NoError(err)
	assert.Equal(body, got)
	var wire map[string]any
	stateBytes, err := os.ReadFile(filepath.Join(s.dir, "9", "pending", id+".state.json"))
	require.NoError(err)
	require.NoError(json.Unmarshal(stateBytes, &wire))
	assert.Contains(wire, "receivedAt")
	assert.Contains(wire, "nextAttemptAt")
	assert.Contains(wire, "attempts")
	due, err := s.Due(10)
	require.NoError(err)
	require.Len(due, 1)
	assert.Equal(StatePending, due[0].State)
	_, _, err = s.Accept("../9", body)
	require.Error(err)
	_, err = s.Body("9", "../body")
	require.Error(err)
}

func TestSpoolRefusesUnsyncableRootParent(t *testing.T) {
	require := require.New(t)
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory read permission enforcement")
	}
	parent := filepath.Join(t.TempDir(), "parent")
	require.NoError(os.Mkdir(parent, 0300))
	t.Cleanup(func() {
		require.NoError(os.Chmod(parent, 0700))
	})
	// Creation is permitted, but the parent cannot be opened for fsync. The
	// receiver must refuse startup rather than acknowledge a non-durable root.
	_, err := OpenSpool(filepath.Join(parent, "meta-webhooks"), 1<<20, time.Now)
	require.Error(err)
	require.ErrorIs(err, os.ErrPermission)
}
func TestSpoolAcceptDuplicate(t *testing.T) {
	for _, state := range []State{StatePending, StateHeld, StateFailed, StateDone} {
		t.Run(string(state), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, _ := testSpool(t)
			id := accept(t, s, "synthetic")
			require.NoError(s.Settle("9", id, Settlement{State: state}))
			got, dup, err := s.Accept("9", []byte("synthetic"))
			require.NoError(err)
			assert.True(dup)
			assert.Equal(id, got)
		})
	}
}
func TestSpoolFull(t *testing.T) {
	require := require.New(t)
	s, err := OpenSpool(t.TempDir(), 3, time.Now)
	require.NoError(err)
	_, _, err = s.Accept("9", []byte("four"))
	require.ErrorIs(err, ErrSpoolFull)
}
func TestSpoolSettleTransitions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	for i := 1; i <= 12; i++ {
		require.NoError(s.Settle("9", id, Settlement{State: StatePending, CountAttempt: true, ErrorClass: "permission_missing"}))
		*now = now.Add(time.Hour)
		due, err := s.Due(10)
		require.NoError(err)
		if i < 12 {
			require.Len(due, 1)
			assert.Equal(i, due[0].Attempts)
		} else {
			assert.Empty(due)
		}
	}
	stats, err := s.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Failed)
}
func TestSpoolBackoffSchedule(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	for _, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour} {
		require.NoError(s.Settle("9", id, Settlement{State: StatePending, CountAttempt: true}))
		due, err := s.Due(1)
		require.NoError(err)
		assert.Empty(due)
		*now = now.Add(delay - time.Nanosecond)
		due, err = s.Due(1)
		require.NoError(err)
		assert.Empty(due)
		*now = now.Add(time.Nanosecond)
		due, err = s.Due(1)
		require.NoError(err)
		require.Len(due, 1)
		assert.Equal(*now, due[0].NextAttemptAt)
	}
}
func TestSpoolLockBusyDoesNotCountAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(s.Settle("9", id, Settlement{State: StatePending, ErrorClass: "sync_active"}))
	*now = now.Add(30 * time.Second)
	due, err := s.Due(1)
	require.NoError(err)
	require.Len(due, 1)
	assert.Zero(due[0].Attempts)
}
func TestSpoolPruneOnlyDone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	for _, state := range []State{StateHeld, StateFailed, StateDone} {
		id := accept(t, s, string(state))
		require.NoError(s.Settle("9", id, Settlement{State: state}))
	}
	*now = now.Add(721 * time.Hour)
	n, err := s.Prune(720 * time.Hour)
	require.NoError(err)
	assert.Equal(1, n)
	stats, err := s.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Held)
	assert.Equal(1, stats["9"].Failed)
}
func TestSpoolRecoversTmpLeftover(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	accept(t, s, "synthetic")
	file := filepath.Join(s.dir, "9", "tmp", "partial")
	require.NoError(os.WriteFile(file, []byte("partial"), 0600))
	_, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
	require.NoError(err)
	_, err = os.Stat(file)
	assert.True(os.IsNotExist(err))
}
func TestSpoolRecoversBodyWithoutState(t *testing.T) {
	for _, state := range []State{StatePending, StateHeld, StateFailed, StateDone} {
		t.Run(string(state), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, now := testSpool(t)
			id := accept(t, s, "synthetic")
			require.NoError(s.Settle("9", id, Settlement{State: state}))
			require.NoError(os.Remove(filepath.Join(s.dir, "9", string(state), id+".state.json")))
			s, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
			require.NoError(err)
			due, err := s.Due(1)
			require.NoError(err)
			require.Len(due, 1)
			assert.Zero(due[0].Attempts)
			body, err := s.Body("9", id)
			require.NoError(err)
			assert.Equal("synthetic", string(body))
		})
	}
}
func TestSpoolRecoversStateWithoutBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(os.Remove(filepath.Join(s.dir, "9", "pending", id+".json")))
	_, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
	require.NoError(err)
	_, err = os.Stat(filepath.Join(s.dir, "9", "pending", id+".state.json"))
	assert.True(os.IsNotExist(err))
}
func TestSpoolRequeueFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(s.Settle("9", id, Settlement{State: StateHeld, Outcomes: map[string]int{"page/messages/unhandled": 1}}))
	for _, f := range []RequeueFilter{{AppID: "8"}, {DeliveryID: hex.EncodeToString(bytes.Repeat([]byte{1}, 32))}, {States: []State{StateFailed}}, {Since: now.Add(time.Second)}} {
		n, err := s.Requeue(f, false)
		require.NoError(err)
		assert.Zero(n)
	}
	f := RequeueFilter{AppID: "9", States: []State{StateHeld}, DeliveryID: id}
	n, err := s.Requeue(f, true)
	require.NoError(err)
	assert.Equal(1, n)
	due, err := s.Due(1)
	require.NoError(err)
	assert.Empty(due)
	n, err = s.Requeue(f, false)
	require.NoError(err)
	assert.Equal(1, n)
	due, err = s.Due(1)
	require.NoError(err)
	require.Len(due, 1)
	assert.Empty(due[0].Outcomes)
}
func TestSpoolRequeueRequestRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(s.Settle("9", id, Settlement{State: StateHeld}))
	for _, dry := range []bool{true, false} {
		request, err := SubmitRequeue(s.dir, RequeueFilter{AppID: "9", States: []State{StateHeld}}, dry)
		require.NoError(err)
		require.NoError(s.ConsumeRequests())
		n, err := AwaitRequeueResult(t.Context(), s.dir, request)
		require.NoError(err)
		assert.Equal(1, n)
		due, err := s.Due(1)
		require.NoError(err)
		if dry {
			assert.Empty(due)
		} else {
			assert.Len(due, 1)
		}
	}
}
func TestAwaitRequeueResultTimesOut(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := AwaitRequeueResult(ctx, t.TempDir(), "0123456789abcdef0123456789abcdef")
	require.ErrorIs(err, context.Canceled)
}
func TestReceiverSnapshotAtomicAndSecretFree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	snap := ReceiverSnapshot{Listener: "enabled", Addr: "127.0.0.1:9000", Counters: Counters{Accepted: 1}, WrittenAt: time.Now().UTC()}
	require.NoError(WriteReceiverSnapshot(root, snap))
	got, err := ReadReceiverSnapshot(root)
	require.NoError(err)
	assert.Equal(snap, got)
	info, err := os.Stat(filepath.Join(root, "receiver.json"))
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
	}
}
func TestReadStatsToleratesConcurrentMoves(t *testing.T) {
	assert := assert.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 30 {
			assert.NoError(s.Settle("9", id, Settlement{State: StateHeld}))
			assert.NoError(s.Settle("9", id, Settlement{State: StatePending}))
		}
	}()
	for range 30 {
		_, err := ReadStats(s.dir)
		assert.NoError(err)
	}
	<-done
}
func TestSpoolStats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	accept(t, s, "pending")
	id := accept(t, s, "held")
	require.NoError(s.Settle("9", id, Settlement{State: StateHeld, Outcomes: map[string]int{"page/messages/unhandled": 1}}))
	stats, err := s.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Pending)
	assert.Equal(1, stats["9"].Held)
	assert.Equal(*now, stats["9"].OldestPending)
	assert.Positive(stats["9"].Bytes)
	assert.Equal(1, stats["9"].HeldOutcomes["page/messages/unhandled"])
}

func TestSpoolDuplicateRepairsMissingStateBeforeAck(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	path := filepath.Join(s.dir, "9", "pending", id+".state.json")
	require.NoError(os.Remove(path))
	got, dup, err := s.Accept("9", []byte("synthetic"))
	require.NoError(err)
	assert.True(dup)
	assert.Equal(id, got)
	_, err = os.Stat(path)
	require.NoError(err)
}

func TestSpoolCachedCapacityCountsNewDeliveries(t *testing.T) {
	require := require.New(t)
	s, now := testSpool(t)
	accept(t, s, "first")
	stats, err := s.Stats()
	require.NoError(err)
	s.maxBytes = stats["9"].Bytes + 64
	// Prove the next delivery fits on its own. The frozen clock keeps the
	// original spool's cached accounting active for the combined admission.
	control, err := OpenSpool(t.TempDir(), s.maxBytes, func() time.Time { return *now })
	require.NoError(err)
	accept(t, control, "other")
	_, _, err = s.Accept("9", []byte("other"))
	require.ErrorIs(err, ErrSpoolFull)
}

func TestSpoolReceiptOrderSurvivesPruneRestartAndReplay(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	s, now := testSpool(t)
	first := accept(t, s, "first")
	due, err := s.Due(-1)
	require.NoError(err)
	require.Len(due, 1)
	original := due[0]
	require.NoError(s.Settle("9", first, Settlement{State: StateDone}))
	filter := RequeueFilter{DeliveryID: first}
	n, err := s.Requeue(filter, true)
	require.NoError(err)
	require.Equal(1, n)
	n, err = s.Requeue(filter, false)
	require.NoError(err)
	require.Equal(1, n)
	due, err = s.Due(-1)
	require.NoError(err)
	require.Len(due, 1)
	assert.Greater(due[0].ReceiptOrder, original.ReceiptOrder)
	assert.Equal(original.ReceivedAt, due[0].ReceivedAt)
	replayOrder := due[0].ReceiptOrder
	_, duplicate, err := s.Accept("9", []byte("first"))
	require.NoError(err)
	assert.True(duplicate)
	due, err = s.Due(-1)
	require.NoError(err)
	assert.Equal(replayOrder, due[0].ReceiptOrder)
	require.NoError(s.Settle("9", first, Settlement{State: StateDone}))
	*now = now.Add(2 * time.Hour)
	n, err = s.Prune(time.Hour)
	require.NoError(err)
	require.Equal(1, n)
	s, err = OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
	require.NoError(err)
	accept(t, s, "after prune")
	due, err = s.Due(-1)
	require.NoError(err)
	require.Len(due, 1)
	assert.Greater(due[0].ReceiptOrder, replayOrder)
}

func TestSpoolRecoversReceiptMetadataAcrossInterruptedMoves(t *testing.T) {
	for _, from := range states {
		t.Run(string(from), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			s, now := testSpool(t)
			id := accept(t, s, "older")
			require.NoError(s.Settle("9", id, Settlement{State: from}))
			var original DeliveryMeta
			require.NoError(readJSON(statePath(s.path("9", from, id)), &original))
			later := accept(t, s, "later")
			to := StatePending
			if from == StatePending {
				to = StateHeld
			}
			// The body rename is durable while its sidecar is still in the old state.
			require.NoError(os.Rename(s.path("9", from, id), s.path("9", to, id)))
			*now = now.Add(time.Minute)
			reopened, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
			require.NoError(err)
			due, err := reopened.Due(-1)
			require.NoError(err)
			require.Len(due, 2)
			assert.Equal(id, due[0].ID)
			assert.Equal(later, due[1].ID)
			assert.Equal(original.ReceiptOrder, due[0].ReceiptOrder)
			assert.Equal(original.ReceivedAt, due[0].ReceivedAt)
			assert.Equal(original.Attempts, due[0].Attempts)
		})
	}
}

func TestSpoolMetadataReaderAllowsReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	path := statePath(s.path("9", StatePending, id))
	reader, err := openSpoolRead(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	// Keep the actual production read handle open across replacement and move.
	require.NoError(s.Settle("9", id, Settlement{State: StateHeld}))
	var old DeliveryMeta
	data, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(json.Unmarshal(data, &old))
	assert.Equal(StatePending, old.State)
	stats, err := ReadStats(s.dir)
	require.NoError(err)
	assert.Equal(1, stats["9"].Held)
}
