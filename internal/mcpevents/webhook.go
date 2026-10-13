package mcpevents

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

type callbackGuardKey struct{}
type webhookClient struct {
	client    *http.Client
	transport *http.Transport
	resolve   func(context.Context, string, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	pins      map[string][]netip.Addr
}

func callbackOrigin(host, port string) string {
	host = strings.ToLower(host)
	if port == "443" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}
func callbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || strings.Contains(raw, "#") {
		return nil, invalid("invalid_callback")
	}
	if port := u.Port(); port != "" && port != "443" && port != "8443" {
		return nil, invalid("invalid_callback")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if netguard.ProhibitedIP(ip) {
			return nil, invalid("invalid_callback")
		}
	} else if netguard.ProhibitedHostname(u.Hostname()) {
		return nil, invalid("invalid_callback")
	}
	return u, nil
}
func newWebhookClient(trusted []TrustedCallback) (*webhookClient, error) {
	w := &webhookClient{resolve: net.DefaultResolver.LookupNetIP, dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, pins: make(map[string][]netip.Addr)}
	for _, entry := range trusted {
		origin, err := callbackURL(entry.Origin)
		if err != nil {
			return nil, invalid("invalid_trusted_callback")
		}
		addresses := make([]netip.Addr, 0, len(entry.Addresses))
		for _, raw := range entry.Addresses {
			address, err := netip.ParseAddr(raw)
			if err != nil {
				return nil, invalid("invalid_trusted_callback")
			}
			addresses = append(addresses, address)
		}
		origin, addresses, err = netguard.ValidateTrustedDestination(origin, addresses)
		if err != nil {
			return nil, invalid("invalid_trusted_callback")
		}
		port := origin.Port()
		if port == "" {
			port = "443"
		}
		key := callbackOrigin(origin.Hostname(), port)
		if _, duplicate := w.pins[key]; duplicate {
			return nil, invalid("invalid_trusted_callback")
		}
		w.pins[key] = addresses
	}
	w.transport = &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 128, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	w.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, invalid("invalid_callback")
		}
		addresses, err := w.addresses(ctx, host, port)
		if err != nil {
			return nil, err
		}
		var last error
		for _, address := range addresses {
			if check, ok := ctx.Value(callbackGuardKey{}).(func() error); ok {
				if err := check(); err != nil {
					return nil, err
				}
			}
			conn, err := w.dial(ctx, network, net.JoinHostPort(address.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last == nil {
			last = &Error{Code: -32015, Reason: "connection_refused"}
		}
		return nil, last
	}
	w.client = &http.Client{Transport: w.transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return w, nil
}
func (w *webhookClient) addresses(ctx context.Context, host, port string) ([]netip.Addr, error) {
	if pins, ok := w.pins[callbackOrigin(host, port)]; ok {
		return append([]netip.Addr(nil), pins...), nil
	}
	var addresses []netip.Addr
	if address, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{address}
	} else {
		if netguard.ProhibitedHostname(host) {
			return nil, invalid("invalid_callback")
		}
		var err error
		addresses, err = w.resolve(ctx, "ip", host)
		if err != nil {
			return nil, callbackError(err)
		}
	}
	if len(addresses) == 0 {
		return nil, &Error{Code: -32015, Reason: "connection_refused"}
	}
	if slices.ContainsFunc(addresses, netguard.ProhibitedIP) {
		return nil, invalid("invalid_callback")
	}
	return addresses, nil
}
func callbackError(err error) *Error {
	if safe, ok := errors.AsType[*Error](err); ok {
		return safe
	}
	netErr, isNetError := errors.AsType[net.Error](err)
	if errors.Is(err, context.DeadlineExceeded) || isNetError && netErr.Timeout() {
		return &Error{Code: -32015, Reason: "timeout"}
	}
	_, isTLSError := errors.AsType[*tls.CertificateVerificationError](err)
	_, isHeaderError := errors.AsType[tls.RecordHeaderError](err)
	_, isCertificateError := errors.AsType[x509.UnknownAuthorityError](err)
	if isTLSError || isHeaderError || isCertificateError {
		return &Error{Code: -32015, Reason: "tls_error"}
	}
	return &Error{Code: -32015, Reason: "connection_refused"}
}

// request sends one signed callback. dialGuard runs before each new
// connection's dial. The transport may dial on its own goroutine with a context
// detached from the request, and that dial can outlive the request, so the
// guard must not touch caller-owned state. Its context ends when request
// returns, which bounds any wait inside the guard by the request's lifetime.
func (w *webhookClient) request(ctx context.Context, callback, id, eventID string, body, current, previous []byte, dialGuard func(context.Context) error) (*http.Response, error) {
	if _, err := callbackURL(callback); err != nil {
		return nil, err
	}
	if len(body) > 262144 {
		return nil, invalid("payload_too_large")
	}
	if dialGuard != nil {
		guardCtx, cancelGuard := context.WithCancel(ctx)
		defer cancelGuard()
		ctx = context.WithValue(ctx, callbackGuardKey{}, func() error {
			if err := guardCtx.Err(); err != nil {
				return err
			}
			if err := dialGuard(guardCtx); err != nil {
				return &dialRefusedError{err: err}
			}
			return nil
		})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return nil, invalid("invalid_callback")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Id", eventID)
	req.Header.Set("Webhook-Timestamp", timestamp)
	signature := webhookSignature(current, eventID, timestamp, body)
	if len(previous) > 0 {
		signature += " " + webhookSignature(previous, eventID, timestamp, body)
	}
	req.Header.Set("Webhook-Signature", signature)
	req.Header.Set("X-Mcp-Subscription-Id", id)
	resp, err := w.client.Do(req)
	if err != nil {
		// The transport returns a dial error only to the request that waited
		// on that dial, so a refusal here means this request sent nothing.
		if refused, ok := errors.AsType[*dialRefusedError](err); ok {
			return nil, &dialRefusedError{err: callbackError(refused.err)}
		}
		return nil, callbackError(err)
	}
	return resp, nil
}

// dialRefusedError marks a request that failed because its dial guard refused
// the connection before anything was sent. It unwraps to the guard's error,
// which request converts to a callback *Error like any other failure.
type dialRefusedError struct{ err error }

func (e *dialRefusedError) Error() string { return e.err.Error() }
func (e *dialRefusedError) Unwrap() error { return e.err }
func (w *webhookClient) verify(ctx context.Context, callback, id string, secret []byte, check func(context.Context) error) error {
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return &Error{Code: -32015, Reason: "challenge_failed"}
	}
	encoded := base64.RawURLEncoding.EncodeToString(challenge)
	body, err := json.Marshal(struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}{"verification", encoded})
	if err != nil {
		return &Error{Code: -32015, Reason: "challenge_failed"}
	}
	if check != nil {
		if err := check(ctx); err != nil {
			return err
		}
	}
	resp, err := w.request(ctx, callback, id, "msg_verification_"+encoded, body, secret, nil, check)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return callbackError(err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &Error{Code: -32015, Reason: "http_4xx"}
	}
	if resp.StatusCode >= 500 {
		return &Error{Code: -32015, Reason: "http_5xx"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || len(response) > 4096 {
		return &Error{Code: -32015, Reason: "challenge_failed"}
	}
	var echo struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(response, &echo) != nil || !hmac.Equal([]byte(echo.Challenge), []byte(encoded)) {
		return &Error{Code: -32015, Reason: "challenge_failed"}
	}
	return nil
}

// post sends a delivery. Callers check authorization before calling post;
// dialGuard repeats that check before each new connection's dial.
func (w *webhookClient) post(ctx context.Context, callback, id, eventID string, body, current, previous []byte, dialGuard func(context.Context) error) (int, string, error) {
	resp, err := w.request(ctx, callback, id, eventID, body, current, previous, dialGuard)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, resp.Header.Get("Retry-After"), nil
}
