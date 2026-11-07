package youtube

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

func resetCaptionCursor(t *hegel.T, st *store.Store, opts CaptionOptions, cursor captionCursor) {
	previous, err := st.YouTubeCaptionStateContext(t.Context(), opts.SourceID)
	require.NoError(t, err)
	encoded, err := json.Marshal(cursor)
	require.NoError(t, err)
	require.NoError(t, st.CompareAndSwapYouTubeCaptionStateContext(t.Context(), opts.SourceID, previous, string(encoded)))
}

func TestCaptionPendingCrashReplayIdentityProperty(t *testing.T) {
	st, opts, roots := captionStore(t, 1)
	opts.MaxUnits = 200
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-").MinSize(1).MaxSize(256))
		pending := captionCursor{Upper: roots[0], Pending: &captionPending{Root: store.YouTubeCaptionRoot{MessageID: roots[0], VideoID: "video-0"}, Track: standardTrack(id, "v1")}}
		api := &captionFixtureAPI{}
		for range 2 { // Replay a crash after storage but before cursor advancement.
			resetCaptionCursor(ht, st, opts, pending)
			summary, err := SyncCaptions(ht.Context(), api, st, opts)
			require.NoError(ht, err)
			assert.Equal(ht, 200, summary.Units)
		}
		assert.Empty(ht, api.listCalls)
		var count int
		require.NoError(ht, st.DB().QueryRowContext(ht.Context(), st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND source_part_key=?`), roots[0], captionPartKey(id)).Scan(&count))
		assert.Equal(ht, 1, count)
	})
}

func TestCaptionObservationReaderProperty(t *testing.T) {
	st, opts, roots := captionStore(t, 1)
	iteration := 0
	hegel.Test(t, func(ht *hegel.T) {
		iteration++
		kind := hegel.Draw(ht, hegel.OneOf[string](hegel.Text(), hegel.SampledFrom([]string{"ASR", "asr", "AsR", "aSr"})))
		resetCaptionCursor(ht, st, opts, captionCursor{})
		track := standardTrack("track-a", strconv.Itoa(iteration))
		track.Snippet.TrackKind = kind
		api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{"video-0": {track}}}
		_, err := SyncCaptions(ht.Context(), api, st, opts)
		require.NoError(ht, err)
		refs, err := st.MessageYouTubeCaptionAttachmentsContext(ht.Context(), roots[0])
		require.NoError(ht, err)
		if strings.EqualFold(kind, "ASR") {
			assert.Empty(ht, api.downloads)
			observation, ok := refs["youtube:caption:status"]
			require.True(ht, ok, "unavailable observations must be returned by caption reader")
			var metadata captionObservation
			require.NoError(ht, json.Unmarshal([]byte(observation.Metadata), &metadata))
			assert.Equal(ht, "asr", metadata.Status)
		} else {
			assert.Equal(ht, []string{"track-a"}, api.downloads)
		}
	})
}

func TestCaptionVersionEvidenceProperty(t *testing.T) {
	st, _, roots := captionStore(t, 1)
	hegel.Test(t, func(ht *hegel.T) {
		etag := hegel.Draw(ht, hegel.Text())
		updated := hegel.Draw(ht, hegel.Text())
		track := standardTrack("version-track", etag)
		track.Snippet.LastUpdated = updated
		metadata, err := json.Marshal(captionObservation{Version: captionVersion(track), Status: "stored"})
		require.NoError(ht, err)
		require.NoError(ht, st.UpsertAttachmentRecord(ht.Context(), roots[0], store.AttachmentWrite{
			SourceAttachmentID: "youtube:caption:track:version-track", SourcePartKey: captionPartKey(track.ID),
			Metadata: string(metadata), State: "stored",
		}))
		unchanged, err := captionUnchanged(ht.Context(), st, roots[0], track)
		require.NoError(ht, err)
		assert.Equal(ht, etag != "" || updated != "", unchanged)
		track.Snippet.LastUpdated += "changed"
		unchanged, err = captionUnchanged(ht.Context(), st, roots[0], track)
		require.NoError(ht, err)
		assert.False(ht, unchanged)
	})
}

func TestCaptionSourceCursorIsolationProperty(t *testing.T) {
	requirements := require.New(t)
	st, opts, _ := captionStore(t, 1)
	other, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCother")
	requirements.NoError(err)
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		encoded, err := json.Marshal(map[string]string{"value": value})
		require.NoError(ht, err)
		previous, err := st.YouTubeCaptionStateContext(ht.Context(), opts.SourceID)
		require.NoError(ht, err)
		require.NoError(ht, st.CompareAndSwapYouTubeCaptionStateContext(ht.Context(), opts.SourceID, previous, string(encoded)))
		otherState, err := st.YouTubeCaptionStateContext(ht.Context(), other.ID)
		require.NoError(ht, err)
		assert.Empty(ht, otherState)
	})
}

func TestCaptionVersionPairIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		etag := hegel.Draw(ht, hegel.Text())
		updated := hegel.Draw(ht, hegel.Text())
		left := standardTrack("track-a", etag+":")
		left.Snippet.LastUpdated = updated
		right := standardTrack("track-a", etag)
		right.Snippet.LastUpdated = ":" + updated
		assert.NotEqual(ht, captionVersion(left), captionVersion(right))
	})
}

func TestCaptionVersionDelimiterRegression(t *testing.T) {
	assertions := assert.New(t)
	left := standardTrack("track-a", "a:b")
	left.Snippet.LastUpdated = "c"
	right := standardTrack("track-a", "a")
	right.Snippet.LastUpdated = "b:c"
	assertions.NotEqual(captionVersion(left), captionVersion(right))
}
