package visual

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoyageDocumentsSendOrderedMultimodalInputsAndRestoreIndices(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var request voyageMultimodalRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		assert.Equal("/v1/multimodalembeddings", incoming.URL.Path)
		assert.Equal("Bearer synthetic-key", incoming.Header.Get("Authorization"))
		if !assert.NoError(json.NewDecoder(incoming.Body).Decode(&request)) {
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
			"data":[
				{"index":1,"embedding":[4,5,6]},
				{"index":0,"embedding":[1,2,3]}
			],
			"usage":{"total_tokens":27}
		}`)
	}))
	t.Cleanup(server.Close)
	client := newVoyageTestClient(t, server.URL+"/v1", 3)
	documents := []DocumentInput{
		voyageTestDocument(1, "aa", "caption", &MediaInput{
			Kind: MediaKindImage, MIMEType: "image/png", BlobHash: strings.Repeat("a", 64),
			Bytes: []byte("png-bytes"), Width: 2, Height: 2,
		}),
		voyageTestDocument(2, "bb", "video caption", &MediaInput{
			Kind: MediaKindVideo, MIMEType: "video/mp4", BlobHash: strings.Repeat("b", 64),
			Bytes: []byte("mp4-bytes"), Width: 4, Height: 3, DurationMS: 100,
		}),
	}

	results, err := client.EmbedDocuments(t.Context(), documents)
	require.NoError(err)
	require.Len(results, 2)
	assert.Equal(documents[0].Owner, results[0].Owner)
	assert.Equal([]float32{1, 2, 3}, results[0].Vector)
	assert.Equal(Usage{TotalTokens: 27, Available: true}, results[0].Usage)
	assert.Equal(documents[1].Owner, results[1].Owner)
	assert.Equal([]float32{4, 5, 6}, results[1].Vector)
	assert.Equal("voyage-multimodal-3.5", request.Model)
	assert.Equal("document", request.InputType)
	assert.False(request.Truncation)
	assert.Equal(3, request.OutputDimension)
	require.Len(request.Inputs, 2)
	assert.Equal([]voyageContentPart{
		{Type: "text", Text: "caption"},
		{Type: "image_base64", ImageBase64: "data:image/png;base64,cG5nLWJ5dGVz"},
	}, request.Inputs[0].Content)
	assert.Equal("video_base64", request.Inputs[1].Content[1].Type)
	assert.Equal("data:video/mp4;base64,bXA0LWJ5dGVz", request.Inputs[1].Content[1].VideoBase64)
}

func TestVoyageQuerySupportsTextImageAndCombinedInputs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := make(chan voyageMultimodalRequest, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		var request voyageMultimodalRequest
		if !assert.NoError(json.NewDecoder(incoming.Body).Decode(&request)) {
			return
		}
		requests <- request
		_, _ = io.WriteString(writer, `{"data":[{"index":0,"embedding":[1,2]}]}`)
	}))
	t.Cleanup(server.Close)
	client := newVoyageTestClient(t, server.URL, 2)
	image := &MediaInput{Kind: MediaKindImage, MIMEType: "image/jpeg", Bytes: []byte("jpeg")}

	for _, query := range []QueryInput{{Text: "find this"}, {Image: image}, {Text: "same package", Image: image}} {
		vector, usage, err := client.EmbedQuery(t.Context(), query)
		require.NoError(err)
		assert.Equal([]float32{1, 2}, vector)
		assert.False(usage.Available)
	}
	close(requests)
	got := make([]voyageMultimodalRequest, 0, 3)
	for request := range requests {
		got = append(got, request)
		assert.Equal("query", request.InputType)
	}
	assert.Equal([]voyageContentPart{{Type: "text", Text: "find this"}}, got[0].Inputs[0].Content)
	assert.Equal("image_base64", got[1].Inputs[0].Content[0].Type)
	assert.Equal([]string{"text", "image_base64"}, []string{
		got[2].Inputs[0].Content[0].Type, got[2].Inputs[0].Content[1].Type,
	})

	_, _, err := client.EmbedQuery(t.Context(), QueryInput{})
	require.Error(err)
	_, _, err = client.EmbedQuery(t.Context(), QueryInput{Image: &MediaInput{
		Kind: MediaKindVideo, MIMEType: "video/mp4", Bytes: []byte("video"),
	}})
	require.ErrorContains(err, "must be an image")
}

func TestVoyageResponseValidationRejectsMalformedVectors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing result", body: `{"data":[]}`},
		{name: "duplicate index", body: `{"data":[{"index":0,"embedding":[1,2]},{"index":0,"embedding":[3,4]}]}`},
		{name: "out of range index", body: `{"data":[{"index":2,"embedding":[1,2]}]}`},
		{name: "wrong dimension", body: `{"data":[{"index":0,"embedding":[1]}]}`},
		{name: "empty vector", body: `{"data":[{"index":0,"embedding":[]}]}`},
		{name: "non finite", body: `{"data":[{"index":0,"embedding":[1e1000,2]}]}`},
		{name: "invalid json", body: `{`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, test.body)
			}))
			t.Cleanup(server.Close)
			client := newVoyageTestClient(t, server.URL, 2)

			_, err := client.EmbedDocuments(t.Context(), []DocumentInput{
				voyageTestDocument(1, "aa", "caption", voyageTestImage()),
			})
			require.ErrorIs(t, err, ErrProviderMalformed)
		})
	}
}

func TestVoyageResponseLimitAndRequestLimitFailBoundedly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(writer, strings.Repeat("x", 1024))
	}))
	t.Cleanup(server.Close)
	client, err := NewVoyageClient(VoyageConfig{
		Endpoint: server.URL, Model: "voyage-multimodal-3.5", Dimension: 2,
		MaxRetries: 1, MaxResponseBytes: 32, MaxRequestBytes: 1024,
	})
	require.NoError(err)
	_, err = client.EmbedDocuments(t.Context(), []DocumentInput{
		voyageTestDocument(1, "aa", "caption", voyageTestImage()),
	})
	require.ErrorIs(err, ErrProviderMalformed)

	tinyClient, err := NewVoyageClient(VoyageConfig{
		Endpoint: server.URL, Model: "voyage-multimodal-3.5", Dimension: 2,
		MaxRetries: 1, MaxResponseBytes: 32, MaxRequestBytes: 10,
	})
	require.NoError(err)
	_, err = tinyClient.EmbedDocuments(t.Context(), []DocumentInput{
		voyageTestDocument(1, "aa", "caption", voyageTestImage()),
	})
	require.ErrorIs(err, ErrProviderBatchTooLarge)
	assert.Equal(int64(1), calls.Load(), "local request rejection must not call the provider")
}

func TestVoyageHTTPFailureClassificationRetryAndSanitization(t *testing.T) {
	t.Run("size rejection", func(t *testing.T) {
		err := voyageHTTPError(t, http.StatusBadRequest, `{"detail":"total number of tokens exceeds maximum secret-body"}`)
		require.ErrorIs(t, err, ErrProviderBatchTooLarge)
		assert.NotContains(t, err.Error(), "secret-body")
	})
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			err := voyageHTTPError(t, status, `{"detail":"synthetic-key private-body"}`)
			require.ErrorIs(t, err, ErrProviderUnauthorized)
			assert.NotContains(t, err.Error(), "synthetic-key")
			assert.NotContains(t, err.Error(), "private-body")
		})
	}
	t.Run("other 4xx", func(t *testing.T) {
		err := voyageHTTPError(t, http.StatusUnprocessableEntity, `{"detail":"private-body"}`)
		require.ErrorIs(t, err, ErrProviderRejected)
		assert.NotContains(t, err.Error(), "private-body")
	})

	for _, firstStatus := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(firstStatus), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					writer.Header().Set("Retry-After", "0")
					writer.WriteHeader(firstStatus)
					return
				}
				_, _ = io.WriteString(writer, `{"data":[{"index":0,"embedding":[1,2]}]}`)
			}))
			t.Cleanup(server.Close)
			client := newVoyageTestClient(t, server.URL, 2)

			_, err := client.EmbedDocuments(t.Context(), []DocumentInput{
				voyageTestDocument(1, "aa", "caption", voyageTestImage()),
			})
			require.NoError(t, err)
			assert.Equal(t, int64(2), calls.Load())
		})
	}
}

func TestVoyageTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(writer, `{"data":[{"index":0,"embedding":[1,2]}]}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewVoyageClient(VoyageConfig{
		Endpoint: server.URL, Model: "voyage-multimodal-3.5", Dimension: 2,
		Timeout: 10 * time.Millisecond, MaxRetries: 1,
	})
	require.NoError(t, err)
	_, err = client.EmbedDocuments(t.Context(), []DocumentInput{
		voyageTestDocument(1, "aa", "caption", voyageTestImage()),
	})
	require.Error(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.EmbedDocuments(ctx, []DocumentInput{
		voyageTestDocument(1, "aa", "caption", voyageTestImage()),
	})
	require.ErrorIs(t, err, context.Canceled)
}

func voyageHTTPError(t *testing.T, status int, body string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	client := newVoyageTestClient(t, server.URL, 2)
	_, err := client.EmbedDocuments(t.Context(), []DocumentInput{
		voyageTestDocument(1, "aa", "caption", voyageTestImage()),
	})
	return err
}

func newVoyageTestClient(t *testing.T, endpoint string, dimension int) *VoyageClient {
	t.Helper()
	client, err := NewVoyageClient(VoyageConfig{
		Endpoint: endpoint, APIKey: "synthetic-key", Model: "voyage-multimodal-3.5",
		Dimension: dimension, MaxRetries: 2, RetryBaseDelay: time.Nanosecond,
	})
	require.NoError(t, err)
	return client
}

func voyageTestDocument(messageID int64, hashPrefix, text string, media *MediaInput) DocumentInput {
	hash := strings.Repeat(hashPrefix, 32)
	if len(hash) > 64 {
		hash = hash[:64]
	}
	media.BlobHash = hash
	return DocumentInput{
		Owner:    Owner{MessageID: messageID, BlobHash: hash, MediaInputKey: OriginalMediaInputKey},
		Revision: "revision-" + hashPrefix,
		Parts:    []InputPart{{Text: text}, {Media: media}},
	}
}

func voyageTestImage() *MediaInput {
	return &MediaInput{Kind: MediaKindImage, MIMEType: "image/png", Bytes: []byte("png"), Width: 1, Height: 1}
}

var _ Provider = (*VoyageClient)(nil)
