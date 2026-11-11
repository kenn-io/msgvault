package wakeups

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

func DecodeSecret(secret string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(secret, "whsec_")
	if !ok {
		return nil, errors.New("invalid secret")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) < 24 || len(decoded) > 64 || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("invalid secret")
	}
	return decoded, nil
}

type webhookClient struct {
	client        *http.Client
	timeout       time.Duration
	resolver      netguard.Resolver
	dial          netguard.DialContext
	roots         *tls.Config
	trustedOrigin *url.URL
	trusted       []netip.Addr
}

func (c webhookClient) send(ctx context.Context, endpoint, id string, secret, body []byte) string {
	if len(body) > 256<<10 {
		return "invalid_callback"
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || target.Opaque != "" || strings.Contains(endpoint, "#") || (target.Port() != "" && target.Port() != "443" && target.Port() != "8443") {
		if c.client == nil {
			return "invalid_callback"
		}
	}
	client := c.client
	if client == nil {
		resolver := c.resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		dial := c.dial
		if dial == nil {
			dial = (&net.Dialer{}).DialContext
		}
		pinned, err := netguard.PinnedAddresses(ctx, resolver, target, c.trustedOrigin, c.trusted, false)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "timeout"
			}
			if errors.Is(err, netguard.ErrUnsafeTarget) {
				return "invalid_callback"
			}
			return "connection_refused"
		}
		tlsConfig := c.roots
		if tlsConfig == nil {
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			tlsConfig = tlsConfig.Clone()
			if tlsConfig.MinVersion < tls.VersionTLS12 {
				tlsConfig.MinVersion = tls.VersionTLS12
			}
		}
		pinnedDial := netguard.PinnedDialer(pinned, dial)
		transport := &http.Transport{DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return pinnedDial(ctx, network, address)
		}, TLSClientConfig: tlsConfig, DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "invalid_callback"
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "%s.%s.%s", id, stamp, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", stamp)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("User-Agent", "msgvault")
	resp, err := clone.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout"
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Timeout() {
			return "timeout"
		}
		var unknown tls.RecordHeaderError
		if errors.As(err, &unknown) {
			return "tls_error"
		}
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return "tls_error"
		}
		return "connection_refused"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ""
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "http_4xx"
	default:
		return "http_5xx"
	}
}
