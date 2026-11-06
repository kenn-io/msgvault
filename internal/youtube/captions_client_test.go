package youtube

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptionClientOfficialRoutesAndArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, _ := captionStore(t, 1)
	var calls []string
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
			return
		}
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("Bearer fixture-access", r.Header.Get("Authorization"))
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/captions":
			assertions.Equal("snippet", r.URL.Query().Get("part"))
			assertions.Equal("video-0", r.URL.Query().Get("videoId"))
			_, _ = fmt.Fprint(w, `{"items":[{"id":"track-a","etag":"version-1","snippet":{"trackKind":"standard","status":"serving","language":"en","lastUpdated":"2026-01-01T00:00:00Z"}}]}`)
		case "/captions/track-a":
			assertions.Equal("vtt", r.URL.Query().Get("tfmt"))
			w.Header().Set("Content-Type", "text/vtt")
			_, _ = fmt.Fprint(w, "WEBVTT\n\n00:00.000 --> 00:01.000\nExample transcript\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	summary, err := SyncCaptions(t.Context(), c, st, opts)
	requirements.NoError(err)
	assertions.Equal(1, summary.Stored)
	assertions.Equal([]string{"/captions", "/captions/track-a"}, calls)
}

func TestCaptionClientQuotaHaltsAndRedacts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, _ := captionStore(t, 2)
	count := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
			return
		}
		count++
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"message":"fixture-secret","errors":[{"reason":"quotaExceeded"}]}}`)
	})
	summary, err := SyncCaptions(t.Context(), c, st, opts)
	requirements.ErrorIs(err, ErrQuota)
	assertions.NotContains(err.Error(), "fixture-secret")
	assertions.Equal(1, count)
	assertions.Zero(summary.Unavailable)
}

func TestCaptionClientRejectsLargeAndMalformedDownloads(t *testing.T) {
	for _, data := range []string{"not a WebVTT file", "WEBVTT\n\n00:00.000 --> 00:01.000\nFirst\n00:02.000 --> 00:03.000\nSecond\n", strings.Repeat("x", maxAPIBytes+1)} {
		t.Run(fmt.Sprintf("bytes-%d", len(data)), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, opts, _ := captionStore(t, 1)
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
				case "/captions":
					_, _ = fmt.Fprint(w, `{"items":[{"id":"track-a","etag":"v1","snippet":{"trackKind":"standard","status":"serving"}}]}`)
				default:
					_, _ = fmt.Fprint(w, data)
				}
			})
			summary, err := SyncCaptions(t.Context(), c, st, opts)
			requirements.Error(err)
			assertions.Zero(summary.Stored)
		})
	}
}

func TestCaptionConversionFailureDefersAndRevisits(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, roots := captionStore(t, 2)
	convertible := false
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
		case "/captions":
			switch r.URL.Query().Get("videoId") {
			case "video-0":
				_, _ = fmt.Fprint(w, `{"items":[{"id":"track-video-0","etag":"v1","snippet":{"trackKind":"standard","status":"serving"}}]}`)
			case "video-1":
				_, _ = fmt.Fprint(w, `{"items":[{"id":"track-video-1","etag":"v1","snippet":{"trackKind":"standard","status":"serving"}}]}`)
			default:
				assertions.Fail("unexpected fixture video")
				w.WriteHeader(http.StatusBadRequest)
			}
		case "/captions/track-video-0":
			if !convertible {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":{"message":"fixture-secret","errors":[{"reason":"couldNotConvert"}]}}`)
				return
			}
			fallthrough
		default:
			_, _ = fmt.Fprint(w, "WEBVTT\n\n00:00.000 --> 00:01.000\nExample transcript\n")
		}
	})
	summary, err := SyncCaptions(t.Context(), c, st, opts)
	requirements.NoError(err)
	assertions.Equal(1, summary.Stored)
	assertions.Equal(1, summary.Unavailable)
	refs, err := st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[0])
	requirements.NoError(err)
	assertions.Contains(refs["youtube:caption:status"].Metadata, "conversion_failed")
	assertions.NotContains(refs["youtube:caption:status"].Metadata, "fixture-secret")
	convertible = true
	summary, err = SyncCaptions(t.Context(), c, st, opts)
	requirements.NoError(err)
	assertions.Equal(1, summary.Stored)
	refs, err = st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[0])
	requirements.NoError(err)
	assertions.Contains(refs, "youtube:caption:track:track-video-0")
	assertions.NotContains(refs, "youtube:caption:status")
}
