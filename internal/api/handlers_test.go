package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/remote"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// stubEmbedder is an EmbeddingClient placeholder for tests where the
// engine never reaches the embed step (e.g. ResolveActiveForFingerprint
// fails first). Calling Embed signals a test bug — guard with a t.Fatal-
// style failure rather than returning silently.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("stubEmbedder.Embed should not be called in this test")
}

func newTestServerWithMockStore(t *testing.T) (*Server, *mockStore) {
	t.Helper()

	store := &mockStore{
		stats: &StoreStats{
			MessageCount:    10,
			ThreadCount:     5,
			SourceCount:     1,
			LabelCount:      3,
			AttachmentCount: 2,
			DatabaseSize:    1024,
		},
		messages: []APIMessage{
			{
				ID:             1,
				Subject:        "Test Subject",
				From:           "sender@example.com",
				To:             []string{"recipient@example.com"},
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Snippet:        "This is a test message snippet",
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
				Body:           "This is the full message body text.",
				Attachments:    nil,
			},
		},
		total: 1,
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}

	sched := newMockScheduler()
	sched.scheduled["test@gmail.com"] = true

	srv := NewServer(cfg, store, sched, testLogger())
	return srv, store
}

func TestHandleStats(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	bodyBytes := w.Body.Bytes()

	var resp StatsResponse
	require.NoError(json.Unmarshal(bodyBytes, &resp), "failed to decode response")

	assert.Equal(int64(10), resp.TotalMessages, "total_messages")
	assert.Equal(int64(1), resp.TotalAccounts, "total_accounts")

	// No backend wired → vector_search field must be ABSENT, not
	// null. Re-decode into raw RawMessage so we distinguish the two:
	// `omitempty` plus a nil pointer drops the key entirely, while
	// any encoder bug that emits `"vector_search": null` would still
	// leave resp.VectorSearch == nil.
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(bodyBytes, &raw), "decode raw")
	_, exists := raw["vector_search"]
	assert.False(exists, "vector_search key present in JSON; want omitted entirely (raw=%s)", string(raw["vector_search"]))
}

func TestHandleListMessages(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "expected messages array in response")
	assert.NotEmpty(messages, "expected at least 1 message")

	// Check first message structure
	msg := messages[0].(map[string]any)
	assert.Equal("Test Subject", msg["subject"], "subject")

	// Verify RFC3339 time format
	sentAt := msg["sent_at"].(string)
	_, err := time.Parse(time.RFC3339, sentAt)
	assert.NoError(err, "sent_at %q is not RFC3339", sentAt)
}

func TestHandleListMessagesPagination(t *testing.T) {
	assert := assertpkg.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages?page=1&page_size=10", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp map[string]any
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(float64(1), resp["page"], "page")
	assert.Equal(float64(10), resp["page_size"], "page_size")
}

func TestHandleGetMessage(t *testing.T) {
	assert := assertpkg.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp MessageDetail
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(int64(1), resp.ID, "id")
	assert.Equal("Test Subject", resp.Subject, "subject")
	assert.Equal("This is the full message body text.", resp.Body, "body")
}

func TestHandleGetMessageNotFound(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages/99999", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestHandleGetMessageInvalidID(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages/invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleGetMessage_EngineBodyHTML(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			42: {
				ID:       42,
				Subject:  "HTML Email",
				From:     []query.Address{{Email: "sender@example.com", Name: "Sender"}},
				To:       []query.Address{{Email: "rcpt@example.com"}},
				SentAt:   time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
				Labels:   []string{"INBOX"},
				BodyText: "plain fallback",
				BodyHTML: "<p>Hello</p>",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/42", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode")
	assert.Equal("plain fallback", resp["body"], "body")
	assert.Equal("<p>Hello</p>", resp["body_html"], "body_html")
	assert.Equal("HTML Email", resp["subject"], "subject")
	assert.Equal("Sender <sender@example.com>", resp["from"], "from")
	assert.NotContains(resp, "deleted_at", "deleted_at should be omitted for live message")
}

// TestHandleGetMessage_EngineDeletedAt verifies the engine path surfaces
// deleted_at in the response when the underlying message has a
// deleted_from_source_at timestamp.
func TestHandleGetMessage_EngineDeletedAt(t *testing.T) {
	deletedAt := time.Date(2024, 7, 1, 9, 30, 0, 0, time.UTC)
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			42: {
				ID:        42,
				Subject:   "Deleted",
				From:      []query.Address{{Email: "sender@example.com"}},
				SentAt:    time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
				DeletedAt: &deletedAt,
				BodyText:  "hello",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/42", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode")
	want := deletedAt.Format(time.RFC3339)
	assertpkg.Equal(t, want, resp["deleted_at"], "deleted_at")
}

