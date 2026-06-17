package syncerr

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTransientNetwork(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "oauth2.googleapis.com", IsNotFound: true}
	dialTimeoutErr := &url.Error{
		Op:  "Post",
		URL: "https://oauth2.googleapis.com/token",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: errors.New("i/o timeout"),
		},
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain string error", errors.New("something broke"), false},
		{"typed DNSError", dnsErr, true},
		{"typed url.Error wrapping dial timeout", dialTimeoutErr, true},
		{"wrapped DNSError via %w", fmt.Errorf("refresh token: %w", dnsErr), true},
		// oauth2 lib wraps with %v (not %w), so typed-unwrap fails;
		// substring fallback must catch it.
		{
			"oauth2-style %v-wrapped dial error",
			errors.New(`refresh token: Post "https://oauth2.googleapis.com/token": dial tcp: lookup oauth2.googleapis.com: i/o timeout`),
			true,
		},
		{
			"no such host substring",
			errors.New(`refresh token: Post "...": dial tcp: lookup oauth2.googleapis.com: no such host`),
			true,
		},
		{
			"connection refused substring",
			errors.New("dial tcp 127.0.0.1:443: connection refused"),
			true,
		},
		{
			"actual auth error (invalid_grant) is NOT transient",
			errors.New(`oauth2: "invalid_grant" "Token has been expired or revoked."`),
			false,
		},
		{
			"generic SQL error is NOT transient",
			errors.New("database disk image is malformed"),
			false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsTransientNetwork(tc.err)
			assert.Equal(t, tc.want, got, "IsTransientNetwork(%v)", tc.err)
		})
	}
}
