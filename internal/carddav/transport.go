package carddav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

// Client sends authenticated DAV requests only to its credential origin.
type Client struct {
	origin             *url.URL
	username           string
	password           string
	requestTimeout     time.Duration
	operationTimeout   time.Duration
	responseBytes      int64
	operationBytes     int64
	resolver           *net.Resolver
	dialContext        func(context.Context, string, string) (net.Conn, error)
	allowPrivateOrigin bool // test seam for local httptest servers
}

// NewClient creates a client with the task-wide default time and byte limits
// when an option is zero.
func NewClient(options ClientOptions) (*Client, error) {
	if options.CredentialOrigin == nil {
		return nil, fmt.Errorf("credential origin: %w", ErrUnsafeTarget)
	}
	origin := *options.CredentialOrigin
	if !validHTTPURL(&origin) {
		return nil, fmt.Errorf("credential origin: %w", ErrUnsafeTarget)
	}
	if origin.Scheme != "https" && (options.Username != "" || options.Password != "") &&
		!options.AllowInsecureCredentials {
		return nil, fmt.Errorf("credential origin requires HTTPS: %w", ErrUnsafeTarget)
	}
	origin.Path = ""
	origin.RawPath = ""
	origin.RawQuery = ""
	origin.Fragment = ""

	if options.RequestTimeout <= 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.OperationTimeout <= 0 {
		options.OperationTimeout = defaultOperationTimeout
	}
	if options.ResponseBytes <= 0 {
		options.ResponseBytes = defaultResponseBytes
	}
	if options.OperationBytes <= 0 {
		options.OperationBytes = defaultOperationBytes
	}
	if options.Resolver == nil {
		options.Resolver = net.DefaultResolver
	}
	if options.DialContext == nil {
		dialer := &net.Dialer{Timeout: options.RequestTimeout}
		options.DialContext = dialer.DialContext
	}
	return &Client{
		origin: originURL(&origin), username: options.Username, password: options.Password,
		requestTimeout: options.RequestTimeout, operationTimeout: options.OperationTimeout,
		responseBytes: options.ResponseBytes, operationBytes: options.OperationBytes,
		resolver: options.Resolver, dialContext: options.DialContext,
	}, nil
}

// Do performs one DAV request, manually validating and following only
// same-origin redirects. Each connection is pinned to one of the addresses
// validated immediately before it is dialed.
func (c *Client) Do(ctx context.Context, request Request) (*Response, error) {
	operationCtx, cancelOperation := context.WithTimeout(ctx, c.operationTimeout)
	defer cancelOperation()

	target, err := url.Parse(request.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing DAV URL: %w", ErrUnsafeTarget)
	}
	var operationBytes int64
	for redirects := 0; ; redirects++ {
		pinned, err := c.validateTarget(operationCtx, target)
		if err != nil {
			return nil, err
		}
		response, status, err := c.doPinned(operationCtx, target, pinned, request, &operationBytes)
		if response != nil {
			response.transferredBytes = operationBytes
		}
		if err != nil {
			return response, err
		}
		if isRedirect(status) {
			if isDAVMutation(request.Method) && status != http.StatusTemporaryRedirect && status != http.StatusPermanentRedirect {
				return response, fmt.Errorf("ambiguous mutation redirect: %w", ErrUnsafeRedirect)
			}
			if redirects >= 3 {
				return response, fmt.Errorf("redirect limit: %w", ErrUnsafeRedirect)
			}
			location := response.Header.Get("Location")
			next, parseErr := target.Parse(location)
			if location == "" || parseErr != nil || !sameOrigin(c.origin, next) {
				return response, fmt.Errorf("redirect target: %w", ErrUnsafeRedirect)
			}
			target = next
			continue
		}
		if status >= http.StatusBadRequest {
			return response, &StatusError{
				StatusCode:   status,
				RetryAfter:   retryAfter(response.Header.Get("Retry-After"), time.Now()),
				Precondition: davErrorPrecondition(response.Body),
			}
		}
		effectiveURL := *target
		response.EffectiveURL = &effectiveURL
		return response, nil
	}
}

