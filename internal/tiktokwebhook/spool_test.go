package tiktokwebhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpoolAcceptanceCountsRetainedFileSizes(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	require.NoError(t, err)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	spool.now = func() time.Time { return now }
	var used int64
	for _, state := range []string{"pending", "done", "held", "failed"} {
		body := []byte(state)
		id, _, err := spool.Accept(t.Context(), "app-example", body)
		require.NoError(t, err)
		delivery := Delivery{ID: id, App: "app-example", Body: body, ReceivedAt: now, State: state}
		if state == "done" {
			delivery.Outcome = "applied"
		}
		if state == "held" {
			delivery.Outcome = "unrouted"
		}
		if state == "failed" {
			delivery.Attempts, delivery.Outcome = 12, "processing_failed"
			delivery.NextAttempt = now.Add(time.Minute)
		}
		require.NoError(t, spool.write(delivery))
		info, err := os.Stat(spool.path(id))
		require.NoError(t, err)
		used += info.Size()
	}
	// A lock file, unpublished temporary file, and directory are not records.
	require.NoError(t, os.WriteFile(filepath.Join(spool.root, ".stage-example"), []byte("staged"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(spool.root, "not-a-delivery.json"), []byte("other"), 0600))
	require.NoError(t, os.Mkdir(spool.path(strings.Repeat("a", 64)), 0700))
	got, err := spool.bytesUsed()
	require.NoError(t, err)
	assert.Equal(t, used, got)

	body := []byte("next")
	sum := sha256.Sum256(append([]byte("app-example\x00"), body...))
	id := hex.EncodeToString(sum[:])
	data, err := json.Marshal(Delivery{ID: id, App: "app-example", Body: body, ReceivedAt: now, State: "pending"})
	require.NoError(t, err)
	spool.maximum = used + int64(len(data)) + 1024 - 1
	_, duplicate, err := spool.Accept(t.Context(), "app-example", body)
	require.ErrorIs(t, err, ErrSpoolFull)
	assert.False(t, duplicate)
	_, err = os.Stat(spool.path(id))
	require.ErrorIs(t, err, os.ErrNotExist)
	spool.maximum++
	gotID, duplicate, err := spool.Accept(t.Context(), "app-example", body)
	require.NoError(t, err)
	assert.False(t, duplicate)
	assert.Equal(t, id, gotID)
	got, err = spool.bytesUsed()
	require.NoError(t, err)
	assert.Equal(t, used+int64(len(data)), got)
}

func TestSpoolDuplicateSyncsDirectoryWithoutChangingDelivery(t *testing.T) {
	for _, state := range []string{"pending", "held", "failed", "done"} {
		t.Run(state, func(t *testing.T) {
			spool, err := OpenSpool(t.TempDir(), 1<<20)
			require.NoError(t, err)
			body := []byte("synthetic delivery")
			id, _, err := spool.Accept(t.Context(), "app-example", body)
			require.NoError(t, err)
			now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			delivery := Delivery{ID: id, App: "app-example", Body: body, ReceivedAt: now, State: state}
			switch state {
			case "pending", "failed":
				delivery.Attempts, delivery.Outcome = 1, "processing_failed"
				if state == "failed" {
					delivery.Attempts = 12
				}
				delivery.NextAttempt = now.Add(time.Minute)
			case "held":
				delivery.Outcome = "unrouted"
			case "done":
				delivery.Outcome = "applied"
			}
			require.NoError(t, spool.write(delivery))
			before, err := os.ReadFile(spool.path(id))
			require.NoError(t, err)
			// A published record can remain visible after a directory-sync
			// error. A retry must not acknowledge it while that error persists.
			failure := errors.New("synthetic directory sync failure")
			calls := 0
			spool.syncDir = func(path string) error {
				assert.Equal(t, spool.root, path)
				calls++
				if calls == 1 {
					return failure
				}
				return syncDir(path)
			}
			// Existing records remain retryable even when no capacity remains.
			spool.maximum = 1
			for attempt := range 2 {
				gotID, duplicate, err := spool.Accept(t.Context(), "app-example", body)
				if attempt == 0 {
					require.ErrorIs(t, err, failure)
				} else {
					require.NoError(t, err)
				}
				assert.True(t, duplicate)
				assert.Equal(t, id, gotID)
				after, err := os.ReadFile(spool.path(id))
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
			assert.Equal(t, 2, calls)
		})
	}
}
