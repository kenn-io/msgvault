package daemonclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/vector"
	apiclient "go.kenn.io/msgvault/pkg/client"
)

type apiErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// APIError preserves the daemon's non-CLI HTTP status, stable machine code,
// and sanitized message. Error retains the historical client-facing text.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (%d): %s", e.Status, e.Message)
}

// APIErrorCode returns the daemon's stable machine-readable error code.
func (e *APIError) APIErrorCode() string {
	return e.Code
}

// Unwrap maps only the daemon's documented vector-state codes to their domain
// sentinels. Unknown daemon codes remain ordinary private API failures.
func (e *APIError) Unwrap() error {
	switch e.Code {
	case "vector_not_enabled":
		return vector.ErrNotEnabled
	case "index_stale":
		return vector.ErrIndexStale
	case "index_building":
		return vector.ErrIndexBuilding
	case "embedding_timeout":
		return vector.ErrEmbeddingTimeout
	case "index_scope_mismatch":
		return vector.ErrIndexScopeMismatch
	default:
		return nil
	}
}

// operationInProgressCode is the daemon's error code for "the operation gate
// is held by other work"; clients wait and retry instead of failing.
const operationInProgressCode = "operation_in_progress"

// OperationInProgressError reports that the daemon turned a request away
// because another operation holds the gate. Message names the holder.
type OperationInProgressError struct {
	Message string
}

func (e *OperationInProgressError) Error() string {
	return e.Message
}

func operationInProgressFromBody(body []byte) error {
	var apiErr apiErrorBody
	if json.Unmarshal(body, &apiErr) == nil &&
		apiErr.Error == operationInProgressCode && apiErr.Message != "" {
		return &OperationInProgressError{Message: apiErr.Message}
	}
	return nil
}

// HandleErrorResponse reads a non-CLI error response body and returns an
// error that includes the HTTP status.
func HandleErrorResponse(resp *http.Response) error {
	return handleRawErrorResponse(resp, handleErrorBody)
}

// HandleCLIErrorResponse reads a CLI error response body and returns the
// daemon's user-facing message when one is available.
func HandleCLIErrorResponse(resp *http.Response) error {
	return handleRawErrorResponse(resp, handleCLIErrorBody)
}

func handleRawErrorResponse(
	resp *http.Response,
	decodeErrorBody func(status int, body []byte) error,
) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("API error (%d): could not read response body: %w", resp.StatusCode, err)
	}
	return decodeErrorBody(resp.StatusCode, body)
}

func handleErrorBody(status int, body []byte) error {
	if err := operationInProgressFromBody(body); err != nil {
		return err
	}
	message, code, _ := apiErrorMessage(body)
	return &APIError{Status: status, Code: code, Message: message}
}

func handleCLIErrorBody(status int, body []byte) error {
	if err := operationInProgressFromBody(body); err != nil {
		return err
	}
	message, _, decoded := apiErrorMessage(body)
	if decoded {
		return errors.New(message)
	}

	return fmt.Errorf("API error (%d): %s", status, message)
}

func apiErrorMessage(body []byte) (message, code string, decoded bool) {
	var apiErr apiErrorBody
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Message != "" {
		return apiErr.Message, apiErr.Error, true
	}

	return string(body), "", false
}

// APIResponseError maps generated non-CLI responses to daemonclient errors.
func APIResponseError(resp any, err error) error {
	return responseError(resp, err, []int{http.StatusOK}, handleErrorBody)
}

// CLIResponseError maps generated CLI responses to user-facing errors.
func CLIResponseError(resp any, err error) error {
	return responseError(resp, err, []int{http.StatusOK}, handleCLIErrorBody)
}

// APIResponse executes a generated request and validates its response.
func APIResponse[R any](
	c *Client,
	request func(*apiclient.Client) (R, error),
) (R, error) {
	return generatedResponse(c, request, APIResponseError)
}

// APIResponseWithStatuses executes a generated request and validates it
// against an explicit set of success statuses instead of the default 200 OK.
func APIResponseWithStatuses[R any](
	c *Client,
	expectedStatuses []int,
	request func(*apiclient.Client) (R, error),
) (R, error) {
	return generatedResponse(c, request, func(resp any, err error) error {
		return responseError(resp, err, expectedStatuses, handleErrorBody)
	})
}

