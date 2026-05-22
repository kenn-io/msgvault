package api

import (
	"bytes"
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
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	bodyBytes := w.Body.Bytes()

	var resp StatsResponse
	if err := json.Unmarshal(bodyBytes, &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TotalMessages != 10 {
		t.Errorf("total_messages = %d, want 10", resp.TotalMessages)
	}
	if resp.TotalAccounts != 1 {
		t.Errorf("total_accounts = %d, want 1", resp.TotalAccounts)
	}

	// No backend wired → vector_search field must be ABSENT, not
	// null. Re-decode into raw RawMessage so we distinguish the two:
	// `omitempty` plus a nil pointer drops the key entirely, while
	// any encoder bug that emits `"vector_search": null` would still
	// leave resp.VectorSearch == nil.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, exists := raw["vector_search"]; exists {
		t.Errorf("vector_search key present in JSON; want omitted entirely (raw=%s)", string(raw["vector_search"]))
	}
}

func TestHandleListMessages(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	messages, ok := resp["messages"].([]interface{})
	if !ok {
		t.Fatal("expected messages array in response")
	}

	if len(messages) == 0 {
		t.Error("expected at least 1 message")
	}

	// Check first message structure
	msg := messages[0].(map[string]interface{})
	if msg["subject"] != "Test Subject" {
		t.Errorf("subject = %v, want 'Test Subject'", msg["subject"])
	}

	// Verify RFC3339 time format
	sentAt := msg["sent_at"].(string)
	if _, err := time.Parse(time.RFC3339, sentAt); err != nil {
		t.Errorf("sent_at %q is not RFC3339: %v", sentAt, err)
	}
}

func TestHandleListMessagesPagination(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages?page=1&page_size=10", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["page"] != float64(1) {
		t.Errorf("page = %v, want 1", resp["page"])
	}
	if resp["page_size"] != float64(10) {
		t.Errorf("page_size = %v, want 10", resp["page_size"])
	}
}

func TestHandleGetMessage(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp MessageDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != 1 {
		t.Errorf("id = %d, want 1", resp.ID)
	}
	if resp.Subject != "Test Subject" {
		t.Errorf("subject = %q, want 'Test Subject'", resp.Subject)
	}
	if resp.Body != "This is the full message body text." {
		t.Errorf("body = %q, want 'This is the full message body text.'", resp.Body)
	}
}

func TestHandleGetMessageNotFound(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages/99999", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleGetMessageInvalidID(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages/invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleGetMessage_EngineBodyHTML(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp["body"] != "plain fallback" {
		t.Errorf("body = %q, want %q", resp["body"], "plain fallback")
	}
	if resp["body_html"] != "<p>Hello</p>" {
		t.Errorf("body_html = %q, want %q", resp["body_html"], "<p>Hello</p>")
	}
	if resp["subject"] != "HTML Email" {
		t.Errorf("subject = %q, want %q", resp["subject"], "HTML Email")
	}
	if resp["from"] != "Sender <sender@example.com>" {
		t.Errorf("from = %q, want %q", resp["from"], "Sender <sender@example.com>")
	}
	if _, ok := resp["deleted_at"]; ok {
		t.Errorf("deleted_at should be omitted for live message, got %v", resp["deleted_at"])
	}
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	want := deletedAt.Format(time.RFC3339)
	if got := resp["deleted_at"]; got != want {
		t.Errorf("deleted_at = %v, want %q", got, want)
	}
}

