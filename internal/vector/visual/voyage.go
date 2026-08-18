package visual

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultVoyageTimeout          = 45 * time.Second
	defaultVoyageRetries          = 3
	defaultVoyageMaxRequestBytes  = int64(64 << 20)
	defaultVoyageMaxResponseBytes = int64(8 << 20)
	defaultVoyageMaxBatchItems    = 64
)

type VoyageConfig struct {
	Endpoint         string
	APIKey           string
	Model            string
	Dimension        int
	Timeout          time.Duration
	MaxRetries       int
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxBatchItems    int
	RetryBaseDelay   time.Duration
	HTTPClient       *http.Client
}

type VoyageClient struct {
	config VoyageConfig
	http   *http.Client
}

func NewVoyageClient(config VoyageConfig) (*VoyageClient, error) {
	if strings.TrimSpace(config.Endpoint) == "" || strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("voyage endpoint and model are required")
	}
	if config.Dimension <= 0 {
		return nil, errors.New("voyage dimension must be positive")
	}
	if config.Timeout == 0 {
		config.Timeout = defaultVoyageTimeout
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = defaultVoyageRetries
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = defaultVoyageMaxRequestBytes
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultVoyageMaxResponseBytes
	}
	if config.MaxBatchItems == 0 {
		config.MaxBatchItems = defaultVoyageMaxBatchItems
	}
	if config.RetryBaseDelay == 0 {
		config.RetryBaseDelay = 100 * time.Millisecond
	}
	if config.Timeout <= 0 || config.MaxRetries < 1 || config.MaxRetries > 10 ||
		config.MaxRequestBytes < 1 || config.MaxResponseBytes < 1 ||
		config.MaxBatchItems < 1 || config.RetryBaseDelay < 0 {
		return nil, errors.New("invalid Voyage request limits")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: config.Timeout}
	}
	return &VoyageClient{config: config, http: httpClient}, nil
}

type voyageMultimodalRequest struct {
	Inputs          []voyageMultimodalInput `json:"inputs"`
	Model           string                  `json:"model"`
	InputType       string                  `json:"input_type"`
	Truncation      bool                    `json:"truncation"`
	OutputDimension int                     `json:"output_dimension"`
}

type voyageMultimodalInput struct {
	Content []voyageContentPart `json:"content"`
}

type voyageContentPart struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	ImageBase64 string `json:"image_base64,omitempty"`
	VideoBase64 string `json:"video_base64,omitempty"`
}

type voyageMultimodalResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		TotalTokens *int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (c *VoyageClient) EmbedDocuments(
	ctx context.Context,
	documents []DocumentInput,
) ([]EmbeddingResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if len(documents) > c.config.MaxBatchItems {
		return nil, providerError(ErrProviderBatchTooLarge, 0, nil)
	}
	if estimatedDocumentsRequestBytes(documents) > c.config.MaxRequestBytes {
		return nil, providerError(ErrProviderBatchTooLarge, 0, nil)
	}
	inputs := make([]voyageMultimodalInput, len(documents))
	for index, document := range documents {
		content, err := voyageDocumentContent(document)
		if err != nil {
			return nil, fmt.Errorf("build visual document %d: %w", index, err)
		}
		inputs[index] = voyageMultimodalInput{Content: content}
	}
	vectors, usage, err := c.embed(ctx, inputs, "document")
	if err != nil {
		return nil, err
	}
	results := make([]EmbeddingResult, len(documents))
	for index := range documents {
		results[index] = EmbeddingResult{Owner: documents[index].Owner, Vector: vectors[index]}
	}
	if len(results) > 0 {
		results[0].Usage = usage
	}
	return results, nil
}

