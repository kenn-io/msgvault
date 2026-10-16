package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/msgvault/internal/sourcescope"
)

// Operation identifies a registered operation. It is never taken from headers.
type Operation struct {
	ID     string
	Method string
	Path   string
}

// Handler serves the complete API with caller-supplied authorization. Unlike
// Router, it adds no daemon transport authentication, listener, or browser UI.
// A nil callback adds no authorization: the caller must supply access control
// when exposing this handler, including browser origin and CSRF policy. On
// rejection the callback writes the response.
//
// Authorized operations then pass through the configured OperationGate, so
// rejected requests never wait on or observe archive work. The handler also
// applies RequestTimeout, request IDs, panic recovery, and default no-store
// response headers as Router does. Unlike Router, the CLI client-class header
// never lifts RequestTimeout or the host server's connection deadlines: a
// request header must not override the caller's server policy.
//
// Without a caller source scope, an admitted request acts as the archive owner.
// That includes daemon shutdown through ServerOptions' shutdown callback, backup
// freezes, and agent token management, without the daemon's shutdown token or
// same-host checks.
func (s *Server) Handler(authorize func(http.ResponseWriter, *http.Request, Operation) bool) http.Handler {
	mux := http.NewServeMux()
	api := s.setupHumaAPI(mux)
	gate := operationGateMiddleware(s.operationGate, nil)
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		r, w := humago.Unwrap(ctx)
		op := ctx.Operation()
		operation := Operation{ID: op.OperationID, Method: op.Method, Path: op.Path}
		if authorize != nil && !authorize(w, r, operation) {
			return
		}
		if sourcescope.Restricted(r.Context()) && !sourceScopedOperation(r, operation) {
			writeError(w, http.StatusForbidden, "unsupported_source_scope", "This operation does not support source authorization")
			return
		}
		// The humago context retains this request pointer. Update it after
		// admission so both raw handlers and the gate see caller authorization.
		security, _ := securityFromRequest(r)
		security.auth = requestAuthentication{Mode: AuthModeCaller}
		*r = *r.WithContext(context.WithValue(r.Context(), requestSecurityContextKey{}, security))
		// Select timeout policy after admission. Keep Huma's request pointer in
		// sync with the context and body passed through timeout and gate handling.
		s.timeoutMiddleware(gate(http.HandlerFunc(func(_ http.ResponseWriter, timed *http.Request) {
			*r = *timed
			if sourcescope.Restricted(r.Context()) {
				release, apiErr := s.beginCallerRead(r)
				if apiErr != nil {
					writeAPIHTTPError(w, apiErr)
					return
				}
				defer release()
			}
			next(ctx)
		}))).ServeHTTP(w, r)
	})
	s.registerHumaRoutes(api, huma.NewGroup(api, "/api/v1"))
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		boundary := &readResponseWriter{ResponseWriter: w}
		ctx := context.WithValue(r.Context(), readResponseKey{}, boundary)
		scheme := "http"
		if r.TLS != nil {
			scheme = schemeHTTPS
		}
		security := requestSecurity{
			auth: requestAuthentication{Mode: AuthModeRequired}, scheme: scheme, host: r.Host,
		}
		mux.ServeHTTP(boundary, r.WithContext(context.WithValue(ctx, requestSecurityContextKey{}, security)))
	})
	h = s.analyticsEngineMiddleware(h)
	h = s.recoverMiddleware(h)
	h = apiCacheControlMiddleware(h)
	return requestIDMiddleware(h)
}
