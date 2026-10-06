package httpretry

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoRetriesUntilSuccessOrNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry bool
		want  int
		calls int
	}{
		{"retryable", true, http.StatusOK, 2},
		{"not retryable", false, http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			res, err := Do(context.Background(), 3, time.Second, func() (*http.Response, error) {
				calls++
				status := http.StatusServiceUnavailable
				if calls > 1 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"0"}}, Body: http.NoBody}, nil
			}, func(*http.Response) bool { return tc.retry })
			require.NoError(t, err)
			defer func() { assert.NoError(t, res.Body.Close()) }()
			assert.Equal(t, tc.want, res.StatusCode)
			assert.Equal(t, tc.calls, calls)
		})
	}
}