func TestHandleSearchMissingQuery(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSearch(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=Test", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp SearchResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Query != "Test" {
		t.Errorf("query = %q, want 'Test'", resp.Query)
	}
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

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
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

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
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

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestErrorResponseShape(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	// Test with invalid ID to get a 400 error
	req := httptest.NewRequest("GET", "/api/v1/messages/invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if resp.Error == "" {
		t.Error("expected error code in response")
	}
	if resp.Message == "" {
		t.Error("expected error message in response")
	}
}

func TestMessageSummaryNilSlices(t *testing.T) {
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
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	messages := resp["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})

	// "to" should be an empty array, not null
	to, ok := msg["to"].([]interface{})
	if !ok {
		t.Fatalf("expected 'to' to be an array, got %T", msg["to"])
	}
	if len(to) != 0 {
		t.Errorf("expected empty 'to' array, got %v", to)
	}

	labels, ok := msg["labels"].([]interface{})
	if !ok {
		t.Fatalf("expected 'labels' to be an array, got %T", msg["labels"])
	}
	if len(labels) != 0 {
		t.Errorf("expected empty 'labels' array, got %v", labels)
	}
}

func TestMessageSummaryCcBccInResponse(t *testing.T) {
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].Cc = []string{"cc1@example.com", "cc2@example.com"}
	ms.messages[0].Bcc = []string{"bcc@example.com"}

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	msg := resp["messages"].([]interface{})[0].(map[string]interface{})

	ccRaw, ok := msg["cc"].([]interface{})
	if !ok {
		t.Fatalf("expected 'cc' array, got %T", msg["cc"])
	}
	var gotCc []string
	for _, v := range ccRaw {
		gotCc = append(gotCc, v.(string))
	}
	slices.Sort(gotCc)
	wantCc := []string{"cc1@example.com", "cc2@example.com"}
	if !slices.Equal(gotCc, wantCc) {
		t.Errorf("cc = %v, want %v", gotCc, wantCc)
	}

	bcc, ok := msg["bcc"].([]interface{})
	if !ok {
		t.Fatalf("expected 'bcc' array, got %T", msg["bcc"])
	}
	if len(bcc) != 1 || bcc[0] != "bcc@example.com" {
		t.Errorf("bcc = %v, want [bcc@example.com]", bcc)
	}
}

func TestMessageSummaryCcBccOmittedWhenEmpty(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	// Parse raw JSON to check field presence
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var messages []json.RawMessage
	if err := json.Unmarshal(raw["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(messages[0], &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}

	if _, exists := msg["cc"]; exists {
		t.Error("expected 'cc' to be omitted from JSON when empty")
	}
	if _, exists := msg["bcc"]; exists {
		t.Error("expected 'bcc' to be omitted from JSON when empty")
	}
}

func TestGetMessageCcBccInResponse(t *testing.T) {
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].Cc = []string{"cc@example.com"}
	ms.messages[0].Bcc = []string{"bcc@example.com"}

	req := httptest.NewRequest("GET", "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp MessageDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Cc) != 1 || resp.Cc[0] != "cc@example.com" {
		t.Errorf("cc = %v, want [cc@example.com]", resp.Cc)
	}
	if len(resp.Bcc) != 1 || resp.Bcc[0] != "bcc@example.com" {
		t.Errorf("bcc = %v, want [bcc@example.com]", resp.Bcc)
	}
}

func TestHandleUploadToken(t *testing.T) {
	// Create temp directory for tokens
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
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

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Verify token file was created
	tokenPath := filepath.Join(tmpDir, "tokens", "test@gmail.com.json")
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		t.Errorf("token file was not created at %s", tokenPath)
	}
}

func TestHandleUploadToken_PreservesClientID(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
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

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Read back the saved token and verify client_id was preserved
	tokenPath := filepath.Join(tmpDir, "tokens", "test@gmail.com.json")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}

	var saved struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("unmarshal saved token: %v", err)
	}
	if saved.ClientID != "myapp.apps.googleusercontent.com" {
		t.Errorf("client_id = %q, want myapp.apps.googleusercontent.com", saved.ClientID)
	}
}

func TestHandleUploadTokenInvalidJSON(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
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

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Error != "invalid_json" {
		t.Errorf("error = %q, want 'invalid_json'", resp.Error)
	}
}

func TestHandleUploadTokenMissingRefreshToken(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-test-tokens-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
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

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Error != "invalid_token" {
		t.Errorf("error = %q, want 'invalid_token'", resp.Error)
	}
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

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d for email %q", w.Code, http.StatusBadRequest, tc.email)
			}
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
	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 404 or 400", w.Code)
	}
}

func TestHandleAddAccount(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-test-config-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
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

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Verify account was added to config
	if len(cfg.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(cfg.Accounts))
	}
	if cfg.Accounts[0].Email != "new@gmail.com" {
		t.Errorf("email = %q, want 'new@gmail.com'", cfg.Accounts[0].Email)
	}

	// Verify scheduler was notified
	if len(sched.addedAccts) != 1 || sched.addedAccts[0] != "new@gmail.com" {
		t.Errorf("scheduler.AddAccount not called, addedAccts = %v", sched.addedAccts)
	}
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "exists" {
		t.Errorf("status = %q, want 'exists'", resp["status"])
	}
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

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "invalid_schedule" {
		t.Errorf("error = %q, want 'invalid_schedule'", resp.Error)
	}
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

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
		})
	}
}

