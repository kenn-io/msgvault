package embed

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

type embeddingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f embeddingRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestClientsRetryWaitReturnsOnPreemption(t *testing.T) {
	for _, provider := range []string{"openai", "voyage"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, requestYield := jobctx.WithPreemption(t.Context())
				calls := 0
				transport := embeddingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"3600"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})
				var run func() error
				if provider == "openai" {
					client := NewClient(Config{Endpoint: "https://example.com/v1", Model: "m", Dimension: 1})
					client.http.Transport = transport
					run = func() error { _, err := client.Embed(ctx, []string{"hello"}); return err }
				} else {
					client := NewVoyageClient(VoyageConfig{Endpoint: "https://example.com/v1", Model: "m", Dimension: 1})
					client.http.Transport = transport
					run = func() error {
						_, err := client.EmbedDocuments(ctx, []DocumentInput{{Chunks: []string{"hello"}}})
						return err
					}
				}
				result := make(chan error, 1)
				go func() { result <- run() }()
				synctest.Wait()
				require.Equal(t, 1, calls)
				requestYield()
				synctest.Wait()
				select {
				case err := <-result:
					require.ErrorIs(t, err, jobctx.ErrYieldedToWaiter)
				default:
					require.FailNow(t, "retry wait did not return on preemption")
				}
				assert.Equal(t, 1, calls, "preemption prevents another HTTP attempt")
			})
		})
	}
}

func TestClientFinishesInFlightResponseAfterPreemption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		ctx, requestYield := jobctx.WithPreemption(t.Context())
		started, finish := make(chan struct{}), make(chan struct{})
		var requestErr error
		client := NewClient(Config{Endpoint: "https://example.com/v1", Model: "m", Dimension: 1})
		client.http.Transport = embeddingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-finish
			requestErr = r.Context().Err()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1]}]}`))}, nil
		})
		var vectors [][]float32
		var err error
		done := make(chan struct{})
		go func() { vectors, err = client.Embed(ctx, []string{"hello"}); close(done) }()
		<-started
		requestYield()
		synctest.Wait()
		require.NoError(context.Cause(ctx))
		close(finish)
		<-done
		require.NoError(err)
		require.NoError(requestErr, "preemption keeps the in-flight request alive")
		assert.Equal(t, [][]float32{{1}}, vectors)
	})
}
