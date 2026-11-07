package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A stale or concurrent confirmer must never acquire a second send permit.
func TestYouTubeReplyReceiptCompareAndSwap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, sourceErr := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/example-owner")
	require.NoError(sourceErr)
	ctx := t.Context()
	key := strings.Repeat("a", 64)
	value, err := st.GetYouTubeReplyReceiptContext(ctx, key)
	require.NoError(err)
	assert.Empty(value)
	ok, err := st.CompareAndSwapYouTubeReplyReceiptContext(ctx, source.ID, key, "", "prepared")
	require.NoError(err)
	require.True(ok)
	ok, err = st.CompareAndSwapYouTubeReplyReceiptContext(ctx, source.ID, key, "", "replacement")
	require.NoError(err)
	assert.False(ok)
	var winners atomic.Int32
	var wg sync.WaitGroup
	updateErrors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			won, updateErr := st.CompareAndSwapYouTubeReplyReceiptContext(ctx, source.ID, key, "prepared", "sending")
			updateErrors <- updateErr
			if won {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	close(updateErrors)
	for updateErr := range updateErrors {
		require.NoError(updateErr)
	}
	assert.Equal(int32(1), winners.Load())
	value, err = st.GetYouTubeReplyReceiptContext(ctx, key)
	require.NoError(err)
	assert.Equal("sending", value)
}

func TestYouTubeReplyReceiptRejectsInvalidInputAndCancellation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, sourceErr := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/example-owner")
	require.NoError(sourceErr)
	for _, key := range []string{"", "schema_version", strings.Repeat("z", 64)} {
		_, err := st.GetYouTubeReplyReceiptContext(t.Context(), key)
		require.Error(err)
		_, err = st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, key, "", "value")
		require.Error(err)
	}
	key := strings.Repeat("b", 64)
	_, err := st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, key, "", "")
	require.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.GetYouTubeReplyReceiptContext(ctx, key)
	require.ErrorIs(err, context.Canceled)
	_, err = st.CompareAndSwapYouTubeReplyReceiptContext(ctx, source.ID, key, "", "value")
	require.ErrorIs(err, context.Canceled)
}

