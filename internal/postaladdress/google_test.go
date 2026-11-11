package postaladdress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// googleTransport replaces the external service while preserving the real
// request context and HTTP client cancellation path.
type googleTransport func(*http.Request) (*http.Response, error)

func (f googleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

const googleClean = `{"result":{"verdict":{"addressComplete":true,"validationGranularity":"PREMISE"},"address":{"postalAddress":{"regionCode":"CA","postalCode":"V0A 0A0","locality":"Example City","addressLines":["12 Example Street"]},"addressComponents":[{"componentType":"route","componentName":{"text":"Example Street"},"confirmationLevel":"CONFIRMED"}]}}}`

func googleTestValidator(t *testing.T, status int, body string) (*GoogleValidator, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return &GoogleValidator{APIKey: "synthetic-secret", Client: server.Client(), endpoint: server.URL}, calls
}

func TestGoogleMapsOnlyExplicitPostalFields(t *testing.T) {
	input := Input{AddressKind: "postal", StreetAddress: " 12 Example Street ", ExtendedAddress: "Unit 7", PostOfficeBox: "PO Box 9", Locality: "Example City", Region: "BC", PostalCode: "V0A 0A0", CountryCode: "ca", CountryName: "Canada", FreeText: "private free form", OriginalValue: "private original", ExtendedComponents: "private JSON", Label: "private label", GeoURI: "private geo", Timezone: "private timezone", PlaceURI: "private place"}
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "synthetic-secret", r.Header.Get("X-Goog-Api-Key"))
		assert.Empty(t, r.URL.RawQuery)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		_, _ = io.WriteString(w, googleClean)
	}))
	defer server.Close()
	v := GoogleValidator{APIKey: "synthetic-secret", Client: server.Client(), endpoint: server.URL}
	got, err := v.Check(t.Context(), input)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(map[string]any{"address": map[string]any{"regionCode": "CA", "postalCode": "V0A 0A0", "locality": "Example City", "administrativeArea": "BC", "addressLines": []any{"12 Example Street", "Unit 7", "PO Box 9"}}}, payload)
	assert.Equal(input, got.Original)
	assert.Equal("Unit 7", got.Proposed.ExtendedAddress)
	assert.Equal("PO Box 9", got.Proposed.PostOfficeBox)
	assert.Equal("private JSON", got.Proposed.ExtendedComponents)
	assert.Equal(StatusNeedsReview, got.Status)
	assert.Equal("google", got.Provider)
	assert.NotNil(got.CheckedAt)
}

func TestGoogleRequiresCompleteConfirmedFineGrainedResult(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     Status
	}{
		{"clean", googleClean, StatusValidated},
		{"subpremise", strings.ReplaceAll(googleClean, "PREMISE", "SUB_PREMISE"), StatusValidated},
		{"incomplete", strings.ReplaceAll(googleClean, `"addressComplete":true`, `"addressComplete":false`), StatusNeedsReview},
		{"coarse", strings.ReplaceAll(googleClean, "PREMISE", "BLOCK"), StatusNeedsReview},
		{"replaced", strings.ReplaceAll(googleClean, `"addressComplete":true`, `"addressComplete":true,"hasReplacedComponents":true`), StatusNeedsReview},
		{"inferred", strings.ReplaceAll(googleClean, `"addressComplete":true`, `"addressComplete":true,"hasInferredComponents":true`), StatusNeedsReview},
		{"unconfirmed verdict", strings.ReplaceAll(googleClean, `"addressComplete":true`, `"addressComplete":true,"hasUnconfirmedComponents":true`), StatusNeedsReview},
		{"unconfirmed component", strings.ReplaceAll(googleClean, `"CONFIRMED"`, `"UNCONFIRMED_BUT_PLAUSIBLE"`), StatusNeedsReview},
		{"unexpected", strings.ReplaceAll(googleClean, `"confirmationLevel":"CONFIRMED"`, `"confirmationLevel":"CONFIRMED","unexpected":true`), StatusNeedsReview},
		{"component replacement", strings.ReplaceAll(googleClean, `"confirmationLevel":"CONFIRMED"`, `"confirmationLevel":"CONFIRMED","replaced":true`), StatusNeedsReview},
		{"component inference", strings.ReplaceAll(googleClean, `"confirmationLevel":"CONFIRMED"`, `"confirmationLevel":"CONFIRMED","inferred":true`), StatusNeedsReview},
		{"missing unit", strings.ReplaceAll(googleClean, `"addressComponents":`, `"missingComponentTypes":["subpremise"],"addressComponents":`), StatusNeedsReview},
		{"unresolved", strings.ReplaceAll(googleClean, `"addressComponents":`, `"unresolvedTokens":["unit"],"addressComponents":`), StatusNeedsReview},
		{"unconfirmed types", strings.ReplaceAll(googleClean, `"addressComponents":`, `"unconfirmedComponentTypes":["route"],"addressComponents":`), StatusNeedsReview},
		{"no components", strings.ReplaceAll(googleClean, `[{"componentType":"route","componentName":{"text":"Example Street"},"confirmationLevel":"CONFIRMED"}]`, `[]`), StatusNeedsReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, calls := googleTestValidator(t, 200, tc.body)
			input := Input{StreetAddress: "12 Example St", CountryCode: "CA"}
			got, err := v.Check(t.Context(), input)
			require.NoError(t, err)
			assert := assert.New(t)
			assert.Equal(tc.status, got.Status)
			assert.Equal(int32(1), calls.Load())
			assert.Equal("12 Example Street", got.Proposed.StreetAddress)
		})
	}
}

