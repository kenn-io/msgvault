package metawebhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSpool(t *testing.T) (*Spool, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s, err := OpenSpool(t.TempDir(), 1<<20, func() time.Time { return now })
	require.NoError(t, err)
	return s, &now
}
func accept(t *testing.T, s *Spool, body string) string {
	t.Helper()
	id, dup, err := s.Accept("9", []byte(body))
	require.NoError(t, err)
	require.False(t, dup)
	return id
}
func TestSpoolAcceptDurableLayout(t *testing.T) {
	s, _ := testSpool(t)
	body := []byte(`{"object":"page","entry":[]}`)
	id, dup, err := s.Accept("9", body)
	require.NoError(t, err)
	assert.False(t, dup)
	hash := sha256.Sum256(append([]byte("9\x00"), body...))
	assert.Equal(t, hex.EncodeToString(hash[:]), id)
	for _, dir := range []string{"tmp", "pending", "held", "failed", "done"} {
		info, err := os.Stat(filepath.Join(s.dir, "9", dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
	}
	info, err := os.Stat(filepath.Join(s.dir, "9", "pending", id+".json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	got, err := s.Body("9", id)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	var wire map[string]any
	stateBytes, err := os.ReadFile(filepath.Join(s.dir, "9", "pending", id+".state.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(stateBytes, &wire))
	assert.Contains(t, wire, "receivedAt")
	assert.Contains(t, wire, "nextAttemptAt")
	assert.Contains(t, wire, "attempts")
	due, err := s.Due(10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, StatePending, due[0].State)
	_, _, err = s.Accept("../9", body)
	assert.Error(t, err)
	_, err = s.Body("9", "../body")
	assert.Error(t, err)
}
func TestSpoolAcceptDuplicate(t *testing.T) {
	for _, state := range []State{StatePending, StateHeld, StateFailed, StateDone} {
		t.Run(string(state), func(t *testing.T) {
			s, _ := testSpool(t)
			id := accept(t, s, "synthetic")
			require.NoError(t, s.Settle("9", id, Settlement{State: state}))
			got, dup, err := s.Accept("9", []byte("synthetic"))
			require.NoError(t, err)
			assert.True(t, dup)
			assert.Equal(t, id, got)
		})
	}
}
func TestSpoolFull(t *testing.T) {
	s, err := OpenSpool(t.TempDir(), 3, time.Now)
	require.NoError(t, err)
	_, _, err = s.Accept("9", []byte("four"))
	assert.ErrorIs(t, err, ErrSpoolFull)
}
func TestSpoolSettleTransitions(t *testing.T) {
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	for i := 1; i <= 12; i++ {
		require.NoError(t, s.Settle("9", id, Settlement{State: StatePending, CountAttempt: true, ErrorClass: "permission_missing"}))
		*now = now.Add(time.Hour)
		due, err := s.Due(10)
		require.NoError(t, err)
		if i < 12 {
			require.Len(t, due, 1)
			assert.Equal(t, i, due[0].Attempts)
		} else {
			assert.Empty(t, due)
		}
	}
	stats, err := s.Stats()
	require.NoError(t, err)
	assert.Equal(t, 1, stats["9"].Failed)
}
func TestSpoolBackoffSchedule(t *testing.T) {
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	for _, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour} {
		require.NoError(t, s.Settle("9", id, Settlement{State: StatePending, CountAttempt: true}))
		due, err := s.Due(1)
		require.NoError(t, err)
		assert.Empty(t, due)
		*now = now.Add(delay - time.Nanosecond)
		due, err = s.Due(1)
		require.NoError(t, err)
		assert.Empty(t, due)
		*now = now.Add(time.Nanosecond)
		due, err = s.Due(1)
		require.NoError(t, err)
		require.Len(t, due, 1)
		assert.Equal(t, *now, due[0].NextAttemptAt)
	}
}
func TestSpoolLockBusyDoesNotCountAttempt(t *testing.T) {
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(t, s.Settle("9", id, Settlement{State: StatePending, ErrorClass: "sync_active"}))
	*now = now.Add(30 * time.Second)
	due, err := s.Due(1)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Zero(t, due[0].Attempts)
}
func TestSpoolPruneOnlyDone(t *testing.T) {
	s, now := testSpool(t)
	for _, state := range []State{StateHeld, StateFailed, StateDone} {
		id := accept(t, s, string(state))
		require.NoError(t, s.Settle("9", id, Settlement{State: state}))
	}
	*now = now.Add(721 * time.Hour)
	n, err := s.Prune(720 * time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	stats, err := s.Stats()
	require.NoError(t, err)
	assert.Equal(t, 1, stats["9"].Held)
	assert.Equal(t, 1, stats["9"].Failed)
}
func TestSpoolRecoversTmpLeftover(t *testing.T) {
	s, now := testSpool(t)
	accept(t, s, "synthetic")
	file := filepath.Join(s.dir, "9", "tmp", "partial")
	require.NoError(t, os.WriteFile(file, []byte("partial"), 0600))
	_, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
	require.NoError(t, err)
	_, err = os.Stat(file)
	assert.True(t, os.IsNotExist(err))
}
func TestSpoolRecoversBodyWithoutState(t *testing.T) {
	for _, state := range []State{StatePending, StateHeld, StateFailed, StateDone} {
		t.Run(string(state), func(t *testing.T) {
			s, now := testSpool(t)
			id := accept(t, s, "synthetic")
			require.NoError(t, s.Settle("9", id, Settlement{State: state}))
			require.NoError(t, os.Remove(filepath.Join(s.dir, "9", string(state), id+".state.json")))
			s, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
			require.NoError(t, err)
			due, err := s.Due(1)
			require.NoError(t, err)
			require.Len(t, due, 1)
			assert.Zero(t, due[0].Attempts)
			body, err := s.Body("9", id)
			require.NoError(t, err)
			assert.Equal(t, "synthetic", string(body))
		})
	}
}
func TestSpoolRecoversStateWithoutBody(t *testing.T) {
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(t, os.Remove(filepath.Join(s.dir, "9", "pending", id+".json")))
	_, err := OpenSpool(s.dir, 1<<20, func() time.Time { return *now })
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(s.dir, "9", "pending", id+".state.json"))
	assert.True(t, os.IsNotExist(err))
}
func TestSpoolRequeueFilters(t *testing.T) {
	s, now := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(t, s.Settle("9", id, Settlement{State: StateHeld, Outcomes: map[string]int{"page/messages/unhandled": 1}}))
	for _, f := range []RequeueFilter{{AppID: "8"}, {DeliveryID: hex.EncodeToString(bytes.Repeat([]byte{1}, 32))}, {States: []State{StateFailed}}, {Since: now.Add(time.Second)}} {
		n, err := s.Requeue(f, false)
		require.NoError(t, err)
		assert.Zero(t, n)
	}
	f := RequeueFilter{AppID: "9", States: []State{StateHeld}, DeliveryID: id}
	n, err := s.Requeue(f, true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	due, err := s.Due(1)
	require.NoError(t, err)
	assert.Empty(t, due)
	n, err = s.Requeue(f, false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	due, err = s.Due(1)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Empty(t, due[0].Outcomes)
}
func TestSpoolRequeueRequestRoundTrip(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	require.NoError(t, s.Settle("9", id, Settlement{State: StateHeld}))
	for _, dry := range []bool{true, false} {
		request, err := SubmitRequeue(s.dir, RequeueFilter{AppID: "9", States: []State{StateHeld}}, dry)
		require.NoError(t, err)
		require.NoError(t, s.ConsumeRequests())
		n, err := AwaitRequeueResult(t.Context(), s.dir, request)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		due, err := s.Due(1)
		require.NoError(t, err)
		if dry {
			assert.Empty(t, due)
		} else {
			assert.Len(t, due, 1)
		}
	}
}
func TestAwaitRequeueResultTimesOut(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := AwaitRequeueResult(ctx, t.TempDir(), "0123456789abcdef0123456789abcdef")
	assert.ErrorIs(t, err, context.Canceled)
}
func TestReceiverSnapshotAtomicAndSecretFree(t *testing.T) {
	root := t.TempDir()
	snap := ReceiverSnapshot{Listener: "enabled", Addr: "127.0.0.1:9000", Counters: Counters{Accepted: 1}, WrittenAt: time.Now().UTC()}
	require.NoError(t, WriteReceiverSnapshot(root, snap))
	got, err := ReadReceiverSnapshot(root)
	require.NoError(t, err)
	assert.Equal(t, snap, got)
	info, err := os.Stat(filepath.Join(root, "receiver.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}
func TestReadStatsToleratesConcurrentMoves(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 30 {
			assert.NoError(t, s.Settle("9", id, Settlement{State: StateHeld}))
			assert.NoError(t, s.Settle("9", id, Settlement{State: StatePending}))
		}
	}()
	for range 30 {
		_, err := ReadStats(s.dir)
		assert.NoError(t, err)
	}
	<-done
}
func TestSpoolStats(t *testing.T) {
	s, now := testSpool(t)
	accept(t, s, "pending")
	id := accept(t, s, "held")
	require.NoError(t, s.Settle("9", id, Settlement{State: StateHeld, Outcomes: map[string]int{"page/messages/unhandled": 1}}))
	stats, err := s.Stats()
	require.NoError(t, err)
	assert.Equal(t, 1, stats["9"].Pending)
	assert.Equal(t, 1, stats["9"].Held)
	assert.Equal(t, *now, stats["9"].OldestPending)
	assert.Positive(t, stats["9"].Bytes)
	assert.Equal(t, 1, stats["9"].HeldOutcomes["page/messages/unhandled"])
}

func TestSpoolDuplicateRepairsMissingStateBeforeAck(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	path := filepath.Join(s.dir, "9", "pending", id+".state.json")
	require.NoError(t, os.Remove(path))
	got, dup, err := s.Accept("9", []byte("synthetic"))
	require.NoError(t, err)
	assert.True(t, dup)
	assert.Equal(t, id, got)
	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestSpoolCachedCapacityCountsNewDeliveries(t *testing.T) {
	s, _ := testSpool(t)
	accept(t, s, "first")
	stats, err := s.Stats()
	require.NoError(t, err)
	s.maxBytes = stats["9"].Bytes + 1
	_, _, err = s.Accept("9", []byte("second"))
	assert.ErrorIs(t, err, ErrSpoolFull)
}
