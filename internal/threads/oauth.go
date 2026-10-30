package threads

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthClientOptions is a constructor dependency, never runtime configuration.
// HTTPClient must carry the operation's shared budget transport, including for
// exchange before owner discovery. Binding the owner must not reset that budget.
type OAuthClientOptions struct {
	// WithOwnerLock binds discovered ownership before long-lived exchange.
	WithOwnerLock func(context.Context, string, func() (TokenRecord, error)) (TokenRecord, error)
	HTTPClient    *http.Client
	OAuthBaseURL  string
	Now           func() time.Time
}

// OAuthRequest retains authorization material only in opaque, pointer-backed
// storage. Neither fmt (including value %p) nor JSON exposes code or app secret.
type OAuthRequest struct {
	AppID          string
	RedirectURI    string
	RequiredScopes []string
	secret         *oauthSecret
}
type oauthSecret struct{ code, appSecret string }

func NewOAuthRequest(appID, redirectURI, code, appSecret string, requiredScopes []string) OAuthRequest {
	return OAuthRequest{AppID: appID, RedirectURI: redirectURI, RequiredScopes: append([]string(nil), requiredScopes...), secret: &oauthSecret{code, appSecret}}
}
func (r OAuthRequest) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "Threads OAuth request [redacted]")
}

// NewOAuthState generates 256 bits of unpredictable callback state.
func NewOAuthState() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("generate Threads OAuth state")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// ParseOAuthCallback accepts only a complete URL matching the registered HTTPS
// callback and its fixed query parameters. It performs no network requests.
func ParseOAuthCallback(registeredURI, returnedURL, expectedState string) (string, error) {
	bad := errors.New("invalid Threads OAuth callback")
	registered, err := oauthCallbackURI(registeredURI)
	if err != nil || expectedState == "" {
		return "", bad
	}
	returned, err := url.Parse(returnedURL)
	if err != nil || returned.User != nil || returned.Opaque != "" || returned.Scheme != registered.Scheme || returned.Host != registered.Host || returned.EscapedPath() != registered.EscapedPath() {
		return "", bad
	}
	// Meta may append #_; the code is parsed solely from the query string.
	if returned.Fragment != "" && returned.Fragment != "_" {
		return "", bad
	}
	fixed, _ := url.ParseQuery(registered.RawQuery)
	query, err := url.ParseQuery(returned.RawQuery)
	if err != nil {
		return "", bad
	}
	for key, values := range query {
		if len(values) != 1 {
			return "", bad
		}
		switch key {
		case "state", "code":
			continue
		default:
			expected, ok := fixed[key]
			if !ok || values[0] != expected[0] {
				return "", bad
			}
		}
	}
	for key, values := range fixed {
		if query.Get(key) != values[0] || len(query[key]) != 1 {
			return "", bad
		}
	}
	state := query.Get("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 || query.Get("code") == "" {
		return "", bad
	}
	return query.Get("code"), nil
}

func oauthCallbackURI(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("Threads requires a registered HTTPS callback")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil {
		return nil, errors.New("Threads callback requires a controlled domain")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid registered Threads callback")
	}
	for key, values := range q {
		if len(values) != 1 {
			return nil, errors.New("duplicate registered Threads callback parameter")
		}
		switch key {
		case "code", "state", "error", "error_reason", "error_description":
			return nil, errors.New("reserved registered Threads callback parameter")
		}
	}
	return u, nil
}

