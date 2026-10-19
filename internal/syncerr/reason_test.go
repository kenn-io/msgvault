package syncerr

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
	"golang.org/x/oauth2"
)

func TestReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "synthetic"}, "network"},
		{&oauth2.RetrieveError{ErrorCode: "invalid_grant"}, "auth"},
		{msgraph.ErrUnauthorized, "auth"},
		{&gmail.ThrottledError{}, "rate_limited"},
		{&msgraph.ThrottledError{}, "rate_limited"},
		{errors.New("invalid_grant status 401 429"), "other"},
		{&gmail.StatusError{StatusCode: 500}, "other"},
	} {
		assert.Equal(t, tc.want, Reason(fmt.Errorf("sync: %w", tc.err)))
	}
}
