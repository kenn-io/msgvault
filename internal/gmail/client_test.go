package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// Gmail API error reason constants for tests.
const (
	reasonRateLimitExceeded     = "rateLimitExceeded"
	reasonUserRateLimitExceeded = "userRateLimitExceeded"
	reasonRateLimitExceededUC   = "RATE_LIMIT_EXCEEDED"
	reasonForbidden             = "forbidden"
	quotaExceededMsg            = "Quota exceeded for quota metric 'Queries'"
)

// GmailErrorBuilder constructs Gmail API error response JSON for tests.
type GmailErrorBuilder struct {
	code    int
	message string
	reasons []string
	details []string
}

// NewGmailError starts building a Gmail API error with the given HTTP status code.
func NewGmailError(code int) *GmailErrorBuilder {
	return &GmailErrorBuilder{code: code}
}

// WithMessage sets the error message.
func (b *GmailErrorBuilder) WithMessage(msg string) *GmailErrorBuilder {
	b.message = msg
	return b
}

// WithReason adds an entry to the errors[].reason array.
func (b *GmailErrorBuilder) WithReason(reason string) *GmailErrorBuilder {
	b.reasons = append(b.reasons, reason)
	return b
}

// WithDetail adds an entry to the details[].reason array.
func (b *GmailErrorBuilder) WithDetail(reason string) *GmailErrorBuilder {
	b.details = append(b.details, reason)
	return b
}

// toReasonMaps converts a string slice into a slice of {"reason": s} maps.
func toReasonMaps(items []string) []map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := make([]map[string]string, len(items))
	for i, item := range items {
		out[i] = map[string]string{"reason": item}
	}
	return out
}

// Build serializes the error to JSON bytes.
func (b *GmailErrorBuilder) Build() []byte {
	inner := map[string]any{"code": b.code}
	if b.message != "" {
		inner["message"] = b.message
	}
	if errs := toReasonMaps(b.reasons); errs != nil {
		inner["errors"] = errs
	}
	if dets := toReasonMaps(b.details); dets != nil {
		inner["details"] = dets
	}
	data, err := json.Marshal(map[string]any{"error": inner})
	if err != nil {
		panic(fmt.Sprintf("failed to marshal test body: %v", err))
	}
	return data
}