func (c *Client) doWithBudget(
	ctx context.Context, request Request, budget *operationBudget,
) (*Response, error) {
	if budget == nil {
		return c.Do(ctx, request)
	}
	if budget.remaining <= 0 {
		return nil, ErrOperationLimit
	}
	limited := *c
	limited.operationBytes = min(limited.operationBytes, budget.remaining)
	response, err := limited.Do(ctx, request)
	if response != nil {
		if budgetErr := budget.consume(response); budgetErr != nil {
			return nil, budgetErr
		}
	}
	return response, err
}

func davErrorPrecondition(body []byte) string {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if ok && start.Name.Space == davNamespace && start.Name.Local == "valid-sync-token" {
			return start.Name.Local
		}
	}
}

func (c *Client) validateTarget(ctx context.Context, target *url.URL) ([]netip.AddrPort, error) {
	if !validHTTPURL(target) || !sameOrigin(c.origin, target) {
		return nil, fmt.Errorf("DAV URL: %w", ErrUnsafeTarget)
	}
	port, err := targetPort(target)
	if err != nil {
		return nil, fmt.Errorf("DAV URL port: %w", ErrUnsafeTarget)
	}
	host := target.Hostname()
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		if netguard.ProhibitedIP(literal) && !c.allowPrivateOrigin {
			return nil, fmt.Errorf("DAV literal destination: %w", ErrUnsafeTarget)
		}
		return []netip.AddrPort{netip.AddrPortFrom(literal.Unmap(), port)}, nil
	}
	if netguard.ProhibitedHostname(host) {
		return nil, fmt.Errorf("DAV hostname: %w", ErrUnsafeTarget)
	}
	addrs, err := c.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("resolving DAV host %q: %w", host, err)
	}
	pinned := make([]netip.AddrPort, 0, len(addrs))
	seen := make(map[netip.AddrPort]bool, len(addrs))
	for _, addr := range addrs {
		if netguard.ProhibitedIP(addr) && !c.allowPrivateOrigin {
			return nil, fmt.Errorf("DAV resolved destination: %w", ErrUnsafeTarget)
		}
		candidate := netip.AddrPortFrom(addr.Unmap(), port)
		if !seen[candidate] {
			seen[candidate] = true
			pinned = append(pinned, candidate)
		}
	}
	return pinned, nil
}

