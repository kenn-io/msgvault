package googlereviews

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleCleanupAtExactTTLs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		s, _, _, _ := serviceFixture(t)
		issue(t, s, "shop")
		synctest.Wait()
		time.Sleep(5*time.Minute - time.Nanosecond)
		synctest.Wait()
		s.mu.Lock()
		pending := len(s.pending)
		s.mu.Unlock()
		assert.Equal(1, pending)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		s.mu.Lock()
		pending = len(s.pending)
		receipts := len(s.receipts)
		s.mu.Unlock()
		assert.Zero(pending)
		assert.Equal(1, receipts)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		s.mu.Lock()
		receipts = len(s.receipts)
		s.mu.Unlock()
		assert.Zero(receipts)
	})
}
func TestReceiptCapacityReservedBeforeWrite(t *testing.T) {
	require := require.New(t)
	s, p, _, _ := serviceFixture(t)
	for range 256 {
		preview := issue(t, s, "shop")
		_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
		require.NoError(err)
	}
	require.Empty(s.pending)
	require.Len(s.receipts, 256)
	_, err := s.Execute(t.Context(), "owner", Request{Action: "preview_reply", Target: "shop", ReviewID: "r", Comment: "Thanks"})
	require.ErrorIs(err, ErrCapacity)
	assert.Equal(t, 256, p.puts)
}
