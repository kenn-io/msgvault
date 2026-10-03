package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/requestsign"
	"golang.org/x/time/rate"
)

const remoteMessageBytes int64 = 16 << 20
const remoteAttachmentBytes int64 = 1 << 30

type remotePrincipalKey struct{}
type remotePrincipal struct{ operation string }

func restrictedRequest(r *http.Request) bool {
	_, ok := r.Context().Value(remotePrincipalKey{}).(remotePrincipal)
	return ok
}

type ingressKey struct {
	client *ingressClient
	secret []byte
	cfg    config.RequestSigningKey
}
type ingressClient struct {
	apiKey           string
	writeCollections bool
	budget           *rate.Limiter
}
type restrictedIngress struct {
	guard        *requestsign.ReplayGuard
	cfg          config.RemoteIngressConfig
	keys         map[string]ingressKey
	proxies      []*net.IPNet
	slots        chan struct{}
	budget       *rate.Limiter
	server       *http.Server
	closing      atomic.Bool
	shutdownOnce sync.Once
	shutdownErr  error
	credentials  []string
}

const remoteAttachmentWriteTimeout = 5 * time.Minute

type attachmentStreamResponseWriter struct {
	http.ResponseWriter

	controller         *http.ResponseController
	firstResponseTimer *time.Timer
	idleTimeout        time.Duration
	writeErr           error
}

func newAttachmentStreamResponseWriter(
	parent context.Context,
	w http.ResponseWriter,
) (context.Context, http.ResponseWriter, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	writer := &attachmentStreamResponseWriter{
		ResponseWriter:     w,
		controller:         http.NewResponseController(w),
		firstResponseTimer: time.AfterFunc(remoteAttachmentWriteTimeout, cancel),
		idleTimeout:        remoteAttachmentWriteTimeout,
	}
	return ctx, writer, func() {
		writer.firstResponseTimer.Stop()
		cancel()
	}
}

