package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

func TestClientBatchLimitAfterYield(t *testing.T) {
	check, must := assert.New(t), require.New(t)
	requests := make(chan int, 10)
	var respond atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		requests <- len(request.Input)
		if !respond.Load() {
			<-r.Context().Done()
			return
		}
		vectors := make([][]float32, len(request.Input))
		for i := range vectors {
			vectors[i] = []float32{1}
		}
		writeEmbeddings(t, w, vectors)
	}))
	t.Cleanup(server.Close)
	client := NewClient(Config{Endpoint: server.URL, Model: "m", Dimension: 1, Timeout: 100 * time.Millisecond, MaxRetries: 1, Builtin: true}) //nolint:kennlint // deadlinetest: the first request must time out before the yield
	inputs := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, inputs)
		done <- err
	}()
	must.Equal(8, <-requests)
	must.Equal(4, <-requests)
	cancel(jobctx.ErrYieldedToWaiter)
	must.ErrorIs(<-done, jobctx.ErrYieldedToWaiter)
	respond.Store(true)
	vectors, err := client.Embed(t.Context(), inputs)
	must.NoError(err)
	check.Len(vectors, 8)
	must.Equal(4, <-requests)
	check.Equal(4, <-requests)
}

func TestClientBatchLimitAfterSuccessfulSplit(t *testing.T) {
	requests := make(chan int, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		requests <- len(request.Input)
		if len(request.Input) > 4 {
			<-r.Context().Done()
			return
		}
		vectors := make([][]float32, len(request.Input))
		for i := range vectors {
			vectors[i] = []float32{1}
		}
		writeEmbeddings(t, w, vectors)
	}))
	t.Cleanup(server.Close)
	client := NewClient(Config{Endpoint: server.URL, Model: "m", Dimension: 1, Timeout: 50 * time.Millisecond, MaxRetries: 1, Builtin: true}) //nolint:kennlint // deadlinetest: force Kit to split the oversized request
	inputs := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for range 2 {
		vectors, err := client.Embed(t.Context(), inputs)
		require.NoError(t, err)
		assert.Len(t, vectors, 8)
	}
	for _, size := range []int{8, 4, 4, 4, 4} {
		require.Equal(t, size, <-requests)
	}
}

func TestClientBatchLimitAfterTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		builtin bool
		want    []int
	}{
		{name: "builtin", builtin: true, want: []int{4, 4}},
		{name: "remote", want: []int{8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			requests := make(chan int, 10)
			var respond atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := decodeRequest(t, r)
				requests <- len(request.Input)
				if !respond.Load() {
					<-r.Context().Done()
					return
				}
				vectors := make([][]float32, len(request.Input))
				for i := range vectors {
					vectors[i] = []float32{1}
				}
				writeEmbeddings(t, w, vectors)
			}))
			t.Cleanup(server.Close)
			client := NewClient(Config{Endpoint: server.URL, Model: "m", Dimension: 1, Timeout: 50 * time.Millisecond, MaxRetries: 1, Builtin: tc.builtin}) //nolint:kennlint // deadlinetest: force Kit to split the timed-out requests
			inputs := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
			_, err := client.Embed(t.Context(), inputs)
			must.Error(err)
			for _, size := range []int{8, 4, 2, 1} {
				must.Equal(size, <-requests)
			}
			respond.Store(true)
			vectors, err := client.Embed(t.Context(), inputs)
			must.NoError(err)
			assert.Len(t, vectors, 8)
			for _, size := range tc.want {
				must.Equal(size, <-requests)
			}
		})
	}
}
