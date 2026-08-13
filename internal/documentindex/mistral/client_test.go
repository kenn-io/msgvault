package mistral

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientProcessStreamsGenericDocumentWithExactLength(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	payload := []byte("synthetic docx bytes")
	document := writeDocument(t, payload, "application/vnd.openxmlformats-officedocument.wordprocessingml.document")

	var gotLength int64
	var gotBody []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/v1/ocr", r.URL.Path)
		assert.Equal("Bearer synthetic-key", r.Header.Get("Authorization"))
		gotLength = r.ContentLength
		var err error
		gotBody, err = io.ReadAll(r.Body)
		assert.NoError(err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, err = io.WriteString(w, `{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"# Synthetic"}],"usage_info":{"pages_processed":1,"doc_size_bytes":20}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	client := newTestClient(t, server, Config{APIKey: "synthetic-key"})
	result, err := client.Process(t.Context(), document, Options{ExtractHeader: true, ExtractFooter: true})
	require.NoError(err)
	assert.Equal(int64(len(gotBody)), gotLength)
	assert.Equal("mistral-ocr-4-0", result.Model)
	require.Len(result.Pages, 1)
	assert.Equal("# Synthetic", result.Pages[0].Markdown)
	require.NotNil(result.UsageInfo)
	require.NotNil(result.UsageInfo.DocSizeBytes)
	assert.Equal(int64(20), *result.UsageInfo.DocSizeBytes)

	var request struct {
		Model    string `json:"model"`
		Document struct {
			Type string `json:"type"`
			URL  string `json:"document_url"`
		} `json:"document"`
		IncludeImageBase64 bool   `json:"include_image_base64"`
		IncludeBlocks      bool   `json:"include_blocks"`
		ExtractHeader      bool   `json:"extract_header"`
		ExtractFooter      bool   `json:"extract_footer"`
		Pages              string `json:"pages"`
	}
	require.NoError(json.Unmarshal(gotBody, &request))
	assert.Equal("mistral-ocr-4-0", request.Model)
	assert.Equal("document_url", request.Document.Type)
	assert.True(request.ExtractHeader)
	assert.True(request.ExtractFooter)
	assert.False(request.IncludeImageBase64)
	assert.False(request.IncludeBlocks)
	assert.Empty(request.Pages)
	encoded := strings.TrimPrefix(request.Document.URL, "data:"+document.MediaType+";base64,")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(err)
	assert.Equal(payload, decoded)
	assert.NotContains(string(gotBody), "table_format")
}

func TestClientProcessRejectsChangedOrPublicSpool(t *testing.T) {
	require := require.New(t)
	document := writeDocument(t, []byte("first"), "application/pdf")
	require.NoError(os.WriteFile(document.Path, []byte("other"), 0o600))
	client := clientWithoutRequests(t)
	_, err := client.Process(t.Context(), document, Options{})
	require.ErrorContains(err, "hash mismatch")

	if runtime.GOOS != "windows" {
		document = writeDocument(t, []byte("private"), "application/pdf")
		require.NoError(os.Chmod(document.Path, 0o644))
		_, err = client.Process(t.Context(), document, Options{})
		require.ErrorContains(err, "permissions must be private")
	}
}

func TestClientProcessRejectsOversizedAndInvalidResponses(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		maxBytes  int64
		wantError string
	}{
		{name: "too large", response: strings.Repeat("x", 65), maxBytes: 64, wantError: ErrResponseTooLarge.Error()},
		{name: "duplicate unit", response: `{"model":"mistral-ocr-4-0","pages":[{"index":0},{"index":0}],"usage_info":{"pages_processed":2}}`, maxBytes: 1024, wantError: "invalid index"},
		{name: "missing middle unit", response: `{"model":"mistral-ocr-4-0","pages":[{"index":0},{"index":2}],"usage_info":{"pages_processed":2}}`, maxBytes: 1024, wantError: "invalid index"},
		{name: "missing model", response: `{"pages":[],"usage_info":{}}`, maxBytes: 1024, wantError: "omitted model"},
		{name: "mismatched model", response: `{"model":"other","pages":[],"usage_info":{"pages_processed":0}}`, maxBytes: 1024, wantError: "does not match requested model"},
		{name: "missing index", response: `{"model":"mistral-ocr-4-0","pages":[{}],"usage_info":{"pages_processed":1}}`, maxBytes: 1024, wantError: "invalid index"},
		{name: "missing usage", response: `{"model":"mistral-ocr-4-0","pages":[]}`, maxBytes: 1024, wantError: "omitted usage"},
		{name: "unit count mismatch", response: `{"model":"mistral-ocr-4-0","pages":[{"index":0}],"usage_info":{"pages_processed":0}}`, maxBytes: 1024, wantError: "processed 0 units but returned 1"},
		{name: "byte count mismatch", response: `{"model":"mistral-ocr-4-0","pages":[],"usage_info":{"pages_processed":0,"doc_size_bytes":2}}`, maxBytes: 1024, wantError: "accounted for 2 document bytes, expected 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, tt.response)
				assert.NoError(t, err)
			}))
			defer server.Close()
			client := newTestClient(t, server, Config{MaxResponseBytes: tt.maxBytes})
			_, err := client.Process(t.Context(), writeDocument(t, []byte("x"), "application/pdf"), Options{})
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestClientDoesNotFollowRedirectsOrExposeErrorBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	redirectTargetCalled := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetCalled = true
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestClient(t, server, Config{})
	_, err := client.Process(t.Context(), writeDocument(t, []byte("x"), "application/pdf"), Options{})
	require.ErrorContains(err, "unexpected HTTP 307")
	assert.False(redirectTargetCalled)

	secret := "synthetic-provider-body-that-must-not-escape"
	errorServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, writeErr := io.WriteString(w, secret)
		assert.NoError(writeErr)
	}))
	defer errorServer.Close()
	client = newTestClient(t, errorServer, Config{})
	_, err = client.Process(t.Context(), writeDocument(t, []byte("x"), "application/pdf"), Options{})
	require.ErrorIs(err, ErrPermanentResponse)
	assert.NotContains(err.Error(), secret)
}

func TestClientRetriesTransientResponsesAndReverifiesSpool(t *testing.T) {
	assert := assert.New(t)
	document := writeDocument(t, []byte("original"), "application/pdf")
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(err)
		if requests == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = io.WriteString(w, `{"model":"mistral-ocr-4-0","pages":[],"usage_info":{"pages_processed":0,"doc_size_bytes":null}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	client := newTestClient(t, server, Config{MaxRetryDelay: 200 * time.Millisecond})
	started := time.Now()
	result, err := client.Process(t.Context(), document, Options{})
	elapsed := time.Since(started)
	require.NoError(t, err)
	assert.Equal("mistral-ocr-4-0", result.Model)
	require.NotNil(t, result.UsageInfo)
	assert.Nil(result.UsageInfo.DocSizeBytes)
	assert.Equal(2, requests)
	assert.Equal(2, result.Metrics.Requests)
	assert.Equal(1, result.Metrics.Retries)
	assert.Positive(result.Metrics.Latency)
	assert.GreaterOrEqual(elapsed-result.Metrics.Latency, 150*time.Millisecond,
		"provider latency must exclude Retry-After backoff")

	requests = 0
	document = writeDocument(t, []byte("first"), "application/pdf")
	mutatingServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, copyErr := io.Copy(io.Discard, r.Body)
		assert.NoError(copyErr)
		assert.NoError(os.WriteFile(document.Path, []byte("other"), 0o600))
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mutatingServer.Close()

	client = newTestClient(t, mutatingServer, Config{})
	_, err = client.Process(t.Context(), document, Options{})
	require.ErrorContains(t, err, "hash mismatch")
	assert.Equal(1, requests)
}

func TestClientReturnsTypedErrorAfterBoundedTransientRetries(t *testing.T) {
	assert := assert.New(t)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(err)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := newTestClient(t, server, Config{})
	_, err := client.Process(t.Context(), writeDocument(t, []byte("x"), "application/pdf"), Options{})
	require.ErrorIs(t, err, ErrTransientResponse)
	assert.Equal(defaultMaxRetries+1, requests)
	metrics := MetricsFromError(err)
	assert.Equal(defaultMaxRetries+1, metrics.Requests)
	assert.Equal(defaultMaxRetries, metrics.Retries)
	assert.Positive(metrics.Latency)
}

func TestClientRetriesTruncatedResponseBody(t *testing.T) {
	assert := assert.New(t)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(err)
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			w.Header().Set("Content-Length", "100")
			_, err = io.WriteString(w, "{")
			assert.NoError(err)
			return
		}
		_, err = io.WriteString(w, `{"model":"mistral-ocr-4-0","pages":[],"usage_info":{"pages_processed":0}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	client := newTestClient(t, server, Config{MaxRetryDelay: time.Millisecond})
	result, err := client.Process(t.Context(), writeDocument(t, []byte("x"), "application/pdf"), Options{})
	require.NoError(t, err)
	assert.Equal("mistral-ocr-4-0", result.Model)
	assert.Equal(2, requests)
}

func TestClientRejectsUnprovenMediaTypeAndOversizedDocument(t *testing.T) {
	client := clientWithoutRequests(t)
	document := writeDocument(t, []byte("x"), "application/octet-stream")
	_, err := client.Process(t.Context(), document, Options{})
	require.ErrorContains(t, err, "not allowlisted")

	document = writeDocument(t, []byte("large"), "application/pdf")
	client.maxDocumentBytes = 4
	_, err = client.Process(t.Context(), document, Options{})
	require.ErrorContains(t, err, "limit 4")
}

func TestNewClientRequiresAllowlistedExactEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "http", endpoint: "http://api.mistral.ai/v1/ocr"},
		{name: "wrong host", endpoint: "https://example.com/v1/ocr"},
		{name: "wrong path", endpoint: "https://api.mistral.ai/v1/files"},
		{name: "query", endpoint: "https://api.mistral.ai/v1/ocr?x=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(Config{Endpoint: tt.endpoint})
			require.Error(t, err)
			assert.Nil(t, client)
		})
	}
}

func newTestClient(t *testing.T, server *httptest.Server, overrides Config) *Client {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	overrides.Endpoint = server.URL + "/v1/ocr"
	overrides.AllowedHosts = []string{parsed.Hostname()}
	if len(overrides.AllowedMediaTypes) == 0 {
		overrides.AllowedMediaTypes = []string{
			"application/pdf",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		}
	}
	overrides.HTTPClient = server.Client()
	client, err := NewClient(overrides)
	require.NoError(t, err)
	return client
}

func clientWithoutRequests(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(Config{
		AllowedMediaTypes: []string{"application/pdf"},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unexpected request")
		})},
	})
	require.NoError(t, err)
	return client
}

func writeDocument(t *testing.T, content []byte, mediaType string) Document {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spool")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	hash := sha256.Sum256(content)
	return Document{
		Path:      path,
		MediaType: mediaType,
		Size:      int64(len(content)),
		SHA256:    hex.EncodeToString(hash[:]),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
