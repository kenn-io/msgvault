package twilio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeErrorTransport wraps real HTTP responses: a server cannot force
// the client's response-body Close method to return a deterministic error.
type closeErrorTransport struct {
	base http.RoundTripper
	err  error
}

func (transport closeErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, fmt.Errorf("test response transport: %w", err)
	}
	response.Body = closeErrorBody{ReadCloser: response.Body, err: transport.err}
	return response, nil
}

type closeErrorBody struct {
	io.ReadCloser

	err error
}

func (body closeErrorBody) Close() error {
	return errors.Join(body.ReadCloser.Close(), body.err)
}

func TestTwilioJSONResponseCloseFailureIsReported(t *testing.T) {
	failure := errors.New("synthetic response close failure")
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"sid": testCA, "account_sid": testAC})
	}, func(options *Options) {
		options.HTTPClient = &http.Client{Transport: closeErrorTransport{base: http.DefaultTransport, err: failure}}
	})
	call, err := client.GetCall(context.Background(), testCA)
	require.ErrorIs(t, err, failure)
	assert.Empty(t, call.SID, "failed acquisition must not expose a partially accepted Call")
}
