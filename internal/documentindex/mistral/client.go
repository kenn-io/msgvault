// Package mistral implements the bounded stateless Mistral OCR transport used
// by document attachment indexing.
package mistral

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/httpretry"
)

const (
	defaultEndpoint         = "https://api.mistral.ai/v1/ocr"
	defaultModel            = "mistral-ocr-4-0"
	defaultMaxDocumentBytes = int64(50 << 20)
	defaultMaxResponseBytes = int64(64 << 20)
	defaultMaxUnits         = 1000
	defaultMaxRetries       = 3
	defaultMaxRetryDelay    = 30 * time.Second
)

var (
	// ErrPermanentResponse marks a provider 4xx other than rate limiting.
	ErrPermanentResponse = errors.New("mistral OCR permanent response")
	// ErrResponseTooLarge marks a response that exceeds MaxResponseBytes.
	ErrResponseTooLarge = errors.New("mistral OCR response too large")
	// ErrTransientResponse marks an exhausted retryable provider or transport failure.
	ErrTransientResponse = errors.New("mistral OCR transient response")
)

// Config controls the stateless OCR client.
type Config struct {
	Endpoint          string
	APIKey            string
	Model             string
	AllowedHosts      []string
	AllowedMediaTypes []string
	Timeout           time.Duration
	MaxDocumentBytes  int64
	MaxResponseBytes  int64
	MaxUnits          int
	MaxRetries        int
	MaxRetryDelay     time.Duration
	HTTPClient        *http.Client
}

// Document identifies a private verified spool. Process re-verifies its size,
// media type, and lowercase SHA-256 before any request bytes are sent.
type Document struct {
	Path      string
	MediaType string
	Size      int64
	SHA256    string
}

// Options controls format-specific OCR request fields. Pages is omitted when
// empty; callers set it only for formats whose authenticated probe establishes
// page semantics.
type Options struct {
	Pages         string `json:"pages"`
	ExtractHeader bool   `json:"extract_header"`
	ExtractFooter bool   `json:"extract_footer"`
}

// DefaultOptions returns the output-bearing extraction policy shared by
// authenticated capability probes and production document processing.
func DefaultOptions() Options {
	return Options{ExtractHeader: true, ExtractFooter: true}
}

// Result is the validated provider response.
type Result struct {
	Model     string         `json:"model"`
	Pages     []Page         `json:"pages"`
	UsageInfo *Usage         `json:"usage_info"`
	Metrics   RequestMetrics `json:"-"`
}

// RequestMetrics describes actual provider HTTP work, including retries
// performed inside one logical extraction attempt.
type RequestMetrics struct {
	Requests int
	Retries  int
	Latency  time.Duration
}

type processError struct {
	err     error
	metrics RequestMetrics
}

func (e *processError) Error() string { return e.err.Error() }
func (e *processError) Unwrap() error { return e.err }

// MetricsFromError recovers bounded provider request accounting from an error
// returned by Process. Pre-request validation failures carry zero metrics.
func MetricsFromError(err error) RequestMetrics {
	var processErr *processError
	if errors.As(err, &processErr) {
		return processErr.metrics
	}
	return RequestMetrics{}
}

// Page is one provider-ordered source unit. Mistral uses the pages array for
// both paginated and converted document formats.
type Page struct {
	Index        int        `json:"index"`
	Markdown     string     `json:"markdown"`
	Header       string     `json:"header"`
	Footer       string     `json:"footer"`
	Dimensions   Dimensions `json:"dimensions"`
	indexPresent bool
}

// Dimensions records provider unit dimensions when present.
type Dimensions struct {
	DPI    int `json:"dpi"`
	Height int `json:"height"`
	Width  int `json:"width"`
}

// Usage contains bounded accounting fields returned by Mistral.
type Usage struct {
	PagesProcessed        int    `json:"pages_processed"`
	DocSizeBytes          *int64 `json:"doc_size_bytes"`
	pagesProcessedPresent bool
}

// Client calls one allowlisted HTTPS /v1/ocr endpoint.
type Client struct {
	endpoint          string
	apiKey            string
	model             string
	allowedMediaTypes map[string]struct{}
	maxDocumentBytes  int64
	maxResponseBytes  int64
	maxUnits          int
	maxRetries        int
	maxRetryDelay     time.Duration
	http              *http.Client
}

type ProcessorTarget struct {
	Endpoint string
	Region   string
	Model    string
}

// Target exposes immutable, non-secret request authority for probe manifests.
func (c *Client) Target() ProcessorTarget {
	return ProcessorTarget{Endpoint: c.endpoint, Region: "eu", Model: c.model}
}