func TestHandleSearchMissingQuery(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleSearch(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=Test", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusOK, w.Code, "status")

	var resp SearchResult
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assertpkg.Equal(t, "Test", resp.Query, "query")
}

func TestHandleTriggerSync(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}

	sched := newMockScheduler()
	sched.scheduled["test@gmail.com"] = true

	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest("POST", "/api/v1/sync/test@gmail.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusAccepted, w.Code, "status (body: %s)", w.Body.String())
}

func TestHandleTriggerSyncNotFound(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}

	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest("POST", "/api/v1/sync/unknown@gmail.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestHandleTriggerSyncConflict(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}

	sched := newMockScheduler()
	sched.scheduled["test@gmail.com"] = true
	sched.triggerFn = func(email string) error {
		return errors.New("sync already running for test@gmail.com")
	}

	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest("POST", "/api/v1/sync/test@gmail.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusConflict, w.Code, "status")
}

func TestErrorResponseShape(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	// Test with invalid ID to get a 400 error
	req := httptest.NewRequest("GET", "/api/v1/messages/invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	var resp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode error response")

	assertpkg.NotEmpty(t, resp.Error, "expected error code in response")
	assertpkg.NotEmpty(t, resp.Message, "expected error message in response")
}

func TestMessageSummaryNilSlices(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{
				ID:      1,
				Subject: "No recipients",
				To:      nil,
				Labels:  nil,
			},
		},
		total: 1,
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, store, sched, testLogger())

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	// Verify nil slices become empty arrays, not null
	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages := resp["messages"].([]any)
	msg := messages[0].(map[string]any)

	// "to" should be an empty array, not null
	to, ok := msg["to"].([]any)
	require.True(ok, "expected 'to' to be an array, got %T", msg["to"])
	assert.Empty(to, "expected empty 'to' array")

	labels, ok := msg["labels"].([]any)
	require.True(ok, "expected 'labels' to be an array, got %T", msg["labels"])
	assert.Empty(labels, "expected empty 'labels' array")
}

func TestMessageSummaryCcBccInResponse(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].Cc = []string{"cc1@example.com", "cc2@example.com"}
	ms.messages[0].Bcc = []string{"bcc@example.com"}

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status")

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	msg := resp["messages"].([]any)[0].(map[string]any)

	ccRaw, ok := msg["cc"].([]any)
	require.True(ok, "expected 'cc' array, got %T", msg["cc"])
	var gotCc []string
	for _, v := range ccRaw {
		gotCc = append(gotCc, v.(string))
	}
	slices.Sort(gotCc)
	wantCc := []string{"cc1@example.com", "cc2@example.com"}
	assert.Equal(wantCc, gotCc, "cc")

	bcc, ok := msg["bcc"].([]any)
	require.True(ok, "expected 'bcc' array, got %T", msg["bcc"])
	require.Len(bcc, 1, "bcc")
	assert.Equal("bcc@example.com", bcc[0], "bcc[0]")
}

func TestMessageSummaryCcBccOmittedWhenEmpty(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	// Parse raw JSON to check field presence
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode")

	var messages []json.RawMessage
	require.NoError(json.Unmarshal(raw["messages"], &messages), "decode messages")

	var msg map[string]json.RawMessage
	require.NoError(json.Unmarshal(messages[0], &msg), "decode message")

	assert.NotContains(msg, "cc", "expected 'cc' to be omitted from JSON when empty")
	assert.NotContains(msg, "bcc", "expected 'bcc' to be omitted from JSON when empty")
}

func TestGetMessageCcBccInResponse(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].Cc = []string{"cc@example.com"}
	ms.messages[0].Bcc = []string{"bcc@example.com"}

	req := httptest.NewRequest("GET", "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status")

	var resp MessageDetail
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	require.Len(resp.Cc, 1, "cc")
	assert.Equal("cc@example.com", resp.Cc[0], "cc[0]")
	require.Len(resp.Bcc, 1, "bcc")
	assert.Equal("bcc@example.com", resp.Bcc[0], "bcc[0]")
}

