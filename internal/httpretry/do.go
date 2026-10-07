package httpretry

import (
	"context"
	"net/http"
	"time"
)

// Do sends a request up to attempts times. A non-2xx response retry accepts is
// closed and sent again after its Retry-After, or a backoff, capped at
// maxDelay. It returns the last response with its body open for the caller to
// classify; a transport failure after ctx ends returns ctx.Err().
func Do(ctx context.Context, attempts int, maxDelay time.Duration, send func() (*http.Response, error), retry func(*http.Response) bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		res, err := send()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices || attempt >= attempts-1 || !retry(res) {
			return res, nil
		}
		delay := RetryAfter(res.Header.Get("Retry-After"), attempt, maxDelay)
		_ = res.Body.Close()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