func TestDecodeBase64URL(t *testing.T) {
	// Test data: "Hello, World!" in various encodings
	plaintext := []byte("Hello, World!")
	// base64url unpadded (Gmail's typical format)
	unpadded := base64.RawURLEncoding.EncodeToString(plaintext)
	// base64url with padding
	padded := base64.URLEncoding.EncodeToString(plaintext)

	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr bool
	}{
		{
			name:    "unpadded base64url",
			input:   unpadded,
			want:    plaintext,
			wantErr: false,
		},
		{
			name:    "padded base64url",
			input:   padded,
			want:    plaintext,
			wantErr: false,
		},
		{
			name:    "empty string",
			input:   "",
			want:    []byte{},
			wantErr: false,
		},
		{
			name:    "single byte unpadded",
			input:   "QQ", // 'A'
			want:    []byte("A"),
			wantErr: false,
		},
		{
			name:    "single byte padded",
			input:   "QQ==", // 'A' with padding
			want:    []byte("A"),
			wantErr: false,
		},
		{
			name:    "two bytes unpadded",
			input:   "QUI", // 'AB'
			want:    []byte("AB"),
			wantErr: false,
		},
		{
			name:    "two bytes padded",
			input:   "QUI=", // 'AB' with single pad
			want:    []byte("AB"),
			wantErr: false,
		},
		{
			name:    "URL-safe characters unpadded",
			input:   "PDw_Pz4-", // "<<??>>", uses - and _ instead of + and /
			want:    []byte("<<??>>"),
			wantErr: false,
		},
		{
			name:    "URL-safe underscore unpadded",
			input:   "Pz8_", // "???" is exactly 3 bytes -> 4 chars, no padding needed
			want:    []byte("???"),
			wantErr: false,
		},
		{
			name:    "URL-safe dash unpadded",
			input:   "Pj4-", // ">>>" is exactly 3 bytes -> 4 chars, no padding needed
			want:    []byte(">>>"),
			wantErr: false,
		},
		{
			name:    "URL-safe dash with padding (1 byte)",
			input:   "-A==", // 0xf8 - exercises URLEncoding path with URL-safe char
			want:    []byte{0xf8},
			wantErr: false,
		},
		{
			name:    "URL-safe underscore with padding (1 byte)",
			input:   "_w==", // 0xff - exercises URLEncoding path with URL-safe char
			want:    []byte{0xff},
			wantErr: false,
		},
		{
			name:    "URL-safe dash with single pad (2 bytes)",
			input:   "A-A=", // 0x03 0xe0 - exercises URLEncoding with single = and URL-safe char
			want:    []byte{0x03, 0xe0},
			wantErr: false,
		},
		{
			name:    "invalid characters",
			input:   "!!!invalid!!!",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "malformed padding single char with equals",
			input:   "A=", // Invalid: 1 char before padding is never valid
			want:    nil,
			wantErr: true,
		},
		{
			name:    "malformed padding excess equals",
			input:   "QQ===", // Invalid: too many padding chars
			want:    nil,
			wantErr: true,
		},
		{
			name:    "malformed padding wrong count",
			input:   "QUI==", // Invalid: "AB" should have single =, not ==
			want:    nil,
			wantErr: true,
		},
		{
			name:    "binary data unpadded",
			input:   base64.RawURLEncoding.EncodeToString([]byte{0x00, 0xFF, 0x80, 0x7F}),
			want:    []byte{0x00, 0xFF, 0x80, 0x7F},
			wantErr: false,
		},
		{
			name:    "binary data padded",
			input:   base64.URLEncoding.EncodeToString([]byte{0x00, 0xFF, 0x80, 0x7F}),
			want:    []byte{0x00, 0xFF, 0x80, 0x7F},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeBase64URL(tt.input)
			if tt.wantErr {
				assertpkg.Error(t, err, "decodeBase64URL()")
				return
			}
			requirepkg.NoError(t, err, "decodeBase64URL()")
			assertpkg.Equal(t, string(tt.want), string(got), "decodeBase64URL()")
		})
	}
}

func TestIsRateLimitError(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{
			name: "RateLimitExceeded",
			body: NewGmailError(http.StatusForbidden).WithReason(reasonRateLimitExceeded).Build(),
			want: true,
		},
		{
			name: "RateLimitExceededByMessage",
			body: NewGmailError(http.StatusForbidden).WithMessage(quotaExceededMsg).WithReason(reasonRateLimitExceeded).Build(),
			want: true,
		},
		{
			name: "RateLimitExceededUpperCase",
			body: NewGmailError(http.StatusForbidden).WithDetail(reasonRateLimitExceededUC).Build(),
			want: true,
		},
		{
			name: "QuotaExceeded",
			body: NewGmailError(http.StatusForbidden).WithMessage(quotaExceededMsg).Build(),
			want: true,
		},
		{
			name: "UserRateLimitExceeded",
			body: NewGmailError(http.StatusForbidden).WithReason(reasonUserRateLimitExceeded).Build(),
			want: true,
		},
		{
			name: "PermissionDenied",
			body: NewGmailError(http.StatusForbidden).WithReason(reasonForbidden).Build(),
			want: false,
		},
		{
			name: "EmptyBody",
			body: []byte{},
			want: false,
		},
		{
			name: "InvalidJSON",
			body: []byte("not valid json but contains rateLimitExceeded"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRateLimitError(tt.body)
			assertpkg.Equal(t, tt.want, got, "isRateLimitError()")
		})
	}
}

