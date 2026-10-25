package apiprotocol

import "context"

type browserOriginKey struct{}

// WithBrowserOrigin carries the API's effective request origin for browser links.
func WithBrowserOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, browserOriginKey{}, origin)
}

// BrowserOrigin returns request-local browser metadata, or the selected daemon URL.
func BrowserOrigin(ctx context.Context, fallback string) string {
	if origin, _ := ctx.Value(browserOriginKey{}).(string); origin != "" {
		return origin
	}
	return fallback
}