// NewClient validates the endpoint and applies conservative defaults.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.MaxDocumentBytes <= 0 {
		cfg.MaxDocumentBytes = defaultMaxDocumentBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	if cfg.MaxUnits <= 0 {
		cfg.MaxUnits = defaultMaxUnits
	}
	if cfg.MaxRetries < 0 {
		return nil, errors.New("mistral OCR max retries cannot be negative")
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.MaxRetryDelay <= 0 {
		cfg.MaxRetryDelay = defaultMaxRetryDelay
	}

	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse Mistral OCR endpoint: %w", err)
	}
	if endpoint.Scheme != "https" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("mistral OCR endpoint must be an HTTPS URL without userinfo, query, or fragment")
	}
	if endpoint.Path != "/v1/ocr" {
		return nil, errors.New("mistral OCR endpoint path must be /v1/ocr")
	}
	allowedHosts := cfg.AllowedHosts
	if len(allowedHosts) == 0 {
		allowedHosts = []string{"api.mistral.ai"}
	}
	if !slices.Contains(allowedHosts, endpoint.Hostname()) {
		return nil, fmt.Errorf("mistral OCR endpoint host %q is not allowlisted", endpoint.Hostname())
	}
	allowedMediaTypes := make(map[string]struct{}, len(cfg.AllowedMediaTypes))
	for _, mediaType := range cfg.AllowedMediaTypes {
		parsed, parameters, parseErr := mime.ParseMediaType(mediaType)
		if parseErr != nil || len(parameters) != 0 || parsed != mediaType || parsed != strings.ToLower(parsed) {
			return nil, fmt.Errorf("invalid allowlisted Mistral OCR media type %q", mediaType)
		}
		allowedMediaTypes[parsed] = struct{}{}
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	} else {
		clone := *httpClient
		httpClient = &clone
		if httpClient.Timeout == 0 || httpClient.Timeout > cfg.Timeout {
			httpClient.Timeout = cfg.Timeout
		}
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{
		endpoint:          endpoint.String(),
		apiKey:            cfg.APIKey,
		model:             cfg.Model,
		allowedMediaTypes: allowedMediaTypes,
		maxDocumentBytes:  cfg.MaxDocumentBytes,
		maxResponseBytes:  cfg.MaxResponseBytes,
		maxUnits:          cfg.MaxUnits,
		maxRetries:        cfg.MaxRetries,
		maxRetryDelay:     cfg.MaxRetryDelay,
		http:              httpClient,
	}, nil
}

// Process verifies document and sends it as an inline base64 data URL. It does
// not create Mistral Files, Libraries, or public URLs.
func (c *Client) Process(ctx context.Context, document Document, options Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if document.Size > c.maxDocumentBytes {
		return Result{}, fmt.Errorf("mistral OCR document is %d bytes, limit %d", document.Size, c.maxDocumentBytes)
	}
	if _, allowed := c.allowedMediaTypes[document.MediaType]; !allowed {
		return Result{}, fmt.Errorf("mistral OCR media type %q is not allowlisted", document.MediaType)
	}

	prefix, suffix, encodedLength, err := requestEnvelope(c.model, document.MediaType, document.Size, options)
	if err != nil {
		return Result{}, err
	}
	requests := 0
	var providerLatency time.Duration
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{}, newProcessError(err, requests, providerLatency)
		}
		result, retryAfter, requested, requestLatency, processErr := c.processOnce(
			ctx, document, prefix, suffix, encodedLength,
		)
		if requested {
			requests++
			providerLatency += requestLatency
		}
		if processErr == nil {
			result.Metrics = requestMetrics(requests, providerLatency)
			return result, nil
		}
		var transient *transientError
		if !errors.As(processErr, &transient) {
			return Result{}, newProcessError(processErr, requests, providerLatency)
		}
		if attempt == c.maxRetries {
			err := fmt.Errorf("%w after %d attempts: %w", ErrTransientResponse, attempt+1, transient)
			return Result{}, newProcessError(err, requests, providerLatency)
		}
		wait := httpretry.RetryAfter(retryAfter, attempt, c.maxRetryDelay)
		if err := waitContext(ctx, wait); err != nil {
			return Result{}, newProcessError(err, requests, providerLatency)
		}
	}
	return Result{}, ErrTransientResponse
}

func newProcessError(err error, requests int, providerLatency time.Duration) error {
	if requests == 0 {
		return err
	}
	return &processError{err: err, metrics: requestMetrics(requests, providerLatency)}
}

func requestMetrics(requests int, providerLatency time.Duration) RequestMetrics {
	return RequestMetrics{
		Requests: requests,
		Retries:  max(requests-1, 0),
		Latency:  providerLatency,
	}
}