func (c *VoyageClient) EmbedQuery(
	ctx context.Context,
	query QueryInput,
) ([]float32, Usage, error) {
	if estimatedQueryRequestBytes(query) > c.config.MaxRequestBytes {
		return nil, Usage{}, providerError(ErrProviderBatchTooLarge, 0, nil)
	}
	content := make([]voyageContentPart, 0, 2)
	if text := strings.TrimSpace(query.Text); text != "" {
		content = append(content, voyageContentPart{Type: "text", Text: text})
	}
	if query.Image != nil {
		part, err := voyageMediaPart(query.Image)
		if err != nil {
			return nil, Usage{}, err
		}
		if part.Type != "image_base64" {
			return nil, Usage{}, errors.New("visual query media must be an image")
		}
		content = append(content, part)
	}
	if len(content) == 0 {
		return nil, Usage{}, errors.New("visual query requires text or image")
	}
	vectors, usage, err := c.embed(ctx, []voyageMultimodalInput{{Content: content}}, "query")
	if err != nil {
		return nil, Usage{}, err
	}
	return vectors[0], usage, nil
}

// Estimate the encoded request before base64 or JSON allocation. The fixed
// allowance covers DTO keys, escaping, and model metadata; the exact marshaled
// limit remains the final authority in embed.
func estimatedDocumentsRequestBytes(documents []DocumentInput) int64 {
	total := int64(512 + len(documents)*128)
	for _, document := range documents {
		for _, part := range document.Parts {
			total += int64(len(part.Text)) * 2
			if part.Media != nil {
				total += int64(base64.StdEncoding.EncodedLen(len(part.Media.Bytes))) + 128
			}
		}
	}
	return total
}

func estimatedQueryRequestBytes(query QueryInput) int64 {
	total := int64(512 + len(query.Text)*6)
	if query.Image != nil {
		total += int64(base64.StdEncoding.EncodedLen(len(query.Image.Bytes))) + 128
	}
	return total
}

func voyageDocumentContent(document DocumentInput) ([]voyageContentPart, error) {
	if document.Owner.MessageID <= 0 || document.Owner.BlobHash == "" ||
		document.Owner.MediaInputKey == "" || document.Revision == "" {
		return nil, errors.New("invalid visual document identity")
	}
	if len(document.Parts) == 0 {
		return nil, errors.New("visual document has no input parts")
	}
	content := make([]voyageContentPart, 0, len(document.Parts))
	mediaCount := 0
	for _, part := range document.Parts {
		switch {
		case part.Media != nil && part.Text != "":
			return nil, errors.New("visual input part cannot contain text and media")
		case part.Media != nil:
			mediaCount++
			mediaPart, err := voyageMediaPart(part.Media)
			if err != nil {
				return nil, err
			}
			content = append(content, mediaPart)
		case strings.TrimSpace(part.Text) != "":
			content = append(content, voyageContentPart{Type: "text", Text: part.Text})
		default:
			return nil, errors.New("visual input part is empty")
		}
	}
	if mediaCount != 1 {
		return nil, errors.New("visual document requires exactly one media part")
	}
	return content, nil
}

func voyageMediaPart(media *MediaInput) (voyageContentPart, error) {
	if media == nil || len(media.Bytes) == 0 {
		return voyageContentPart{}, errors.New("visual media bytes are required")
	}
	encoded := base64.StdEncoding.EncodeToString(media.Bytes)
	dataURL := "data:" + media.MIMEType + ";base64," + encoded
	switch media.Kind {
	case MediaKindImage:
		switch media.MIMEType {
		case "image/jpeg", "image/png", "image/webp", "image/gif":
			return voyageContentPart{Type: "image_base64", ImageBase64: dataURL}, nil
		default:
			return voyageContentPart{}, errors.New("unsupported visual image media type")
		}
	case MediaKindVideo:
		if media.MIMEType != "video/mp4" {
			return voyageContentPart{}, errors.New("unsupported visual video media type")
		}
		return voyageContentPart{Type: "video_base64", VideoBase64: dataURL}, nil
	default:
		return voyageContentPart{}, errors.New("unsupported visual media kind")
	}
}

