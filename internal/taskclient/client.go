// Package taskclient implements the narrow, provider-neutral protocol used by
// the optional server-side task integration.
package taskclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// ProtocolVersion is the only task-integration protocol understood by this
	// client. Descriptor and capability responses must match it exactly.
	ProtocolVersion = "1"

	DefaultRequestTimeout  = 5 * time.Second
	DefaultMaxResponseSize = int64(1 << 20)
)

var (
	ErrAuthenticationRequired      = errors.New("task integration authentication required")
	ErrConflict                    = errors.New("task integration revision conflict")
	ErrIdempotencyKeyRequired      = errors.New("task integration idempotency key required")
	ErrIncompatible                = errors.New("task integration incompatible")
	ErrInsecureDescriptor          = errors.New("task integration descriptor is insecure")
	ErrInsecureEndpoint            = errors.New("task integration endpoint is insecure")
	ErrInvalidResponse             = errors.New("task integration response is invalid")
	ErrNotFound                    = errors.New("task integration not found")
	ErrPlatformSecurityLimit       = errors.New("task integration platform security limitation")
	ErrDescriptorFileSecurityLimit = fmt.Errorf(
		"descriptor file security unavailable: %w", ErrPlatformSecurityLimit,
	)
	ErrUnixSocketSecurityLimit = fmt.Errorf(
		"unix socket security unavailable: %w", ErrPlatformSecurityLimit,
	)
	ErrRedirect         = errors.New("task integration redirect rejected")
	ErrRequestRejected  = errors.New("task integration request rejected")
	ErrResponseTooLarge = errors.New("task integration response is too large")
	ErrRevisionRequired = errors.New("task integration revision required")
	ErrUnreachable      = errors.New("task integration unreachable")
	ErrWrongProject     = errors.New("task integration project mismatch")
)

type EndpointKind string

const (
	EndpointHTTPS        EndpointKind = "https"
	EndpointLoopbackHTTP EndpointKind = "loopback_http"
	EndpointUnix         EndpointKind = "unix"
)

type ClientOptions struct {
	Endpoint         string
	APIKey           string
	Timeout          time.Duration
	MaxResponseBytes int64
	HTTPClient       *http.Client
}

// Client is a bounded protocol client. Credentials remain private to this
// server-side object and are never exposed by its public accessors.
type Client struct {
	baseURL          *url.URL
	apiKey           string
	endpointKind     EndpointKind
	instanceID       string
	securityNote     string
	httpClient       *http.Client
	maxResponseBytes int64
}

type httpStatusError struct {
	statusCode     int
	classification error
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: HTTP status %d", e.classification, e.statusCode)
}

func (e *httpStatusError) Unwrap() error { return e.classification }

type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision string `json:"revision,omitempty"`
}

type Task struct {
	ID       string         `json:"id"`
	Project  string         `json:"project"`
	Title    string         `json:"title"`
	Notes    string         `json:"notes,omitempty"`
	Revision string         `json:"revision"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type TaskCreate struct {
	Title    string         `json:"title"`
	Notes    string         `json:"notes,omitempty"`
	Priority string         `json:"priority,omitempty"`
	Labels   []string       `json:"labels,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type TaskList struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type metadataMutation struct {
	Metadata map[string]any `json:"metadata"`
}

