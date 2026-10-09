//go:build sqlite_vec

package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Use the real Client so Kit's pre-request validation participates in worker
// regressions, rather than teaching the fake client a second blank-text rule.
func newWorkerHTTPClient(t *testing.T) (*Client, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request embeddingRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls = append(calls, append([]string(nil), request.Input...))
		mu.Unlock()
		vectors := make([][]float32, len(request.Input))
		for i := range vectors {
			vectors[i] = []float32{1, float32(i + 1), 0, 0}
		}
		writeEmbeddings(t, w, vectors)
	}))
	t.Cleanup(server.Close)
	return NewClient(Config{Endpoint: server.URL, Model: "test-model", Dimension: 4}), func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), calls...)
	}
}

func TestWorker_SkipsHTMLControlAfterQuotedMessage(t *testing.T) {
	f := newWorkerFixture(t, 3)
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = ''`)
	require.NoError(t, err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, "> quoted text only")
	require.NoError(t, err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = '', body_html = ? WHERE message_id = 2`, "<p>\x7f</p>")
	require.NoError(t, err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 3`, "visible message")
	require.NoError(t, err)
	require.Equal(t, "\x7f", BodyTextForEmbedding("", "<p>\x7f</p>"))

	client, calls := newWorkerHTTPClient(t)
	w := newTestWorker(f, 8)
	w.deps.Client = client
	w.deps.Preprocess.StripQuotes = true
	res, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)
	assert.Zero(t, res.Failed)
	assert.Equal(t, [][]string{{"visible message"}}, calls())
	assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
	var messageID int64
	require.NoError(t, f.VectorsDB.QueryRow(`SELECT message_id FROM embeddings WHERE generation_id = ?`, f.BuildingGen).Scan(&messageID))
	assert.Equal(t, int64(3), messageID)
	stats, err := f.Backend.Stats(t.Context(), f.BuildingGen)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.EmbeddingCount)

	res, err = w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)
	assert.Len(t, calls(), 1)
}

func TestWorker_SkipsInvisibleOnlyMessages(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"whitespace", " \t\n\u00a0"},
		{"nul", "\x00"},
		{"del", "\x7f"},
		{"c1 control", "\u009f"},
		{"zero width space", "\u200b"},
		{"formatting", "\u200e\u2060\ufeff"},
		{"mixed blank", " \x00\x7f\u200b\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t, 1)
			_, err := f.MainDB.Exec(`UPDATE messages SET subject = ''`)
			require.NoError(t, err)
			_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ?`, tc.body)
			require.NoError(t, err)
			client, calls := newWorkerHTTPClient(t)
			w := newTestWorker(f, 8)
			w.deps.Client = client
			res, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.Zero(t, res.Succeeded)
			assert.Zero(t, res.Failed)
			assert.Empty(t, calls())
			assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			stats, err := f.Backend.Stats(t.Context(), f.BuildingGen)
			require.NoError(t, err)
			assert.Zero(t, stats.EmbeddingCount)
		})
	}
}

func TestWorker_PreservesVisibleEmbeddingText(t *testing.T) {
	for _, tc := range []struct{ name, subject, body, want string }{
		{name: "visible unicode and controls", body: "\x7f界\u200b", want: "\x7f界\u200b"},
		{name: "braille blank", body: "\u2800", want: "\u2800"},
		{name: "subject and control", subject: "visible subject", body: "\x7f", want: "Subject: visible subject\n\n\x7f"},
		{name: "subject only", subject: "subject only", want: "Subject: subject only\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t, 1)
			_, err := f.MainDB.Exec(`UPDATE messages SET subject = ?`, tc.subject)
			require.NoError(t, err)
			_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ?`, tc.body)
			require.NoError(t, err)
			client, calls := newWorkerHTTPClient(t)
			w := newTestWorker(f, 8)
			w.deps.Client = client
			res, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.Equal(t, 1, res.Succeeded)
			assert.Equal(t, [][]string{{tc.want}}, calls())
			assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
		})
	}
}

func TestWorker_InvisibleSkipPreservesCASAndDeletesStaleVectors(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "unchanged"
		if edited {
			name = "concurrent edit"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t, 1)
			w := newTestWorker(f, 8)
			_, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err)
			_, err = f.MainDB.Exec(`UPDATE messages SET subject = '', embed_gen = NULL`)
			require.NoError(t, err)
			_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ?`, "\x7f")
			require.NoError(t, err)
			if edited {
				w.deps.beforeSkipStamp = func(_ context.Context, ids []int64) {
					assert.Equal(t, []int64{1}, ids)
					_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = 'repaired content' WHERE message_id = 1`)
					require.NoError(t, err)
					_, err = f.MainDB.Exec(`UPDATE messages SET last_modified = '2099-01-01 00:00:00' WHERE id = 1`)
					require.NoError(t, err)
				}
			}
			client, calls := newWorkerHTTPClient(t)
			w.deps.Client = client
			res, err := w.RunBackstop(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.Zero(t, res.Succeeded)
			assert.Empty(t, calls())
			var rows int
			require.NoError(t, f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`, f.BuildingGen).Scan(&rows))
			if edited {
				assert.Equal(t, 1, rows, "CAS miss preserves existing vectors")
				assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			} else {
				assert.Zero(t, rows, "skip removes stale vectors")
				assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			}
		})
	}
}

// The input domain is deliberately whitespace/Cf/Cc, with an optional visible
// suffix. This checks both skip and acceptance directions without duplicating
// the production predicate in the oracle.
func FuzzWorkerBlankText(f *testing.F) {
	f.Add([]byte{0, 1, 4, 6}, false)
	f.Add([]byte{4}, false)
	f.Add([]byte{4, 6}, true)
	f.Fuzz(func(t *testing.T, data []byte, visible bool) {
		blankRunes := []rune{' ', '\t', '\n', 0, 0x7f, 0x9f, 0x200b, 0x200e, 0x2060, 0xfeff}
		var body []rune
		for _, b := range data[:min(len(data), 64)] {
			body = append(body, blankRunes[int(b)%len(blankRunes)])
		}
		if visible {
			body = append(body, '界')
		}
		fixture := newWorkerFixture(t, 1)
		_, err := fixture.MainDB.Exec(`UPDATE messages SET subject = ''`)
		require.NoError(t, err)
		_, err = fixture.MainDB.Exec(`UPDATE message_bodies SET body_text = ?`, string(body))
		require.NoError(t, err)
		batch, err := newTestWorker(fixture, 8).embedBatch(t.Context(), []int64{1})
		require.NoError(t, err)
		if visible {
			assert.Empty(t, batch.empty)
			assert.Equal(t, []int64{1}, batch.embeddedIDs)
			require.Len(t, fixture.FakeClient.LastInputs, 1)
			assert.Contains(t, fixture.FakeClient.LastInputs[0], "界")
		} else {
			assert.Equal(t, []int64{1}, batch.empty)
			assert.Empty(t, batch.chunks)
			assert.Zero(t, fixture.FakeClient.calls)
		}
	})
}