func (c *Client) processOnce(
	ctx context.Context,
	document Document,
	prefix, suffix []byte,
	encodedLength int64,
) (Result, string, bool, time.Duration, error) {
	file, err := openVerifiedDocument(document)
	if err != nil {
		return Result{}, "", false, 0, err
	}
	defer func() { _ = file.Close() }()

	reader := streamRequest(file, document.Size, prefix, suffix)
	defer func() { _ = reader.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, reader)
	if err != nil {
		return Result{}, "", false, 0, fmt.Errorf("build Mistral OCR request: %w", err)
	}
	req.ContentLength = encodedLength
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	requestStarted := time.Now()
	response, err := c.http.Do(req)
	requestLatency := time.Since(requestStarted)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, "", true, requestLatency, ctxErr
		}
		return Result{}, "", true, requestLatency, &transientError{cause: fmt.Errorf("send Mistral OCR request: %w", err)}
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		return Result{}, response.Header.Get("Retry-After"), true, requestLatency, &transientError{status: response.StatusCode}
	}
	if response.StatusCode >= http.StatusBadRequest {
		return Result{}, "", true, requestLatency, fmt.Errorf("mistral OCR HTTP %d: %w", response.StatusCode, ErrPermanentResponse)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Result{}, "", true, requestLatency, fmt.Errorf("mistral OCR unexpected HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Result{}, "", true, requestLatency, errors.New("mistral OCR returned non-JSON content type")
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	requestLatency = time.Since(requestStarted)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, "", true, requestLatency, ctxErr
		}
		return Result{}, "", true, requestLatency, &transientError{cause: fmt.Errorf("read Mistral OCR response: %w", err)}
	}
	if int64(len(body)) > c.maxResponseBytes {
		return Result{}, "", true, requestLatency, ErrResponseTooLarge
	}
	if !utf8.Valid(body) {
		return Result{}, "", true, requestLatency, errors.New("mistral OCR response contains invalid UTF-8")
	}
	var result Result
	if err := json.Unmarshal(body, &result); err != nil {
		return Result{}, "", true, requestLatency, fmt.Errorf("decode Mistral OCR response: %w", err)
	}
	if err := validateResult(result, c.model, document.Size, c.maxUnits); err != nil {
		return Result{}, "", true, requestLatency, err
	}
	return result, "", true, requestLatency, nil
}

type transientError struct {
	status int
	cause  error
}