func New(options ClientOptions) (*Client, error) {
	endpoint := strings.TrimSpace(options.Endpoint)
	parsed, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	u, kind, socketPath := parsed.url, parsed.kind, parsed.socketPath
	key := strings.TrimSpace(options.APIKey)
	if kind == EndpointLoopbackHTTP && key == "" {
		return nil, ErrAuthenticationRequired
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = DefaultMaxResponseSize
	}
	client := http.Client{Timeout: timeout}
	if options.HTTPClient != nil {
		client = *options.HTTPClient
		client.Timeout = timeout
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return ErrRedirect
	}

	securityNote := ""
	if kind == EndpointUnix {
		expectedOwner := currentUserID()
		if err := validateSecureSocket(socketPath, expectedOwner); err != nil {
			return nil, err
		}
		if !peerCredentialsSupported() {
			securityNote = "peer credentials unavailable on this platform; socket path and parent ownership/mode enforced"
		}
		dialer := &net.Dialer{Timeout: timeout}
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, "unix", socketPath)
				if err != nil {
					return nil, fmt.Errorf("%w: connect Unix socket", ErrUnreachable)
				}
				if err := verifyPeerCredentials(conn, expectedOwner); err != nil {
					_ = conn.Close()
					return nil, err
				}
				return conn, nil
			},
		}
	}

	return &Client{
		baseURL:          u,
		apiKey:           key,
		endpointKind:     kind,
		securityNote:     securityNote,
		httpClient:       &client,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func (c *Client) EndpointKind() EndpointKind { return c.endpointKind }
func (c *Client) InstanceID() string         { return c.instanceID }
func (c *Client) SecurityNote() string       { return c.securityNote }
func (c *Client) HasAuthentication() bool    { return c.apiKey != "" || c.endpointKind == EndpointUnix }

func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var result Capabilities
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/capabilities", nil, nil, &result, http.StatusOK); err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) {
			return Capabilities{}, &httpStatusError{
				statusCode:     statusErr.statusCode,
				classification: classifyCapabilityHTTPStatus(statusErr.statusCode),
			}
		}
		return Capabilities{}, err
	}
	if result.ProtocolVersion == "" {
		return Capabilities{}, ErrInvalidResponse
	}
	return result, nil
}

func (c *Client) ResolveProject(ctx context.Context, project string) (Project, error) {
	if err := validatePathSegment(project); err != nil {
		return Project{}, err
	}
	var result Project
	headers, err := c.doJSONWithHeaders(ctx, http.MethodGet, "/api/v1/projects/"+project, nil, nil, &result, http.StatusOK)
	if err != nil {
		return Project{}, err
	}
	if result.ID == "" || result.Name == "" {
		return Project{}, ErrInvalidResponse
	}
	if result.Revision == "" {
		result.Revision = headers.Get("ETag")
	}
	if result.Revision == "" {
		return Project{}, ErrInvalidResponse
	}
	if result.Name != project && result.ID != project {
		return Project{}, ErrWrongProject
	}
	return result, nil
}

func (c *Client) CreateTask(ctx context.Context, project, idempotencyKey string, create TaskCreate) (Task, error) {
	if err := validatePathSegment(project); err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return Task{}, ErrIdempotencyKeyRequired
	}
	if strings.TrimSpace(create.Title) == "" {
		return Task{}, fmt.Errorf("%w: task title is required", ErrInvalidResponse)
	}
	headers := http.Header{"Idempotency-Key": []string{idempotencyKey}}
	var result Task
	responseHeaders, err := c.doJSONWithHeaders(ctx, http.MethodPost, "/api/v1/projects/"+project+"/tasks", create, headers, &result, http.StatusOK, http.StatusCreated)
	if err != nil {
		return Task{}, err
	}
	fillRevisionFromETag(&result, responseHeaders)
	return validateTask(result, project, "")
}

func (c *Client) SearchTasks(ctx context.Context, project, query string, limit int) (TaskList, error) {
	values := make(url.Values)
	values.Set("q", query)
	values.Set("limit", strconv.Itoa(boundedLimit(limit)))
	return c.listTasks(ctx, project, values)
}

func (c *Client) ListTasks(ctx context.Context, project string, limit int, cursor string) (TaskList, error) {
	values := make(url.Values)
	values.Set("limit", strconv.Itoa(boundedLimit(limit)))
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	return c.listTasks(ctx, project, values)
}

func (c *Client) listTasks(ctx context.Context, project string, query url.Values) (TaskList, error) {
	if err := validatePathSegment(project); err != nil {
		return TaskList{}, err
	}
	path := "/api/v1/projects/" + project + "/tasks?" + query.Encode()
	var result TaskList
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &result, http.StatusOK); err != nil {
		return TaskList{}, err
	}
	for i := range result.Tasks {
		validated, err := validateTask(result.Tasks[i], project, "")
		if err != nil {
			return TaskList{}, err
		}
		result.Tasks[i] = validated
	}
	return result, nil
}

