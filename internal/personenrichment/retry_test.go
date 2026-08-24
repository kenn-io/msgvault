package personenrichment_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestRetryDelayHonorsHTTPDateAndInjectedJitter(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	delay, err := personenrichment.RetryDelay(personenrichment.RetryInput{
		Attempt: 3, Base: time.Second, Maximum: time.Minute,
		RetryAfter: now.Add(37 * time.Second).Format(http.TimeFormat),
		Now:        now, Jitter: func(time.Duration) time.Duration { return 2 * time.Second },
	})
	require.NoError(t, err)
	assert.Equal(t, 39*time.Second, delay)
}

func TestRetryDelayBoundsAndValidatesInputs(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		input personenrichment.RetryInput
		want  time.Duration
		err   string
	}{
		{
			name: "numeric retry after",
			input: personenrichment.RetryInput{
				Attempt: 1, Base: time.Second, Maximum: time.Minute, RetryAfter: "11", Now: now,
				Jitter: func(delay time.Duration) time.Duration {
					assert.Equal(t, 11*time.Second, delay)
					return time.Second
				},
			},
			want: 12 * time.Second,
		},
		{
			name: "past HTTP date",
			input: personenrichment.RetryInput{
				Attempt: 1, Base: time.Second, Maximum: time.Minute,
				RetryAfter: now.Add(-time.Second).Format(http.TimeFormat), Now: now,
				Jitter: func(time.Duration) time.Duration { return 0 },
			},
			want: 0,
		},
		{
			name: "capped exponential",
			input: personenrichment.RetryInput{
				Attempt: 20, Base: 3 * time.Second, Maximum: 50 * time.Second, Now: now,
				Jitter: func(time.Duration) time.Duration { return 0 },
			},
			want: 50 * time.Second,
		},
		{
			name: "deterministic injected jitter",
			input: personenrichment.RetryInput{
				Attempt: 2, Base: 2 * time.Second, Maximum: time.Minute, Now: now,
				Jitter: func(delay time.Duration) time.Duration { return delay / 4 },
			},
			want: 10 * time.Second,
		},
		{
			name: "malformed retry after uses configured base",
			input: personenrichment.RetryInput{
				Attempt: 2, Base: 3 * time.Second, Maximum: time.Minute,
				RetryAfter: "not-a-retry-delay", Now: now,
				Jitter: func(delay time.Duration) time.Duration {
					assert.Equal(t, 12*time.Second, delay)
					return time.Second
				},
			},
			want: 13 * time.Second,
		},
		{
			name: "negative jitter rejected",
			input: personenrichment.RetryInput{
				Attempt: 1, Base: time.Second, Maximum: time.Minute, Now: now,
				Jitter: func(time.Duration) time.Duration { return -time.Nanosecond },
			},
			err: "jitter",
		},
		{
			name: "jitter cannot exceed cap",
			input: personenrichment.RetryInput{
				Attempt: 20, Base: time.Second, Maximum: time.Minute, Now: now,
				Jitter: func(time.Duration) time.Duration { return time.Second },
			},
			want: time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delay, err := personenrichment.RetryDelay(tt.input)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, delay)
		})
	}
}
