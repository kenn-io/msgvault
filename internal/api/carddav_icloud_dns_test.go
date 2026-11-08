package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
)

func syntheticICloudDNSError() error {
	return &net.DNSError{Err: "no such host", Name: "p42-contacts.icloud.com", IsNotFound: true}
}

func TestICloudDNSFailureRediscoversBeforeRequiringReconnect(t *testing.T) {
	for _, kind := range []string{"direct", "wrapped", "joined"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			f := savedICloudAccountFixture(t)
			f.partition, f.partitionErr = "p43", syntheticICloudDNSError()
			switch kind {
			case "wrapped":
				f.partitionErr = fmt.Errorf("synthetic discovery: %w", f.partitionErr)
			case "joined":
				f.partitionErr = errors.Join(f.partitionErr, &net.OpError{Op: "dial", Err: errors.New("synthetic failure")})
			}
			_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
			require.ErrorIs(t, err, carddav.ErrICloudReconnectRequired)
			assert.Equal("https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
			assert.Equal([]string{"https://p42-contacts.icloud.com", carddav.ICloudDiscoveryURL}, f.discovered)
			assert.Zero(f.syncCalls)
		})
	}
}

func TestICloudSafetyCauseDominatesJoinedDNSError(t *testing.T) {
	for _, cause := range []error{carddav.ErrUnsafeTarget, carddav.ErrOperationLimit, &carddav.StatusError{StatusCode: http.StatusUnauthorized}, &carddav.StatusError{StatusCode: http.StatusGone, RetryAfter: time.Minute}} {
		assert.False(t, iCloudHostFailure(t.Context(), errors.Join(syntheticICloudDNSError(), cause)))
	}
}