func TestHandleUploadToken(t *testing.T) {
	// Create temp directory for tokens
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	requirepkg.NoError(t, err, "failed to create temp dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tokenJSON := `{
		"access_token": "ya29.test",
		"token_type": "Bearer",
		"refresh_token": "1//test-refresh-token",
		"expiry": "2024-12-31T23:59:59Z"
	}`

	req := httptest.NewRequest("POST", "/api/v1/auth/token/test@gmail.com", strings.NewReader(tokenJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())

	// Verify token file was created
	tokenPath := filepath.Join(tmpDir, "tokens", "test@gmail.com.json")
	_, statErr := os.Stat(tokenPath)
	assertpkg.False(t, os.IsNotExist(statErr), "token file was not created at %s", tokenPath)
}

func TestHandleUploadToken_PreservesClientID(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	require.NoError(err, "failed to create temp dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tokenJSON := `{
		"access_token": "ya29.test",
		"token_type": "Bearer",
		"refresh_token": "1//test-refresh-token",
		"client_id": "myapp.apps.googleusercontent.com"
	}`

	req := httptest.NewRequest("POST", "/api/v1/auth/token/test@gmail.com", strings.NewReader(tokenJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())

	// Read back the saved token and verify client_id was preserved
	tokenPath := filepath.Join(tmpDir, "tokens", "test@gmail.com.json")
	data, err := os.ReadFile(tokenPath)
	require.NoError(err, "read token file")

	var saved struct {
		ClientID string `json:"client_id"`
	}
	require.NoError(json.Unmarshal(data, &saved), "unmarshal saved token")
	assertpkg.Equal(t, "myapp.apps.googleusercontent.com", saved.ClientID, "client_id")
}

func TestHandleUploadTokenInvalidJSON(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	require.NoError(err, "failed to create temp dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest("POST", "/api/v1/auth/token/test@gmail.com", strings.NewReader("not valid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusBadRequest, w.Code, "status")

	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode error response")
	assert.Equal("invalid_json", resp.Error, "error")
}

func TestHandleUploadTokenMissingRefreshToken(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	require.NoError(err, "failed to create temp dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	// Token without refresh_token
	tokenJSON := `{
		"access_token": "ya29.test",
		"token_type": "Bearer"
	}`

	req := httptest.NewRequest("POST", "/api/v1/auth/token/test@gmail.com", strings.NewReader(tokenJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusBadRequest, w.Code, "status")

	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode error response")
	assert.Equal("invalid_token", resp.Error, "error")
}

func TestHandleUploadTokenInvalidEmail(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tokenJSON := `{"refresh_token": "test"}`

	tests := []struct {
		name  string
		email string
	}{
		{"no at sign", "testgmail.com"},
		{"no domain", "test@"},
		{"no dot", "test@gmailcom"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/auth/token/"+tc.email, strings.NewReader(tokenJSON))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Router().ServeHTTP(w, req)

			assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status for email %q", tc.email)
		})
	}
}

func TestHandleUploadTokenMissingEmail(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	// Request without email in path - should 404 since route doesn't match
	req := httptest.NewRequest("POST", "/api/v1/auth/token/", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	// Chi router will 404 on missing path parameter
	assertpkg.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, w.Code, "status = %d, want 404 or 400", w.Code)
}

func TestHandleAddAccount(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir, err := os.MkdirTemp("", "msgvault-test-config-*")
	require.NoError(err, "failed to create temp dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &config.Config{
		Server:  config.ServerConfig{APIPort: 8080},
		HomeDir: tmpDir,
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "new@gmail.com", "schedule": "0 3 * * *"}`
	req := httptest.NewRequest("POST", "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())

	// Verify account was added to config
	require.Len(cfg.Accounts, 1, "expected 1 account")
	assert.Equal("new@gmail.com", cfg.Accounts[0].Email, "email")

	// Verify scheduler was notified
	require.Len(sched.addedAccts, 1, "scheduler.AddAccount not called, addedAccts = %v", sched.addedAccts)
	assert.Equal("new@gmail.com", sched.addedAccts[0], "scheduler.AddAccount account")
}

func TestHandleAddAccountDuplicate(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "existing@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "existing@gmail.com"}`
	req := httptest.NewRequest("POST", "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusOK, w.Code, "status")

	var resp map[string]string
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assertpkg.Equal(t, "exists", resp["status"], "status")
}

func TestHandleAddAccountInvalidCron(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "new@gmail.com", "schedule": "not a cron"}`
	req := httptest.NewRequest("POST", "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())

	var resp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assertpkg.Equal(t, "invalid_schedule", resp.Error, "error")
}

func TestHandleAddAccountInvalidEmail(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tests := []struct {
		name string
		body string
		code int
	}{
		{"empty email", `{"email": ""}`, http.StatusBadRequest},
		{"no at sign", `{"email": "nope"}`, http.StatusBadRequest},
		{"no dot", `{"email": "nope@nope"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/accounts", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assertpkg.Equal(t, tt.code, w.Code, "status")
		})
	}
}