func (c *Client) doPinned(ctx context.Context, target *url.URL, pinned []netip.AddrPort, davRequest Request, operationBytes *int64) (*Response, int, error) {
	requestCtx, cancelRequest := context.WithTimeout(ctx, c.requestTimeout)
	defer cancelRequest()
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
			var failures []error
			for index, address := range pinned {
				attemptCtx := dialCtx
				cancelAttempt := func() {}
				if deadline, ok := dialCtx.Deadline(); ok && index < len(pinned)-1 {
					remaining := time.Until(deadline)
					if remaining <= 0 {
						return nil, dialCtx.Err()
					}
					attemptCtx, cancelAttempt = context.WithTimeout(
						dialCtx, remaining/time.Duration(len(pinned)-index),
					)
				}
				connection, err := c.dialContext(attemptCtx, network, address.String())
				cancelAttempt()
				if err == nil {
					return connection, nil
				}
				failures = append(failures, err)
			}
			return nil, errors.Join(failures...)
		},
		TLSHandshakeTimeout: c.requestTimeout,
		DisableKeepAlives:   true,
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(requestCtx, davRequest.Method, target.String(), bytes.NewReader(davRequest.Body))
	if err != nil {
		return nil, 0, fmt.Errorf("building DAV request: %w", err)
	}
	if len(davRequest.Body) > 0 {
		contentType := "application/xml; charset=utf-8"
		if davRequest.Method == http.MethodPut {
			contentType = "text/vcard; charset=utf-8"
		}
		req.Header.Set("Content-Type", contentType)
	}
	if davRequest.Depth != nil {
		req.Header.Set("Depth", strconv.Itoa(*davRequest.Depth))
	}
	if davRequest.Create {
		req.Header.Set("If-None-Match", "*")
	} else if davRequest.ETag != "" {
		req.Header.Set("If-Match", davRequest.ETag)
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	httpResponse, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("DAV request: %w", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()
	body, err := c.readResponseBody(httpResponse.Body, operationBytes)
	response := &Response{StatusCode: httpResponse.StatusCode, Header: httpResponse.Header.Clone(), Body: body}
	if err != nil {
		return response, httpResponse.StatusCode, err
	}
	return response, httpResponse.StatusCode, nil
}

func (c *Client) readResponseBody(reader io.Reader, operationBytes *int64) ([]byte, error) {
	remaining := c.operationBytes - *operationBytes
	if remaining < 0 {
		return nil, ErrOperationLimit
	}
	limit := min(c.responseBytes, remaining)
	limited := io.LimitReader(reader, limit+1)
	var body bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		read, err := limited.Read(buffer)
		if read > 0 {
			*operationBytes += int64(read)
			_, _ = body.Write(buffer[:read])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return body.Bytes(), fmt.Errorf("reading DAV response: %w", err)
		}
	}
	if int64(body.Len()) > limit {
		if remaining <= c.responseBytes {
			return body.Bytes(), ErrOperationLimit
		}
		return body.Bytes(), ErrResponseLimit
	}
	return body.Bytes(), nil
}

// ValidateChildHref resolves href from collection and rejects resources that
// leave the credential origin or collection. It reuses the request target
// validation path so literal and resolved destinations remain denylisted.
func (c *Client) ValidateChildHref(ctx context.Context, collection *url.URL, href string) (*url.URL, error) {
	if collection == nil || !validHTTPURL(collection) || href == "" || !sameOrigin(c.origin, collection) {
		return nil, ErrUnsafeHref
	}
	resolved, err := collection.Parse(href)
	if err != nil || resolved.User != nil || resolved.Fragment != "" || !sameOrigin(c.origin, resolved) {
		return nil, ErrUnsafeHref
	}
	basePath := path.Clean(collection.Path)
	if !strings.HasSuffix(collection.Path, "/") {
		basePath = path.Dir(basePath)
	}
	childPath := path.Clean(resolved.Path)
	if childPath != basePath && !strings.HasPrefix(childPath, strings.TrimSuffix(basePath, "/")+"/") {
		return nil, ErrUnsafeHref
	}
	if _, err := c.validateTarget(ctx, resolved); err != nil {
		return nil, fmt.Errorf("href target: %w: %w", ErrUnsafeHref, err)
	}
	return resolved, nil
}

func validHTTPURL(target *url.URL) bool {
	return target != nil && (target.Scheme == "http" || target.Scheme == "https") && target.Hostname() != "" && target.User == nil
}

func originURL(target *url.URL) *url.URL {
	clone := *target
	clone.Path, clone.RawPath, clone.RawQuery, clone.Fragment = "", "", "", ""
	return &clone
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		originPort(left) == originPort(right)
}

func originPort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	return "80"
}

func targetPort(target *url.URL) (uint16, error) {
	if rawPort := target.Port(); rawPort != "" {
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil || port == 0 {
			return 0, errors.New("invalid port")
		}
		return uint16(port), nil
	}
	if target.Scheme == "https" {
		return 443, nil
	}
	return 80, nil
}

func isRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther || status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
}

func isDAVMutation(method string) bool {
	return method == http.MethodPut || method == http.MethodDelete
}

func retryAfter(value string, now time.Time) time.Duration {
	const maximum = time.Hour
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, maximum)
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return min(time.Until(deadline), maximum)
	}
	return 0
}
