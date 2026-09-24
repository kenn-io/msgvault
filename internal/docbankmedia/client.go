// Package docbankmedia contains the small HTTP contract used by Msgvault's
// Beeper media worker. Docbank's application packages remain server-owned.
package docbankmedia

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

const (
	maxResponseBytes   = 1 << 20
	maxMetadataBytes   = 64 << 10
	maxTranscriptBytes = 16 << 20
	maxMediaBytes      = 2 << 30
)

var (
	ErrCredentialUnavailable = errors.New("docbank credential unavailable")
	ErrInvalidReceipt        = errors.New("docbank returned an invalid media receipt")
	ErrResponseTooLarge      = errors.New("docbank response exceeds the size limit")
	ErrContentTooLarge       = errors.New("docbank upload exceeds the size limit")
	ErrInvalidRequest        = errors.New("docbank request is invalid")
)

// HTTPError preserves only the response status class and a stable local code.
// Response bodies are deliberately discarded because Docbank may echo input.
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	return "docbank request failed: " + e.Code
}

// Retryable reports whether another scheduled attempt may make progress.
func (e *HTTPError) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Retryable reports whether err is a transport or server failure.
func Retryable(err error) bool {
	if httpErr, ok := errors.AsType[*HTTPError](err); ok {
		return httpErr.Retryable()
	}
	// Replays reuse the saved operation ID, so only local request faults block.
	return err != nil && !errors.Is(err, ErrCredentialUnavailable) &&
		!errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrContentTooLarge)
}

// ErrorCode returns a stable, bounded code suitable for a local job row.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if httpErr, ok := errors.AsType[*HTTPError](err); ok {
		return httpErr.Code
	}
	switch {
	case errors.Is(err, ErrCredentialUnavailable):
		return "credential_unavailable"
	case errors.Is(err, ErrInvalidReceipt):
		return "invalid_receipt"
	case errors.Is(err, ErrResponseTooLarge):
		return "response_too_large"
	case errors.Is(err, ErrContentTooLarge):
		return "content_too_large"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	default:
		return "transport"
	}
}

// Client is a transport-only adapter for the Docbank media routes.
type Client struct {
	baseURL   string
	lookupKey func() (string, error)
	http      *http.Client
}

type Timestamp struct {
	Normalized     string `json:"normalized"`
	Raw            string `json:"raw"`
	Precision      string `json:"precision"`
	Timezone       string `json:"timezone"`
	ZoneText       string `json:"zone_text,omitempty"`
	OffsetSeconds  *int   `json:"offset_seconds,omitempty"`
	FractionDigits int    `json:"fraction_digits"`
}

type Occurrence struct {
	Ref          string    `json:"ref"`
	Revision     string    `json:"revision"`
	Filename     string    `json:"filename"`
	PersonRef    string    `json:"person_ref,omitempty"`
	SpeakerLabel string    `json:"speaker_label,omitempty"`
	Message      Timestamp `json:"message"`
}

type SuppliedMetadata struct {
	OperationID string     `json:"operation_id"`
	Filename    string     `json:"filename"`
	MediaType   string     `json:"media_type"`
	SHA256      string     `json:"sha256"`
	ByteLength  int64      `json:"byte_length"`
	Occurrence  Occurrence `json:"occurrence"`
}

type ArtifactMetadata struct {
	OperationID  string `json:"operation_id"`
	OccurrenceID string `json:"occurrence_id"`
	Kind         string `json:"kind"`
	Origin       string `json:"origin,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Language     string `json:"language,omitempty"`
	Filename     string `json:"filename"`
	MediaType    string `json:"media_type"`
	SHA256       string `json:"sha256"`
	ByteLength   int64  `json:"byte_length"`
}

type Processing struct {
	Profile         string `json:"profile"`
	SuppliedInputID string `json:"supplied_input_id,omitempty"`
}

type Receipt struct {
	VaultUID         string `json:"vault_uid"`
	SourceID         string `json:"source_id"`
	SourceVersionID  string `json:"source_version_id,omitempty"`
	ContentVersionID string `json:"content_version_id,omitempty"`
	OccurrenceID     string `json:"occurrence_id,omitempty"`
	OperationID      string `json:"operation_id"`
	JobID            string `json:"job_id,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	OperationState   string `json:"operation_state"`
	CoverageState    string `json:"coverage_state"`
	SuppliedInputID  string `json:"supplied_input_id,omitempty"`
}