func TestHandleAddAccountSaveFailure(t *testing.T) {
	// Point HomeDir to a file (not a directory) so Save() fails
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	requirepkg.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0600), "create blocker file")

	cfg := &config.Config{
		Server:  config.ServerConfig{APIPort: 8080},
		HomeDir: tmpFile, // Save() will fail: can't mkdir inside a file
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "fail@gmail.com", "schedule": "0 2 * * *"}`
	req := httptest.NewRequest("POST", "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusInternalServerError, w.Code, "status")

	// In-memory state should be rolled back
	assertpkg.Empty(t, cfg.Accounts, "cfg.Accounts has %d entries, want 0 (rollback failed)", len(cfg.Accounts))
}

func TestSanitizeTokenPath(t *testing.T) {
	tokensDir := "/data/tokens"

	tests := []struct {
		name  string
		email string
	}{
		{"normal email", "user@gmail.com"},
		{"email with plus", "user+tag@gmail.com"},
		{"email with dots", "first.last@gmail.com"},
		{"path traversal attempt", "../../../etc/passwd"},
		{"slash in email", "user/evil@gmail.com"},
		{"backslash in email", "user\\evil@gmail.com"},
		{"null byte", "user\x00evil@gmail.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := sanitizeTokenPath(tokensDir, tc.email)

			// Result must be within tokensDir (path traversal prevention)
			cleanResult := filepath.Clean(result)
			cleanTokensDir := filepath.Clean(tokensDir)
			assertpkg.True(t, strings.HasPrefix(cleanResult, cleanTokensDir+string(os.PathSeparator)),
				"path %q escapes tokensDir %q", result, tokensDir)

			// Result must end with .json
			assertpkg.True(t, strings.HasSuffix(result, ".json"), "path %q doesn't end with .json", result)

			// Result must not contain path separators in the filename
			base := filepath.Base(result)
			assertpkg.False(t, strings.ContainsAny(base, "/\\"), "filename %q contains path separators", base)
		})
	}
}

// newTestServerWithEngine creates a test server with both mock store and mock engine.
func newTestServerWithEngine(t *testing.T, engine *querytest.MockEngine) *Server {
	t.Helper()

	store := &mockStore{
		stats: &StoreStats{
			MessageCount:    10,
			ThreadCount:     5,
			SourceCount:     1,
			LabelCount:      3,
			AttachmentCount: 2,
			DatabaseSize:    1024,
		},
		messages: []APIMessage{
			{
				ID:             1,
				Subject:        "Test Subject",
				From:           "sender@example.com",
				To:             []string{"recipient@example.com"},
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Snippet:        "This is a test message snippet",
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
				Body:           "This is the full message body text.",
			},
		},
		total: 1,
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()

	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     store,
		Engine:    engine,
		Scheduler: sched,
		Logger:    testLogger(),
	})
	return srv
}

func TestHandleAggregates(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	engine := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{
			{Key: "alice@example.com", Count: 100, TotalSize: 50000, AttachmentSize: 10000, AttachmentCount: 5},
			{Key: "bob@example.com", Count: 50, TotalSize: 25000, AttachmentSize: 5000, AttachmentCount: 2},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/aggregates?view_type=senders", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp AggregateResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal("senders", resp.ViewType, "view_type")
	require.Len(resp.Rows, 2, "rows count")
	assert.Equal("alice@example.com", resp.Rows[0].Key, "first row key")
}

func TestHandleAggregatesNoEngine(t *testing.T) {
	// Server without engine
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     nil,
		Engine:    nil,
		Scheduler: sched,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest("GET", "/api/v1/aggregates?view_type=senders", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status")
}

func TestHandleAggregatesInvalidViewType(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/aggregates?view_type=invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleSubAggregates(t *testing.T) {
	assert := assertpkg.New(t)
	engine := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{
			{Key: "INBOX", Count: 80, TotalSize: 40000},
			{Key: "SENT", Count: 20, TotalSize: 10000},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/aggregates/sub?view_type=labels&sender=alice@example.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp AggregateResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal("labels", resp.ViewType, "view_type")
	assert.Len(resp.Rows, 2, "rows count")
}

func TestHandleFilteredMessages(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	var gotFilter query.MessageFilter
	engine := &querytest.MockEngine{
		ListMessagesFunc: func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
			gotFilter = filter
			return []query.MessageSummary{{
				ID:             1,
				Subject:        "Test Email",
				MessageType:    "sms",
				FromEmail:      "alice@example.com",
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
			}}, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/filter?sender=alice@example.com&message_type=sms&limit=100", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "expected messages array in response")
	assert.Len(messages, 1, "messages count")
	require.Equal("sms", gotFilter.MessageType, "message_type filter")
	msg, ok := messages[0].(map[string]any)
	require.True(ok, "message row = %#v, want object", messages[0])
	require.Equal("sms", msg["message_type"], "response message_type")
}

func TestHandleFilteredMessagesIncludesDeletedAt(t *testing.T) {
	require := requirepkg.New(t)
	deletedAt := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:        1,
				Subject:   "Deleted Email",
				FromEmail: "alice@example.com",
				SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				DeletedAt: &deletedAt,
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/filter?limit=100", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "messages = %#v, want array", resp["messages"])
	require.Len(messages, 1, "messages count")

	message, ok := messages[0].(map[string]any)
	require.True(ok, "message = %#v, want object", messages[0])

	require.Equal(deletedAt.Format(time.RFC3339), message["deleted_at"], "deleted_at")
}

func TestHandleFilteredMessagesFormatsPhoneBackedSMSParticipants(t *testing.T) {
	require := requirepkg.New(t)
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:          1,
				Subject:     "",
				MessageType: "sms",
				FromName:    "SMS Sender",
				FromPhone:   "+15551234567",
				To:          []query.Address{{Email: "+15557654321", Name: "Me"}},
				SentAt:      time.Date(2024, 4, 1, 8, 0, 0, 0, time.UTC),
				Snippet:     "known sms snippet",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/filter?message_type=sms&limit=1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp struct {
		Messages []MessageSummary `json:"messages"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Messages, 1, "messages count")
	require.Equal("SMS Sender <+15551234567>", resp.Messages[0].From, "from")
	require.Len(resp.Messages[0].To, 1, "to")
	require.Equal("Me <+15557654321>", resp.Messages[0].To[0], "to[0]")
}