// ExchangeCredential verifies a long-lived user token. Callers must save the
// result under source lifecycle ownership before registering/using the source.
func ExchangeCredential(ctx context.Context, opts OAuthClientOptions, req OAuthRequest) (TokenRecord, error) {
	if !tokenValidID(req.AppID) || req.secret == nil || req.secret.code == "" || req.secret.appSecret == "" {
		return TokenRecord{}, errors.New("incomplete Threads OAuth request")
	}
	if _, err := oauthCallbackURI(req.RedirectURI); err != nil {
		return TokenRecord{}, err
	}
	required := append([]string{"threads_basic", "threads_read_replies"}, req.RequiredScopes...)
	c, err := newOAuthHTTP(opts)
	if err != nil {
		return TokenRecord{}, err
	}
	var short struct {
		AccessToken string         `json:"access_token"`
		UserID      oauthDecimalID `json:"user_id"`
	}
	err = c.request(ctx, http.MethodPost, "/oauth/access_token", url.Values{"client_id": {req.AppID}, "client_secret": {req.secret.appSecret}, "code": {req.secret.code}, "grant_type": {"authorization_code"}, "redirect_uri": {req.RedirectURI}}, &short)
	if err != nil {
		return TokenRecord{}, err
	}
	if short.AccessToken == "" || !tokenValidID(string(short.UserID)) {
		return TokenRecord{}, errors.New("incomplete Threads short-lived credential")
	}
	finish := func() (TokenRecord, error) {
		var long oauthTokenResponse
		if err = c.request(ctx, http.MethodGet, "/access_token", url.Values{"grant_type": {"th_exchange_token"}, "client_secret": {req.secret.appSecret}, "access_token": {short.AccessToken}}, &long); err != nil {
			return TokenRecord{}, err
		}
		if err = long.validate(); err != nil {
			return TokenRecord{}, err
		}
		var owner struct {
			ID       oauthDecimalID `json:"id"`
			Username string         `json:"username"`
			Name     string         `json:"name"`
		}
		if err = c.request(ctx, http.MethodGet, "/me", url.Values{"fields": {"id,username,name"}, "access_token": {long.AccessToken}}, &owner); err != nil {
			return TokenRecord{}, err
		}
		if string(owner.ID) != string(short.UserID) {
			return TokenRecord{}, ErrReauthorizationRequired
		}
		// The documented debug response omits app_id. Same-app inspection with
		// the app credential proves the app binding without persisting its secret.
		record, err := c.verify(ctx, long.AccessToken, "TH|"+req.AppID+"|"+req.secret.appSecret, req.AppID, string(owner.ID), required)
		if err != nil {
			return TokenRecord{}, err
		}
		record.Username = owner.Username
		record.Name = owner.Name
		record.RedirectURI = req.RedirectURI
		record.ExpiresAt = minTokenExpiry(record.ExpiresAt, c.now().Add(time.Duration(long.ExpiresIn)*time.Second))
		return record, nil
	}
	if opts.WithOwnerLock != nil {
		source, err := SourceIdentifier(req.AppID, string(short.UserID))
		if err != nil {
			return TokenRecord{}, err
		}
		return opts.WithOwnerLock(ctx, source, finish)
	}
	return finish()
}

type oauthHTTP struct {
	client *http.Client
	base   string
	now    func() time.Time
}

func newOAuthHTTP(opts OAuthClientOptions) (*oauthHTTP, error) {
	base := opts.OAuthBaseURL
	if base == "" {
		base = "https://graph.threads.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid Threads OAuth origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("insecure Threads OAuth origin")
	}
	client := http.Client{Timeout: 30 * time.Second}
	if opts.HTTPClient != nil {
		client = *opts.HTTPClient
	}
	// Preserve the injected transport (and its budget) but deny every redirect,
	// including same-origin 307/308 replays of authorization material.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("Threads OAuth redirect refused") }
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &oauthHTTP{client: &client, base: strings.TrimSuffix(base, "/"), now: now}, nil
}

// oauthResponseError intentionally retains only numeric provider categories.
// It is private to avoid overlapping the read client's APIError contract.
type oauthResponseError struct{ status, code int }

func (e *oauthResponseError) Error() string {
	return fmt.Sprintf("Threads OAuth: HTTP %d (Graph code %d)", e.status, e.code)
}
func (e *oauthResponseError) Unwrap() error {
	if e.code == 190 {
		return ErrReauthorizationRequired
	}
	return nil
}

