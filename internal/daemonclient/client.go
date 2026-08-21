package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
	"go.opentelemetry.io/otel/propagation"
)

var daemonW3CPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

type RequestMode uint8

const (
	RequestModeBounded RequestMode = iota
	RequestModeCLI
)

// Config holds configuration for creating a daemon HTTP client.
type Config struct {
	URL              string
	APIKey           string
	LocalDaemonToken string
	AllowInsecure    bool
	Timeout          time.Duration
	HTTPClient       *http.Client
	Context          context.Context
	RequestMode      RequestMode
}

// Client provides HTTP access to a local or configured remote msgvault daemon.
type Client struct {
	baseURL          string
	apiKey           string
	httpClient       *http.Client
	typedClient      *apiclient.Client
	busyNotify       func(message string)
	rootContext      context.Context
	requestMode      RequestMode
	localDaemonToken string
}

// SetBusyNotifier registers a callback invoked when the daemon reports that
// another operation holds its gate and this client is waiting to retry. The
// message names the running operation.
func (c *Client) SetBusyNotifier(f func(message string)) {
	c.busyNotify = f
}

// operationBusyRetryDelay is variable only so tests can shorten it.
var operationBusyRetryDelay = time.Second

const operationBusyNotifyEvery = 30 * time.Second

// operationBusyWaiter coordinates retrying requests the daemon turned away
// because another operation holds its gate: notify (rate-limited), pause,
// retry until the gate frees or the context ends.
type operationBusyWaiter struct {
	c          *Client
	lastNotify time.Time
}

// wait reports whether err is a gate-busy rejection worth retrying, pausing
// one retry delay before the next attempt.
func (w *operationBusyWaiter) wait(ctx context.Context, err error) bool {
	var busy *OperationInProgressError
	if !errors.As(err, &busy) {
		return false
	}
	if w.c.busyNotify != nil &&
		(w.lastNotify.IsZero() || time.Since(w.lastNotify) >= operationBusyNotifyEvery) {
		w.c.busyNotify(busy.Message)
		w.lastNotify = time.Now()
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(operationBusyRetryDelay):
	}
	return true
}

// New creates a daemon HTTP client.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("daemon URL is required")
	}

	parsedURL, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	if parsedURL.Scheme == "http" && !cfg.AllowInsecure {
		return nil, errors.New("HTTPS required for daemon connections\n\n" +
			"Options:\n" +
			"  1. Use HTTPS for configured remote servers\n" +
			"  2. For trusted networks: add 'allow_insecure = true' to [remote] in config.toml")
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme must be http or https, got: %s", parsedURL.Scheme)
	}

	if parsedURL.Host == "" {
		return nil, errors.New("daemon URL must include a host (e.g., http://nas:8080)")
	}

	timeout := cfg.Timeout
	if cfg.RequestMode == RequestModeCLI {
		timeout = 0
	} else if timeout == 0 {
		timeout = 30 * time.Second
	}
	rootContext := cfg.Context
	if rootContext == nil {
		rootContext = context.Background()
	}

	httpClient := &http.Client{}
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		httpClient = &clone
	}
	httpClient.Timeout = timeout

	c := &Client{
		baseURL:          strings.TrimSuffix(cfg.URL, "/"),
		apiKey:           cfg.APIKey,
		httpClient:       httpClient,
		rootContext:      rootContext,
		requestMode:      cfg.RequestMode,
		localDaemonToken: cfg.LocalDaemonToken,
	}
	if _, err := c.GeneratedClient(); err != nil {
		return nil, err
	}
	return c, nil
}

// Close is a no-op for HTTP clients.
func (c *Client) Close() error {
	return nil
}

// BaseURL returns the daemon base URL used by this client.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// Timeout returns the HTTP timeout configured for non-streaming requests.
func (c *Client) Timeout() time.Duration {
	if c == nil || c.httpClient == nil {
		return 0
	}
	return c.httpClient.Timeout
}

func (c *Client) requestContext() context.Context {
	if c == nil || c.rootContext == nil {
		return context.Background()
	}
	return c.rootContext
}

// GeneratedClient returns the typed OpenAPI client used for daemon requests.
func (c *Client) GeneratedClient() (*apiclient.Client, error) {
	if c.typedClient != nil {
		return c.typedClient, nil
	}
	apiClient, err := apiclient.New(
		c.baseURL,
		runtime.WithHTTPClient(httpDoer{
			client:      c.httpClient,
			rootContext: c.requestContext(),
		}),
		runtime.WithRequestEditorFn(requestEditor(c.apiKey, c.requestMode, c.localDaemonToken)),
	)
	if err != nil {
		return nil, fmt.Errorf("create generated API client: %w", err)
	}
	c.typedClient = apiClient
	return apiClient, nil
}