func TestHandleFilteredMessagesFallsBackToContactNameWhenAddressMissing(t *testing.T) {
	require := requirepkg.New(t)
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:          1,
				Subject:     "",
				MessageType: "sms",
				FromName:    "ShortCode Alerts",
				// No email or phone — e.g. a short-code sender stored only via
				// participant_identifiers. Without the fallback the From
				// field would render empty.
				To:      []query.Address{{Name: "Me"}},
				SentAt:  time.Date(2024, 4, 1, 8, 0, 0, 0, time.UTC),
				Snippet: "your code is 123456",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/filter?message_type=sms&limit=1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp struct {
		Messages []MessageSummary `json:"messages"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Messages, 1, "messages count")
	require.Equal("ShortCode Alerts", resp.Messages[0].From, "from")
	require.Len(resp.Messages[0].To, 1, "to")
	require.Equal("Me", resp.Messages[0].To[0], "to[0]")
}

func TestHandleTotalStats(t *testing.T) {
	assert := assertpkg.New(t)
	engine := &querytest.MockEngine{
		Stats: &query.TotalStats{
			MessageCount:    1000,
			TotalSize:       5000000,
			AttachmentCount: 100,
			AttachmentSize:  1000000,
			LabelCount:      10,
			AccountCount:    2,
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/stats/total", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp TotalStatsResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(int64(1000), resp.MessageCount, "message_count")
	assert.Equal(int64(5000000), resp.TotalSize, "total_size")
}

func TestHandleFastSearch(t *testing.T) {
	assert := assertpkg.New(t)
	engine := &querytest.MockEngine{
		SearchFastResults: []query.MessageSummary{
			{
				ID:        1,
				Subject:   "Invoice 12345",
				FromEmail: "billing@example.com",
				SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			},
		},
		Stats: &query.TotalStats{
			MessageCount: 1,
			TotalSize:    1024,
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/fast?q=invoice", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp SearchFastResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal("invoice", resp.Query, "query")
	assert.Len(resp.Messages, 1, "messages count")
}

func TestHandleFastSearchMissingQuery(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/fast", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleFastSearchInvalidViewType(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/fast?q=test&view_type=invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status")

	var errResp map[string]string
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")

	assertpkg.Equal(t, "invalid_view_type", errResp["error"], "error")
}

// TestSearchRejectsMessageTypeFilter guards against silently dropping
// the message_type filter. MergeFilterIntoQuery does not propagate
// MessageType, so accepting it in fast/deep search would let
// /search/fast?q=hello&message_type=sms return unscoped results. Reject
// until the search pipeline gains a message_type predicate.
func TestSearchRejectsMessageTypeFilter(t *testing.T) {
	for _, path := range []string{
		"/api/v1/search/fast?q=hello&message_type=sms",
		"/api/v1/search/deep?q=hello&message_type=sms",
	} {
		t.Run(path, func(t *testing.T) {
			engine := &querytest.MockEngine{}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()

			srv.Router().ServeHTTP(w, req)

			requirepkg.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
			var errResp map[string]string
			requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode error")
			requirepkg.Equal(t, "unsupported_filter", errResp["error"], "error")
		})
	}
}

func TestHandleDeepSearch(t *testing.T) {
	engine := &querytest.MockEngine{
		SearchResults: []query.MessageSummary{
			{
				ID:        1,
				Subject:   "Meeting Notes",
				FromEmail: "team@example.com",
				SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/deep?q=agenda", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assertpkg.Equal(t, "agenda", resp["query"], "query")
}

func TestHandleDeepSearchMissingQuery(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/deep", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status")
}

// mockSQLQueryEngine wraps MockEngine and adds SQLQuerier support.
type mockSQLQueryEngine struct {
	querytest.MockEngine
	queryResult *query.QueryResult
	queryErr    error
}

func (m *mockSQLQueryEngine) QuerySQL(_ context.Context, _ string) (*query.QueryResult, error) {
	return m.queryResult, m.queryErr
}

func TestHandleQuery(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	engine := &mockSQLQueryEngine{
		queryResult: &query.QueryResult{
			Columns:  []string{"from_email"},
			Rows:     [][]any{{"alice@example.com"}},
			RowCount: 1,
		},
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg,
		Engine: engine,
		Logger: testLogger(),
	})

	body := `{"sql": "SELECT from_email FROM v_senders LIMIT 1"}`
	req := httptest.NewRequest("POST", "/api/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var result query.QueryResult
	require.NoError(json.NewDecoder(w.Body).Decode(&result), "failed to decode response")

	assert.Equal(1, result.RowCount, "row_count")
	require.Len(result.Columns, 1, "columns")
	assert.Equal("from_email", result.Columns[0], "columns[0]")
}

func TestHandleSearch_FTSModeUnchanged(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv, _ := newTestServerWithMockStore(t)

	// mode=fts (or unset) should still return the legacy SearchResult shape.
	req := httptest.NewRequest("GET", "/api/v1/search?q=Test&mode=fts", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp SearchResult
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")
	assert.Equal("Test", resp.Query, "query")
	// Legacy shape exposes page + page_size; hybrid shape would not.
	assert.NotZero(resp.Page, "expected page field in FTS response")
}

func TestHandleSearch_HybridModeNotConfigured(t *testing.T) {
	// newTestServerWithMockStore does not inject a HybridEngine, so
	// the server must return 503 for any vector/hybrid query.
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=test&mode=hybrid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assertpkg.Equal(t, "vector_not_enabled", errResp.Error, "error")
}

// newHybridServerForErrorTest constructs an API server with a real
// hybrid.Engine wired around a fakeVectorBackend in the supplied state.
// mainDB is nil because the test queries used here have no operators,
// so BuildFilter never touches it.
func newHybridServerForErrorTest(t *testing.T, backend vector.Backend) *Server {
	t.Helper()
	engine := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "nomic-embed:768",
		RRFK:                60,
		KPerSignal:          10,
	})
	cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}}
	return NewServerWithOptions(ServerOptions{
		Config:       cfg,
		Store:        &mockStore{},
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})
}

// TestHandleSearch_HybridErrIndexBuilding regression-guards the API's
// translation of vector.ErrIndexBuilding from the hybrid engine into a
// 503 with error code "index_building". The engine returns this when
// no active generation exists yet but a build is in progress.
func TestHandleSearch_HybridErrIndexBuilding(t *testing.T) {
	building := &vector.Generation{
		ID: 1, Model: "nomic-embed", Dimension: 768,
		Fingerprint: "nomic-embed:768", State: vector.GenerationBuilding,
	}
	backend := &fakeVectorBackend{building: building}
	srv := newHybridServerForErrorTest(t, backend)

	req := httptest.NewRequest("GET", "/api/v1/search?q=anything&mode=hybrid", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assertpkg.Equal(t, "index_building", errResp.Error, "error")
}

// TestHandleSearch_HybridErrNotEnabled regression-guards the API's
// translation of vector.ErrNotEnabled from the hybrid engine into a
// 503 with error code "vector_not_enabled". The engine returns this
// when no generation exists at all (no active, no building).
func TestHandleSearch_HybridErrNotEnabled(t *testing.T) {
	backend := &fakeVectorBackend{} // no active, no building
	srv := newHybridServerForErrorTest(t, backend)

	req := httptest.NewRequest("GET", "/api/v1/search?q=anything&mode=hybrid", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assertpkg.Equal(t, "vector_not_enabled", errResp.Error, "error")
}

// realEmbedder returns a deterministic single vector per input. Used
// when the test exercises the end-to-end engine path (mode=vector hits
// backend.Search, which requires a query vector to have been produced).
type realEmbedder struct {
	dim int
}

func (e realEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, e.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

// blockingEmbedder waits for ctx cancellation, then returns ctx.Err().
// Used to simulate a slow embedder that must be cancelled by the
// request-scoped timeout to terminate.
type blockingEmbedder struct{}

func (blockingEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestHandleSearch_HybridEmbeddingTimeoutFiresChi regresses the
// concern that chi/v5's request Timeout middleware would preempt our
// structured 503 embedding_timeout response. chi v5's Timeout is a
// "gentle" cancellation: it wraps the request context with a deadline
// and, in a deferred function, conditionally writes a 504 — but only
// AFTER the inline handler returns. Because handlers run inline (not
// in a separate goroutine via http.TimeoutHandler), our handler sees
// ctx.DeadlineExceeded from the embed call, the engine wraps it as
// vector.ErrEmbeddingTimeout, the handler writes 503 embedding_timeout
// JSON, and chi's deferred WriteHeader(504) is a no-op against the
// already-written response.
//
// The test sets a tight RequestTimeout so the chi middleware fires
// during the embed call. If a future chi version switches to a
// preemptive timeout (or http.TimeoutHandler is reintroduced), this
// test will fail because the response would be a bare 504 instead of
// the structured 503.
func TestHandleSearch_HybridEmbeddingTimeoutFiresChi(t *testing.T) {
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
	}
	engine := hybrid.NewEngine(backend, nil, blockingEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          &mockStore{},
		HybridEngine:   engine,
		Backend:        backend,
		Logger:         testLogger(),
		RequestTimeout: 100 * time.Millisecond,
	})

	req := httptest.NewRequest("GET", "/api/v1/search?q=meeting&mode=hybrid", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assertpkg.Equal(t, "embedding_timeout", errResp.Error,
		"error (chi may have preempted with a bare 504)")
}

// TestHandleSearch_HybridFilterOnlyReturnsBadRequest regression-guards
// the spec contract that mode=vector|hybrid requires at least one
// free-text term to embed. A query containing only operators (e.g.
// `from:alice@example.com`) parses to an empty TextTerms slice; the
// handler must reject this with 400 missing_free_text rather than
// passing an empty string into the embedder.
func TestHandleSearch_HybridFilterOnlyReturnsBadRequest(t *testing.T) {
	// Construct a real engine so the handler progresses past the
	// "vector_not_enabled" check before evaluating freeText.
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
	}
	engine := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        &mockStore{},
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest("GET",
		"/api/v1/search?q=from:alice@example.com&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assertpkg.Equal(t, "missing_free_text", errResp.Error, "error")
}

// TestHandleSearch_HybridResponseItemShape regression-guards the
// hybrid response item shape: each result must be a MessageSummary
// (snake-case fields shared with /api/v1/search FTS mode), not a
// bespoke object that diverges from the legacy summary surface.
// Catches regressions where the embedded type or omitempty rules
// drift away from MessageSummary.
func TestHandleSearch_HybridResponseItemShape(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	deletedAt := time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)
	store := &mockStore{
		messages: []APIMessage{{
			ID:             42,
			ConversationID: 7,
			Subject:        "Quarterly Plan",
			From:           "alice@example.com",
			To:             []string{"bob@example.com"},
			Cc:             []string{"carol@example.com"},
			SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			DeletedAt:      &deletedAt,
			Snippet:        "discussion of Q1 priorities",
			Labels:         []string{"INBOX", "Work"},
			HasAttachments: true,
			SizeEstimate:   2048,
		}},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 42, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest("GET", "/api/v1/search?q=quarterly&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	// Decode into a raw map so we can verify field names (snake-case
	// keys) and that no unexpected wrapper is present.
	var resp struct {
		Mode    string           `json:"mode"`
		Results []map[string]any `json:"results"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal("vector", resp.Mode, "mode")
	require.Len(resp.Results, 1, "results len")
	got := resp.Results[0]
	// Required MessageSummary fields. Score must be ABSENT (no explain=1).
	wantKeys := []string{
		"id", "conversation_id", "subject", "from", "to", "cc",
		"sent_at", "deleted_at", "snippet", "labels",
		"has_attachments", "size_bytes",
	}
	for _, k := range wantKeys {
		assert.Contains(got, k, "missing key %q in hybrid result item, got keys: %v", k, mapKeys(got))
	}
	assert.NotContains(got, "score", "'score' key present without explain=1, got %v", got["score"])
	// Spot-check a couple of values to make sure it's the same message.
	id, _ := got["id"].(float64)
	assert.Equal(int64(42), int64(id), "id")
	subj, _ := got["subject"].(string)
	assert.Equal("Quarterly Plan", subj, "subject")
	hasA, _ := got["has_attachments"].(bool)
	assert.True(hasA, "has_attachments")
}

// TestHandleSearch_HybridUsesBulkHydration regresses the N+1 bug
// where each hit triggered its own GetMessage call (which fetches
// body, all four recipient sets, labels, and attachments per id —
// roughly 7 queries per hit). Hybrid search must instead make a
// single GetMessagesSummariesByIDs call carrying every hit's id.
func TestHandleSearch_HybridUsesBulkHydration(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "first", From: "a@x", Snippet: "..."},
			{ID: 2, Subject: "second", From: "b@x", Snippet: "..."},
			{ID: 3, Subject: "third", From: "c@x", Snippet: "..."},
		},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: 0.9, Rank: 1},
			{MessageID: 2, Score: 0.8, Rank: 2},
			{MessageID: 3, Score: 0.7, Rank: 3},
		},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest("GET", "/api/v1/search?q=test&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(int32(0), store.getMessageCalls.Load(),
		"GetMessage call count, want 0 (must use bulk hydration, not per-hit)")
	assert.Equal(int32(1), store.getSummariesByIDsCalls.Load(),
		"GetMessagesSummariesByIDs call count, want 1 (single bulk lookup)")
	wantIDs := []int64{1, 2, 3}
	require.Equal(wantIDs, store.getSummariesByIDsLastIDs, "getSummariesByIDs last ids")
}

