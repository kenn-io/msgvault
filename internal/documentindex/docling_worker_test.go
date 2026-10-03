package documentindex

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/msgvault/internal/store"
)

type doclingUnexpectedTransport struct{}

func (doclingUnexpectedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected transport must not receive Docling bytes")
}

func TestDoclingClientRejectsUnexpectedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = doclingUnexpectedTransport{}
	t.Cleanup(func() { http.DefaultTransport = original })
	c := doclingTestConfig()
	provider, err := NewDoclingClient(&c)
	require.ErrorContains(t, err, "transport")
	assert.Nil(t, provider)
}

func TestDoclingWorkerPublishesRealAsyncResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	content := mistraltest.MinimalPDF("synthetic report")
	var calls atomic.Int32
	var pollCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls.Add(1)
		assertions.Equal("synthetic-secret", r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/v1/convert/file/async":
			assertions.Equal(http.MethodPost, r.Method)
			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
			if !assertions.NoError(r.ParseMultipartForm(1 << 20)) { // #nosec G120 -- MaxBytesReader bounds the body above; gosec does not track this field assignment.
				return
			}
			defer func() {
				assertions.NoError(r.MultipartForm.RemoveAll())
			}()
			assertions.Equal([]string{"md", "json"}, r.MultipartForm.Value["to_formats"])
			file, header, err := r.FormFile("files")
			if !assertions.NoError(err) {
				return
			}
			got, err := io.ReadAll(file)
			assertions.NoError(err)
			assertions.NoError(file.Close())
			assertions.Equal(content, got)
			assertions.Equal("document.pdf", header.Filename)
			_, err = io.WriteString(w, `{"task_id":"synthetic-job","task_type":"convert","task_status":"pending"}`)
			assertions.NoError(err)
		case "/v1/status/poll/synthetic-job":
			status := `{"task_id":"synthetic-job","task_type":"convert","task_status":"pending"}`
			if pollCalls.Add(1) == 2 {
				status = `{"task_id":"synthetic-job","task_type":"convert","task_status":"success"}`
			}
			_, err := io.WriteString(w, status)
			assertions.NoError(err)
		case "/v1/result/synthetic-job":
			assertions.NoError(json.NewEncoder(w).Encode(doclingWorkerResponse(false)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := doclingTestConfig()
	c.Endpoint = server.URL
	c.APIKeyEnv = "SYNTHETIC_DOCLING_KEY"
	c.PollInterval = time.Millisecond
	t.Setenv(c.APIKeyEnv, "synthetic-secret")
	provider, err := NewDoclingClient(&c)
	requirements.NoError(err)
	catalog := &workerCatalog{}
	closed := &atomic.Int32{}
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content, closed: closed}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	requirements.NoError(err)
	digest := sha256.Sum256(content)
	result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	requirements.NoError(err)
	requirements.NotNil(catalog.publication)
	assertions.Nil(catalog.failure)
	assertions.Equal(1, result.Units)
	assertions.Equal("page", catalog.publication.UnitKind)
	assertions.Equal("searchable sentinel", catalog.publication.Units[0].Text)
	assertions.Equal(int64(len(content)), catalog.publication.SourceBytes)
	assertions.Equal(4, catalog.publication.RequestCount)
	assertions.Zero(catalog.publication.RetryCount, "normal async polling is not a provider retry")
	assertions.Equal(int32(4), calls.Load())
	assertions.Equal(int32(2), pollCalls.Load())
	assertions.Equal(int32(1), closed.Load())
}

func TestDoclingWorkerRecordsRetriesWhenRenderFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var requests atomic.Int32
	var resultCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/convert/file/async":
			_, err := io.WriteString(w, `{"task_id":"synthetic-job","task_type":"convert","task_status":"success"}`)
			assertions.NoError(err)
		case "/v1/result/synthetic-job":
			if resultCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err := io.WriteString(w, `{}`)
				assertions.NoError(err)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, err := io.WriteString(w, `{}`)
			assertions.NoError(err)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	c := doclingTestConfig()
	c.Endpoint, c.APIKeyEnv, c.PollInterval, c.MaxPollAttempts = server.URL, "SYNTHETIC_DOCLING_KEY", time.Millisecond, 3
	t.Setenv(c.APIKeyEnv, "synthetic-secret")
	provider, err := NewDoclingClient(&c)
	requirements.NoError(err)
	content := mistraltest.MinimalPDF("synthetic source")
	digest := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	requirements.NoError(err)

	_, err = worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	requirements.Error(err)
	requirements.NotNil(catalog.failure)
	assertions.Equal(int32(3), requests.Load())
	assertions.Equal(3, catalog.failure.RequestCount)
	assertions.Equal(1, catalog.failure.RetryCount)
	assertions.Positive(catalog.failure.ProviderLatencyMS)
	assertions.Equal("provider_rejected", catalog.failure.ReasonCode)
}