// DoGeneratedRequestWithContext uses the generated request builder while
// leaving the response body open for callers that need raw responses.
func (c *Client) DoGeneratedRequestWithContext(
	ctx context.Context,
	method string,
	path string,
	options runtime.RequestOptions,
) (*http.Response, error) {
	return c.doGeneratedRequestWithHTTPClient(ctx, method, path, options, c.httpClient)
}

// DoGeneratedStreamingRequestWithContext is like DoGeneratedRequestWithContext
// but disables http.Client.Timeout so long-running NDJSON streams are not cut
// off by an absolute body-read deadline.
func (c *Client) DoGeneratedStreamingRequestWithContext(
	ctx context.Context,
	method string,
	path string,
	options runtime.RequestOptions,
) (*http.Response, error) {
	return c.doGeneratedRequestWithHTTPClient(ctx, method, path, options, httpClientWithoutTimeout(c.httpClient))
}

func (c *Client) doGeneratedRequestWithHTTPClient(
	ctx context.Context,
	method string,
	path string,
	options runtime.RequestOptions,
	httpClient *http.Client,
) (*http.Response, error) {
	client, err := c.GeneratedClient()
	if err != nil {
		return nil, err
	}

	apiClient := client.APIClient()
	if apiClient == nil {
		return nil, errors.New("generated API client is unavailable")
	}
	req, err := apiClient.CreateRequest(ctx, runtime.RequestOptionsParameters{
		RequestURL: apiClient.GetBaseURL() + path,
		Method:     method,
		Options:    options,
	})
	if err != nil {
		return nil, fmt.Errorf("create generated request: %w", err)
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := doRequestWithRootContext(c.requestContext(), httpClient, req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func httpClientWithoutTimeout(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.Timeout = 0
	return &clone
}

type httpDoer struct {
	client      *http.Client
	rootContext context.Context
}

func (d httpDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	return doRequestWithRootContext(d.rootContext, client, req)
}

func doRequestWithRootContext(
	rootContext context.Context,
	client *http.Client,
	req *http.Request,
) (*http.Response, error) {
	requestContext := req.Context()
	switch {
	case rootContext == nil || rootContext.Done() == nil:
		// The per-call context remains the only cancellation source.
	case requestContext.Done() == nil:
		req = req.WithContext(rootContext)
	default:
		mergedContext, cancel := context.WithCancelCause(requestContext)
		stopRootCancellation := context.AfterFunc(rootContext, func() {
			cancel(context.Cause(rootContext))
		})
		req = req.WithContext(mergedContext)

		// #nosec G704 -- daemonclient intentionally sends requests to the
		// caller-resolved msgvault daemon URL after New validates the scheme.
		resp, err := client.Do(req)
		if err != nil {
			stopRootCancellation()
			cancel(nil)
			return nil, err
		}
		resp.Body = &cancelOnCloseBody{
			ReadCloser:           resp.Body,
			stopRootCancellation: stopRootCancellation,
			cancel:               cancel,
		}
		return resp, nil
	}

	// #nosec G704 -- daemonclient intentionally sends requests to the
	// caller-resolved msgvault daemon URL after New validates the scheme.
	return client.Do(req)
}

type cancelOnCloseBody struct {
	io.ReadCloser

	stopRootCancellation func() bool
	cancel               context.CancelCauseFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.stopRootCancellation()
	b.cancel(nil)
	return err
}

type grantDecisionContextKey struct{}

func requestEditor(apiKey string, mode RequestMode, localDaemonToken string) apiclient.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		if apiKey != "" {
			req.Header.Set("X-Api-Key", apiKey)
		}
		if mode == RequestModeCLI {
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		}
		if decided, _ := ctx.Value(grantDecisionContextKey{}).(bool); decided {
			req.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, localDaemonToken)
		}
		req.Header.Set("Accept", "application/json")
		daemonW3CPropagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
		return nil
	}
}

// DaemonHealth is the subset of the daemon's /health payload the CLI
// consumes.
type DaemonHealth struct {
	Status          string
	AnalyticsEngine string
}

// GetHealth fetches the daemon's health status, including the analytics
// engine mode the daemon selected at startup (empty on daemons that
// predate the field).
func (c *Client) GetHealth(ctx context.Context) (*DaemonHealth, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.HealthResp, error) {
		return client.HealthWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	health := &DaemonHealth{}
	if resp.JSON200 != nil {
		health.Status = resp.JSON200.Status
		if resp.JSON200.AnalyticsEngine != nil {
			health.AnalyticsEngine = *resp.JSON200.AnalyticsEngine
		}
	}
	return health, nil
}