// TestHandleSearch_HybridPoolSaturatedAlwaysEmitted regression-guards
// the wire-level contract that pool_saturated is always present on a
// successful hybrid response (never omitted, never null). Without an
// explicit test, an `omitempty` slip on the response struct would
// silently drop the field for false values — clients that read
// "pool not saturated" as a positive signal would break.
func TestHandleSearch_HybridPoolSaturatedAlwaysEmitted(t *testing.T) {
	require := requirepkg.New(t)
	store := &mockStore{}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		// No hits → pool_saturated will be false (len(hits) < limit).
		searchHits: nil,
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest("GET", "/api/v1/search?q=hello&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode raw")
	val, exists := raw["pool_saturated"]
	require.True(exists, "pool_saturated key missing from successful response; want present (raw=%s)", w.Body.String())
	assertpkg.Equal(t, "false", string(val), "pool_saturated")
}

// mapKeys returns the keys of a map[string]interface{} for use in
// assertion error messages.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestHandleSearch_HybridModePaginationUnsupported(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=test&mode=vector&page=2", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assertpkg.Equal(t, "pagination_unsupported", errResp.Error, "error")
}

func TestHandleSearch_UnknownMode(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=test&mode=bogus", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assertpkg.Equal(t, "invalid_mode", errResp.Error, "error")
}