func (c *VoyageClient) embed(
	ctx context.Context,
	inputs []voyageMultimodalInput,
	inputType string,
) ([][]float32, Usage, error) {
	body, err := json.Marshal(voyageMultimodalRequest{
		Inputs: inputs, Model: c.config.Model, InputType: inputType,
		Truncation: false, OutputDimension: c.config.Dimension,
	})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("marshal visual provider request: %w", err)
	}
	if int64(len(body)) > c.config.MaxRequestBytes {
		return nil, Usage{}, providerError(ErrProviderBatchTooLarge, 0, nil)
	}

	var lastErr error
	for attempt := 1; attempt <= c.config.MaxRetries; attempt++ {
		vectors, usage, requestErr := c.doOnce(ctx, body, len(inputs))
		if requestErr == nil {
			return vectors, usage, nil
		}
		lastErr = requestErr
		if !errors.Is(requestErr, ErrProviderRetryable) || attempt == c.config.MaxRetries {
			return nil, Usage{}, requestErr
		}
		delay := c.config.RetryBaseDelay * time.Duration(1<<min(attempt-1, 8))
		if retryAfter, ok := ProviderRetryAfter(requestErr); ok {
			delay = retryAfter
		}
		if delay <= 0 {
			if err := ctx.Err(); err != nil {
				return nil, Usage{}, err
			}
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, Usage{}, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, Usage{}, lastErr
}

func (c *VoyageClient) doOnce(
	ctx context.Context,
	body []byte,
	want int,
) ([][]float32, Usage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.config.Endpoint, "/")+"/multimodalembeddings", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("build visual provider request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.config.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Usage{}, ctx.Err()
		}
		return nil, Usage{}, providerRetryError(0, 0, false, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests {
		retryAfter, retrySet := parseVisualRetryAfter(response.Header.Get("Retry-After"))
		return nil, Usage{}, providerRetryError(response.StatusCode, retryAfter, retrySet, nil)
	}
	if response.StatusCode >= 500 {
		return nil, Usage{}, providerRetryError(response.StatusCode, 0, false, nil)
	}
	if response.StatusCode >= 400 {
		limited, readErr := readBounded(response.Body, 4_096)
		if readErr != nil {
			return nil, Usage{}, providerRetryError(response.StatusCode, 0, false, readErr)
		}
		switch {
		case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
			return nil, Usage{}, providerError(ErrProviderUnauthorized, response.StatusCode, nil)
		case response.StatusCode == http.StatusBadRequest && visualSizeRejection(limited):
			return nil, Usage{}, providerError(ErrProviderBatchTooLarge, response.StatusCode, nil)
		default:
			return nil, Usage{}, providerError(ErrProviderRejected, response.StatusCode, nil)
		}
	}

	payload, err := readBounded(response.Body, c.config.MaxResponseBytes)
	if err != nil {
		return nil, Usage{}, providerMalformedError(err)
	}
	var decoded voyageMultimodalResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, Usage{}, providerMalformedError(err)
	}
	return c.decodeResponse(decoded, want)
}

func (c *VoyageClient) decodeResponse(
	response voyageMultimodalResponse,
	want int,
) ([][]float32, Usage, error) {
	if len(response.Data) != want {
		return nil, Usage{}, providerMalformedError(nil)
	}
	vectors := make([][]float32, want)
	seen := make([]bool, want)
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= want || seen[item.Index] ||
			len(item.Embedding) != c.config.Dimension {
			return nil, Usage{}, providerMalformedError(nil)
		}
		for _, value := range item.Embedding {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, Usage{}, providerMalformedError(nil)
			}
		}
		seen[item.Index] = true
		vectors[item.Index] = item.Embedding
	}
	for _, present := range seen {
		if !present {
			return nil, Usage{}, providerMalformedError(nil)
		}
	}
	usage := Usage{}
	if response.Usage.TotalTokens != nil {
		usage = Usage{TotalTokens: *response.Usage.TotalTokens, Available: true}
	}
	return vectors, usage, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("visual provider response exceeds configured limit")
	}
	return data, nil
}

func visualSizeRejection(body []byte) bool {
	message := strings.ToLower(string(body))
	for _, marker := range []string{
		"too large", "too many", "exceed", "maximum", "context length", "total number of tokens",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func parseVisualRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, time.Hour), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := max(time.Until(when), 0)
	return min(delay, time.Hour), true
}