func TestHandleAddAccountSaveFailure(t *testing.T) {
	// Point HomeDir to a file (not a directory) so Save() fails
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0600); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}

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

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}

	// In-memory state should be rolled back
	if len(cfg.Accounts) != 0 {
		t.Errorf("cfg.Accounts has %d entries, want 0 (rollback failed)", len(cfg.Accounts))
	}
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
			if !strings.HasPrefix(cleanResult, cleanTokensDir+string(os.PathSeparator)) {
				t.Errorf("path %q escapes tokensDir %q", result, tokensDir)
			}

			// Result must end with .json
			if !strings.HasSuffix(result, ".json") {
				t.Errorf("path %q doesn't end with .json", result)
			}

			// Result must not contain path separators in the filename
			base := filepath.Base(result)
			if strings.ContainsAny(base, "/\\") {
				t.Errorf("filename %q contains path separators", base)
			}
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp AggregateResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ViewType != "senders" {
		t.Errorf("view_type = %q, want 'senders'", resp.ViewType)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("rows count = %d, want 2", len(resp.Rows))
	}
	if resp.Rows[0].Key != "alice@example.com" {
		t.Errorf("first row key = %q, want 'alice@example.com'", resp.Rows[0].Key)
	}
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

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleAggregatesInvalidViewType(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/aggregates?view_type=invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSubAggregates(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp AggregateResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ViewType != "labels" {
		t.Errorf("view_type = %q, want 'labels'", resp.ViewType)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("rows count = %d, want 2", len(resp.Rows))
	}
}

func TestHandleFilteredMessages(t *testing.T) {
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:             1,
				Subject:        "Test Email",
				FromEmail:      "alice@example.com",
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/messages/filter?sender=alice@example.com&limit=100", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	messages, ok := resp["messages"].([]interface{})
	if !ok {
		t.Fatal("expected messages array in response")
	}
	if len(messages) != 1 {
		t.Errorf("messages count = %d, want 1", len(messages))
	}
}

func TestHandleFilteredMessagesIncludesDeletedAt(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	messages, ok := resp["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want single message", resp["messages"])
	}

	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", messages[0])
	}

	if got := message["deleted_at"]; got != deletedAt.Format(time.RFC3339) {
		t.Fatalf("deleted_at = %#v, want %q", got, deletedAt.Format(time.RFC3339))
	}
}

func TestHandleTotalStats(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp TotalStatsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.MessageCount != 1000 {
		t.Errorf("message_count = %d, want 1000", resp.MessageCount)
	}
	if resp.TotalSize != 5000000 {
		t.Errorf("total_size = %d, want 5000000", resp.TotalSize)
	}
}

func TestHandleFastSearch(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SearchFastResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Query != "invoice" {
		t.Errorf("query = %q, want 'invoice'", resp.Query)
	}
	if len(resp.Messages) != 1 {
		t.Errorf("messages count = %d, want 1", len(resp.Messages))
	}
}

func TestHandleFastSearchMissingQuery(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/fast", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleFastSearchInvalidViewType(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/fast?q=test&view_type=invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var errResp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp["error"] != "invalid_view_type" {
		t.Errorf("error = %q, want 'invalid_view_type'", errResp["error"])
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["query"] != "agenda" {
		t.Errorf("query = %v, want 'agenda'", resp["query"])
	}
}

func TestHandleDeepSearchMissingQuery(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", "/api/v1/search/deep", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var result query.QueryResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result.RowCount != 1 {
		t.Errorf("row_count = %d, want 1", result.RowCount)
	}
	if len(result.Columns) != 1 || result.Columns[0] != "from_email" {
		t.Errorf("columns = %v, want [from_email]", result.Columns)
	}
}