func TestHandleQuery_SQLiteEngine503(t *testing.T) {
	engine := query.NewSQLiteEngine(nil)

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg,
		Engine: engine,
		Logger: testLogger(),
	})

	body := `{"sql": "SELECT 1"}`
	req := httptest.NewRequest("POST", "/api/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assertpkg.Equal(t, "engine_unavailable", errResp.Error, "error")
}

// fakeVectorBackend is a test stub implementing vector.Backend. Tests
// that need canned ANN hits set searchHits/searchErr; Stats and the
// generation-resolution paths use the other fields.
type fakeVectorBackend struct {
	active     *vector.Generation
	building   *vector.Generation
	stats      vector.Stats
	searchHits []vector.Hit
	searchErr  error
}

func (f *fakeVectorBackend) CreateGeneration(_ context.Context, _ string, _ int, _ string) (vector.GenerationID, error) {
	return 0, errors.New("not implemented")
}
func (f *fakeVectorBackend) ActivateGeneration(_ context.Context, _ vector.GenerationID) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) RetireGeneration(_ context.Context, _ vector.GenerationID) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) ActiveGeneration(_ context.Context) (vector.Generation, error) {
	if f.active == nil {
		return vector.Generation{}, vector.ErrNoActiveGeneration
	}
	return *f.active, nil
}
func (f *fakeVectorBackend) BuildingGeneration(_ context.Context) (*vector.Generation, error) {
	return f.building, nil
}
func (f *fakeVectorBackend) Upsert(_ context.Context, _ vector.GenerationID, _ []vector.Chunk) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) Search(
	_ context.Context, _ vector.GenerationID, _ []float32, _ int, _ vector.Filter,
) ([]vector.Hit, error) {
	return f.searchHits, f.searchErr
}
func (f *fakeVectorBackend) Delete(_ context.Context, _ vector.GenerationID, _ []int64) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) Stats(_ context.Context, _ vector.GenerationID) (vector.Stats, error) {
	return f.stats, nil
}
func (f *fakeVectorBackend) LoadVector(_ context.Context, _ int64) ([]float32, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeVectorBackend) Close() error { return nil }
func (f *fakeVectorBackend) EnsureSeeded(_ context.Context, _ vector.GenerationID) error {
	return errors.New("not implemented")
}

func TestHandleStats_VectorDisabled(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	// newTestServerWithMockStore uses NewServer (no Backend), so backend == nil.

	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	// Parse raw JSON to verify "vector_search" is absent entirely.
	var raw map[string]json.RawMessage
	requirepkg.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw), "decode")
	assertpkg.NotContains(t, raw, "vector_search",
		"expected 'vector_search' to be absent from JSON when backend is nil")
}

func TestHandleStats_VectorEnabledWithActive(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID:          5,
			Model:       "nomic-embed",
			Dimension:   768,
			Fingerprint: "nomic-embed:768",
			State:       vector.GenerationActive,
		},
		stats: vector.Stats{EmbeddingCount: 100, PendingCount: 7, StorageBytes: 4096},
	}

	store := &mockStore{
		stats: &StoreStats{
			MessageCount: 100,
			ThreadCount:  50,
			SourceCount:  1,
		},
	}
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     store,
		Backend:   backend,
		Scheduler: sched,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	vs, ok := resp["vector_search"].(map[string]any)
	require.True(ok, "expected 'vector_search' object, got %T: %v", resp["vector_search"], resp["vector_search"])

	assert.Equal(true, vs["enabled"], "enabled")
	assert.Equal(float64(7), vs["pending_embeddings_total"], "pending_embeddings_total")

	active, ok := vs["active_generation"].(map[string]any)
	require.True(ok, "expected 'vector_search.active_generation' object, got %T", vs["active_generation"])

	assert.Equal(float64(100), active["message_count"], "message_count")
	assert.Equal("nomic-embed", active["model"], "model")
	assert.Equal(float64(5), active["id"], "id")
	assert.Equal(float64(768), active["dimension"], "dimension")
	assert.Equal("nomic-embed:768", active["fingerprint"], "fingerprint")
	assert.Equal("active", active["state"], "state")

	assert.NotContains(vs, "building_generation",
		"expected 'building_generation' to be absent when there is no building generation")
}

