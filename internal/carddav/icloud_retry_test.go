package carddav

import (
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestICloudRetryAfterStopsEveryDiscoverySubrequest(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusMovedPermanently, http.StatusUnauthorized, http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests int
			client := newICloudTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests > 1 {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Retry-After", "120")
				w.Header().Set("Location", "/.well-known/carddav")
				w.Header().Set("WWW-Authenticate", `Digest realm="synthetic", nonce="synthetic-nonce", qop="auth", algorithm=MD5`)
				w.WriteHeader(status)
			}))
			partition, err := url.Parse("https://p42-contacts.icloud.com/")
			require.NoError(err)
			client.origin = originURL(partition)
			_, err = NewICloudService(nil, client).DiscoverConnection(t.Context(), partition.String())
			var failure *StatusError
			require.ErrorAs(err, &failure)
			assert.Equal(2*time.Minute, failure.RetryAfter)
			assert.Equal(1, requests)
		})
	}
}

func TestDiscoveryFallbackNeverDiscardsPositiveRetryAfterProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.SampledFrom([]int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusNotImplemented}))
		delay := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		assert.False(ht, discoveryFallbackAllowed(&StatusError{StatusCode: status, RetryAfter: time.Duration(delay)}))
	}, hegel.WithDatabase(""))
}

// Minimal regression for the shrunk positive-delay fallback counterexample.
func TestDiscoveryFallbackRejectsMinimalRetryDelay(t *testing.T) {
	assert.False(t, discoveryFallbackAllowed(&StatusError{StatusCode: http.StatusNotFound, RetryAfter: time.Nanosecond}))
}

func TestICloudDiscoveryRetryPolicyDoesNotTurnSuccessfulWriteIntoFailure(t *testing.T) {
	client := newICloudTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusOK)
	}))
	partition, err := url.Parse("https://p42-contacts.icloud.com/")
	require.NoError(t, err)
	client.origin = originURL(partition)
	err = NewICloudService(nil, client).remote.Put(t.Context(), partition.String()+"synthetic.vcf", []byte("synthetic body"), "", true)
	require.NoError(t, err)
}