func (e *transientError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("Mistral OCR transient HTTP %d", e.status)
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func streamRequest(file *os.File, size int64, prefix, suffix []byte) *io.PipeReader {
	reader, writer := io.Pipe()
	go func() {
		if _, err := writer.Write(prefix); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		encoder := base64.NewEncoder(base64.StdEncoding, writer)
		written, err := io.Copy(encoder, file)
		if closeErr := encoder.Close(); err == nil {
			err = closeErr
		}
		if err == nil && written != size {
			err = errors.New("mistral OCR spool size changed during request")
		}
		if err == nil {
			_, err = writer.Write(suffix)
		}
		_ = writer.CloseWithError(err)
	}()
	return reader
}

func openVerifiedDocument(document Document) (*os.File, error) {
	if document.Path == "" || document.Size < 0 {
		return nil, errors.New("invalid mistral OCR document metadata")
	}
	parsedMediaType, _, err := mime.ParseMediaType(document.MediaType)
	if err != nil || parsedMediaType == "" || parsedMediaType != strings.ToLower(parsedMediaType) {
		return nil, errors.New("invalid mistral OCR document media type")
	}
	if len(document.SHA256) != sha256.Size*2 || strings.ToLower(document.SHA256) != document.SHA256 {
		return nil, errors.New("invalid mistral OCR document SHA-256")
	}
	if _, err := hex.DecodeString(document.SHA256); err != nil {
		return nil, fmt.Errorf("invalid Mistral OCR document SHA-256: %w", err)
	}
	info, err := os.Lstat(document.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect Mistral OCR spool: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("mistral OCR spool must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("mistral OCR spool permissions must be private")
	}
	if info.Size() != document.Size {
		return nil, errors.New("mistral OCR spool size changed")
	}

	file, err := os.Open(document.Path)
	if err != nil {
		return nil, fmt.Errorf("open Mistral OCR spool: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect opened Mistral OCR spool: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("mistral OCR spool changed while opening")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("verify Mistral OCR spool: %w", err)
	}
	if written != document.Size || hex.EncodeToString(hash.Sum(nil)) != document.SHA256 {
		_ = file.Close()
		return nil, errors.New("mistral OCR spool hash mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("rewind Mistral OCR spool: %w", err)
	}
	return file, nil
}

func requestEnvelope(model, mediaType string, size int64, options Options) ([]byte, []byte, int64, error) {
	const maxBase64Input = (math.MaxInt64 / 4 * 3) - 2
	if size < 0 || size > maxBase64Input {
		return nil, nil, 0, errors.New("mistral OCR document size is not representable")
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encode Mistral OCR model: %w", err)
	}
	dataPrefixJSON, err := json.Marshal("data:" + mediaType + ";base64,")
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encode Mistral OCR media type: %w", err)
	}
	prefix := append([]byte(`{"model":`), modelJSON...)
	prefix = append(prefix, []byte(`,"document":{"type":"document_url","document_url":`)...)
	prefix = append(prefix, dataPrefixJSON[:len(dataPrefixJSON)-1]...)

	tail := struct {
		IncludeImageBase64 bool   `json:"include_image_base64"`
		IncludeBlocks      bool   `json:"include_blocks"`
		ExtractHeader      bool   `json:"extract_header"`
		ExtractFooter      bool   `json:"extract_footer"`
		Pages              string `json:"pages,omitempty"`
	}{
		ExtractHeader: options.ExtractHeader,
		ExtractFooter: options.ExtractFooter,
		Pages:         options.Pages,
	}
	tailJSON, err := json.Marshal(tail)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encode Mistral OCR options: %w", err)
	}
	suffix := append([]byte(`"},`), tailJSON[1:]...)
	base64Length := ((size + 2) / 3) * 4
	if base64Length > math.MaxInt64-int64(len(prefix))-int64(len(suffix)) {
		return nil, nil, 0, errors.New("mistral OCR request length overflow")
	}
	return prefix, suffix, int64(len(prefix)) + base64Length + int64(len(suffix)), nil
}

func validateResult(result Result, expectedModel string, expectedBytes int64, maxUnits int) error {
	if result.Model == "" {
		return errors.New("mistral OCR response omitted model")
	}
	if result.Model != expectedModel {
		return fmt.Errorf("mistral OCR response model %q does not match requested model", result.Model)
	}
	if result.Pages == nil {
		return errors.New("mistral OCR response omitted pages")
	}
	if len(result.Pages) > maxUnits {
		return fmt.Errorf("mistral OCR response has %d units, limit %d", len(result.Pages), maxUnits)
	}
	for i, page := range result.Pages {
		if !page.indexPresent || page.Index != i {
			return fmt.Errorf("mistral OCR response unit %d has invalid index %d", i, page.Index)
		}
	}
	if result.UsageInfo == nil || !result.UsageInfo.pagesProcessedPresent {
		return errors.New("mistral OCR response omitted usage")
	}
	if result.UsageInfo.PagesProcessed < 0 ||
		(result.UsageInfo.DocSizeBytes != nil && *result.UsageInfo.DocSizeBytes < 0) {
		return errors.New("mistral OCR response has invalid usage")
	}
	if result.UsageInfo.PagesProcessed != len(result.Pages) {
		return fmt.Errorf("mistral OCR response processed %d units but returned %d",
			result.UsageInfo.PagesProcessed, len(result.Pages))
	}
	if result.UsageInfo.DocSizeBytes != nil && *result.UsageInfo.DocSizeBytes != expectedBytes {
		return fmt.Errorf("mistral OCR response accounted for %d document bytes, expected %d",
			*result.UsageInfo.DocSizeBytes, expectedBytes)
	}
	return nil
}

// UnmarshalJSON records whether index was present so a missing first index is
// not silently accepted as index zero.
func (p *Page) UnmarshalJSON(data []byte) error {
	type pageJSON struct {
		Index      *int       `json:"index"`
		Markdown   string     `json:"markdown"`
		Header     string     `json:"header"`
		Footer     string     `json:"footer"`
		Dimensions Dimensions `json:"dimensions"`
	}
	var decoded pageJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	p.Markdown = decoded.Markdown
	p.Header = decoded.Header
	p.Footer = decoded.Footer
	p.Dimensions = decoded.Dimensions
	if decoded.Index != nil {
		p.Index = *decoded.Index
		p.indexPresent = true
	}
	return nil
}

// UnmarshalJSON records whether pages_processed was present while preserving
// the provider's nullable doc_size_bytes field.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type usageJSON struct {
		PagesProcessed *int   `json:"pages_processed"`
		DocSizeBytes   *int64 `json:"doc_size_bytes"`
	}
	var decoded usageJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	u.DocSizeBytes = decoded.DocSizeBytes
	if decoded.PagesProcessed != nil {
		u.PagesProcessed = *decoded.PagesProcessed
		u.pagesProcessedPresent = true
	}
	return nil
}