func (c *Client) GetTask(ctx context.Context, project, taskID string) (Task, error) {
	if err := validatePathSegment(project); err != nil {
		return Task{}, err
	}
	if err := validatePathSegment(taskID); err != nil {
		return Task{}, err
	}
	var result Task
	path := "/api/v1/projects/" + project + "/tasks/" + taskID
	headers, err := c.doJSONWithHeaders(ctx, http.MethodGet, path, nil, nil, &result, http.StatusOK)
	if err != nil {
		return Task{}, err
	}
	fillRevisionFromETag(&result, headers)
	return validateTask(result, project, taskID)
}

func (c *Client) MutateMetadata(ctx context.Context, project, taskID, revision string, metadata map[string]any) (Task, error) {
	if err := validatePathSegment(project); err != nil {
		return Task{}, err
	}
	if err := validatePathSegment(taskID); err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(revision) == "" {
		return Task{}, ErrRevisionRequired
	}
	headers := http.Header{"If-Match": []string{revision}}
	path := "/api/v1/projects/" + project + "/tasks/" + taskID + "/metadata"
	var result Task
	responseHeaders, err := c.doJSONWithHeaders(ctx, http.MethodPatch, path, metadataMutation{Metadata: metadata}, headers, &result, http.StatusOK)
	if err != nil {
		return Task{}, err
	}
	fillRevisionFromETag(&result, responseHeaders)
	return validateTask(result, project, taskID)
}

func (c *Client) doJSON(ctx context.Context, method, path string, requestBody any, headers http.Header, result any, successStatuses ...int) error {
	_, err := c.doJSONWithHeaders(ctx, method, path, requestBody, headers, result, successStatuses...)
	return err
}

// doJSONWithHeaders takes path as a decoded URL path — identifier segments are
// embedded raw (validatePathSegment excludes separator characters) — with an
// optional pre-encoded query after "?". Assigning the decoded path to
// URL.Path with an empty RawPath makes URL.String percent-encode it exactly
// once during serialization.
func (c *Client) doJSONWithHeaders(ctx context.Context, method, path string, requestBody any, headers http.Header, result any, successStatuses ...int) (http.Header, error) {
	target := *c.baseURL
	parts := strings.SplitN(path, "?", 2)
	target.Path = strings.TrimRight(target.Path, "/") + parts[0]
	target.RawPath = ""
	if len(parts) == 2 {
		target.RawQuery = parts[1]
	}
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, fmt.Errorf("encode task integration request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build task integration request: %w", err)
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return nil, ErrRedirect
		}
		return nil, fmt.Errorf("%w: request failed", ErrUnreachable)
	}
	defer func() { _ = resp.Body.Close() }()
	if !containsStatus(successStatuses, resp.StatusCode) {
		return nil, &httpStatusError{
			statusCode:     resp.StatusCode,
			classification: classifyHTTPStatus(resp.StatusCode),
		}
	}
	data, err := readBounded(resp.Body, c.maxResponseBytes)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return nil, fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing response data", ErrInvalidResponse)
	}
	return resp.Header.Clone(), nil
}

func classifyCapabilityHTTPStatus(statusCode int) error {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return ErrAuthenticationRequired
	}
	if isTransientHTTPStatus(statusCode) {
		return ErrUnreachable
	}
	return ErrIncompatible
}

func classifyHTTPStatus(statusCode int) error {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAuthenticationRequired
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return ErrConflict
	case http.StatusBadRequest, http.StatusNotAcceptable,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return ErrRequestRejected
	default:
		if isTransientHTTPStatus(statusCode) {
			return ErrUnreachable
		}
		return ErrIncompatible
	}
}

func isTransientHTTPStatus(statusCode int) bool {
	if statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooEarly ||
		statusCode == http.StatusTooManyRequests {
		return true
	}
	return statusCode >= http.StatusInternalServerError &&
		statusCode <= 599 &&
		statusCode != http.StatusNotImplemented
}