func (w *attachmentStreamResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *attachmentStreamResponseWriter) refreshWriteDeadline() error {
	if err := w.controller.SetWriteDeadline(time.Now().Add(w.idleTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	w.firstResponseTimer.Stop()
	return nil
}

func (w *attachmentStreamResponseWriter) WriteHeader(statusCode int) {
	if w.writeErr != nil {
		return
	}
	if err := w.refreshWriteDeadline(); err != nil {
		w.writeErr = err
		return
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *attachmentStreamResponseWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if err := w.refreshWriteDeadline(); err != nil {
		w.writeErr = err
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

func (w *attachmentStreamResponseWriter) FlushError() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if err := w.refreshWriteDeadline(); err != nil {
		w.writeErr = err
		return err
	}
	return w.controller.Flush()
}

func (w *attachmentStreamResponseWriter) Flush() { _ = w.FlushError() }

// RestrictedRouter is a separate, fail-closed native mount. The main router's
// owner credential is never substituted for the client's native API credential.
func (s *Server) RestrictedRouter() (http.Handler, error) {
	s.remoteIngressMu.Lock()
	defer s.remoteIngressMu.Unlock()
	if s.remoteIngress != nil {
		if s.remoteIngress.closing.Load() {
			return nil, errors.New("restricted ingress is closing")
		}
		return http.HandlerFunc(s.remoteIngress.serve(s.router)), nil
	}
	cfg := s.cfg.Server.RemoteIngress
	if !cfg.Enabled {
		return nil, errors.New("restricted remote ingress is disabled")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ingress := &restrictedIngress{cfg: cfg, keys: make(map[string]ingressKey), slots: make(chan struct{}, cfg.ConcurrentLimit()), budget: rate.NewLimiter(10, 20)}
	credentials := make(map[string]bool)
	secrets := make(map[[32]byte]bool)
	for _, clientCfg := range cfg.Clients {
		apiKey, err := requestsign.ReadAPIKey(clientCfg.APIKeyFile)
		if err != nil {
			return nil, err
		}
		if constantTimeAPIKeyEqual(apiKey, s.cfg.Server.APIKey) || credentials[apiKey] {
			return nil, errors.New("remote clients require distinct dedicated API credentials")
		}
		credentials[apiKey] = true
		ingress.credentials = append(ingress.credentials, apiKey)
		client := &ingressClient{apiKey: apiKey, budget: rate.NewLimiter(10, 20)}
		for _, grant := range clientCfg.Grants {
			if grant == "collections-write" {
				client.writeCollections = true
			}
		}
		for _, keyCfg := range clientCfg.Keys {
			secret, err := requestsign.ReadSigningSecret(keyCfg.SecretFile)
			if err != nil {
				return nil, err
			}
			hash := sha256.Sum256(secret)
			if secrets[hash] {
				return nil, errors.New("each signing key requires an independent secret")
			}
			secrets[hash] = true
			ingress.keys[keyCfg.KeyID] = ingressKey{client: client, secret: secret, cfg: keyCfg}
		}
	}
	for _, key := range ingress.keys {
		if signingSecretMatchesCredential(key.secret, s.cfg.Server.APIKey) {
			return nil, errors.New("each signing key requires an independent secret")
		}
		for credential := range credentials {
			if signingSecretMatchesCredential(key.secret, credential) {
				return nil, errors.New("each signing key requires an independent secret")
			}
		}
	}
	for _, allowed := range cfg.ProxyAllowlist() {
		_, prefix, err := net.ParseCIDR(allowed)
		if err != nil {
			ip := net.ParseIP(allowed)
			bits := 128
			if ip.To4() != nil {
				ip = ip.To4()
				bits = 32
			}
			prefix = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		}
		ingress.proxies = append(ingress.proxies, prefix)
	}
	guard, err := requestsign.OpenReplayGuard(cfg.ReplayStateFile, time.Now(), 10000)
	if err != nil {
		return nil, err
	}
	ingress.guard = guard
	s.remoteIngress = ingress
	return http.HandlerFunc(ingress.serve(s.router)), nil
}

// Possession of any API credential must not reveal a signing key, including
// when the same bytes were stored as a conventional encoded string.
func signingSecretMatchesCredential(secret []byte, credential string) bool {
	if hmac.Equal(secret, []byte(credential)) {
		return true
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(credential)
		if err == nil && hmac.Equal(secret, decoded) {
			return true
		}
	}
	decoded, err := hex.DecodeString(credential)
	return err == nil && hmac.Equal(secret, decoded)
}

func remoteOperation(method, path string, write bool) string {
	if method == http.MethodGet {
		switch path {
		case "/api/v1/health":
			return "getHealth"
		case "/api/v1/cli/stats":
			return "getCLIStats"
		case "/api/v1/cli/search":
			return "searchCLI"
		case "/api/v1/cli/accounts":
			return "listCLIAccounts"
		case "/api/v1/cli/cache-stats":
			return "getCLICacheStats"
		case "/api/v1/cli/message":
			return "getCLIMessage"
		case "/api/v1/cli/message/original":
			return "getCLIMessageOriginal"
		case "/api/v1/cli/message/thread":
			return "getCLIMessageThread"
		case "/api/v1/cli/message/raw":
			return "getCLIMessageRaw"
		case "/api/v1/cli/attachment":
			return "getCLIAttachment"
		case "/api/v1/cli/collections":
			return "listCLICollections"
		case "/api/v1/cli/collection":
			return "getCLICollection"
		case "/api/v1/cli/identities":
			return "listCLIIdentities"
		}
	}
	if !write {
		return ""
	}
	if method == http.MethodPost && path == "/api/v1/cli/collections" {
		return "createCLICollection"
	}
	name, ok := strings.CutPrefix(path, "/api/v1/cli/collections/")
	if !ok || name == "" {
		return ""
	}
	if collection, sources := strings.CutSuffix(name, "/sources"); sources && collection != "" && !strings.Contains(collection, "/") {
		if method == http.MethodPatch {
			return "addCLICollectionSources"
		}
		if method == http.MethodDelete {
			return "removeCLICollectionSources"
		}
	}
	if method == http.MethodDelete && !strings.Contains(name, "/") {
		return "deleteCLICollection"
	}
	return ""
}

func (k ingressKey) active(now time.Time) bool {
	return (k.cfg.NotBefore == 0 || now.Unix() >= k.cfg.NotBefore) && (k.cfg.NotAfter == 0 || now.Unix() < k.cfg.NotAfter)
}

func (i *restrictedIngress) serve(next http.Handler) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		wireRequest := r
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code, message string) {
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "1")
			}
			writeError(w, status, code, message)
		}
		if !i.budget.Allow() {
			fail(429, "remote_rate_limited", "Remote request rate exceeded")
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		trusted := false
		if err == nil {
			for _, prefix := range i.proxies {
				if prefix.Contains(ip) {
					trusted = true
					break
				}
			}
		}
		if !trusted {
			fail(403, "untrusted_proxy", "Request peer is not a configured reverse proxy")
			return
		}
		size := len(r.RequestURI)
		for name, values := range r.Header {
			size += len(name)
			for _, v := range values {
				size += len(v) + 4
			}
		}
		if size > 16<<10 {
			fail(431, "remote_headers_too_large", "Remote request headers exceed 16 KiB")
			return
		}
		// No second authentication mode may alter the verified native identity.
		for _, name := range []string{"Authorization", "Cookie", apiprotocol.AgentTokenHeader, apiprotocol.DaemonRuntimeTokenHeader} {
			for header := range r.Header {
				if strings.EqualFold(header, name) {
					fail(401, "invalid_signature", "Invalid remote request authentication")
					return
				}
			}
		}
		target, err := requestsign.ExternalTarget(i.cfg.ExternalURL, r)
		if err != nil {
			fail(401, "invalid_signature", "Invalid remote request target")
			return
		}
		metadata, err := requestsign.ParseMetadata(r)
		key, ok := i.keys[metadata.KeyID]
		apiKey, keyErr := requestsign.SingleHeader(r.Header, "X-Api-Key")
		now := time.Now()
		if err != nil || !ok || keyErr != nil || !key.active(now) || !constantTimeAPIKeyEqual(apiKey, key.client.apiKey) {
			fail(401, "invalid_signature", "Invalid remote request authentication")
			return
		}
		if key.cfg.NotBefore != 0 && metadata.Created < key.cfg.NotBefore || key.cfg.NotAfter != 0 && metadata.Expires > key.cfg.NotAfter {
			fail(401, "invalid_signature", "Signature lifetime is outside the accepted key window")
			return
		}
		metadata, err = requestsign.VerifyHeaders(r, target, key.secret, metadata.KeyID, now)
		if err != nil {
			fail(401, "invalid_signature", "Invalid remote request signature")
			return
		}
		operation := remoteOperation(r.Method, r.URL.Path, key.client.writeCollections)
		if operation == "" {
			fail(403, "remote_operation_denied", "Operation is not granted on remote ingress")
			return
		}
		if !key.client.budget.Allow() {
			fail(429, "remote_rate_limited", "Client request rate exceeded")
			return
		}
		select {
		case i.slots <- struct{}{}:
			defer func() { <-i.slots }()
		default:
			fail(429, "remote_concurrency_exceeded", "Remote request concurrency exceeded")
			return
		}
		if i.guard.Ready(now) != nil {
			fail(503, "replay_unavailable", "Remote signing verifier is not ready")
			return
		}
		requestContext := r.Context()
		ctx, cancel := context.WithTimeout(requestContext, 5*time.Minute)
		defer cancel()
		r = r.WithContext(ctx)
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Minute))
		bodyCtx, bodyCancel := context.WithTimeout(ctx, 30*time.Second)
		bodyRequest := r.WithContext(bodyCtx)
		digest, cleanup, err := requestsign.Spool(bodyRequest, i.cfg.BodyLimit())
		bodyCancel()
		if cleanup != nil {
			defer cleanup()
		}
		if errors.Is(err, requestsign.ErrBodyTooLarge) {
			fail(413, "remote_body_too_large", "Remote request body exceeds configured limit")
			return
		}
		if err != nil || len(wireRequest.Trailer) > 0 || len(bodyRequest.Trailer) > 0 || !hmac.Equal([]byte(digest), []byte(metadata.Digest)) {
			fail(401, "invalid_signature", "Invalid remote request body")
			return
		}
		r.Body = bodyRequest.Body
		r.ContentLength = bodyRequest.ContentLength
		r.GetBody = nil
		fresh := func() bool {
			if i.guard.Ready(time.Now()) != nil {
				return false
			}
			now := time.Now()
			return ctx.Err() == nil && key.active(now) && now.Before(time.Unix(metadata.Expires, 0)) && metadata.Created <= now.Unix()+5
		}
		if !fresh() {
			fail(401, "invalid_signature", "Remote signature expired or verifier unavailable")
			return
		}
		if err := i.guard.Use(metadata.KeyID, metadata.Nonce, time.Unix(metadata.Expires, 0), time.Now()); err != nil {
			status := 401
			if errors.Is(err, requestsign.ErrReplayUnavailable) {
				status = 503
			}
			fail(status, "replay_rejected", "Remote request replay protection rejected admission")
			return
		}
		if !fresh() {
			fail(401, "invalid_signature", "Remote signature expired or verifier unavailable")
			return
		}
		_ = controller.SetReadDeadline(time.Time{})
		dispatchContext := ctx
		dispatchWriter := w
		if operation == "getCLIAttachment" {
			_ = controller.SetWriteDeadline(time.Time{})
			var streamCancel context.CancelFunc
			dispatchContext, dispatchWriter, streamCancel = newAttachmentStreamResponseWriter(requestContext, w)
			defer streamCancel()
		}
		dispatchContext = context.WithValue(dispatchContext, remotePrincipalKey{}, remotePrincipal{operation: operation})
		dispatchContext = query.WithMessageByteLimit(dispatchContext, remoteMessageBytes)
		next.ServeHTTP(dispatchWriter, r.WithContext(dispatchContext))
	}
}

