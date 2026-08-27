package embed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

type voyageRequestConsentState struct{ active atomic.Bool }

func (s *voyageRequestConsentState) HasActivePersonSemanticEmbeddingConsent(
	context.Context, string,
) (bool, error) {
	return s.active.Load(), nil
}

func newVoyageSemanticRequestGate() (*voyageRequestConsentState, vector.SemanticPersonEmbeddingGate) {
	consent := &voyageRequestConsentState{}
	consent.active.Store(true)
	config := vector.Config{
		Enabled: true,
		Embeddings: vector.EmbeddingsConfig{
			Endpoint: "https://embedding.example.test/v1", APIFormat: vector.APIFormatVoyageContextual,
			Model: "voyage-context-4", Dimension: 4,
		},
		People: vector.PeopleConfig{
			Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		},
	}
	return consent, vector.NewExactSemanticPersonEmbeddingGate(
		func() (vector.Config, error) { return config, nil }, consent,
	)
}

type capturedVoyageRequest struct {
	Path               string     `json:"-"`
	Authorization      string     `json:"-"`
	Inputs             [][]string `json:"inputs"`
	Model              string     `json:"model"`
	InputType          string     `json:"input_type"`
	OutputDimension    int        `json:"output_dimension"`
	OutputDType        string     `json:"output_dtype"`
	EnableAutoChunking bool       `json:"enable_auto_chunking"`
}

type voyageResponseItem struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
	Text      string    `json:"text,omitempty"`
}

type voyageResponseResult struct {
	Data  []voyageResponseItem `json:"data"`
	Index int                  `json:"index"`
}

type liveContractRequestSummary struct {
	InputShape          string `json:"input_shape"`
	OuterCount          int    `json:"outer_count"`
	ChunkCount          int    `json:"chunk_count"`
	InputUTF8Bytes      int    `json:"input_utf8_bytes"`
	Model               string `json:"model"`
	InputType           string `json:"input_type"`
	OutputDimension     int    `json:"output_dimension"`
	OutputDType         string `json:"output_dtype"`
	EnableAutoChunking  bool   `json:"enable_auto_chunking"`
	AuthorizationHeader bool   `json:"authorization_header_present"`
}

type liveContractResponseSummary struct {
	StatusCode   int      `json:"status_code"`
	OuterCount   int      `json:"outer_count"`
	ChunkCount   int      `json:"chunk_count"`
	Dimensions   []int    `json:"dimensions,omitempty"`
	TotalTokens  int      `json:"total_tokens"`
	ErrorKeys    []string `json:"error_keys,omitempty"`
	ErrorSignals []string `json:"error_signals,omitempty"`
}

type liveContractEvidence struct {
	Requests    []liveContractRequestSummary  `json:"requests"`
	Responses   []liveContractResponseSummary `json:"responses"`
	Cases       map[string]string             `json:"cases"`
	ProxyErrors []string                      `json:"proxy_errors,omitempty"`
}

type liveContractObserver struct {
	mu       sync.Mutex
	evidence liveContractEvidence
}

func decodeVoyageRequest(t *testing.T, r *http.Request) capturedVoyageRequest {
	t.Helper()
	var got capturedVoyageRequest
	require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
	got.Path = r.URL.Path
	got.Authorization = r.Header.Get("Authorization")
	return got
}

func writeVoyageResponse(t *testing.T, w http.ResponseWriter, results []voyageResponseResult) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"data":  results,
		"model": "voyage-context-4",
		"usage": map[string]int{"total_tokens": 10},
	}))
}

func sequentialVoyageResults(inputs [][]string) []voyageResponseResult {
	results := make([]voyageResponseResult, len(inputs))
	value := float32(1)
	for outer, chunks := range inputs {
		results[outer].Index = outer
		for inner, text := range chunks {
			vector := make([]float32, 4)
			for i := range vector {
				vector[i] = value
			}
			value++
			results[outer].Data = append(results[outer].Data, voyageResponseItem{
				Embedding: vector,
				Index:     inner,
				Text:      text,
			})
		}
	}
	return results
}

