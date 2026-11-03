package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

// LocalDaemonEndpoint is an identity-checked local runtime and its credentials.
// The resolver must discover the daemon that owns the same archive, starting it
// if permitted by the caller's local daemon policy.
type LocalDaemonEndpoint struct {
	URL              string
	APIKey           string
	LocalDaemonToken string
}

// LocalDaemonUnavailableMessage is safe to show to an MCP client. Detailed
// discovery failures may contain private paths or credentials.
const LocalDaemonUnavailableMessage = "local archive daemon is unavailable; retry the tool call or check msgvault daemon status"

// LocalDaemonUnavailableError means a local request could not connect, and
// recovery failed before the request could be sent. Post-connect failures are
// deliberately excluded: replaying a write could duplicate its effects.
type LocalDaemonUnavailableError struct{ Err error }

func (e *LocalDaemonUnavailableError) Error() string {
	return fmt.Sprintf("local daemon unavailable: %v", e.Err)
}
func (e *LocalDaemonUnavailableError) Unwrap() error        { return e.Err }
func (e *LocalDaemonUnavailableError) APIErrorCode() string { return "daemon_unavailable" }

type localDaemonConnection struct {
	endpoint   LocalDaemonEndpoint
	url        *url.URL
	generation uint64
}

type localDaemonRecovery struct {
	transport http.RoundTripper
	origin    *url.URL
	resolve   func(context.Context) (LocalDaemonEndpoint, error)
	refresh   chan struct{}
	mu        sync.RWMutex
	current   localDaemonConnection
}

// EnableLocalDaemonRecovery opts a long-lived local client into rediscovery on
// dial failure. Call it once, before issuing requests. Configured remote and
// delegated clients must keep their fixed endpoint behavior. The original
// BaseURL stays unchanged; the shared transport routes even previously created
// generated clients to the latest runtime.
func (c *Client) EnableLocalDaemonRecovery(resolve func(context.Context) (LocalDaemonEndpoint, error)) error {
	if resolve == nil {
		return errors.New("local daemon resolver is required")
	}
	if c.agentToken != "" {
		return errors.New("delegated daemon clients cannot enable local recovery")
	}
	origin, err := parseLocalDaemonEndpoint(c.baseURL)
	if err != nil {
		return err
	}
	transport := c.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, enabled := transport.(*localDaemonRecovery); enabled {
		return errors.New("local daemon recovery is already enabled")
	}
	c.httpClient.Transport = &localDaemonRecovery{
		transport: transport, origin: origin, resolve: resolve, refresh: make(chan struct{}, 1),
		current: localDaemonConnection{url: origin, endpoint: LocalDaemonEndpoint{URL: c.baseURL, APIKey: c.apiKey, LocalDaemonToken: c.localDaemonToken}},
	}
	return nil
}

func parseLocalDaemonEndpoint(endpoint string) (*url.URL, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid local daemon endpoint: %w", err)
	}
	// Runtime discovery already proves local ownership. Do not require loopback:
	// a local daemon may have an explicit non-loopback bind address.
	if parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("local daemon endpoint must be an HTTP origin")
	}
	return parsed, nil
}

func (t *localDaemonRecovery) connection() localDaemonConnection {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.current
}

func (t *localDaemonRecovery) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.origin.Scheme || request.URL.Host != t.origin.Host {
		// Redirect destinations are not local runtime discovery targets.
		return t.transport.RoundTrip(request)
	}
	current := t.connection()
	response, err := t.transport.RoundTrip(t.rewrite(request, current))
	if !localDaemonDialFailure(request.Context(), err) {
		return response, err
	}
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		return nil, &LocalDaemonUnavailableError{Err: err}
	}
	next, resolveErr := t.reconnect(request.Context(), current.generation)
	if resolveErr != nil {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		return nil, &LocalDaemonUnavailableError{Err: fmt.Errorf("rediscover daemon: %w", resolveErr)}
	}
	retry := t.rewrite(request, next)
	if request.Body != nil && request.Body != http.NoBody {
		retry.Body, err = request.GetBody()
		if err != nil {
			return nil, &LocalDaemonUnavailableError{Err: fmt.Errorf("recreate daemon request: %w", err)}
		}
	}
	response, err = t.transport.RoundTrip(retry)
	if localDaemonDialFailure(request.Context(), err) {
		return nil, &LocalDaemonUnavailableError{Err: err}
	}
	return response, err
}

func localDaemonDialFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var networkError *net.OpError
	return errors.As(err, &networkError) && networkError.Op == "dial"
}

func (t *localDaemonRecovery) rewrite(request *http.Request, connection localDaemonConnection) *http.Request {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = connection.url.Scheme
	clone.URL.Host = connection.url.Host
	clone.Host = connection.url.Host
	if connection.endpoint.APIKey == "" {
		clone.Header.Del("X-Api-Key")
	} else {
		clone.Header.Set("X-Api-Key", connection.endpoint.APIKey)
	}
	// The runtime token authorizes an already-confirmed decision. Refresh its
	// value only if the original request carried that decision marker.
	if _, decided := request.Header[http.CanonicalHeaderKey(apiprotocol.DaemonRuntimeTokenHeader)]; decided {
		clone.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, connection.endpoint.LocalDaemonToken)
	}
	return clone
}

func (t *localDaemonRecovery) reconnect(ctx context.Context, failedGeneration uint64) (localDaemonConnection, error) {
	select {
	case t.refresh <- struct{}{}:
	case <-ctx.Done():
		return localDaemonConnection{}, ctx.Err()
	}
	defer func() { <-t.refresh }()
	if err := ctx.Err(); err != nil {
		return localDaemonConnection{}, err
	}
	current := t.connection()
	if current.generation != failedGeneration {
		return current, nil
	}
	endpoint, err := t.resolve(ctx)
	if err != nil {
		return localDaemonConnection{}, err
	}
	parsed, err := parseLocalDaemonEndpoint(endpoint.URL)
	if err != nil {
		return localDaemonConnection{}, err
	}
	next := localDaemonConnection{endpoint: endpoint, url: parsed, generation: current.generation + 1}
	t.mu.Lock()
	t.current = next
	t.mu.Unlock()
	return next, nil
}

func (t *localDaemonRecovery) CloseIdleConnections() {
	if closer, ok := t.transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