func (s *Server) startRestrictedListener() error {
	if !s.cfg.Server.RemoteIngress.Enabled {
		return nil
	}
	handler, err := s.RestrictedRouter()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.cfg.Server.RemoteIngress.ListenAddr())
	if err != nil {
		_ = s.closeRestrictedIngress(context.Background())
		return fmt.Errorf("listen on restricted ingress: %w", err)
	}
	server := &http.Server{Addr: ln.Addr().String(), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	s.remoteIngressMu.Lock()
	if s.remoteIngress.closing.Load() {
		s.remoteIngressMu.Unlock()
		_ = ln.Close()
		return errors.New("restricted ingress is closing")
	}
	s.remoteIngress.server = server
	s.remoteIngressMu.Unlock()
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("restricted ingress stopped", "error", err)
			_ = s.closeRestrictedIngress(context.Background())
		}
	}()
	s.logger.Info("restricted remote ingress started", "addr", ln.Addr().String())
	return nil
}

// Main listener authentication reads only the immutable credential snapshot.
func (s *Server) restrictedCredentialPresented(r *http.Request) bool {
	s.remoteIngressMu.Lock()
	ingress := s.remoteIngress
	s.remoteIngressMu.Unlock()
	if ingress == nil {
		return false
	}
	for name, values := range r.Header {
		if strings.EqualFold(name, "X-Api-Key") || strings.EqualFold(name, "Authorization") {
			for _, value := range values {
				value = strings.TrimPrefix(value, "Bearer ")
				for _, credential := range ingress.credentials {
					if constantTimeAPIKeyEqual(value, credential) {
						return true
					}
				}
			}
		}
	}
	return false
}

func (s *Server) closeRestrictedIngress(ctx context.Context) error {
	s.remoteIngressMu.Lock()
	ingress := s.remoteIngress
	if ingress == nil {
		s.remoteIngressMu.Unlock()
		return nil
	}
	ingress.closing.Store(true)
	server := ingress.server
	s.remoteIngressMu.Unlock()
	ingress.shutdownOnce.Do(func() {
		ingress.guard.Fence()
		if server != nil {
			if err := server.Shutdown(ctx); err != nil {
				ingress.shutdownErr = errors.Join(err, server.Close())
				// Keep the replay lock until process exit after an incomplete drain.
				return
			}
		}
		ingress.shutdownErr = ingress.guard.Close()
	})
	return ingress.shutdownErr
}