func (o *liveContractObserver) recordRequest(r *http.Request, body []byte) {
	var envelope struct {
		Inputs             json.RawMessage `json:"inputs"`
		Model              string          `json:"model"`
		InputType          string          `json:"input_type"`
		OutputDimension    int             `json:"output_dimension"`
		OutputDType        string          `json:"output_dtype"`
		EnableAutoChunking bool            `json:"enable_auto_chunking"`
	}
	_ = json.Unmarshal(body, &envelope)
	summary := liveContractRequestSummary{
		InputUTF8Bytes:      len(envelope.Inputs),
		Model:               envelope.Model,
		InputType:           envelope.InputType,
		OutputDimension:     envelope.OutputDimension,
		OutputDType:         envelope.OutputDType,
		EnableAutoChunking:  envelope.EnableAutoChunking,
		AuthorizationHeader: r.Header.Get("Authorization") != "",
	}
	var nested [][]json.RawMessage
	if json.Unmarshal(envelope.Inputs, &nested) == nil {
		summary.InputShape = "nested"
		summary.OuterCount = len(nested)
		for _, chunks := range nested {
			summary.ChunkCount += len(chunks)
		}
	} else {
		var flat []json.RawMessage
		if json.Unmarshal(envelope.Inputs, &flat) == nil {
			summary.InputShape = "flat"
			summary.OuterCount = len(flat)
			summary.ChunkCount = len(flat)
		} else {
			summary.InputShape = "invalid"
		}
	}
	o.mu.Lock()
	o.evidence.Requests = append(o.evidence.Requests, summary)
	o.mu.Unlock()
}