func TestDoclingWorkerRejectsRedirectToAnotherOrigin(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var redirectedCalls atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"task_id":"unexpected-job","task_type":"convert","task_status":"success"}`)
		assertions.NoError(err)
	}))
	t.Cleanup(external.Close)
	var originCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		http.Redirect(w, r, external.URL+"/redirected", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	c := doclingTestConfig()
	c.Endpoint, c.APIKeyEnv = origin.URL, "SYNTHETIC_DOCLING_KEY"
	t.Setenv(c.APIKeyEnv, "synthetic-secret")
	provider, err := NewDoclingClient(&c)
	requirements.NoError(err)
	content := mistraltest.MinimalPDF("synthetic source")
	digest := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	requirements.NoError(err)

	_, err = worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	requirements.Error(err)
	requirements.NotNil(catalog.failure)
	assertions.Equal(int32(1), originCalls.Load())
	assertions.Zero(redirectedCalls.Load())
	assertions.Equal(1, catalog.failure.RequestCount)
	assertions.Equal("invalid_provider_output", catalog.failure.ReasonCode)
}

func doclingWorkerResponse(tables bool) map[string]any {
	doc := map[string]any{
		"schema_name": "DoclingDocument", "version": "1.7.0", "origin": map[string]any{"filename": "document.pdf"},
		"pages": map[string]any{"1": map[string]any{}},
		"texts": []any{map[string]any{"text": "searchable sentinel", "prov": []any{map[string]any{"page_no": 1}}}},
	}
	if tables {
		doc["tables"] = []any{map[string]any{"data": map[string]any{"table_cells": []any{}}}}
	}
	return map[string]any{"status": "success", "errors": []any{}, "document": map[string]any{
		"filename": "document.pdf", "md_content": "searchable sentinel\n\n| table | value |\n| --- | --- |\n| retained-table-sentinel | 42 |\n", "json_content": doc,
	}}
}

func doclingWorkerFixture(t *testing.T, response map[string]any, configure func(*DocumentsConfig), catalog *workerCatalog) (*DoclingWorker, store.DocumentExtractionCandidate, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls.Add(1)
		switch r.URL.Path {
		case "/v1/convert/file/async":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"task_id": "synthetic-job", "task_type": "convert", "task_status": "success"}))
		case "/v1/result/synthetic-job":
			assert.NoError(t, json.NewEncoder(w).Encode(response))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := doclingTestConfig()
	c.Endpoint = server.URL
	if configure != nil {
		configure(&c)
	}
	provider, err := NewDoclingClient(&c)
	require.NoError(t, err)
	content := mistraltest.MinimalPDF("synthetic source")
	digest := sha256.Sum256(content)
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	require.NoError(t, err)
	return worker, store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	}, calls
}

func TestDoclingWorkerPreservesTablesWithoutPageClaims(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	response := doclingWorkerResponse(true)
	responseDocument, ok := response["document"].(map[string]any)
	requirements.True(ok)
	doc, ok := responseDocument["json_content"].(map[string]any)
	requirements.True(ok)
	pages, ok := doc["pages"].(map[string]any)
	requirements.True(ok)
	pages["2"] = map[string]any{}
	texts, ok := doc["texts"].([]any)
	requirements.True(ok)
	doc["texts"] = append(texts, map[string]any{"text": "second page", "prov": []any{map[string]any{"page_no": 2}}})
	catalog := &workerCatalog{}
	worker, candidate, _ := doclingWorkerFixture(t, response, nil, catalog)
	result, err := worker.ProcessCandidate(t.Context(), candidate)
	requirements.NoError(err)
	requirements.NotNil(catalog.publication)
	assertions.Equal("document", catalog.publication.UnitKind)
	assertions.Contains(catalog.publication.Units[0].Text, "retained-table-sentinel")
	assertions.False(result.Truncated)
	assertions.Equal(1, result.Units)
	assertions.Equal(2, result.ProviderUnits)
	assertions.Equal(2, catalog.publication.UnitsProcessed)
}

func TestDoclingWorkerBoundsReturnedUnitsBeforeFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	response := doclingWorkerResponse(true)
	responseDocument, ok := response["document"].(map[string]any)
	requirements.True(ok)
	doc, ok := responseDocument["json_content"].(map[string]any)
	requirements.True(ok)
	pages, ok := doc["pages"].(map[string]any)
	requirements.True(ok)
	pages["2"] = map[string]any{}
	catalog := &workerCatalog{}
	worker, candidate, _ := doclingWorkerFixture(t, response, func(c *DocumentsConfig) { c.MaxPagesPerDocument = 1 }, catalog)
	_, err := worker.ProcessCandidate(t.Context(), candidate)
	requirements.Error(err)
	assertions.Nil(catalog.publication)
	requirements.NotNil(catalog.failure)
	assertions.True(catalog.failure.Terminal)
}

func TestDoclingWorkerRejectsChangedSourceBeforeUpload(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DoclingWorker, *store.DocumentExtractionCandidate)
	}{
		{"hash", func(_ *DoclingWorker, c *store.DocumentExtractionCandidate) {
			c.CanonicalBlobHash = strings.Repeat("0", 64)
		}},
		{"size", func(_ *DoclingWorker, c *store.DocumentExtractionCandidate) { c.Size++ }},
		{"size cap", func(w *DoclingWorker, _ *store.DocumentExtractionCandidate) { w.config.Documents.MaxFileBytes = 1 }},
		{"invalid PDF", func(w *DoclingWorker, c *store.DocumentExtractionCandidate) {
			content := []byte("synthetic text mislabeled as PDF")
			digest := sha256.Sum256(content)
			w.opener = &workerOpener{content: content}
			c.CanonicalBlobHash, c.Size = hex.EncodeToString(digest[:]), int64(len(content))
		}},
		{"unsupported type", func(_ *DoclingWorker, c *store.DocumentExtractionCandidate) { c.MIMEType = "image/jpeg" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			catalog := &workerCatalog{}
			worker, candidate, calls := doclingWorkerFixture(t, doclingWorkerResponse(false), nil, catalog)
			test.mutate(worker, &candidate)
			_, err := worker.ProcessCandidate(t.Context(), candidate)
			requirements.Error(err)
			assertions.Zero(calls.Load())
			assertions.Nil(catalog.publication)
			if test.name != "unsupported type" {
				requirements.NotNil(catalog.failure)
				assertions.True(catalog.failure.Terminal)
				assertions.Equal("invalid_local_source", catalog.failure.ReasonCode)
			}
		})
	}
}

func TestDoclingWorkerRefusesPartialSuccess(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	response := doclingWorkerResponse(false)
	response["status"] = "partial_success"
	response["errors"] = []any{map[string]any{"page_no": 1}}
	catalog := &workerCatalog{}
	worker, candidate, _ := doclingWorkerFixture(t, response, nil, catalog)
	_, err := worker.ProcessCandidate(t.Context(), candidate)
	requirements.ErrorContains(err, "partial result")
	assertions.Nil(catalog.publication)
	requirements.NotNil(catalog.failure)
	assertions.True(catalog.failure.Terminal)
	assertions.Equal("invalid_provider_output", catalog.failure.ReasonCode)
	assertions.True(catalog.failure.RetryAt.IsZero())
}

func TestDoclingWorkerCancellationDuringUploadSchedulesRetry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cancel()
		_, err := io.WriteString(w, `{"task_id":"synthetic-job","task_type":"convert","task_status":"success"}`)
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	c := doclingTestConfig()
	c.Endpoint = server.URL
	provider, err := NewDoclingClient(&c)
	requirements.NoError(err)
	content := mistraltest.MinimalPDF("synthetic source")
	digest := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	requirements.NoError(err)
	_, err = worker.ProcessCandidate(ctx, store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	requirements.ErrorIs(err, context.Canceled)
	assertions.Nil(catalog.publication)
	requirements.NotNil(catalog.failure)
	assertions.False(catalog.failure.Terminal)
	assertions.Equal("provider_interrupted", catalog.failure.ReasonCode)
	requirements.NoError(catalog.failureContextErr)
	assertions.False(catalog.failure.RetryAt.IsZero())
}

func TestDoclingClientBypassesAmbientProxy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		http.Error(w, "synthetic proxy refuses upload", http.StatusForbidden)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	requirements.NoError(err)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	var endpointCalls atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpointCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/convert/file/async":
			assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"task_id": "synthetic-job", "task_type": "convert", "task_status": "success"}))
		case "/v1/result/synthetic-job":
			assertions.NoError(json.NewEncoder(w).Encode(doclingWorkerResponse(false)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(endpoint.Close)
	// A non-loopback origin avoids net/http's automatic loopback proxy bypass.
	// Only DNS resolution is redirected; both endpoint and proxy are real servers.
	original := http.DefaultTransport
	defaultTransport, ok := original.(*http.Transport)
	requirements.True(ok)
	transport := defaultTransport.Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	endpointClientTransport, ok := endpoint.Client().Transport.(*http.Transport)
	requirements.True(ok)
	transport.TLSClientConfig = endpointClientTransport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "docling.example.com:443" {
			address = endpoint.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, address)
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	c := doclingTestConfig()
	c.Endpoint = "https://docling.example.com"
	provider, err := NewDoclingClient(&c)
	requirements.NoError(err)
	content := mistraltest.MinimalPDF("synthetic proxy test")
	digest := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	requirements.NoError(err)
	_, err = worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	requirements.NoError(err)
	assertions.Equal(int32(2), endpointCalls.Load())
	assertions.Zero(proxyCalls.Load())
}

func TestDoclingWorkerAcceptsNativeDocumentBytes(t *testing.T) {
	requirements := require.New(t)

	var docx bytes.Buffer
	writer := zip.NewWriter(&docx)
	for _, file := range []struct{ name, content string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
		{"word/document.xml", `<document><p>synthetic document</p></document>`},
	} {
		entry, err := writer.Create(file.name)
		requirements.NoError(err)
		_, err = io.WriteString(entry, file.content)
		requirements.NoError(err)
	}
	requirements.NoError(writer.Close())
	for _, test := range []struct {
		name, mediaType string
		content         []byte
	}{
		{"DOCX", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", docx.Bytes()},
		{"text", "text/plain", []byte("synthetic plain text")},
		{"Markdown", "text/markdown", []byte("# synthetic Markdown\n")},
		{"CSV", "text/csv", []byte("name,value\nsynthetic,42\n")},
		{"HTML", "text/html", []byte("<html><body><p>synthetic HTML</p></body></html>")},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			catalog := &workerCatalog{}
			worker, candidate, calls := doclingWorkerFixture(t, doclingWorkerResponse(false), nil, catalog)
			digest := sha256.Sum256(test.content)
			candidate.CanonicalBlobHash, candidate.Size, candidate.MIMEType = hex.EncodeToString(digest[:]), int64(len(test.content)), test.mediaType
			worker.opener = &workerOpener{content: test.content}
			_, err := worker.ProcessCandidate(t.Context(), candidate)
			requirements.NoError(err)
			requirements.NotNil(catalog.publication)
			assertions.Equal("document", catalog.publication.UnitKind)
			assertions.Contains(catalog.publication.Units[0].Text, "retained-table-sentinel")
			assertions.Equal(int32(2), calls.Load())
		})
	}
}

func TestDoclingWorkerLeaseLossAfterUploadSchedulesRetry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	catalog := &workerCatalog{renewErr: errors.New("synthetic lost lease")}
	worker, candidate, calls := doclingWorkerFixture(t, doclingWorkerResponse(false), nil, catalog)
	_, err := worker.ProcessCandidate(t.Context(), candidate)
	requirements.Error(err)
	assertions.Equal(int32(2), calls.Load())
	assertions.Nil(catalog.publication)
	requirements.NotNil(catalog.failure)
	assertions.False(catalog.failure.Terminal)
	assertions.Equal("lease_renewal_failed", catalog.failure.ReasonCode)
	assertions.False(catalog.failure.RetryAt.IsZero())
}

func TestDoclingWorkerClassifiesJobFailures(t *testing.T) {
	for _, mode := range []string{"ambiguous_submission", "poll_limit", "authentication", "missing_credential"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var submissions, requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					submissions.Add(1)
					if mode == "authentication" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					if mode == "ambiguous_submission" {
						assertions.NoError(json.NewEncoder(w).Encode(map[string]any{}))
						return
					}
				}
				assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"task_id": "synthetic-job", "task_type": "convert", "task_status": "pending"}))
			}))
			t.Cleanup(server.Close)
			c := doclingTestConfig()
			c.Endpoint, c.MaxPollAttempts, c.PollInterval = server.URL, 1, time.Millisecond
			if mode == "missing_credential" {
				c.APIKeyEnv = "SYNTHETIC_DOCLING_CREDENTIAL"
				t.Setenv(c.APIKeyEnv, "")
			}
			provider, err := NewDoclingClient(&c)
			requirements.NoError(err)
			content := mistraltest.MinimalPDF("synthetic source")
			digest := sha256.Sum256(content)
			catalog := &workerCatalog{}
			worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
				Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
			})
			requirements.NoError(err)
			_, err = worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
				AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
			})
			requirements.Error(err)
			t.Logf("synthetic %s failure: %v", mode, err)
			if mode == "missing_credential" {
				assertions.Zero(submissions.Load())
			} else {
				assertions.Equal(int32(1), submissions.Load())
			}
			assertions.Nil(catalog.publication)
			requirements.NotNil(catalog.failure)
			assertions.Equal(int(requests.Load()), catalog.failure.RequestCount)
			rejected := mode == "authentication" || mode == "missing_credential"
			assertions.Equal(rejected, catalog.failure.Terminal)
			assertions.Equal(rejected, catalog.failure.RetryAt.IsZero())
			if rejected {
				assertions.Equal("provider_rejected", catalog.failure.ReasonCode)
			} else {
				assertions.Equal("provider_interrupted", catalog.failure.ReasonCode)
			}
		})
	}
}

func TestDoclingWorkerSchedulesRetryWhenJobOutlastsTotalTimeout(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"task_id": "synthetic-job", "task_type": "convert", "task_status": "pending"}))
	}))
	t.Cleanup(server.Close)
	c := doclingTestConfig()
	c.Endpoint, c.TotalTimeout, c.PollInterval, c.MaxPollAttempts = server.URL, 200*time.Millisecond, 10*time.Millisecond, 10_000
	provider, err := NewDoclingClient(&c)
	requirements.NoError(err)
	content := mistraltest.MinimalPDF("synthetic source")
	digest := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{
		Documents: c, ProfileID: "synthetic-profile", LeaseOwner: "synthetic-worker", LeaseDuration: time.Minute, RetryDelay: time.Minute,
	})
	requirements.NoError(err)
	_, err = worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	requirements.Error(err)
	requirements.NotNil(catalog.failure)
	assertions.False(catalog.failure.Terminal, "a job that outlasts total_timeout must retry, not need a manual reset")
	assertions.False(catalog.failure.RetryAt.IsZero())
}

func TestDoclingAuthorizationUsesCanonicalProviderTimestamps(t *testing.T) {
	requirements := require.New(t)

	catalog := &workerCatalog{}
	worker, candidate, _ := doclingWorkerFixture(t, doclingWorkerResponse(false), nil, catalog)
	content := mistraltest.MinimalPDF("synthetic source")
	for _, fractional := range []time.Duration{0, 10 * time.Millisecond, time.Microsecond, time.Nanosecond} {
		now := time.Now().UTC().Add(-time.Second).Truncate(time.Second).Add(fractional)
		upload, authorization, err := worker.authorizeUploadAt(content, candidate, worker.input.Routes[candidate.MIMEType].Format,
			store.DocumentExtractionClaim{ExtractionID: "synthetic-extraction"}, now)
		requirements.NoError(err)
		_, err = document.RenderRendition(t.Context(), worker.provider, upload, authorization)
		requirements.NoError(err, "fractional timestamp %s must pass the actual Docbank boundary validator", fractional)
	}
}