func fillRevisionFromETag(task *Task, headers http.Header) {
	if task.Revision == "" {
		task.Revision = headers.Get("ETag")
	}
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrUnreachable)
	}
	if int64(len(data)) > maximum {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

// ValidateEndpoint checks that an endpoint has one of the shapes the runtime
// client accepts: https://host, loopback http (http://localhost or a loopback
// IP), or unix:///absolute/socket/path with no host. URLs carrying userinfo, a
// query string, or a fragment are rejected for every scheme. Runtime-only
// requirements (API keys, socket existence and ownership) are checked by New,
// not here, so configuration validation can share this without touching the
// filesystem or credentials.
func ValidateEndpoint(endpoint string) error {
	_, err := parseEndpoint(endpoint)
	return err
}

// parsedEndpoint is the shape-validated form of an endpoint: the base URL the
// client dials, its kind, and (for unix endpoints) the socket path.
type parsedEndpoint struct {
	url        *url.URL
	kind       EndpointKind
	socketPath string
}

func parseEndpoint(endpoint string) (parsedEndpoint, error) {
	if endpoint == "" {
		return parsedEndpoint{}, fmt.Errorf("%w: endpoint is empty", ErrInsecureEndpoint)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return parsedEndpoint{}, fmt.Errorf("%w: endpoint must be an absolute URL", ErrInsecureEndpoint)
	}
	if u.User != nil {
		return parsedEndpoint{}, fmt.Errorf("%w: endpoint must not contain userinfo", ErrInsecureEndpoint)
	}
	if u.Fragment != "" {
		return parsedEndpoint{}, fmt.Errorf("%w: endpoint must not contain a fragment", ErrInsecureEndpoint)
	}
	if u.RawQuery != "" {
		return parsedEndpoint{}, fmt.Errorf("%w: endpoint must not contain a query string", ErrInsecureEndpoint)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.Host == "" {
			return parsedEndpoint{}, fmt.Errorf("%w: https endpoint requires a host", ErrInsecureEndpoint)
		}
		return parsedEndpoint{url: u, kind: EndpointHTTPS}, nil
	case "http":
		if u.Host == "" || !isLoopbackHost(u.Hostname()) {
			return parsedEndpoint{}, fmt.Errorf(
				"%w: remote plaintext HTTP is not allowed; use https:// or a loopback host like http://localhost",
				ErrInsecureEndpoint)
		}
		return parsedEndpoint{url: u, kind: EndpointLoopbackHTTP}, nil
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return parsedEndpoint{}, fmt.Errorf(
				"%w: unix endpoint requires an absolute socket path and no host (unix:///path/to/socket.sock)",
				ErrInsecureEndpoint)
		}
		return parsedEndpoint{
			url:        &url.URL{Scheme: "http", Host: "unix"},
			kind:       EndpointUnix,
			socketPath: u.Path,
		}, nil
	default:
		return parsedEndpoint{}, fmt.Errorf("%w: unsupported scheme %q (use https, http, or unix)",
			ErrInsecureEndpoint, u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validatePathSegment rejects identifiers that could alter URL structure when
// embedded raw as a single path segment (see doJSONWithHeaders).
func validatePathSegment(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#") {
		return fmt.Errorf("%w: invalid path identity", ErrInvalidResponse)
	}
	return nil
}

func validateTask(task Task, expectedProject, expectedID string) (Task, error) {
	if task.ID == "" || (expectedID != "" && task.ID != expectedID) {
		return Task{}, ErrInvalidResponse
	}
	if task.Project == "" || task.Project != expectedProject {
		return Task{}, ErrInvalidResponse
	}
	if task.Revision == "" {
		return Task{}, ErrInvalidResponse
	}
	return task, nil
}

func boundedLimit(limit int) int {
	if limit < 1 {
		return 100
	}
	if limit > 500 {
		return 500
	}
	return limit
}

func containsStatus(statuses []int, candidate int) bool {
	return slices.Contains(statuses, candidate)
}