func (c *oauthHTTP) request(ctx context.Context, method, path string, values url.Values, out any) error {
	target := c.base + path
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(values.Encode())
	} else {
		target += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New("construct Threads OAuth request")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if pause := workloadPause(ctx); pause != nil {
			return pause
		}
		// A shared transport cap is a normal pause, including when exchange
		// or refresh consumed the remaining attempts. Preserve only this
		// known-safe type, never a credential-bearing transport diagnostic.
		if pause, ok := errors.AsType[*PauseError](err); ok {
			return pause
		}
		// Never wrap url.Error or an arbitrary transport error: either may carry
		// credentials or a response body. Preserve only safe cancellation identity.
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errors.New("Threads OAuth request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	const maxBody = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		if pause := workloadPause(ctx); pause != nil {
			return pause
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("read Threads OAuth response")
	}
	var envelope struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Error != nil {
		code := 0
		if envelope.Error != nil {
			code = envelope.Error.Code
		}
		return &oauthResponseError{status: resp.StatusCode, code: code}
	}
	if err = json.Unmarshal(data, out); err != nil {
		return errors.New("invalid Threads OAuth response")
	}
	return nil
}

type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

func (r oauthTokenResponse) validate() error {
	if r.AccessToken == "" || r.ExpiresIn <= 0 || r.ExpiresIn > int64((1<<63-1)/time.Second) {
		return errors.New("incomplete Threads long-lived credential")
	}
	return nil
}

func (c *oauthHTTP) verify(ctx context.Context, token, inspector, appID, ownerID string, required []string) (TokenRecord, error) {
	var debug struct {
		Data struct {
			AppID         *oauthDecimalID `json:"app_id"`
			UserID        oauthDecimalID  `json:"user_id"`
			Valid         bool            `json:"is_valid"`
			Scopes        []string        `json:"scopes"`
			IssuedAt      int64           `json:"issued_at"`
			ExpiresAt     int64           `json:"expires_at"`
			DataExpiresAt int64           `json:"data_access_expires_at"`
		} `json:"data"`
	}
	if err := c.request(ctx, http.MethodGet, "/debug_token", url.Values{"input_token": {token}, "access_token": {inspector}}, &debug); err != nil {
		return TokenRecord{}, err
	}
	d := debug.Data
	now := c.now()
	if !d.Valid || (d.AppID != nil && string(*d.AppID) != appID) || string(d.UserID) != ownerID || !tokenValidID(ownerID) || d.IssuedAt <= 0 || time.Unix(d.IssuedAt, 0).After(now) || d.ExpiresAt <= now.Unix() || d.DataExpiresAt <= now.Unix() {
		return TokenRecord{}, ErrReauthorizationRequired
	}
	for _, scope := range required {
		found := false
		for _, granted := range d.Scopes {
			if scope == granted {
				found = true
				break
			}
		}
		if !found {
			return TokenRecord{}, ErrReauthorizationRequired
		}
	}
	return TokenRecord{AppID: appID, OwnerID: ownerID, Scopes: append([]string(nil), d.Scopes...), IssuedAt: time.Unix(d.IssuedAt, 0).UTC(), ExpiresAt: time.Unix(d.ExpiresAt, 0).UTC(), DataAccessExpiresAt: time.Unix(d.DataExpiresAt, 0).UTC(), secret: &credentialSecret{token}}, nil
}
func minTokenExpiry(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// oauthDecimalID accepts provider string/number IDs without any float64 step.
type oauthDecimalID string

func (id *oauthDecimalID) UnmarshalJSON(data []byte) error {
	value := string(data)
	if strings.HasPrefix(value, "\"") {
		if err := json.Unmarshal(data, &value); err != nil {
			return errors.New("invalid Threads credential ID")
		}
	}
	if !tokenValidID(value) {
		return errors.New("invalid Threads credential ID")
	}
	*id = oauthDecimalID(value)
	return nil
}
