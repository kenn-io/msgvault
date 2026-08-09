package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// VoyageConfig controls the contextualized embeddings client.
type VoyageConfig struct {
	Endpoint   string
	APIKey     string
	Model      string
	Dimension  int
	Timeout    time.Duration
	MaxRetries int
	Limits     RequestLimits
}

// VoyageClient calls Voyage's nested contextualized embeddings endpoint.
type VoyageClient struct {
	cfg  VoyageConfig
	http *http.Client
}

// NewVoyageClient constructs a contextualized embeddings client.
func NewVoyageClient(cfg VoyageConfig) *VoyageClient {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Limits.MaxDocuments == 0 {
		cfg.Limits.MaxDocuments = defaultVoyageRequestLimits.MaxDocuments
	}
	if cfg.Limits.MaxChunks == 0 {
		cfg.Limits.MaxChunks = defaultVoyageRequestLimits.MaxChunks
	}
	if cfg.Limits.MaxUTF8Bytes == 0 {
		cfg.Limits.MaxUTF8Bytes = defaultVoyageRequestLimits.MaxUTF8Bytes
	}
	cfg.Limits = capVoyageRequestLimits(cfg.Limits)
	return &VoyageClient{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

type voyageRequest struct {
	Inputs             [][]string `json:"inputs"`
	Model              string     `json:"model"`
	InputType          string     `json:"input_type"`
	OutputDimension    int        `json:"output_dimension"`
	OutputDType        string     `json:"output_dtype"`
	EnableAutoChunking bool       `json:"enable_auto_chunking"`
}

type voyageResponse struct {
	Data []struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Index int `json:"index"`
	} `json:"data"`
}

// EmbedQuery embeds one independent query with Voyage's query role.
func (c *VoyageClient) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	batches, err := PackDocuments([]DocumentInput{{Chunks: []string{text}}}, c.cfg.Limits)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	inputs := documentInputs(batches[0])
	results, err := c.embedRequest(ctx, inputs, "query")
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(results) != 1 || len(results[0]) != 1 {
		return nil, errors.New("embed query: expected one vector")
	}
	return results[0][0], nil
}

// EmbedDocuments embeds complete document groups and preserves their nested
// order. Local packing splits only between documents. A provider size rejection
// is returned to ContextWorker, which owns partial-success recovery. When a
// later packed request fails, results contains the successful document prefix.
func (c *VoyageClient) EmbedDocuments(ctx context.Context, documents []DocumentInput) ([][][]float32, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	batches, err := PackDocuments(documents, c.cfg.Limits)
	if err != nil {
		return nil, err
	}

	results := make([][][]float32, 0, len(documents))
	for _, batch := range batches {
		batchResults, err := c.embedRequest(ctx, documentInputs(batch), "document")
		if err != nil {
			return results, err
		}
		results = append(results, batchResults...)
	}
	return results, nil
}

func documentInputs(documents []DocumentInput) [][]string {
	inputs := make([][]string, len(documents))
	for i, document := range documents {
		inputs[i] = document.Chunks
	}
	return inputs
}