type JobStatus struct {
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	Phase       string `json:"phase"`
	FailureCode string `json:"failure_code,omitzero"`
}

// NewClient validates the destination without making a network request.
// HTTPS is required for remote destinations; HTTP is allowed only on loopback.
func NewClient(baseURL string, lookupKey func() (string, error)) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("invalid Docbank URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return nil, errors.New("docbank HTTP URL must use loopback")
		}
	default:
		return nil, errors.New("docbank URL must use HTTPS or loopback HTTP")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &Client{
		baseURL:   strings.TrimRight(parsed.String(), "/"),
		lookupKey: lookupKey,
		http: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) Submit(ctx context.Context, metadata SuppliedMetadata, content io.Reader) (Receipt, error) {
	receipt, err := c.multipart(ctx, "/api/v1/media/sources", metadata, metadata.Filename, metadata.MediaType, content)
	if err = validateReceipt(receipt, metadata.OperationID, err); err != nil {
		return Receipt{}, err
	}
	if receipt.SourceVersionID == "" || receipt.ContentVersionID == "" || receipt.OccurrenceID == "" {
		return Receipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

func (c *Client) ImportTranscript(ctx context.Context, sourceID string, metadata ArtifactMetadata, content io.Reader) (Receipt, error) {
	endpoint := "/api/v1/media/sources/" + url.PathEscape(sourceID) + "/artifacts"
	receipt, err := c.multipart(ctx, endpoint, metadata, metadata.Filename, metadata.MediaType, content)
	if err = validateReceipt(receipt, metadata.OperationID, err); err != nil {
		return Receipt{}, err
	}
	if receipt.SuppliedInputID == "" {
		return Receipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

func (c *Client) Process(ctx context.Context, sourceID, operationID, suppliedInputID string) (Receipt, error) {
	body := struct {
		OperationID string     `json:"operation_id"`
		Processing  Processing `json:"processing"`
	}{
		OperationID: operationID,
		Processing: Processing{
			Profile:         "supplied-transcript",
			SuppliedInputID: suppliedInputID,
		},
	}
	var receipt Receipt
	err := c.jsonRequest(ctx, http.MethodPost,
		"/api/v1/media/sources/"+url.PathEscape(sourceID)+"/retry", body, &receipt)
	if err = validateReceipt(receipt, operationID, err); err != nil {
		return Receipt{}, err
	}
	// A saved failed receipt is terminal and may carry no job.
	if receipt.JobID == "" && receipt.OperationState != "failed" {
		return Receipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

func (c *Client) Status(ctx context.Context, sourceID string) (Receipt, error) {
	var receipt Receipt
	err := c.jsonRequest(ctx, http.MethodGet,
		"/api/v1/media/sources/"+url.PathEscape(sourceID), nil, &receipt)
	return receipt, validateReceipt(receipt, "", err)
}

func (c *Client) JobStatus(ctx context.Context, jobID string) (JobStatus, error) {
	var status JobStatus
	err := c.jsonRequest(ctx, http.MethodGet,
		"/api/v1/processing/jobs/"+url.PathEscape(jobID), nil, &status)
	if err != nil {
		return JobStatus{}, err
	}
	if status.JobID == "" || status.JobID != jobID || status.State == "" || status.Phase == "" {
		return JobStatus{}, ErrInvalidReceipt
	}
	return status, nil
}

func (c *Client) multipart(
	ctx context.Context, endpoint string, metadata any, filename, mediaType string, content io.Reader,
) (Receipt, error) {
	if content == nil || filename == "" || mediaType == "" {
		return Receipt{}, fmt.Errorf("%w: upload requires named content", ErrInvalidRequest)
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: encode metadata", ErrInvalidRequest)
	}
	if len(metadataBytes) > maxMetadataBytes {
		return Receipt{}, fmt.Errorf("%w: metadata exceeds the size limit", ErrInvalidRequest)
	}

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeResult := make(chan error, 1)
	go func() {
		err := writeMultipart(metadataBytes, filename, mediaType, content, multipartWriter)
		_ = writer.CloseWithError(err)
		writeResult <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return Receipt{}, err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	if err := c.setAPIKey(req); err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return Receipt{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return Receipt{}, transportError(err)
	}
	body, readErr := readResponse(resp)
	_ = resp.Body.Close()
	if readErr != nil {
		_ = reader.CloseWithError(readErr)
		<-writeResult
		return Receipt{}, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = reader.Close()
		<-writeResult
		return Receipt{}, &HTTPError{Status: resp.StatusCode, Code: statusCode(resp.StatusCode)}
	}
	if err := <-writeResult; err != nil {
		return Receipt{}, fmt.Errorf("write Docbank upload: %w", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("%w: undecodable response", ErrInvalidReceipt)
	}
	return receipt, validateReceipt(receipt, "", nil)
}

func writeMultipart(
	metadata []byte, filename, mediaType string, content io.Reader, writer *multipart.Writer,
) error {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="metadata"`)
	header.Set("Content-Type", "application/json")
	part, err := writer.CreatePart(header)
	if err == nil {
		_, err = part.Write(metadata)
	}
	if err == nil {
		header = make(textproto.MIMEHeader)
		header.Set("Content-Disposition", multipart.FileContentDisposition("file", filename))
		header.Set("Content-Type", mediaType)
		part, err = writer.CreatePart(header)
	}
	if err == nil {
		limit := int64(maxMediaBytes)
		if strings.HasPrefix(strings.ToLower(mediaType), "text/") {
			limit = maxTranscriptBytes
		}
		written, copyErr := io.Copy(part, io.LimitReader(content, limit+1))
		if copyErr != nil {
			err = copyErr
		} else if written > limit {
			err = ErrContentTooLarge
		}
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (c *Client) jsonRequest(ctx context.Context, method, endpoint string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: encode request", ErrInvalidRequest)
		}
		if len(encoded) > maxMetadataBytes {
			return fmt.Errorf("%w: request exceeds the size limit", ErrInvalidRequest)
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.setAPIKey(req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readResponse(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Status: resp.StatusCode, Code: statusCode(resp.StatusCode)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: undecodable response", ErrInvalidReceipt)
	}
	return nil
}

// transportError drops the request URL that net/http adds to its errors.
func transportError(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}
	return fmt.Errorf("docbank transport: %w", err)
}

func (c *Client) setAPIKey(req *http.Request) error {
	if c.lookupKey == nil {
		return nil
	}
	key, err := c.lookupKey()
	if err != nil {
		return ErrCredentialUnavailable
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	return nil
}

func readResponse(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Docbank response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func validateReceipt(receipt Receipt, operationID string, err error) error {
	if err != nil {
		return err
	}
	if receipt.VaultUID == "" || receipt.SourceID == "" ||
		(operationID != "" && receipt.OperationID != operationID) {
		return ErrInvalidReceipt
	}
	switch receipt.OperationState {
	case "queued", "running", "succeeded", "failed", "cancelled":
	default:
		return ErrInvalidReceipt
	}
	return nil
}

func statusCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusUnprocessableEntity:
		return "validation"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusRequestTimeout:
		return "timeout"
	case http.StatusBadRequest:
		return "bad_request"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "http_error"
	}
}
