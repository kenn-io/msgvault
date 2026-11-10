//go:build sqlite_vec

package embed

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerEmbeddingTimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allTimeout bool
		wantCalls  []int
	}{
		{"split succeeds", false, []int{4, 2, 2}},
		{"singleton timeout stops pass", true, []int{4, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := decodeRequest(t, r)
				mu.Lock()
				calls = append(calls, len(request.Input))
				mu.Unlock()
				if tc.allTimeout || len(request.Input) > 2 {
					<-r.Context().Done()
					return
				}
				vectors := make([][]float32, len(request.Input))
				for i := range vectors {
					vectors[i] = []float32{1, 0, 0, 0}
				}
				writeEmbeddings(t, w, vectors)
			}))
			defer server.Close()
			f := newWorkerFixture(t, 4)
			client := NewClient(Config{Endpoint: server.URL, Model: "test-model", Dimension: 4, Timeout: 100 * time.Millisecond, MaxRetries: 1}) //nolint:kennlint // deadlinetest: the request timeout triggers splitting
			worker := NewWorker(WorkerDeps{Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB, Store: f.Store, Client: client, BatchSize: 4, Recorder: f.Recorder})
			_, err := worker.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			if tc.allTimeout {
				require.ErrorContains(t, err, "embed request timed out")
				assert.Equal(t, 4, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			} else {
				require.NoError(t, err)
				assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			}
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tc.wantCalls, calls)
		})
	}
}