func apiResponseWithErrorDecoder[R any](
	c *Client,
	request func(*apiclient.Client) (R, error),
	decodeErrorBody func(status int, body []byte) error,
) (R, error) {
	return generatedResponse(c, request, func(resp any, err error) error {
		return responseError(resp, err, []int{http.StatusOK}, decodeErrorBody)
	})
}

// APIResponseWithNotFound executes a generated request and allows callers to
// translate 404 responses into domain-specific not-found errors.
func APIResponseWithNotFound[R any](
	c *Client,
	request func(*apiclient.Client) (R, error),
	notFound func(R) error,
) (R, error) {
	var zero R
	client, err := c.GeneratedClient()
	if err != nil {
		return zero, err
	}
	resp, err := request(client)
	if meta, ok := responseMetadata(resp); ok &&
		meta.Status == http.StatusNotFound && !responseDecodeError(err) {
		return zero, notFound(resp)
	}
	if err := APIResponseError(resp, err); err != nil {
		return zero, err
	}
	return resp, nil
}

// CLIResponse executes a generated CLI request and validates its response.
func CLIResponse[R any](
	c *Client,
	request func(*apiclient.Client) (R, error),
) (R, error) {
	return generatedResponse(c, request, CLIResponseError)
}

func generatedResponse[R any](
	c *Client,
	request func(*apiclient.Client) (R, error),
	checkResponse func(any, error) error,
) (R, error) {
	var zero R
	client, err := c.GeneratedClient()
	if err != nil {
		return zero, err
	}
	waiter := &operationBusyWaiter{c: c}
	for {
		resp, err := request(client)
		checkErr := checkResponse(resp, err)
		if checkErr == nil {
			return resp, nil
		}
		waitCtx := c.requestContext()
		if waiter.wait(waitCtx, checkErr) {
			continue
		}
		if err := waitCtx.Err(); err != nil {
			return zero, err
		}
		return zero, checkErr
	}
}

func responseError(
	resp any,
	err error,
	expectedStatuses []int,
	decodeErrorBody func(status int, body []byte) error,
) error {
	if resp == nil {
		if err != nil {
			return err
		}
		return errors.New("generated response missing")
	}
	meta, ok := responseMetadata(resp)
	if !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected generated response type %T", resp)
	}
	if responseDecodeError(err) {
		return fmt.Errorf("decode generated response: %w", err)
	}
	if !slices.Contains(expectedStatuses, meta.Status) {
		return decodeErrorBody(meta.Status, meta.Body)
	}
	if err != nil {
		return fmt.Errorf("decode generated response: %w", err)
	}
	if meta.Status == http.StatusOK && meta.HasJSON200 && len(bytes.TrimSpace(meta.Body)) == 0 {
		return errors.New("decode generated response: empty 200 JSON response body")
	}
	if meta.Status == http.StatusOK && meta.MissingJSON200 {
		return errors.New("decode generated response: missing 200 JSON response body")
	}
	return nil
}

type responseInfo struct {
	Status         int
	Body           []byte
	HasJSON200     bool
	MissingJSON200 bool
}

func responseMetadata(resp any) (responseInfo, bool) {
	// oapi-codegen emits distinct response structs per operation, but each
	// WithResponse struct carries the same StatusCode, Body, and optional
	// JSON200 fields.
	v := reflect.ValueOf(resp)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return responseInfo{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return responseInfo{}, false
	}
	status := v.FieldByName("StatusCode")
	body := v.FieldByName("Body")
	if !status.IsValid() || !body.IsValid() ||
		status.Kind() != reflect.Int || body.Kind() != reflect.Slice ||
		body.Type().Elem().Kind() != reflect.Uint8 {
		return responseInfo{}, false
	}
	info := responseInfo{
		Status: int(status.Int()),
		Body:   body.Bytes(),
	}
	json200 := v.FieldByName("JSON200")
	if !json200.IsValid() {
		return info, true
	}
	info.HasJSON200 = true
	switch json200.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		info.MissingJSON200 = json200.IsNil()
	default:
	}
	return info, true
}

func responseDecodeError(err error) bool {
	var decodeErr *runtime.ResponseDecodeError
	return errors.As(err, &decodeErr)
}

// DecodeGeneratedSearchBody decodes a generated search response body into the
// concrete response type required by callers.
func DecodeGeneratedSearchBody[T any](operation string, body []byte) (T, error) {
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode %s response: %w", operation, err)
	}
	return out, nil
}
