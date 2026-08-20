package visual

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
)

func testManifest(t *testing.T, policy MediaPolicy, passed ...string) voyage.CapabilityManifest {
	t.Helper()
	docPolicy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: policy.documentPolicy()})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(docPolicy, passed...)
	require.NoError(t, err)
	return manifest
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// newServerProvider builds a provider whose docbank client is rewired to the
// fake server while the pinned endpoint stays in the policy identity.
func newServerProvider(t *testing.T, server *httptest.Server, policy MediaPolicy, passed ...string) *VoyageProvider {
	t.Helper()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := server.Client().Transport
	provider, err := NewVoyageProvider(VoyageConfig{
		APIKey: "synthetic-key", Media: policy,
		Manifest: testManifest(t, policy, passed...),
		Timeout:  5 * time.Second,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			clone := request.Clone(request.Context())
			clone.URL.Scheme = target.Scheme
			clone.URL.Host = target.Host
			return transport.RoundTrip(clone)
		})},
	})
	require.NoError(t, err)
	return provider
}

func echoVectors(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Inputs []struct {
				Content []json.RawMessage `json:"content"`
			} `json:"inputs"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, len(request.Inputs))
		for index := range items {
			vector := make([]float32, 1024)
			vector[index] = 1
			items[index] = item{Embedding: vector, Index: index}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": voyage.DefaultModel, "data": items,
			"usage": map[string]any{"total_tokens": 11},
		})
	}
}

func pngMediaInput(t *testing.T) *MediaInput {
	t.Helper()
	data := mediatest.PNG(4, 4, nil)
	metadata, err := media.DetectBytes(data, "image/png")
	require.NoError(t, err)
	input := mediaInputFrom(metadata)
	input.BlobHash = testHash
	input.Bytes = data
	return input
}

func testDocument(t *testing.T, messageID int64, text string) DocumentInput {
	t.Helper()
	parts := []InputPart{}
	if text != "" {
		parts = append(parts, InputPart{Text: text})
	}
	parts = append(parts, InputPart{Media: pngMediaInput(t)})
	return DocumentInput{
		Owner:    Owner{MessageID: messageID, BlobHash: testHash, MediaInputKey: OriginalMediaInputKey},
		Revision: "rev-1", Parts: parts,
	}
}

func TestVoyageProviderEmbedsAuthorizedDocumentsAndRestoresOwners(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewTLSServer(echoVectors(t))
	defer server.Close()
	provider := newServerProvider(t, server, DefaultMediaPolicy())

	documents := []DocumentInput{testDocument(t, 1, "first context"), testDocument(t, 2, "")}
	results, err := provider.EmbedDocuments(t.Context(), documents)
	require.NoError(err)
	require.Len(results, 2)
	assert.Equal(documents[0].Owner, results[0].Owner)
	assert.Equal(documents[1].Owner, results[1].Owner)
	assert.InDelta(1.0, results[0].Vector[0], 0)
	assert.InDelta(1.0, results[1].Vector[1], 0)
	assert.Equal(Usage{TotalTokens: 11, Available: true}, results[0].Usage)

	empty, err := provider.EmbedDocuments(t.Context(), nil)
	require.NoError(err)
	assert.Nil(empty)
}

func TestVoyageProviderFailsClosedWithoutManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewTLSServer(echoVectors(t))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(err)
	transport := server.Client().Transport
	provider, err := NewVoyageProvider(VoyageConfig{
		APIKey: "synthetic-key", Media: DefaultMediaPolicy(),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			clone := request.Clone(request.Context())
			clone.URL.Scheme = target.Scheme
			clone.URL.Host = target.Host
			return transport.RoundTrip(clone)
		})},
	})
	require.NoError(err)
	assert.Empty(provider.AuthorizedCapabilities())
	assert.Empty(provider.PolicyFingerprint())

	_, err = provider.EmbedDocuments(t.Context(), []DocumentInput{testDocument(t, 1, "")})
	require.ErrorIs(err, ErrProviderRejected, "no manifest means no upload authority")
	_, _, err = provider.EmbedQuery(t.Context(), QueryInput{Text: "red square"})
	require.ErrorIs(err, ErrProviderRejected)
}

func TestVoyageProviderEnforcesPerCapabilityAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewTLSServer(echoVectors(t))
	defer server.Close()
	// PNG documents and text queries probed; interleaved, batch, and image
	// queries not probed.
	provider := newServerProvider(t, server, DefaultMediaPolicy(),
		voyage.CapabilityImagePNG, voyage.CapabilityQueryText)

	_, err := provider.EmbedDocuments(t.Context(), []DocumentInput{testDocument(t, 1, "")})
	require.NoError(err, "media-only png document is authorized")

	_, err = provider.EmbedDocuments(t.Context(), []DocumentInput{testDocument(t, 1, "context")})
	require.ErrorIs(err, ErrProviderRejected, "text-plus-media needs the interleaved capability")

	_, err = provider.EmbedDocuments(t.Context(), []DocumentInput{testDocument(t, 1, ""), testDocument(t, 2, "")})
	require.ErrorIs(err, ErrProviderRejected, "batches need the batch capability")

	_, _, err = provider.EmbedQuery(t.Context(), QueryInput{Text: "red square"})
	require.NoError(err)
	_, _, err = provider.EmbedQuery(t.Context(), QueryInput{Image: pngMediaInput(t)})
	require.ErrorIs(err, ErrProviderRejected, "image queries need the format's query capability")

	assert.Equal([]string{voyage.CapabilityImagePNG, voyage.CapabilityQueryText},
		provider.AuthorizedCapabilities())
	assert.Len(provider.PolicyFingerprint(), 64)
}

func TestVoyageProviderMapsFailureClassification(t *testing.T) {
	tests := []struct {
		name               string
		status             int
		body               string
		retryAfter         string
		want               error
		retryAfterExpected bool
	}{
		{name: "unauthorized", status: 401, want: ErrProviderUnauthorized},
		{name: "size rejection", status: 400, body: `{"detail":"input is too large"}`, want: ErrProviderBatchTooLarge},
		{name: "other rejection", status: 422, body: `{"detail":"bad input"}`, want: ErrProviderRejected},
		{name: "rate limited", status: 429, retryAfter: "3", want: ErrProviderRetryable, retryAfterExpected: true},
		{name: "server error", status: 503, want: ErrProviderRetryable},
		{name: "malformed", status: 200, body: `<html>`, want: ErrProviderMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				if tt.status != 200 {
					w.WriteHeader(tt.status)
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			provider := newServerProvider(t, server, DefaultMediaPolicy())
			_, err := provider.EmbedDocuments(t.Context(), []DocumentInput{testDocument(t, 1, "")})
			require.ErrorIs(err, tt.want)
			if tt.retryAfterExpected {
				delay, ok := ProviderRetryAfter(err)
				require.True(ok)
				assert.Equal(3*time.Second, delay)
			}
			var providerErr *ProviderError
			require.ErrorAs(err, &providerErr)
			assert.NotContains(err.Error(), "bad input", "provider bodies never reach error strings")
		})
	}
}

func TestVoyageProviderRejectsInvalidDocumentIdentity(t *testing.T) {
	require := require.New(t)
	server := httptest.NewTLSServer(echoVectors(t))
	defer server.Close()
	provider := newServerProvider(t, server, DefaultMediaPolicy())

	invalid := testDocument(t, 1, "")
	invalid.Revision = ""
	_, err := provider.EmbedDocuments(t.Context(), []DocumentInput{invalid})
	require.ErrorContains(err, "invalid visual document identity")

	lying := testDocument(t, 1, "")
	lying.Parts[0].Media.Bytes = []byte("not a picture")
	_, err = provider.EmbedDocuments(t.Context(), []DocumentInput{lying})
	require.Error(err)

	_, _, err = provider.EmbedQuery(t.Context(), QueryInput{})
	require.ErrorContains(err, "requires text or image")
}

func TestVoyageProviderRequiresPinnedTarget(t *testing.T) {
	_, err := NewVoyageProvider(VoyageConfig{
		APIKey: "k", Model: "other-model", Media: DefaultMediaPolicy(),
	})
	require.Error(t, err)
	var missing *ProviderError
	assert.NotErrorAs(t, err, &missing, "construction failures are configuration errors, not provider failures")
}
