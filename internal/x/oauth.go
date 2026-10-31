package x

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	DefaultAuthURL      = "https://x.com/i/oauth2/authorize"
	DefaultTokenURL     = "https://api.x.com/2/oauth2/token"
	DefaultCallbackPort = 8765
)

var readScopes = []string{"tweet.read", "users.read", "offline.access"}

// OAuthOptions selects the exact loopback callback and injectable boundaries.
// ReadSecret must read without echoing (for example the CLI's masked prompt).
// Without it, confidential clients use MSGVAULT_X_CLIENT_SECRET. No secret is
// accepted in an authorization URL, command argument, or progress message.
type OAuthOptions struct {
	ClientID                      string
	ClientType                    ClientType
	AllowSend                     bool
	CallbackPort                  int
	AuthURL, TokenURL, APIBaseURL string
	HTTPClient                    *http.Client
	ReadSecret                    func(context.Context) (string, error)
	OpenBrowser                   func(context.Context, string) error
	Output                        io.Writer
	Now                           func() time.Time
	Timeout                       time.Duration
}

// Authorize exchanges one single-use S256 authorization code on IPv4 loopback,
// verifies users/me, and returns staged credentials without installing them.
func Authorize(parent context.Context, opts OAuthOptions) (TokenRecord, error) {
	if opts.ClientType == "" {
		opts.ClientType = PublicClient
	}
	if opts.AuthURL == "" {
		opts.AuthURL = DefaultAuthURL
	}
	if opts.TokenURL == "" {
		opts.TokenURL = DefaultTokenURL
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = DefaultBaseURL
	}
	if opts.CallbackPort == 0 {
		opts.CallbackPort = DefaultCallbackPort
	}
	if opts.CallbackPort < 1 || opts.CallbackPort > 65535 {
		return TokenRecord{}, errors.New("invalid X callback port")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Timeout <= 0 || opts.Timeout > 10*time.Minute {
		return TokenRecord{}, errors.New("invalid X authorization timeout")
	}
	for _, endpoint := range []string{opts.AuthURL, opts.TokenURL, opts.APIBaseURL} {
		if err := validateOAuthEndpoint(endpoint); err != nil {
			return TokenRecord{}, err
		}
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return TokenRecord{}, err
	}
	secret := ""
	if opts.ClientType == ConfidentialClient {
		var err error
		if opts.ReadSecret != nil {
			secret, err = opts.ReadSecret(ctx)
		} else {
			secret = os.Getenv("MSGVAULT_X_CLIENT_SECRET")
		}
		if err != nil {
			return TokenRecord{}, errors.New("cannot read X client secret")
		}
	}
	record := NewTokenRecord("1", opts.ClientID, opts.ClientType, "", "", secret)
	cfg, err := tokenConfig(record, opts.TokenURL)
	if err != nil {
		return TokenRecord{}, err
	}
	if strings.TrimSpace(opts.ClientID) == "" {
		return TokenRecord{}, errors.New("missing X client ID")
	}
	cfg.Endpoint.AuthURL = opts.AuthURL
	cfg.Scopes = slices.Clone(readScopes)
	if opts.AllowSend {
		cfg.Scopes = append(cfg.Scopes, "tweet.write")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.CallbackPort))
	cfg.RedirectURL = "http://" + address + "/callback"
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return TokenRecord{}, errors.New("cannot listen on X loopback callback port")
	}
	state, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	expires := opts.Now().Add(opts.Timeout)
	type result struct {
		record TokenRecord
		err    error
	}
	completed := make(chan result, 1)
	var mu sync.Mutex
	used := false
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		if used {
			mu.Unlock()
			http.Error(w, "Authorization callback already used", http.StatusBadRequest)
			return
		}
		used = true
		mu.Unlock()
		query := r.URL.Query()
		if len(query["state"]) != 1 || query.Get("state") != state || !opts.Now().Before(expires) || len(query["code"]) != 1 || query.Get("code") == "" || query.Get("error") != "" {
			http.Error(w, "Invalid or expired authorization callback", http.StatusBadRequest)
			completed <- result{err: errors.New("invalid or expired X authorization callback")}
			return
		}
		tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient(opts.HTTPClient))
		token, exchangeErr := cfg.Exchange(tokenCtx, query.Get("code"), oauth2.VerifierOption(verifier))
		if exchangeErr != nil {
			exchangeErr = safeOAuthError(ctx, "X authorization exchange failed")
			http.Error(w, "Authorization failed", http.StatusBadRequest)
			completed <- result{err: exchangeErr}
			return
		}
		authorized, exchangeErr := recordFromToken(record, token, opts.Now(), true)
		if exchangeErr == nil {
			prepared := &accountCredentials{record: authorized}
			base := oauthHTTPClient(opts.HTTPClient)
			transport := base.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			base.Transport = &bearerTransport{credentials: prepared, base: transport}
			owner, verifyErr := NewClient(opts.APIBaseURL, base).Me(ctx)
			if verifyErr != nil {
				exchangeErr = safeOAuthError(ctx, "cannot verify X authorized owner")
			} else {
				authorized.OwnerID = owner.ID
				authorized.Username = owner.Username
			}
		}
		if exchangeErr != nil {
			http.Error(w, "Authorization failed", http.StatusBadRequest)
			completed <- result{err: exchangeErr}
			return
		}
		_, _ = io.WriteString(w, "Authorization complete. You may close this window.")
		completed <- result{record: authorized}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	defer func() { _ = server.Close() }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	authorizationURL := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	open := opts.OpenBrowser
	if open == nil {
		_, _ = fmt.Fprintf(opts.Output, "Open this URL to authorize X:\n%s\n", authorizationURL)
		open = openOAuthBrowser
	}
	if err := open(ctx, authorizationURL); err != nil {
		return TokenRecord{}, safeOAuthError(ctx, "cannot open X authorization browser")
	}
	select {
	case outcome := <-completed:
		return outcome.record, outcome.err
	case <-ctx.Done():
		return TokenRecord{}, ctx.Err()
	case <-serveDone:
		return TokenRecord{}, errors.New("X authorization callback server stopped")
	}
}