func TestGoogleBoundsInputWithoutTruncation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input Input
	}{
		{"unknown country", Input{StreetAddress: "12 Example St", CountryCode: "ZZ"}},
		{"no postal input", Input{OriginalValue: "private original", CountryCode: "CA"}},
		{"too long", Input{StreetAddress: strings.Repeat("é", 279), CountryCode: "CA"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, calls := googleTestValidator(t, 200, googleClean)
			got, err := v.Check(t.Context(), tc.input)
			require.NoError(t, err)
			assert := assert.New(t)
			assert.Equal(StatusNeedsReview, got.Status)
			assert.Equal(tc.input, got.Original)
			assert.Zero(calls.Load())
		})
	}
	v, calls := googleTestValidator(t, 200, googleClean)
	_, err := v.Check(t.Context(), Input{StreetAddress: strings.Repeat("é", 278), CountryCode: "CA"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "280 Unicode characters, not bytes")
}

func TestGoogleFailuresAreBoundedRedactedAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unsupported", 400, `{"error":{"message":"12 Private Lane synthetic-secret"}}`},
		{"malformed", 200, "synthetic-secret 12 Private Lane"},
		{"oversize", 200, strings.Repeat(" ", 1<<20) + googleClean},
		{"empty response", 200, "{}"},
		{"trailing payload", 200, googleClean + "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, calls := googleTestValidator(t, tc.status, tc.body)
			got, err := v.Check(t.Context(), Input{StreetAddress: "12 Private Lane", CountryCode: "CA"})
			require.Error(t, err)
			assert := assert.New(t)
			assert.NotContains(err.Error(), "synthetic-secret")
			assert.NotContains(err.Error(), "12 Private Lane")
			assert.Equal(StatusUnavailable, got.Status)
			assert.Equal("12 Private Lane", got.Proposed.StreetAddress)
			assert.Equal(int32(1), calls.Load())
		})
	}
}

func TestGoogleDoesNotSendWithoutKeyOrAfterCancellation(t *testing.T) {
	v, calls := googleTestValidator(t, 200, googleClean)
	v.APIKey = ""
	got, err := v.Check(t.Context(), Input{StreetAddress: "12 Example Street", CountryCode: "CA"})
	require := require.New(t)
	require.Error(err)
	assert := assert.New(t)
	assert.Equal(StatusUnavailable, got.Status)
	assert.Zero(calls.Load())
	v.APIKey = "synthetic-secret"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = v.Check(ctx, Input{StreetAddress: "12 Example Street", CountryCode: "CA"})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(StatusUnavailable, got.Status)
	assert.Zero(calls.Load())
}