func TestHandleSearch_FTSModeUnchanged(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	// mode=fts (or unset) should still return the legacy SearchResult shape.
	req := httptest.NewRequest("GET", "/api/v1/search?q=Test&mode=fts", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SearchResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Query != "Test" {
		t.Errorf("query = %q, want 'Test'", resp.Query)
	}
	// Legacy shape exposes page + page_size; hybrid shape would not.
	if resp.Page == 0 {
		t.Errorf("expected page field in FTS response, got 0")
	}
}

func TestHandleSearch_HybridModeNotConfigured(t *testing.T) {
	// newTestServerWithMockStore does not inject a HybridEngine, so
	// the server must return 503 for any vector/hybrid query.
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=test&mode=hybrid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d, body: %s",
			w.Code, http.StatusServiceUnavailable, w.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error != "vector_not_enabled" {
		t.Errorf("error = %q, want 'vector_not_enabled'", errResp.Error)
	}
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

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error != "index_building" {
		t.Errorf("error = %q, want 'index_building'", errResp.Error)
	}
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

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error != "vector_not_enabled" {
		t.Errorf("error = %q, want 'vector_not_enabled'", errResp.Error)
	}
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

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error != "embedding_timeout" {
		t.Errorf("error = %q, want 'embedding_timeout' (chi may have preempted with a bare 504)", errResp.Error)
	}
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

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error != "missing_free_text" {
		t.Errorf("error = %q, want 'missing_free_text'", errResp.Error)
	}
}

// TestHandleSearch_HybridResponseItemShape regression-guards the
// hybrid response item shape: each result must be a MessageSummary
// (snake-case fields shared with /api/v1/search FTS mode), not a
// bespoke object that diverges from the legacy summary surface.
// Catches regressions where the embedded type or omitempty rules
// drift away from MessageSummary.
func TestHandleSearch_HybridResponseItemShape(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Decode into a raw map so we can verify field names (snake-case
	// keys) and that no unexpected wrapper is present.
	var resp struct {
		Mode    string                   `json:"mode"`
		Results []map[string]interface{} `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mode != "vector" {
		t.Errorf("mode = %q, want 'vector'", resp.Mode)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	got := resp.Results[0]
	// Required MessageSummary fields. Score must be ABSENT (no explain=1).
	wantKeys := []string{
		"id", "conversation_id", "subject", "from", "to", "cc",
		"sent_at", "deleted_at", "snippet", "labels",
		"has_attachments", "size_bytes",
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in hybrid result item, got keys: %v", k, mapKeys(got))
		}
	}
	if _, ok := got["score"]; ok {
		t.Errorf("'score' key present without explain=1, got %v", got["score"])
	}
	// Spot-check a couple of values to make sure it's the same message.
	if id, _ := got["id"].(float64); int64(id) != 42 {
		t.Errorf("id = %v, want 42", got["id"])
	}
	if subj, _ := got["subject"].(string); subj != "Quarterly Plan" {
		t.Errorf("subject = %v, want 'Quarterly Plan'", got["subject"])
	}
	if hasA, _ := got["has_attachments"].(bool); !hasA {
		t.Errorf("has_attachments = %v, want true", got["has_attachments"])
	}
}

// TestHandleSearch_HybridUsesBulkHydration regresses the N+1 bug
// where each hit triggered its own GetMessage call (which fetches
// body, all four recipient sets, labels, and attachments per id —
// roughly 7 queries per hit). Hybrid search must instead make a
// single GetMessagesSummariesByIDs call carrying every hit's id.
func TestHandleSearch_HybridUsesBulkHydration(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := store.getMessageCalls.Load(); got != 0 {
		t.Errorf("GetMessage call count = %d, want 0 (must use bulk hydration, not per-hit)", got)
	}
	if got := store.getSummariesByIDsCalls.Load(); got != 1 {
		t.Errorf("GetMessagesSummariesByIDs call count = %d, want 1 (single bulk lookup)", got)
	}
	wantIDs := []int64{1, 2, 3}
	if len(store.getSummariesByIDsLastIDs) != len(wantIDs) {
		t.Fatalf("getSummariesByIDs last ids = %v, want %v", store.getSummariesByIDsLastIDs, wantIDs)
	}
	for i, want := range wantIDs {
		if store.getSummariesByIDsLastIDs[i] != want {
			t.Errorf("getSummariesByIDs last ids[%d] = %d, want %d", i, store.getSummariesByIDsLastIDs[i], want)
		}
	}
}

// TestHandleSearch_HybridPoolSaturatedAlwaysEmitted regression-guards
// the wire-level contract that pool_saturated is always present on a
// successful hybrid response (never omitted, never null). Without an
// explicit test, an `omitempty` slip on the response struct would
// silently drop the field for false values — clients that read
// "pool not saturated" as a positive signal would break.
func TestHandleSearch_HybridPoolSaturatedAlwaysEmitted(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	val, exists := raw["pool_saturated"]
	if !exists {
		t.Fatalf("pool_saturated key missing from successful response; want present (raw=%s)", w.Body.String())
	}
	if string(val) != "false" {
		t.Errorf("pool_saturated = %s, want false", string(val))
	}
}

// mapKeys returns the keys of a map[string]interface{} for use in
// assertion error messages.
func mapKeys(m map[string]interface{}) []string {
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

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s",
			w.Code, http.StatusBadRequest, w.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error != "pagination_unsupported" {
		t.Errorf("error = %q, want 'pagination_unsupported'", errResp.Error)
	}
}

func TestHandleSearch_UnknownMode(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", "/api/v1/search?q=test&mode=bogus", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s",
			w.Code, http.StatusBadRequest, w.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error != "invalid_mode" {
		t.Errorf("error = %q, want 'invalid_mode'", errResp.Error)
	}
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

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d, body: %s",
			w.Code, http.StatusServiceUnavailable, w.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error != "engine_unavailable" {
		t.Errorf("error = %q, want 'engine_unavailable'", errResp.Error)
	}
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

func (f *fakeVectorBackend) CreateGeneration(_ context.Context, _ string, _ int) (vector.GenerationID, error) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Parse raw JSON to verify "vector_search" is absent entirely.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := raw["vector_search"]; exists {
		t.Error("expected 'vector_search' to be absent from JSON when backend is nil")
	}
}

func TestHandleStats_VectorEnabledWithActive(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	vs, ok := resp["vector_search"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'vector_search' object, got %T: %v", resp["vector_search"], resp["vector_search"])
	}

	if got := vs["enabled"]; got != true {
		t.Errorf("enabled = %v, want true", got)
	}
	if got := vs["pending_embeddings_total"]; got != float64(7) {
		t.Errorf("pending_embeddings_total = %v, want 7", got)
	}

	active, ok := vs["active_generation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'vector_search.active_generation' object, got %T", vs["active_generation"])
	}

	if got := active["message_count"]; got != float64(100) {
		t.Errorf("message_count = %v, want 100", got)
	}
	if got := active["model"]; got != "nomic-embed" {
		t.Errorf("model = %v, want 'nomic-embed'", got)
	}
	if got := active["id"]; got != float64(5) {
		t.Errorf("id = %v, want 5", got)
	}
	if got := active["dimension"]; got != float64(768) {
		t.Errorf("dimension = %v, want 768", got)
	}
	if got := active["fingerprint"]; got != "nomic-embed:768" {
		t.Errorf("fingerprint = %v, want 'nomic-embed:768'", got)
	}
	if got := active["state"]; got != "active" {
		t.Errorf("state = %v, want 'active'", got)
	}

	if _, exists := vs["building_generation"]; exists {
		t.Error("expected 'building_generation' to be absent when there is no building generation")
	}
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
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithInlineImage("logo@example", "image/png", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(1, "logo@example"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, should contain 'private'", cc)
	}
	if cc := w.Header().Get("Cache-Control"); strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q, must not contain 'public'", cc)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("Content-Disposition = %q, want 'inline'", cd)
	}
	if !bytes.Equal(w.Body.Bytes(), imgData) {
		t.Errorf("response body = %x, want %x", w.Body.Bytes(), imgData)
	}
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

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d for non-inline attachment", w.Code, http.StatusNotFound)
	}
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

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d for application/xhtml+xml inline part", w.Code, http.StatusUnsupportedMediaType)
	}
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

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d for image/svg+xml inline part", w.Code, http.StatusUnsupportedMediaType)
	}
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

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleMessageInline_NoEngine(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest("GET", inlineURL(1, "any@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleMessageInline_MessageNotFound(t *testing.T) {
	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest("GET", inlineURL(999, "any@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), imgData) {
		t.Errorf("response body = %x, want %x", w.Body.Bytes(), imgData)
	}
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

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for missing cid", w.Code, http.StatusBadRequest)
	}
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

			if w.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
			}
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["subject"] != "Test Subject" {
		t.Errorf("subject = %q, want %q (store path response)", resp["subject"], "Test Subject")
	}
}