func (c *VoyageClient) embedRequest(ctx context.Context, inputs [][]string, inputType string) ([][][]float32, error) {
	results, err := c.embedWithRetry(ctx, inputs, inputType)
	if err == nil {
		return results, nil
	}
	var sizeErr *voyageSizeError
	if !errors.As(err, &sizeErr) {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %w", ErrDocumentTooLarge, sizeErr)
}

func (c *VoyageClient) embedWithRetry(ctx context.Context, inputs [][]string, inputType string) ([][][]float32, error) {
	body, err := json.Marshal(voyageRequest{
		Inputs:             inputs,
		Model:              c.cfg.Model,
		InputType:          inputType,
		OutputDimension:    c.cfg.Dimension,
		OutputDType:        "float",
		EnableAutoChunking: false,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal Voyage request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxRetries; attempt++ {
		results, err := c.doVoyageOnce(ctx, body, inputs)
		if err == nil {
			return results, nil
		}
		lastErr = err
		var retry *retryError
		if !errors.As(err, &retry) {
			return nil, err
		}
		if attempt == c.cfg.MaxRetries {
			break
		}
		shift := min(attempt, 8)
		backoff := time.Duration(1<<shift) * 100 * time.Millisecond
		if retry.retryAfterSet {
			backoff = retry.retryAfter
		}
		if backoff <= 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("embed: context canceled during backoff: %w", err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("embed: context canceled during backoff: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
	return nil, fmt.Errorf("embed: giving up after %d attempts: %w", c.cfg.MaxRetries, lastErr)
}

func (c *VoyageClient) doVoyageOnce(ctx context.Context, body []byte, inputs [][]string) ([][][]float32, error) {
	endpoint := strings.TrimRight(c.cfg.Endpoint, "/") + "/contextualizedembeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Voyage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &retryError{err: fmt.Errorf("voyage HTTP request: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, &retryError{
			err:           errors.New("embed: Voyage HTTP 429 (rate limited)"),
			retryAfter:    retryAfter,
			retryAfterSet: ok,
		}
	}
	if resp.StatusCode >= 500 {
		return nil, &retryError{err: fmt.Errorf("embed: Voyage HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 400 {
		return nil, voyageClientError(resp)
	}

	var response voyageResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, &retryError{err: fmt.Errorf("decode Voyage response: %w", err)}
	}
	return c.decodeVoyageResponse(response, inputs)
}

func (c *VoyageClient) decodeVoyageResponse(response voyageResponse, inputs [][]string) ([][][]float32, error) {
	if len(response.Data) != len(inputs) {
		return nil, fmt.Errorf("embed: Voyage outer response count mismatch: got %d, expected %d", len(response.Data), len(inputs))
	}
	results := make([][][]float32, len(inputs))
	outerSeen := make([]bool, len(inputs))
	for _, outer := range response.Data {
		if outer.Index < 0 || outer.Index >= len(inputs) {
			return nil, fmt.Errorf("embed: Voyage invalid outer index %d (len=%d)", outer.Index, len(inputs))
		}
		if outerSeen[outer.Index] {
			return nil, fmt.Errorf("embed: Voyage duplicate outer index %d", outer.Index)
		}
		outerSeen[outer.Index] = true

		expectedChunks := len(inputs[outer.Index])
		if len(outer.Data) != expectedChunks {
			return nil, fmt.Errorf("embed: Voyage inner response count mismatch at outer index %d: got %d, expected %d", outer.Index, len(outer.Data), expectedChunks)
		}
		vectors := make([][]float32, expectedChunks)
		innerSeen := make([]bool, expectedChunks)
		for _, inner := range outer.Data {
			if inner.Index < 0 || inner.Index >= expectedChunks {
				return nil, fmt.Errorf("embed: Voyage invalid inner index %d at outer index %d (len=%d)", inner.Index, outer.Index, expectedChunks)
			}
			if innerSeen[inner.Index] {
				return nil, fmt.Errorf("embed: Voyage duplicate inner index %d at outer index %d", inner.Index, outer.Index)
			}
			innerSeen[inner.Index] = true
			if len(inner.Embedding) != c.cfg.Dimension {
				return nil, fmt.Errorf("embed: Voyage dimension mismatch at outer index %d, inner index %d: got %d, configured %d", outer.Index, inner.Index, len(inner.Embedding), c.cfg.Dimension)
			}
			vectors[inner.Index] = inner.Embedding
		}
		results[outer.Index] = vectors
	}
	for i, seen := range outerSeen {
		if !seen {
			return nil, fmt.Errorf("embed: Voyage missing outer index %d", i)
		}
	}
	return results, nil
}

type voyageSizeError struct {
	message string
}

func (e *voyageSizeError) Error() string { return e.message }

func voyageClientError(resp *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4_096))
	if readErr != nil {
		return fmt.Errorf("embed: Voyage HTTP %d (read error body: %w): %w", resp.StatusCode, readErr, ErrPermanent4xx)
	}
	message := voyageErrorMessage(body)
	if resp.StatusCode == http.StatusBadRequest && isVoyageSizeMessage(message) {
		return &voyageSizeError{message: "embed: Voyage HTTP 400: " + message}
	}
	if message == "" {
		return fmt.Errorf("embed: Voyage HTTP %d: %w", resp.StatusCode, ErrPermanent4xx)
	}
	return fmt.Errorf("embed: Voyage HTTP %d: %s: %w", resp.StatusCode, message, ErrPermanent4xx)
}

func voyageErrorMessage(body []byte) string {
	var payload struct {
		Detail  string `json:"detail"`
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Detail != "" {
			return strings.TrimSpace(payload.Detail)
		}
		if payload.Message != "" {
			return strings.TrimSpace(payload.Message)
		}
		switch value := payload.Error.(type) {
		case string:
			return strings.TrimSpace(value)
		case map[string]any:
			if message, ok := value["message"].(string); ok {
				return strings.TrimSpace(message)
			}
		}
	}
	return strings.TrimSpace(string(body))
}

func isVoyageSizeMessage(message string) bool {
	message = strings.ToLower(message)
	// The live contextual endpoint can describe an oversized example without
	// words such as "exceeds" or "limit". It still identifies the tokenized
	// chunk, its batch, and the model context. Keep this structural match narrow
	// so unrelated model and parameter 400 responses remain permanent errors.
	if strings.Contains(message, "token") && strings.Contains(message, "chunk") &&
		(strings.Contains(message, "batch") || strings.Contains(message, "context")) {
		for _, excess := range []string{"at most", "exceed", "limit", "maximum", "more", "too large", "too long", "too many"} {
			if strings.Contains(message, excess) {
				return true
			}
		}
	}
	checks := []string{
		"batch size is too large",
		"batch size exceeds",
		"total number of tokens",
		"number of tokens in an example",
		"too many chunks",
		"number of chunks exceeds",
		"too many inputs",
		"context length exceeded",
		"exceeds the context length",
	}
	for _, check := range checks {
		if strings.Contains(message, check) {
			return true
		}
	}
	return false
}