func tokenConfig(record TokenRecord, tokenURL string) (*oauth2.Config, error) {
	if err := validateOAuthEndpoint(tokenURL); err != nil {
		return nil, err
	}
	style := oauth2.AuthStyleInParams
	switch record.ClientType {
	case PublicClient:
		if record.ClientSecret() != "" {
			return nil, errors.New("public X client cannot use a client secret")
		}
	case ConfidentialClient:
		style = oauth2.AuthStyleInHeader
		if record.ClientSecret() == "" {
			return nil, errors.New("missing X confidential client secret")
		}
	default:
		return nil, errors.New("invalid X OAuth client type")
	}
	return &oauth2.Config{ClientID: record.ClientID, ClientSecret: record.ClientSecret(), Endpoint: oauth2.Endpoint{TokenURL: tokenURL, AuthStyle: style}}, nil
}
func recordFromToken(previous TokenRecord, token *oauth2.Token, now time.Time, initial bool) (TokenRecord, error) {
	if token == nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") || token.ExpiresIn <= 0 || token.ExpiresIn > math.MaxInt64/int64(time.Second) {
		return TokenRecord{}, errors.New("invalid X token response")
	}
	scopes := slices.Clone(previous.GrantedScopes)
	returned, hasScope := token.Extra("scope").(string)
	if hasScope {
		scopes = strings.Fields(returned)
	}
	if initial {
		for _, required := range readScopes {
			if !slices.Contains(scopes, required) {
				return TokenRecord{}, errors.New("X authorization did not grant required read scopes")
			}
		}
	}
	if len(scopes) == 0 {
		return TokenRecord{}, errors.New("X token response has no granted scopes")
	}
	refresh := token.RefreshToken
	if refresh == "" {
		refresh = previous.RefreshToken()
	}
	next := NewTokenRecord(previous.OwnerID, previous.ClientID, previous.ClientType, token.AccessToken, refresh, previous.ClientSecret())
	next.Username = previous.Username
	next.consumedStage = previous.consumedStage
	next.GrantedScopes = scopes
	next.Expiry = now.Add(time.Duration(token.ExpiresIn) * time.Second)
	if err := validateRecord(next); err != nil {
		return TokenRecord{}, err
	}
	return next, nil
}
func validateOAuthEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid X OAuth endpoint")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("X OAuth endpoints require HTTPS or a loopback test server")
}
func oauthHTTPClient(base *http.Client) *http.Client {
	client := singleRequestHTTPClient(base)
	if client.Timeout > 30*time.Second {
		client.Timeout = 30 * time.Second
	}
	return client
}
func safeOAuthError(ctx context.Context, message string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(message)
}
func openOAuthBrowser(ctx context.Context, address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", address)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.CommandContext(ctx, "xdg-open", address)
	}
	// The URL is also printed, so unavailable desktop integration need not stop
	// authorization on a machine where the user opens it manually.
	_ = command.Run()
	return nil
}