func (o *liveContractObserver) recordResponse(resp *http.Response, body []byte) {
	summary := liveContractResponseSummary{StatusCode: resp.StatusCode}
	var envelope struct {
		Data []struct {
			Data []struct {
				Embedding []json.RawMessage `json:"embedding"`
				Index     int               `json:"index"`
			} `json:"data"`
			Index int `json:"index"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		summary.OuterCount = len(envelope.Data)
		for _, outer := range envelope.Data {
			summary.ChunkCount += len(outer.Data)
			for _, inner := range outer.Data {
				summary.Dimensions = append(summary.Dimensions, len(inner.Embedding))
			}
		}
		summary.TotalTokens = envelope.Usage.TotalTokens
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var payload any
		if json.Unmarshal(body, &payload) == nil {
			keys := make(map[string]struct{})
			collectLiveContractJSONKeys(payload, keys)
			for key := range keys {
				summary.ErrorKeys = append(summary.ErrorKeys, key)
			}
			sort.Strings(summary.ErrorKeys)
		}
		lower := strings.ToLower(string(body))
		for _, signal := range []string{"at most", "batch", "chunk", "context", "exceed", "input", "limit", "long", "maximum", "more", "token", "too large", "too many"} {
			if strings.Contains(lower, signal) {
				summary.ErrorSignals = append(summary.ErrorSignals, signal)
			}
		}
	}
	o.mu.Lock()
	o.evidence.Responses = append(o.evidence.Responses, summary)
	o.mu.Unlock()
}

func (o *liveContractObserver) recordProxyError(class string) {
	o.mu.Lock()
	o.evidence.ProxyErrors = append(o.evidence.ProxyErrors, class)
	o.mu.Unlock()
}

func collectLiveContractJSONKeys(value any, keys map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			keys[key] = struct{}{}
			collectLiveContractJSONKeys(child, keys)
		}
	case []any:
		for _, child := range typed {
			collectLiveContractJSONKeys(child, keys)
		}
	}
}

func newLiveContractProxy(t *testing.T, observer *liveContractObserver) *httptest.Server {
	t.Helper()
	target, err := url.Parse("https://api.voyageai.com")
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) {
		request.SetURL(target)
		request.Out.Host = target.Host
	}}
	proxy.ModifyResponse = func(response *http.Response) error {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			observer.recordProxyError("response_body_read")
			return err
		}
		_ = response.Body.Close()
		observer.recordResponse(response, body)
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		observer.recordProxyError("reverse_proxy")
		http.Error(w, "contract proxy error", http.StatusBadGateway)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			observer.recordProxyError("request_body_read")
			http.Error(w, "contract proxy request error", http.StatusInternalServerError)
			return
		}
		_ = request.Body.Close()
		observer.recordRequest(request, body)
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		proxy.ServeHTTP(w, request)
	}))
}

func classifyLiveContractError(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, embed.ErrDocumentTooLarge) {
		return "document_too_large"
	}
	if errors.Is(err, embed.ErrPermanent4xx) {
		return "permanent_4xx"
	}
	return "unexpected_error"
}

func liveVoyageClient(endpoint, apiKey, model string, dimension int) *embed.VoyageClient {
	return embed.NewVoyageClient(embed.VoyageConfig{
		Endpoint: endpoint, APIKey: apiKey, Model: model, Dimension: dimension,
		Timeout: 45 * time.Second, MaxRetries: 1,
	})
}

func TestVoyageLiveContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if os.Getenv("VOYAGE_LIVE_CONTRACT") != "1" {
		t.Skip("set VOYAGE_LIVE_CONTRACT=1 to run the authenticated contract probe")
	}
	apiKey := os.Getenv("VOYAGE_API_KEY")
	require.NotEmpty(apiKey, "VOYAGE_API_KEY is required when VOYAGE_LIVE_CONTRACT=1")

	observer := &liveContractObserver{evidence: liveContractEvidence{Cases: make(map[string]string)}}
	proxy := newLiveContractProxy(t, observer)
	t.Cleanup(proxy.Close)
	defer func() {
		observer.mu.Lock()
		payload, err := json.Marshal(observer.evidence)
		observer.mu.Unlock()
		require.NoError(err)
		t.Logf("VOYAGE_CONTRACT_JSON=%s", payload)
	}()
	endpoint := proxy.URL + "/v1"

	client := liveVoyageClient(endpoint, apiKey, "voyage-context-4", 1024)
	documents, err := client.EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"Synthetic contract document A, chunk 1.", "Synthetic contract document A, chunk 2."}},
		{Chunks: []string{"Synthetic contract document B, chunk 1."}},
	})
	require.NoError(err)
	require.Len(documents, 2)
	assert.Len(documents[0], 2)
	assert.Len(documents[1], 1)
	assert.Len(documents[0][0], 1024)
	observer.evidence.Cases["nested_documents"] = "ok"

	query, err := client.EmbedQuery(t.Context(), "Synthetic contract query.")
	require.NoError(err)
	assert.Len(query, 1024)
	observer.evidence.Cases["nested_query"] = "ok"

	_, err = client.EmbedDocuments(t.Context(), []embed.DocumentInput{{
		Chunks: []string{strings.Repeat("🧬", 24_000)},
	}})
	overContextClass := classifyLiveContractError(err)
	observer.evidence.Cases["over_32k_context"] = overContextClass
	assert.Equal("document_too_large", overContextClass)

	_, err = liveVoyageClient(endpoint, "invalid-live-contract-key", "voyage-context-4", 1024).
		EmbedQuery(t.Context(), "Synthetic auth-error query.")
	authClass := classifyLiveContractError(err)
	observer.evidence.Cases["invalid_auth"] = authClass
	assert.Equal("permanent_4xx", authClass)

	_, err = liveVoyageClient(endpoint, apiKey, "voyage-context-4-invalid", 1024).
		EmbedQuery(t.Context(), "Synthetic model-error query.")
	modelClass := classifyLiveContractError(err)
	observer.evidence.Cases["invalid_model"] = modelClass
	assert.Equal("permanent_4xx", modelClass)

	_, err = liveVoyageClient(endpoint, apiKey, "voyage-context-4", 7).
		EmbedQuery(t.Context(), "Synthetic dimension-error query.")
	dimensionClass := classifyLiveContractError(err)
	observer.evidence.Cases["invalid_dimension"] = dimensionClass
	assert.Equal("permanent_4xx", dimensionClass)

	flatBody, err := json.Marshal(map[string]any{
		"inputs": []string{"Synthetic flat query."}, "model": "voyage-context-4", "input_type": "query",
		"output_dimension": 1024, "output_dtype": "float", "enable_auto_chunking": false,
	})
	require.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		endpoint+"/contextualizedembeddings", bytes.NewReader(flatBody))
	require.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := (&http.Client{Timeout: 45 * time.Second}).Do(request)
	require.NoError(err)
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	require.NoError(readErr)
	require.NoError(closeErr)
	observer.evidence.Cases["flat_query"] = fmt.Sprintf("http_%d", response.StatusCode)
	assert.Equal(http.StatusOK, response.StatusCode)

	observer.mu.Lock()
	evidence := observer.evidence
	observer.mu.Unlock()
	require.GreaterOrEqual(len(evidence.Requests), 7)
	require.GreaterOrEqual(len(evidence.Responses), 7)
	assert.Empty(evidence.ProxyErrors)
	assert.Equal("nested", evidence.Requests[0].InputShape)
	assert.Equal(2, evidence.Requests[0].OuterCount)
	assert.Equal(3, evidence.Requests[0].ChunkCount)
	assert.Equal("document", evidence.Requests[0].InputType)
	assert.Equal(1024, evidence.Requests[0].OutputDimension)
	assert.Equal("float", evidence.Requests[0].OutputDType)
	assert.False(evidence.Requests[0].EnableAutoChunking)
	assert.True(evidence.Requests[0].AuthorizationHeader)
	assert.Equal(2, evidence.Responses[0].OuterCount)
	assert.Equal(3, evidence.Responses[0].ChunkCount)
	assert.Equal([]int{1024, 1024, 1024}, evidence.Responses[0].Dimensions)
	assert.Positive(evidence.Responses[0].TotalTokens)
	assert.Equal("nested", evidence.Requests[1].InputShape)
	assert.Equal("query", evidence.Requests[1].InputType)
	assert.Equal(1, evidence.Responses[1].OuterCount)
	assert.Equal(1, evidence.Responses[1].ChunkCount)
	assert.Equal([]int{1024}, evidence.Responses[1].Dimensions)
	assert.Positive(evidence.Responses[1].TotalTokens)
	assert.Subset(evidence.Responses[2].ErrorSignals, []string{"batch", "chunk", "context", "input", "token"})
	assert.Condition(func() bool {
		for _, signal := range evidence.Responses[2].ErrorSignals {
			if signal == "at most" || signal == "exceed" || signal == "limit" || signal == "long" ||
				signal == "maximum" || signal == "more" || signal == "too large" || signal == "too many" {
				return true
			}
		}
		return false
	}, "size error must include a sanitized excess signal")
}

func newVoyageClient(endpoint string, overrides ...func(*embed.VoyageConfig)) *embed.VoyageClient {
	cfg := embed.VoyageConfig{
		Endpoint:  endpoint,
		APIKey:    "test-key",
		Model:     "voyage-context-4",
		Dimension: 4,
		Limits: embed.RequestLimits{
			MaxDocuments: 10,
			MaxChunks:    20,
			MaxUTF8Bytes: 10_000,
		},
	}
	for _, override := range overrides {
		override(&cfg)
	}
	return embed.NewVoyageClient(cfg)
}

// TestVoyageClient_EmbedDocumentsUsesNestedDocumentInput catches accidental
// flattening and omission of the parameters that make the endpoint contextual.
func TestVoyageClient_EmbedDocumentsUsesNestedDocumentInput(t *testing.T) {
	assert := assert.New(t)
	var captured capturedVoyageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = decodeVoyageRequest(t, r)
		results := sequentialVoyageResults(captured.Inputs)
		results[0].Data[0], results[0].Data[1] = results[0].Data[1], results[0].Data[0]
		results[0], results[1] = results[1], results[0]
		writeVoyageResponse(t, w, results)
	}))
	t.Cleanup(server.Close)
	client := newVoyageClient(server.URL + "/v1")

	got, err := client.EmbedDocuments(context.Background(), []embed.DocumentInput{
		{Chunks: []string{"first", "second"}},
		{Chunks: []string{"third"}},
	})

	require.NoError(t, err)
	assert.Equal("/v1/contextualizedembeddings", captured.Path)
	assert.Equal("Bearer test-key", captured.Authorization)
	assert.Equal("document", captured.InputType)
	assert.Equal("voyage-context-4", captured.Model)
	assert.Equal(4, captured.OutputDimension)
	assert.Equal("float", captured.OutputDType)
	assert.False(captured.EnableAutoChunking)
	assert.Equal([][]string{{"first", "second"}, {"third"}}, captured.Inputs)
	assert.Equal([][][]float32{
		{{1, 1, 1, 1}, {2, 2, 2, 2}},
		{{3, 3, 3, 3}},
	}, got)
}

func TestVoyageClient_LaterPackedRequestReturnsSuccessfulPrefix(t *testing.T) {
	assert := assert.New(t)
	var requests atomic.Int32
	var handlerMu sync.Mutex
	var handlerErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request capturedVoyageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerMu.Lock()
			handlerErr = err
			handlerMu.Unlock()
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		if requests.Add(1) == 2 {
			http.Error(w, "synthetic provider failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data":  sequentialVoyageResults(request.Inputs),
			"model": "voyage-context-4",
		}); err != nil {
			handlerMu.Lock()
			handlerErr = err
			handlerMu.Unlock()
		}
	}))
	t.Cleanup(server.Close)

	got, err := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
		cfg.MaxRetries = 1
		cfg.Limits.MaxDocuments = 1
	}).EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"first document"}},
		{Chunks: []string{"second document"}},
	})

	require.Error(t, err)
	handlerMu.Lock()
	defer handlerMu.Unlock()
	require.NoError(t, handlerErr)
	assert.Equal(int32(2), requests.Load())
	assert.Equal([][][]float32{{{1, 1, 1, 1}}}, got,
		"the caller needs the successful prefix to avoid repeating a billed request")
}

// TestVoyageClientBeforeRequestFencesLaterPackedRequest catches a gate that is
// checked only once around EmbedDocuments. Voyage may split one logical call
// into several provider requests, and each packed request needs fresh
// authorization.
func TestVoyageClientBeforeRequestFencesLaterPackedRequest(t *testing.T) {
	var requests atomic.Int32
	consent, gate := newVoyageSemanticRequestGate()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeVoyageRequest(t, r)
		requests.Add(1)
		consent.active.Store(false)
		writeVoyageResponse(t, w, sequentialVoyageResults(request.Inputs))
	}))
	t.Cleanup(server.Close)

	got, err := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
		cfg.MaxRetries = 1
		cfg.Limits.MaxDocuments = 1
		cfg.BeforeRequest = gate.Check
	}).EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"first curated person"}},
		{Chunks: []string{"second curated person"}},
	})

	require.ErrorIs(t, err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	assert.Equal(t, int32(1), requests.Load(),
		"policy removal after the first packed request must fence the second")
	assert.Equal(t, [][][]float32{{{1, 1, 1, 1}}}, got,
		"the authorized successful prefix remains available to the caller")
}

// TestVoyageClientBeforeRequestRejectsProviderRedirects catches gated person
// clients that let net/http replay nested document or query input to a
// provider-selected URL.
func TestVoyageClientBeforeRequestRejectsProviderRedirects(t *testing.T) {
	operations := []struct {
		name    string
		wantErr string
		run     func(context.Context, *embed.VoyageClient) error
	}{
		{
			name:    "documents",
			wantErr: "embedding provider redirects are not allowed",
			run: func(ctx context.Context, client *embed.VoyageClient) error {
				_, err := client.EmbedDocuments(ctx, []embed.DocumentInput{{Chunks: []string{"curated person"}}})
				return err
			},
		},
		{
			name:    "query",
			wantErr: "embed query: embedding provider redirects are not allowed",
			run: func(ctx context.Context, client *embed.VoyageClient) error {
				_, err := client.EmbedQuery(ctx, "curated person query")
				return err
			},
		},
	}
	statuses := []struct {
		name string
		code int
	}{
		{name: "307", code: http.StatusTemporaryRedirect},
		{name: "308", code: http.StatusPermanentRedirect},
	}
	destinations := []struct {
		name        string
		crossOrigin bool
	}{
		{name: "same_origin"},
		{name: "cross_origin", crossOrigin: true},
	}

	for _, operation := range operations {
		for _, status := range statuses {
			for _, destination := range destinations {
				t.Run(operation.name+"/"+status.name+"/"+destination.name, func(t *testing.T) {
					var originRequests atomic.Int32
					var targetRequests atomic.Int32
					redirectLocation := "/redirected"
					if destination.crossOrigin {
						target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							targetRequests.Add(1)
							writeVoyageResponse(t, w, sequentialVoyageResults([][]string{{"redirected"}}))
						}))
						t.Cleanup(target.Close)
						redirectLocation = target.URL + "/redirected"
					}
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/contextualizedembeddings":
							originRequests.Add(1)
							w.Header().Set("Location", redirectLocation)
							w.WriteHeader(status.code)
						case "/redirected":
							targetRequests.Add(1)
							writeVoyageResponse(t, w, sequentialVoyageResults([][]string{{"redirected"}}))
						default:
							http.NotFound(w, r)
						}
					}))
					t.Cleanup(origin.Close)
					_, gate := newVoyageSemanticRequestGate()
					client := newVoyageClient(origin.URL, func(cfg *embed.VoyageConfig) {
						cfg.MaxRetries = 3
						cfg.BeforeRequest = gate.Check
					})

					err := operation.run(t.Context(), client)

					assert := assert.New(t)
					require.ErrorIs(t, err, embed.ErrEmbeddingProviderRedirect)
					assert.Equal(operation.wantErr, err.Error())
					assert.Equal(int32(1), originRequests.Load(), "redirect responses must not be retried")
					assert.Zero(targetRequests.Load(), "person text must not reach the redirect target")
				})
			}
		}
	}
}

// TestVoyageClientWithoutBeforeRequestFollowsProviderRedirects protects the
// message client composition, which intentionally retains default redirects.
func TestVoyageClientWithoutBeforeRequestFollowsProviderRedirects(t *testing.T) {
	for _, status := range []struct {
		name string
		code int
	}{
		{name: "307", code: http.StatusTemporaryRedirect},
		{name: "308", code: http.StatusPermanentRedirect},
	} {
		t.Run(status.name, func(t *testing.T) {
			var originRequests atomic.Int32
			var targetRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/contextualizedembeddings":
					originRequests.Add(1)
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status.code)
				case "/redirected":
					targetRequests.Add(1)
					request := decodeVoyageRequest(t, r)
					assert.Equal(t, "query", request.InputType)
					assert.Equal(t, [][]string{{"message query"}}, request.Inputs)
					writeVoyageResponse(t, w, sequentialVoyageResults(request.Inputs))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
				cfg.MaxRetries = 1
			})

			vector, err := client.EmbedQuery(t.Context(), "message query")

			assert := assert.New(t)
			require.NoError(t, err)
			assert.Equal([]float32{1, 1, 1, 1}, vector)
			assert.Equal(int32(1), originRequests.Load())
			assert.Equal(int32(1), targetRequests.Load())
		})
	}
}

// TestVoyageClient_EmbedQueryUsesQueryRole catches query calls that use the
// document role or the flat /embeddings request shape.
func TestVoyageClient_EmbedQueryUsesQueryRole(t *testing.T) {
	assert := assert.New(t)
	var captured capturedVoyageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = decodeVoyageRequest(t, r)
		writeVoyageResponse(t, w, sequentialVoyageResults(captured.Inputs))
	}))
	t.Cleanup(server.Close)

	got, err := newVoyageClient(server.URL+"/v1").EmbedQuery(context.Background(), "find this")

	require.NoError(t, err)
	assert.Equal("/v1/contextualizedembeddings", captured.Path)
	assert.Equal("query", captured.InputType)
	assert.Equal([][]string{{"find this"}}, captured.Inputs)
	assert.Equal([]float32{1, 1, 1, 1}, got)
}

func TestVoyageClient_AppliesEmbeddingPrefixesByRole(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var captured []capturedVoyageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeVoyageRequest(t, r)
		captured = append(captured, request)
		writeVoyageResponse(t, w, sequentialVoyageResults(request.Inputs))
	}))
	t.Cleanup(server.Close)
	client := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
		cfg.DocumentPrefix = "search_document: "
		cfg.QueryPrefix = "search_query: "
	})

	_, err := client.EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"first chunk", "second chunk"}},
	})
	must.NoError(err)
	_, err = client.EmbedQuery(t.Context(), "find this")
	must.NoError(err)

	must.Len(captured, 2)
	check.Equal("document", captured[0].InputType)
	check.Equal([][]string{{"search_document: first chunk", "search_document: second chunk"}}, captured[0].Inputs)
	check.Equal("query", captured[1].InputType)
	check.Equal([][]string{{"search_query: find this"}}, captured[1].Inputs)
}

func TestVoyageClient_RejectsDuplicateOuterIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeVoyageResponse(t, w, []voyageResponseResult{
			{Index: 0, Data: []voyageResponseItem{{Index: 0, Embedding: []float32{1, 1, 1, 1}}}},
			{Index: 0, Data: []voyageResponseItem{{Index: 0, Embedding: []float32{2, 2, 2, 2}}}},
		})
	}))
	t.Cleanup(server.Close)

	_, err := newVoyageClient(server.URL).EmbedDocuments(context.Background(), []embed.DocumentInput{
		{Chunks: []string{"first"}},
		{Chunks: []string{"second"}},
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate outer index 0")
}

func TestVoyageClient_RejectsDuplicateInnerIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeVoyageResponse(t, w, []voyageResponseResult{{
			Index: 0,
			Data: []voyageResponseItem{
				{Index: 0, Embedding: []float32{1, 1, 1, 1}},
				{Index: 0, Embedding: []float32{2, 2, 2, 2}},
			},
		}})
	}))
	t.Cleanup(server.Close)

	_, err := newVoyageClient(server.URL).EmbedDocuments(context.Background(), []embed.DocumentInput{
		{Chunks: []string{"first", "second"}},
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate inner index 0")
}

func TestVoyageClient_RejectsMalformedIndexedResponse(t *testing.T) {
	tests := []struct {
		name    string
		result  voyageResponseResult
		wantErr string
	}{
		{
			name: "negative outer index",
			result: voyageResponseResult{Index: -1, Data: []voyageResponseItem{
				{Index: 0, Embedding: []float32{1, 1, 1, 1}},
			}},
			wantErr: "outer index -1",
		},
		{
			name: "out of range inner index",
			result: voyageResponseResult{Index: 0, Data: []voyageResponseItem{
				{Index: 1, Embedding: []float32{1, 1, 1, 1}},
			}},
			wantErr: "inner index 1",
		},
		{
			name: "wrong dimension",
			result: voyageResponseResult{Index: 0, Data: []voyageResponseItem{
				{Index: 0, Embedding: []float32{1, 1, 1}},
			}},
			wantErr: "dimension mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeVoyageResponse(t, w, []voyageResponseResult{tt.result})
			}))
			t.Cleanup(server.Close)

			_, err := newVoyageClient(server.URL).EmbedDocuments(context.Background(), []embed.DocumentInput{
				{Chunks: []string{"first"}},
			})

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestVoyageClient_Size400ReturnsBatchToWorker keeps size recovery in one
// layer. ContextWorker owns document bisection so it can retain successful
// siblings when one leaf document is too large.
func TestVoyageClient_Size400ReturnsBatchToWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var mu sync.Mutex
	var calls [][][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeVoyageRequest(t, r)
		mu.Lock()
		calls = append(calls, request.Inputs)
		mu.Unlock()
		if len(request.Inputs) == 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"The total number of tokens in the batch exceeds the limit"}`))
			return
		}
		writeVoyageResponse(t, w, sequentialVoyageResults(request.Inputs))
	}))
	t.Cleanup(server.Close)
	documents := []embed.DocumentInput{
		{Chunks: []string{"a1", "a2"}},
		{Chunks: []string{"b1", "b2", "b3"}},
		{Chunks: []string{"c1"}},
	}

	_, err := newVoyageClient(server.URL).EmbedDocuments(context.Background(), documents)

	require.ErrorIs(err, embed.ErrDocumentTooLarge)
	assert.Equal([][][]string{
		{{"a1", "a2"}, {"b1", "b2", "b3"}, {"c1"}},
	}, calls)
}

