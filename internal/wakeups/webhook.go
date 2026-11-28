package wakeups

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json/v2"
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
	timeout       time.Duration
	resolver      netguard.Resolver
	dial          netguard.DialContext
	roots         *tls.Config
	trustedOrigin *url.URL
	trusted       []netip.Addr
}

// VerifyEventCallback sends a signed challenge through the same checked transport as delivery.
func VerifyEventCallback(ctx context.Context, endpoint, subscriptionID string, secret []byte) string {
	return (webhookClient{timeout: 10 * time.Second}).verifyEventCallback(ctx, endpoint, subscriptionID, secret)
}

func (c webhookClient) verifyEventCallback(ctx context.Context, endpoint, subscriptionID string, secret []byte) string {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "challenge_failed"
	}
	challenge := base64.RawURLEncoding.EncodeToString(nonce[:])
	body, err := json.Marshal(struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}{"verification", challenge})
	if err != nil {
		return "challenge_failed"
	}
	category, _, response := c.request(ctx, endpoint, "msg_verification_"+challenge, subscriptionID, secret, body, true)
	var echo struct {
		Challenge string `json:"challenge"`
	}
	if category != "" {
		return category
	}
	if json.Unmarshal(response, &echo) != nil || !hmac.Equal([]byte(echo.Challenge), []byte(challenge)) {
		return "challenge_failed"
	}
	return ""
}

func (c webhookClient) request(ctx context.Context, endpoint, id, subscriptionID string, secret, body []byte, verification bool) (string, int, []byte) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target, err := url.Parse(endpoint)
	if err != nil {
		return "invalid_callback", 0, nil
	}
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
			return "timeout", 0, nil
		}
		if _, ok := errors.AsType[*net.DNSError](err); ok {
			return "dns_error", 0, nil
		}
		if errors.Is(err, netguard.ErrUnsafeTarget) {
			return "invalid_callback", 0, nil
		}
		return "connection_refused", 0, nil
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
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "invalid_callback", 0, nil
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "%s.%s.%s", id, stamp, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Id", id)
	req.Header.Set("Webhook-Timestamp", stamp)
	req.Header.Set("Webhook-Signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("User-Agent", "msgvault")
	if subscriptionID != "" {
		req.Header.Set("X-Mcp-Subscription-Id", subscriptionID)
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout", 0, nil
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Timeout() {
			return "timeout", 0, nil
		}
		if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
			return "tls_error", 0, nil
		}
		if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
			return "tls_error", 0, nil
		}
		return "connection_refused", 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var response []byte
	if verification {
		response, err = io.ReadAll(io.LimitReader(resp.Body, 4097))
		if err != nil || len(response) > 4096 {
			return "invalid_callback", resp.StatusCode, nil
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "", resp.StatusCode, response
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return "http_3xx", resp.StatusCode, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "http_4xx", resp.StatusCode, nil
	default:
		return "http_5xx", resp.StatusCode, nil
	}
}
