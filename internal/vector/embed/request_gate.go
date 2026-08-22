package embed

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ErrEmbeddingProviderRedirect classifies redirects rejected by a gated
// embeddings client. Redirect responses are permanent policy failures and are
// never retried or followed.
var ErrEmbeddingProviderRedirect = errors.New("embedding provider redirects are not allowed")

// BeforeRequestFunc authorizes one concrete HTTP attempt. It runs from the
// transport boundary, so retries and provider-specific request packing each
// receive a fresh check. Clients with a BeforeRequestFunc reject redirects.
type BeforeRequestFunc func(context.Context) error

type beforeRequestTransport struct {
	base   http.RoundTripper
	before BeforeRequestFunc
}

type beforeRequestError struct{ err error }

func (e *beforeRequestError) Error() string { return e.err.Error() }
func (e *beforeRequestError) Unwrap() error { return e.err }

func (t beforeRequestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.before(request.Context()); err != nil {
		return nil, &beforeRequestError{err: err}
	}
	return t.base.RoundTrip(request)
}

func newHTTPClient(timeout time.Duration, before BeforeRequestFunc) *http.Client {
	client := &http.Client{Timeout: timeout}
	if before != nil {
		client.Transport = beforeRequestTransport{
			base: http.DefaultTransport, before: before,
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

func beforeRequestCause(err error) error {
	var requestErr *beforeRequestError
	if !errors.As(err, &requestErr) {
		return nil
	}
	return requestErr.err
}
