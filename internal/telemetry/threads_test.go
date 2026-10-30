package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/threads"
)

func TestThreadsTelemetryCategories(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&threads.APIError{StatusCode: 400, Code: 190}, "auth"},
		{threads.ErrReauthorizationRequired, "auth"},
		{&threads.APIError{StatusCode: 400, Code: 4}, "rate_limited"},
		{&threads.APIError{StatusCode: 429}, "rate_limited"},
		{&threads.PauseError{Reason: "high_usage"}, "rate_limited"},
	} {
		assert.Equal(t, tc.want, reason(tc.err))
	}
}