func TestYouTubeReplyInvalidEnvelopeErrorsUsePlainLanguage(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		key := "youtube_reply:v1:" + strings.Repeat("a", 64)
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`), key, `"not-json"`)
		require.NoError(err)
		_, err = st.GetYouTubeReplyReceiptContext(t.Context(), strings.Repeat("a", 64))
		require.EqualError(err, "saved reply is invalid")
	})

	t.Run("source merge", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		from, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/invalid-envelope-source")
		require.NoError(err)
		into, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/valid-envelope-destination")
		require.NoError(err)
		key := "youtube_reply:v1:" + strings.Repeat("b", 64)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`), key, `"not-json"`)
		require.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
		require.Error(err)
		assert.Contains(err.Error(), "invalid saved reply; cannot merge sources")
	})

	t.Run("source removal", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/invalid-envelope-removal")
		require.NoError(err)
		key := "youtube_reply:v1:" + strings.Repeat("c", 64)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`), key, `"not-json"`)
		require.NoError(err)
		_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
		require.Error(err)
		assert.Contains(err.Error(), "invalid saved reply; cannot remove source")
	})
}

func TestMergeSourcesRejectsUnresolvedYouTubeReplyReceipts(t *testing.T) {
	for _, test := range []struct {
		name          string
		state         string
		receiptOnInto bool
		dryRun        bool
		wantReject    bool
	}{
		{name: "prepared source receipt is discarded", state: "prepared"},
		{name: "prepared preview stays active during dry run", state: "prepared", dryRun: true},
		{name: "sending source receipt", state: "sending", wantReject: true},
		{name: "accepted source receipt", state: "accepted", wantReject: true},
		{name: "archived source receipt", state: "archived"},
		{name: "rejected source receipt", state: "rejected"},
		{name: "discarded source receipt", state: "discarded"},
		{name: "accepted destination receipt remains active", state: "accepted", receiptOnInto: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/history-owner")
			require.NoError(err)
			into, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/live-owner")
			require.NoError(err)
			receiptSource := from
			if test.receiptOnInto {
				receiptSource = into
			}
			key := strings.Repeat("1", 64)
			payload := fmt.Sprintf(`{"state":%q}`, test.state)
			won, err := st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), receiptSource.ID, key, "", payload)
			require.NoError(err)
			require.True(won)

			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: from.ID, IntoSourceID: into.ID,
				DryRun: test.dryRun,
			})
			if test.wantReject {
				require.ErrorIs(err, store.ErrSourceMergeInvalid)
				require.ErrorContains(err, "unresolved YouTube reply receipts")
			} else {
				require.NoError(err)
			}
			stored, err := st.GetYouTubeReplyReceiptContext(t.Context(), key)
			require.NoError(err)
			wantStored := payload
			if test.state == "prepared" && !test.receiptOnInto && !test.dryRun {
				wantStored = `{"state":"discarded"}`
			}
			assert.JSONEq(wantStored, stored)
		})
	}
}

// remove-account must erase reply payloads while preserving a non-sendable
// fingerprint tombstone, including for drafts which were never public.
func TestYouTubeReplyReceiptRemovalRedactsPayload(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		for _, state := range []string{"prepared", "sending", "accepted", "archived", "rejected"} {
			t.Run(fmt.Sprintf("%t/%s", serialized, state), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st := testutil.NewTestStore(t)
				source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/example-owner")
				require.NoError(err)
				key := strings.Repeat("d", 64)
				payload := fmt.Sprintf(`{"target":{"source_id":%d},"state":%q,"body":"private fixture draft","token":"fixture-confirmation"}`, source.ID, state)
				won, err := st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, key, "", payload)
				require.NoError(err)
				require.True(won)
				if serialized {
					_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
				} else {
					err = st.RemoveSource(source.ID)
				}
				require.NoError(err)
				remaining, err := st.GetYouTubeReplyReceiptContext(t.Context(), key)
				require.NoError(err)
				assert.JSONEq(`{"state":"removed"}`, remaining)
				assert.NotContains(remaining, "private fixture draft")
				assert.NotContains(remaining, "fixture-confirmation")
			})
		}
	}
}

func TestYouTubeReplyReceiptRemovalFencesStaleWritesAndPreservesOtherSources(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(strconv.FormatBool(serialized), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/example-owner")
			require.NoError(err)
			other, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/other-owner")
			require.NoError(err)
			key := strings.Repeat("e", 64)
			otherKey := strings.Repeat("f", 64)
			original := `{"body":"private fixture draft","token":"fixture-token","state":"sending"}`
			won, err := st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, key, "", original)
			require.NoError(err)
			require.True(won)
			won, err = st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), other.ID, otherKey, "", "other receipt")
			require.NoError(err)
			require.True(won)
			if serialized {
				_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
			} else {
				err = st.RemoveSource(source.ID)
			}
			require.NoError(err)
			remaining, err := st.GetYouTubeReplyReceiptContext(t.Context(), otherKey)
			require.NoError(err)
			assert.Equal("other receipt", remaining)
			won, err = st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, key, original, "private completion")
			require.ErrorIs(err, store.ErrSourceNotFound)
			assert.False(won)
			won, err = st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, strings.Repeat("a", 64), "", "new private draft")
			require.ErrorIs(err, store.ErrSourceNotFound)
			assert.False(won)
			var raw string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), "youtube_reply:v1:"+key).Scan(&raw))
			assert.JSONEq(`{"source_id":0,"receipt":"{\"state\":\"removed\"}"}`, raw)
		})
	}
}

func TestYouTubeReplyReceiptRejectsRetiredSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/example-owner")
	require.NoError(err)
	target, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/merged-owner")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO source_settings (source_id,merged_into_source_id) VALUES (?,?) ON CONFLICT(source_id) DO UPDATE SET merged_into_source_id = excluded.merged_into_source_id`), source.ID, target.ID)
	require.NoError(err)
	key := strings.Repeat("b", 64)
	won, err := st.CompareAndSwapYouTubeReplyReceiptContext(t.Context(), source.ID, key, "", "private fixture draft")
	require.ErrorIs(err, store.ErrSourceNotFound)
	assert.False(won)
	receipt, err := st.GetYouTubeReplyReceiptContext(t.Context(), key)
	require.NoError(err)
	assert.Empty(receipt)
}