// inlineURL builds the inline endpoint URL for a given message ID and CID,
// URL-encoding the CID so values with reserved characters (including `/`)
// round-trip correctly.
func inlineURL(id int64, cid string) string {
	return fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id, url.QueryEscape(cid))
}

// rawMIMEWithImagePart returns a multipart MIME message containing a single
// image part with the given Content-ID, content type, and Content-Disposition
// (typically "inline" or "attachment").
func rawMIMEWithImagePart(cid, contentType, disposition string, body []byte) []byte {
	boundary := "test-boundary-123"
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/related; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("Subject: test\r\n")
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString("<html><body><img src=\"cid:" + cid + "\"></body></html>\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: " + contentType + "\r\n")
	b.WriteString("Content-Disposition: " + disposition + "\r\n")
	b.WriteString("Content-ID: <" + cid + ">\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString(body))
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

// rawMIMEWithInlineImage is a convenience wrapper for the common inline case.
func rawMIMEWithInlineImage(cid, contentType string, body []byte) []byte {
	return rawMIMEWithImagePart(cid, contentType, "inline", body)
}

func TestHandleMessageInline_ImagePNG(t *testing.T) {
	assert := assertpkg.New(t)
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithInlineImage("logo@example", "image/png", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, "logo@example"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("image/png", w.Header().Get("Content-Type"), "Content-Type")
	cc := w.Header().Get("Cache-Control")
	assert.Contains(cc, "private", "Cache-Control should contain 'private'")
	assert.NotContains(cc, "public", "Cache-Control must not contain 'public'")
	assert.Equal("inline", w.Header().Get("Content-Disposition"), "Content-Disposition")
	assert.Equal(imgData, w.Body.Bytes(), "response body")
}

// TestHandleMessageInline_NonInlineSkipped ensures that a part with a matching
// Content-ID but Content-Disposition: attachment is not served via the inline
// endpoint — only parts flagged IsInline by the MIME parser should be reachable.
func TestHandleMessageInline_NonInlineSkipped(t *testing.T) {
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithImagePart("logo@example", "image/png", "attachment", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, "logo@example"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusNotFound, w.Code, "status for non-inline attachment")
}

func TestHandleMessageInline_RejectsXHTML(t *testing.T) {
	raw := rawMIMEWithInlineImage("evil@nasty", "application/xhtml+xml", []byte("<script>alert(1)</script>"))

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, "evil@nasty"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusUnsupportedMediaType, w.Code, "status for application/xhtml+xml inline part")
}

func TestHandleMessageInline_RejectsSVG(t *testing.T) {
	raw := rawMIMEWithInlineImage("vuln@svg", "image/svg+xml", []byte("<svg onload='alert(1)'/>"))

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, "vuln@svg"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusUnsupportedMediaType, w.Code, "status for image/svg+xml inline part")
}

func TestHandleMessageInline_CIDNotFound(t *testing.T) {
	raw := rawMIMEWithInlineImage("logo@example", "image/png", []byte{0x89, 'P', 'N', 'G'})

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, "nonexistent@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestHandleMessageInline_NoEngine(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", inlineURL(1, "any@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusServiceUnavailable, w.Code, "status")
}

func TestHandleMessageInline_MessageNotFound(t *testing.T) {
	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(999, "any@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusNotFound, w.Code, "status")
}

// TestHandleMessageInline_CIDWithSlash verifies that Content-IDs containing
// `/` round-trip correctly through the query parameter.
func TestHandleMessageInline_CIDWithSlash(t *testing.T) {
	cid := "path/with/slashes@example.com"
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithInlineImage(cid, "image/png", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, cid), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assertpkg.Equal(t, imgData, w.Body.Bytes(), "response body")
}

// TestHandleMessageInline_MissingCID verifies that a request without the
// `cid` query parameter returns 400.
func TestHandleMessageInline_MissingCID(t *testing.T) {
	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: rawMIMEWithInlineImage("logo@example", "image/png", []byte{0x89})},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/1/inline", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertpkg.Equal(t, http.StatusBadRequest, w.Code, "status for missing cid")
}

// TestHandleMessageInline_UnsupportedEngine verifies that engines which
// can't fetch raw MIME (Postgres scaffold, remote engine) surface a stable
// 501 instead of a generic 500.
func TestHandleMessageInline_UnsupportedEngine(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"ErrNotImplemented", query.ErrNotImplemented},
		{"ErrNotSupported", remote.ErrNotSupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &querytest.MockEngine{
				GetMessageRawFunc: func(_ context.Context, _ int64) ([]byte, error) {
					return nil, tc.err
				},
			}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest("GET", inlineURL(1, "logo@example"), nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assertpkg.Equal(t, http.StatusNotImplemented, w.Code, "status")
		})
	}
}

// TestHandleGetMessage_EngineUnsupportedFallsBackToStore verifies that when
// the configured engine reports the operation is unsupported, the handler
// falls through to the store path so engine-only errors don't break detail
// responses for engines that don't implement GetMessage.
func TestHandleGetMessage_EngineUnsupportedFallsBackToStore(t *testing.T) {
	engine := &querytest.MockEngine{
		GetMessageFunc: func(_ context.Context, _ int64) (*query.MessageDetail, error) {
			return nil, query.ErrNotImplemented
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	requirepkg.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp map[string]any
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assertpkg.Equal(t, "Test Subject", resp["subject"], "subject (store path response)")
}