// logRecord holds a captured log entry for test assertions.
type logRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string // key-value pairs from log attributes
}

// testLogHandler captures slog records for test assertions.
type testLogHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *testLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *testLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := logRecord{Level: r.Level, Message: r.Message, Attrs: make(map[string]string)}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}
func (h *testLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *testLogHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *testLogHandler) getRecords() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]logRecord, len(h.records))
	copy(out, h.records)
	return out
}

func TestGetMessagesRawBatch_LogLevels(t *testing.T) {
	assert := assertpkg.New(t)
	// Set up a test HTTP server that returns different responses per message ID.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/messages/msg_ok"):
			raw := base64.RawURLEncoding.EncodeToString([]byte("MIME-Version: 1.0\r\n\r\ntest"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "msg_ok",
				"threadId":     "thread_ok",
				"labelIds":     []string{"INBOX"},
				"internalDate": "1704067200000",
				"sizeEstimate": 100,
				"raw":          raw,
			})
		case strings.Contains(r.URL.Path, "/messages/msg_404"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(NewGmailError(404).WithMessage("Not Found").Build())
		case strings.Contains(r.URL.Path, "/messages/msg_err"):
			// 401 is a non-retryable error — returned immediately as a generic error.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Unauthorized"}}`))
		}
	}))
	defer srv.Close()

	// Override baseURL for test — we need to create a client that hits our test server.
	// Since baseURL is a const, create the client with a custom HTTP transport that
	// redirects requests to the test server.
	transport := &rewriteTransport{base: srv.URL, wrapped: http.DefaultTransport}
	httpClient := &http.Client{Transport: transport}

	handler := &testLogHandler{}
	logger := slog.New(handler)

	client := &Client{
		httpClient:  httpClient,
		userID:      "me",
		concurrency: 2,
		logger:      logger,
		rateLimiter: NewRateLimiter(1000), // High QPS to avoid rate limit delays
	}

	ctx := context.Background()
	results, err := client.GetMessagesRawBatch(ctx, []string{"msg_ok", "msg_404", "msg_err"})
	requirepkg.NoError(t, err, "GetMessagesRawBatch()")

	// msg_ok should succeed
	assert.NotNil(results[0], "expected msg_ok to return a result")
	// msg_404 and msg_err should be nil (errors logged, not returned)
	assert.Nil(results[1], "expected msg_404 to return nil (logged)")
	assert.Nil(results[2], "expected msg_err to return nil (logged)")

	// Check log levels per message ID.
	// msg_404 must produce ONLY a debug log (no warn), and msg_err must produce ONLY a warn log.
	records := handler.getRecords()

	var foundDebug404, foundWarnErr bool
	for _, r := range records {
		id := r.Attrs["id"]
		switch {
		case id == "msg_404" && r.Level == slog.LevelDebug && r.Message == "message deleted before fetch":
			foundDebug404 = true
		case id == "msg_404" && r.Level == slog.LevelWarn:
			assert.Failf("unexpected warn log", "msg_404 should not produce a warn log, got: %q", r.Message)
		case id == "msg_err" && r.Level == slog.LevelWarn && r.Message == "failed to fetch message":
			foundWarnErr = true
		case id == "msg_err" && r.Level == slog.LevelDebug:
			assert.Failf("unexpected debug log", "msg_err should not produce a debug log, got: %q", r.Message)
		}
	}

	assert.True(foundDebug404, "expected debug log for msg_404 with message %q, got records: %v", "message deleted before fetch", records)
	assert.True(foundWarnErr, "expected warn log for msg_err with message %q, got records: %v", "failed to fetch message", records)
}

// rewriteTransport rewrites requests from the Gmail API baseURL to a test server URL.
type rewriteTransport struct {
	base    string // test server URL
	wrapped http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite the URL to point at our test server
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.base, "http://")
	return t.wrapped.RoundTrip(req)
}