func TestVoyageClient_OneDocumentSize400ReturnsDocumentTooLarge(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"The total number of tokens in the batch exceeds the limit"}`))
	}))
	t.Cleanup(server.Close)

	_, err := newVoyageClient(server.URL).EmbedDocuments(context.Background(), []embed.DocumentInput{
		{Chunks: []string{"a1", "a2"}},
	})

	require.ErrorIs(t, err, embed.ErrDocumentTooLarge)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestVoyageClient_LiveContextSizeShapeReturnsDocumentTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"An input chunk in the batch has more tokens than the model context supports"}`))
	}))
	t.Cleanup(server.Close)

	_, err := newVoyageClient(server.URL).EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"synthetic oversized chunk"}},
	})

	assert.ErrorIs(t, err, embed.ErrDocumentTooLarge)
}

func TestVoyageClient_StructuralNounsWithoutExcessRemainPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Chunk token batches are no longer supported for this model context"}`))
	}))
	t.Cleanup(server.Close)

	_, err := newVoyageClient(server.URL).EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"synthetic parameter-error chunk"}},
	})

	require.ErrorIs(t, err, embed.ErrPermanent4xx)
	assert.NotErrorIs(t, err, embed.ErrDocumentTooLarge)
}

func TestVoyageClient_AuthAndParameter4xxFailFast(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: `{"detail":"invalid API key"}`},
		{name: "forbidden", statusCode: http.StatusForbidden, body: `{"detail":"forbidden IP"}`},
		{name: "bad model", statusCode: http.StatusBadRequest, body: `{"detail":"model must be voyage-context-4"}`},
		{name: "bad parameter", statusCode: http.StatusBadRequest, body: `{"detail":"output_dimension is invalid"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			_, err := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
				cfg.MaxRetries = 5
			}).EmbedQuery(context.Background(), "query")

			require.Error(t, err)
			require.NotErrorIs(t, err, embed.ErrDocumentTooLarge)
			assert.Equal(t, int32(1), attempts.Load())
		})
	}
}

func TestVoyageClient_RetriesTransientResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "rate limit", statusCode: http.StatusTooManyRequests},
		{name: "server error", statusCode: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := decodeVoyageRequest(t, r)
				if attempts.Add(1) == 1 {
					if tt.statusCode == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "0")
					}
					http.Error(w, "transient", tt.statusCode)
					return
				}
				writeVoyageResponse(t, w, sequentialVoyageResults(request.Inputs))
			}))
			t.Cleanup(server.Close)

			got, err := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
				cfg.MaxRetries = 2
			}).EmbedQuery(context.Background(), "query")

			require.NoError(t, err)
			assert.Equal(t, []float32{1, 1, 1, 1}, got)
			assert.Equal(t, int32(2), attempts.Load())
		})
	}
}

// TestVoyageClientBeforeRequestFencesQueryRetry covers the query-role retry
// path separately from packed document calls.
func TestVoyageClientBeforeRequestFencesQueryRetry(t *testing.T) {
	var requests atomic.Int32
	consent, gate := newVoyageSemanticRequestGate()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		consent.active.Store(false)
		w.Header().Set("Retry-After", "0")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	_, err := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
		cfg.MaxRetries = 3
		cfg.BeforeRequest = gate.Check
	}).EmbedQuery(t.Context(), "curated person query")

	require.ErrorIs(t, err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	assert.Equal(t, int32(1), requests.Load(),
		"revocation after the first 429 must fence the Voyage query retry")
}

func TestVoyageClient_ContextCancellationStopsRetryBackoff(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "transient", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	time.AfterFunc(25*time.Millisecond, cancel)

	_, err := newVoyageClient(server.URL, func(cfg *embed.VoyageConfig) {
		cfg.MaxRetries = 5
	}).EmbedQuery(ctx, "query")

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), attempts.Load(), "attempts after cancellation")
}