func TestGoogleTimeoutAndRedirectDoNotLeakKey(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			calls := 0
			v := GoogleValidator{APIKey: "synthetic-secret", timeout: 20 * time.Millisecond, Client: &http.Client{
				Transport: googleTransport(func(request *http.Request) (*http.Response, error) {
					calls++
					<-request.Context().Done()
					return nil, request.Context().Err()
				}),
			}}
			started := time.Now()
			got, err := v.Check(t.Context(), Input{StreetAddress: "12 Example Street", CountryCode: "CA"})
			require.ErrorIs(err, context.DeadlineExceeded)
			assert.Equal(StatusUnavailable, got.Status)
			assert.Equal(20*time.Millisecond, time.Since(started), "the request ends at its deadline in synthetic time")
			assert.Equal(1, calls, "timed-out provider requests must not be retried")
		})
	})

	t.Run("redirect", func(t *testing.T) {
		destinationCalls := new(atomic.Int32)
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			destinationCalls.Add(1)
			assert.Empty(t, r.Header.Get("X-Goog-Api-Key"))
		}))
		defer destination.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
		}))
		defer source.Close()
		v := GoogleValidator{APIKey: "synthetic-secret", Client: source.Client(), endpoint: source.URL}
		got, err := v.Check(t.Context(), Input{StreetAddress: "12 Example Street", CountryCode: "CA"})
		require.Error(t, err)
		assert.Equal(t, StatusUnavailable, got.Status)
		assert.Zero(t, destinationCalls.Load())
	})
}

func TestGoogleRetainedComponentsNeedReview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input Input
		body  string
	}{
		{"unit only", Input{StreetAddress: "12 Example St", ExtendedAddress: "Unit 7", CountryCode: "CA"}, strings.ReplaceAll(strings.ReplaceAll(googleClean, "PREMISE", "SUB_PREMISE"), "12 Example Street", "Unit 7")},
		{"street and box", Input{StreetAddress: "12 Example St", PostOfficeBox: "PO Box 9", CountryCode: "CA"}, strings.ReplaceAll(strings.ReplaceAll(googleClean, "PREMISE", "SUB_PREMISE"), "12 Example Street", "PO Box 9")},
		{"box only", Input{PostOfficeBox: "PO Box 9", CountryCode: "CA"}, googleClean},
		{"postal code", Input{StreetAddress: "12 Example Street", CountryCode: "CA", PostalCode: "V0A 0A0"}, strings.ReplaceAll(googleClean, `"postalCode":"V0A 0A0",`, ``)},
		{"locality", Input{StreetAddress: "12 Example Street", CountryCode: "CA", Locality: "Example City"}, strings.ReplaceAll(googleClean, `"locality":"Example City",`, ``)},
		{"region", Input{StreetAddress: "12 Example Street", CountryCode: "CA", Region: "BC"}, googleClean},
		{"country name", Input{StreetAddress: "12 Example Street", CountryCode: "CA", CountryName: "United States"}, googleClean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, calls := googleTestValidator(t, 200, tc.body)
			got, err := v.Check(t.Context(), tc.input)
			require.NoError(t, err)
			assert.Equal(t, StatusNeedsReview, got.Status)
			assert.Equal(t, int32(1), calls.Load())
			switch tc.name {
			case "unit only", "street and box":
				assert.Contains(t, got.ReasonCodes, "provider_component_not_confirmed")
				assert.Equal(t, tc.input.StreetAddress, got.Proposed.StreetAddress)
				assert.Equal(t, tc.input.ExtendedAddress, got.Proposed.ExtendedAddress)
				assert.Equal(t, tc.input.PostOfficeBox, got.Proposed.PostOfficeBox)
			case "box only":
				assert.Equal(t, tc.input.PostOfficeBox, got.Proposed.PostOfficeBox)
			case "postal code":
				assert.Equal(t, tc.input.PostalCode, got.Proposed.PostalCode)
			case "locality":
				assert.Equal(t, tc.input.Locality, got.Proposed.Locality)
			case "region":
				assert.Equal(t, tc.input.Region, got.Proposed.Region)
			case "country name":
				assert.Equal(t, tc.input.CountryName, got.Proposed.CountryName)
			}
		})
	}
}
